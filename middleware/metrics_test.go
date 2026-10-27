package middleware_test

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/middleware"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/telemetry"
	"github.com/nostosx/agentx/tool"
)

type measurement struct {
	value float64
	attrs map[string]any
	ctx   context.Context
}

// recordingMeter records every measurement by instrument name.
type recordingMeter struct {
	mu          sync.Mutex
	instruments map[string]telemetry.Instrument
	points      map[string][]measurement
}

func newRecordingMeter() *recordingMeter {
	return &recordingMeter{instruments: map[string]telemetry.Instrument{}, points: map[string][]measurement{}}
}

type recordingInstrument struct {
	m    *recordingMeter
	name string
}

func (i recordingInstrument) record(ctx context.Context, v float64, attrs []telemetry.Attr) {
	i.m.mu.Lock()
	defer i.m.mu.Unlock()
	pt := measurement{value: v, attrs: map[string]any{}, ctx: ctx}
	for _, a := range attrs {
		pt.attrs[a.Key] = a.Value
	}
	i.m.points[i.name] = append(i.m.points[i.name], pt)
}

func (i recordingInstrument) Add(ctx context.Context, n int64, attrs ...telemetry.Attr) {
	i.record(ctx, float64(n), attrs)
}

type intHistogram struct{ recordingInstrument }

func (i intHistogram) Record(ctx context.Context, v int64, attrs ...telemetry.Attr) {
	i.record(ctx, float64(v), attrs)
}

type floatHistogram struct{ recordingInstrument }

func (i floatHistogram) Record(ctx context.Context, v float64, attrs ...telemetry.Attr) {
	i.record(ctx, v, attrs)
}

func (m *recordingMeter) register(in telemetry.Instrument) recordingInstrument {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.instruments[in.Name] = in
	return recordingInstrument{m, in.Name}
}

func (m *recordingMeter) Int64Counter(in telemetry.Instrument) telemetry.Int64Counter {
	return m.register(in)
}

func (m *recordingMeter) Int64Histogram(in telemetry.Instrument) telemetry.Int64Histogram {
	return intHistogram{m.register(in)}
}

func (m *recordingMeter) Float64Histogram(in telemetry.Instrument) telemetry.Float64Histogram {
	return floatHistogram{m.register(in)}
}

func (m *recordingMeter) get(name string) []measurement {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.points[name])
}

func (m *recordingMeter) one(t *testing.T, name string) measurement {
	t.Helper()
	pts := m.get(name)
	if len(pts) != 1 {
		t.Fatalf("%s has %d measurements: %+v", name, len(pts), pts)
	}
	return pts[0]
}

func checkAttrs(t *testing.T, name string, got, want map[string]any) {
	t.Helper()
	if !maps.Equal(got, want) {
		t.Errorf("%s attributes = %v, want %v", name, got, want)
	}
}

func TestMetricsAgentRun(t *testing.T) {
	t.Parallel()
	meter := newRecordingMeter()
	tracer := &recordingTracer{}
	researcher := agent.MustNew("researcher", agent.WithDescription("Researches."),
		agent.WithModel(described{modeltest.New(modeltest.Reply("found")), fixtureDescription}),
		agent.WithMiddleware(middleware.Metrics(meter)))
	usage := model.Usage{InputTokens: 7, OutputTokens: 5, CacheReadTokens: 2, CacheWriteTokens: 1, ReasoningTokens: 3}
	m := modeltest.New(
		modeltest.CallTools(modeltest.Call{ID: "c1", Name: "echo", Args: map[string]string{"text": "hi"}}, modeltest.Call{ID: "c2", Name: "researcher", Args: map[string]string{"prompt": "go"}}),
		modeltest.Respond(&model.Response{Model: "gpt-fixture-2026", Message: model.AssistantText("done"), FinishReason: model.FinishStop, Usage: usage}),
	)
	a := agent.MustNew("assistant", agent.WithModel(described{m, fixtureDescription}), agent.WithTools(echoTool()), agent.WithSubAgents(researcher),
		agent.WithMiddleware(middleware.Tracing(tracer), middleware.Metrics(meter)))
	if _, err := a.Ask(t.Context(), "hi"); err != nil {
		t.Fatal(err)
	}

	base := map[string]any{
		"gen_ai.operation.name": "chat", "gen_ai.provider.name": "openai", "gen_ai.request.model": "gpt-fixture",
		"server.address": "api.openai.com", "server.port": 443,
	}
	with := func(extra map[string]any) map[string]any {
		out := maps.Clone(base)
		maps.Copy(out, extra)
		return out
	}
	durations := meter.get("gen_ai.client.inference.duration")
	if len(durations) != 3 {
		t.Fatalf("inference durations = %+v", durations)
	}
	var final measurement
	for _, d := range durations {
		if d.attrs["gen_ai.response.model"] != nil {
			final = d
		}
		if d.value < 0 {
			t.Errorf("negative duration %v", d.value)
		}
	}
	checkAttrs(t, "inference duration", final.attrs, with(map[string]any{"gen_ai.response.model": "gpt-fixture-2026", "gen_ai.agent.name": "assistant"}))
	// The measurement's context holds the model span, for exemplars.
	if s := telemetry.SpanFromContext(final.ctx).(*recordingSpan); s.name != "chat gpt-fixture" {
		t.Errorf("measurement span = %q", s.name)
	}

	tokens := func(name string, want float64) {
		t.Helper()
		for _, pt := range meter.get(name) {
			if pt.attrs["gen_ai.response.model"] == "gpt-fixture-2026" {
				if pt.value != want {
					t.Errorf("%s = %v, want %v", name, pt.value, want)
				}
				checkAttrs(t, name, pt.attrs, with(map[string]any{"gen_ai.response.model": "gpt-fixture-2026", "gen_ai.agent.name": "assistant", "gen_ai.token.modality": "unknown"}))
				return
			}
		}
		t.Errorf("no %s measurement for the final call", name)
	}
	tokens("gen_ai.client.inference.usage.input_tokens", 10)
	tokens("gen_ai.client.inference.usage.output_tokens", 5)
	tokens("gen_ai.client.inference.usage.cache_read.input_tokens", 2)
	tokens("gen_ai.client.inference.usage.cache_write.input_tokens", 1)
	tokens("gen_ai.client.inference.usage.reasoning.output_tokens", 3)
	if n := len(meter.get("gen_ai.client.inference.operation.input_tokens")); n != 3 {
		t.Errorf("operation.input_tokens has %d measurements", n)
	}

	tools := meter.get("gen_ai.execute_tool.duration")
	if len(tools) != 2 {
		t.Fatalf("tool durations = %+v", tools)
	}
	names := map[any]bool{}
	for _, pt := range tools {
		names[pt.attrs["gen_ai.tool.name"]] = true
		if pt.attrs["gen_ai.tool.type"] != "function" || pt.attrs["gen_ai.agent.name"] != "assistant" || pt.attrs["error.type"] != nil {
			t.Errorf("tool attrs = %v", pt.attrs)
		}
	}
	if !names["echo"] || !names["researcher"] {
		t.Errorf("tools measured = %v", names)
	}

	// Each run records its own calls: the sub-agent's call is not counted
	// for the coordinator.
	runs := map[any]measurement{}
	for _, pt := range meter.get("gen_ai.invoke_agent.duration") {
		runs[pt.attrs["gen_ai.agent.name"]] = pt
	}
	checkAttrs(t, "invoke_agent.duration", runs["assistant"].attrs, map[string]any{"gen_ai.agent.name": "assistant", "agentx.run.status": "completed"})
	checkAttrs(t, "invoke_agent.duration", runs["researcher"].attrs, map[string]any{"gen_ai.agent.name": "researcher", "agentx.run.status": "completed"})
	counts := func(name string) map[any]float64 {
		out := map[any]float64{}
		for _, pt := range meter.get(name) {
			out[pt.attrs["gen_ai.agent.name"]] = pt.value
		}
		return out
	}
	if got := counts("gen_ai.invoke_agent.inference_calls"); got["assistant"] != 2 || got["researcher"] != 1 {
		t.Errorf("inference calls = %v", got)
	}
	if got := counts("gen_ai.invoke_agent.tool_calls"); got["assistant"] != 2 || got["researcher"] != 0 {
		t.Errorf("tool calls = %v", got)
	}

	in := meter.instruments["gen_ai.client.inference.operation.input_tokens"]
	if in.Unit != "{token}" || len(in.Buckets) != 14 || in.Buckets[13] != 67108864 {
		t.Errorf("token histogram = %+v", in)
	}
	if d := meter.instruments["gen_ai.invoke_agent.duration"]; d.Unit != "s" || d.Buckets[0] != 0.1 {
		t.Errorf("agent duration = %+v", d)
	}
	if _, ok := meter.instruments["gen_ai.client.token.usage"]; ok {
		t.Error("legacy instrument created without LegacyMetrics")
	}
}

func TestMetricsFailures(t *testing.T) {
	t.Parallel()
	meter := newRecordingMeter()
	failure := &model.APIError{Provider: "openai", StatusCode: http.StatusTooManyRequests, Type: "rate_limit_exceeded"}
	a := agent.MustNew("assistant", agent.WithModel(modeltest.New(modeltest.Fail(failure))), agent.WithMiddleware(middleware.Metrics(meter)))
	if _, err := a.Ask(t.Context(), "q"); !errors.Is(err, failure) {
		t.Fatalf("Ask = %v", err)
	}
	d := meter.one(t, "gen_ai.client.inference.duration")
	checkAttrs(t, "duration", d.attrs, map[string]any{"gen_ai.operation.name": "chat", "gen_ai.agent.name": "assistant", "error.type": "rate_limit_exceeded"})
	// Usage of failed calls is recorded as zero.
	if in := meter.one(t, "gen_ai.client.inference.usage.input_tokens"); in.value != 0 || in.attrs["gen_ai.token.modality"] != "unknown" || in.attrs["error.type"] != nil {
		t.Errorf("input tokens = %+v", in)
	}
	if op := meter.one(t, "gen_ai.client.inference.operation.output_tokens"); op.value != 0 || op.attrs["error.type"] != "rate_limit_exceeded" {
		t.Errorf("operation output tokens = %+v", op)
	}
	run := meter.one(t, "gen_ai.invoke_agent.duration")
	checkAttrs(t, "run", run.attrs, map[string]any{"gen_ai.agent.name": "assistant", "agentx.run.status": "failed", "error.type": "rate_limit_exceeded"})
	if calls := meter.one(t, "gen_ai.invoke_agent.inference_calls"); calls.value != 1 {
		t.Errorf("inference calls = %v", calls.value)
	}
}

func TestMeterModelStream(t *testing.T) {
	t.Parallel()
	meter := newRecordingMeter()
	m := middleware.Wrap(modeltest.New(modeltest.Reply("hello world")), middleware.MeterModel(meter, middleware.LegacyMetrics()))
	for _, err := range m.Stream(t.Context(), &model.Request{Model: "m"}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	attrs := map[string]any{"gen_ai.operation.name": "chat", "gen_ai.request.model": "m"}
	first := meter.one(t, "gen_ai.client.inference.time_to_first_chunk")
	checkAttrs(t, "time to first chunk", first.attrs, attrs)
	// modeltest splits the text into two deltas.
	if n := len(meter.get("gen_ai.client.inference.time_per_output_chunk")); n != 1 {
		t.Errorf("time per output chunk has %d measurements", n)
	}
	checkAttrs(t, "duration", meter.one(t, "gen_ai.client.inference.duration").attrs, attrs)
	if out := meter.one(t, "gen_ai.client.inference.usage.output_tokens"); out.value != 5 {
		t.Errorf("output tokens = %v", out.value)
	}
	// Legacy metrics.
	usage := map[any]float64{}
	for _, pt := range meter.get("gen_ai.client.token.usage") {
		usage[pt.attrs["gen_ai.token.type"]] = pt.value
	}
	if usage["input"] != 10 || usage["output"] != 5 {
		t.Errorf("legacy token usage = %v", usage)
	}
	checkAttrs(t, "legacy duration", meter.one(t, "gen_ai.client.operation.duration").attrs, attrs)

	// A non-streaming call records no chunk metrics.
	if _, err := middleware.Wrap(modeltest.New(modeltest.Reply("x")), middleware.MeterModel(meter)).Generate(t.Context(), &model.Request{}); err != nil {
		t.Fatal(err)
	}
	if n := len(meter.get("gen_ai.client.inference.time_to_first_chunk")); n != 1 {
		t.Errorf("time to first chunk has %d measurements", n)
	}
}

func TestMeterTools(t *testing.T) {
	t.Parallel()
	meter := newRecordingMeter()
	for _, tc := range []struct {
		res *tool.Result
		err error
	}{{tool.Text("ok"), nil}, {tool.Errorf("bad"), nil}, {nil, &tool.PanicError{Tool: "t", Value: "boom"}}} {
		invoke := middleware.MeterTools(meter)(func(context.Context, *tool.Call) (*tool.Result, error) { return tc.res, tc.err })
		if _, err := invoke(t.Context(), &tool.Call{Name: "t"}); !errors.Is(err, tc.err) {
			t.Fatal(err)
		}
	}
	var types []any
	for _, pt := range meter.get("gen_ai.execute_tool.duration") {
		types = append(types, pt.attrs["error.type"])
		if pt.attrs["gen_ai.tool.name"] != "t" {
			t.Errorf("attrs = %v", pt.attrs)
		}
	}
	if !slices.Equal(types, []any{nil, "tool_error", "panic"}) {
		t.Errorf("error types = %v", types)
	}
	// A nil meter discards measurements.
	if _, err := middleware.MeterTools(nil)(func(context.Context, *tool.Call) (*tool.Result, error) { return nil, nil })(t.Context(), &tool.Call{}); err != nil {
		t.Fatal(err)
	}
	if _, err := middleware.Wrap(modeltest.New(modeltest.Reply("x")), middleware.MeterModel(telemetry.NoopMeter{}, middleware.LegacyMetrics())).Generate(t.Context(), &model.Request{}); err != nil {
		t.Fatal(err)
	}
}
