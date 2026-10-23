package middleware_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sync"
	"testing"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/middleware"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/tool"
)

type recordingSpan struct {
	name   string
	kind   middleware.SpanKind
	parent *recordingSpan
	mu     sync.Mutex
	attrs  map[string]any
	errors []error
	events []recordedEvent
	ended  int
}

type recordedEvent struct {
	name  string
	attrs map[string]any
}

func (s *recordingSpan) AddEvent(name string, attrs ...middleware.Attr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ev := recordedEvent{name: name, attrs: map[string]any{}}
	for _, attr := range attrs {
		ev.attrs[attr.Key] = attr.Value
	}
	s.events = append(s.events, ev)
}

func (s *recordingSpan) attr(key string) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attrs[key]
}

func (s *recordingSpan) SetAttributes(attrs ...middleware.Attr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, attr := range attrs {
		s.attrs[attr.Key] = attr.Value
	}
}

func (s *recordingSpan) RecordError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errors = append(s.errors, err)
}

func (s *recordingSpan) End() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ended++
}

type spanKey struct{}

// recordingTracer records every span and its parent, found in the context
// like an OpenTelemetry tracer does.
type recordingTracer struct {
	mu    sync.Mutex
	spans []*recordingSpan
}

func (tr *recordingTracer) Start(ctx context.Context, name string, kind middleware.SpanKind, attrs ...middleware.Attr) (context.Context, middleware.Span) {
	s := &recordingSpan{name: name, kind: kind, attrs: map[string]any{}}
	s.parent, _ = ctx.Value(spanKey{}).(*recordingSpan)
	s.SetAttributes(attrs...)
	tr.mu.Lock()
	tr.spans = append(tr.spans, s)
	tr.mu.Unlock()
	return context.WithValue(ctx, spanKey{}, s), s
}

func (tr *recordingTracer) find(t *testing.T, name string) *recordingSpan {
	t.Helper()
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, s := range tr.spans {
		if s.name == name {
			return s
		}
	}
	t.Fatalf("no span %q among %d spans", name, len(tr.spans))
	return nil
}

// all returns the spans named name, in start order.
func (tr *recordingTracer) all(name string) []*recordingSpan {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	var out []*recordingSpan
	for _, s := range tr.spans {
		if s.name == name {
			out = append(out, s)
		}
	}
	return out
}

// described adds a model.Describer to a model, as the providers have.
type described struct {
	model.Model
	d model.Description
}

func (m described) Describe() model.Description { return m.d }

var fixtureDescription = model.Description{Provider: "openai", Operation: "chat", Model: "gpt-fixture", ServerAddress: "api.openai.com", ServerPort: 443}

func TestTraceModel(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name                string
		stream, fail, early bool
	}{{"generate", false, false, false}, {"stream", true, false, false}, {"failure", false, true, false}, {"stream failure", true, true, false}, {"early stop", true, false, true}} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tracer := &recordingTracer{}
			failure := &model.APIError{Provider: "openai", StatusCode: http.StatusTooManyRequests, Type: "rate_limit_exceeded"}
			base := modeltest.New(modeltest.Respond(&model.Response{ID: "resp_1", Model: "gpt-fixture-2026", Message: model.AssistantText("hello"), FinishReason: model.FinishStop, Usage: model.Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 2, ReasoningTokens: 3}}))
			if tt.fail {
				base = modeltest.New(modeltest.Fail(failure))
			}
			checking := &middleware.Funcs{Next: base, GenerateFunc: func(ctx context.Context, req *model.Request) (*model.Response, error) {
				if ctx.Value(spanKey{}) == nil {
					t.Error("span context not propagated")
				}
				return base.Generate(ctx, req)
			}}
			wrapped := middleware.Wrap(described{checking, fixtureDescription}, middleware.TraceModel(tracer))
			temp := 0.5
			req := &model.Request{MaxTokens: 100, Temperature: &temp, Reasoning: &model.Reasoning{Effort: "high"}, ResponseFormat: &model.ResponseFormat{Name: "r"}}
			var callErr error
			if tt.stream {
				for _, err := range wrapped.Stream(t.Context(), req) {
					if err != nil {
						callErr = err
					}
					if tt.early {
						break
					}
				}
			} else {
				_, callErr = wrapped.Generate(t.Context(), req)
			}
			if (tt.fail && !errors.Is(callErr, failure)) || (!tt.fail && callErr != nil) {
				t.Fatalf("tracing changed model error: %v", callErr)
			}
			span := tracer.find(t, "chat gpt-fixture")
			want := map[string]any{
				"gen_ai.operation.name":          "chat",
				"gen_ai.provider.name":           "openai",
				"gen_ai.request.model":           "gpt-fixture",
				"server.address":                 "api.openai.com",
				"server.port":                    443,
				"gen_ai.request.max_tokens":      100,
				"gen_ai.request.temperature":     0.5,
				"gen_ai.request.reasoning.level": "high",
				"gen_ai.output.type":             "json",
			}
			if tt.stream {
				want["gen_ai.request.stream"] = true
			}
			for k, v := range want {
				if span.attrs[k] != v {
					t.Errorf("%s = %v, want %v", k, span.attrs[k], v)
				}
			}
			if _, ok := span.attrs["gen_ai.request.stream"]; ok != tt.stream {
				t.Errorf("gen_ai.request.stream present = %v", ok)
			}
			if span.kind != middleware.SpanKindClient || span.ended != 1 {
				t.Fatalf("span = %+v", span)
			}
			switch {
			case tt.fail:
				if len(span.errors) != 1 || span.attrs["error.type"] != "rate_limit_exceeded" {
					t.Fatalf("span errors = %v, error.type = %v", span.errors, span.attrs["error.type"])
				}
			case !tt.early:
				if span.attrs["gen_ai.response.id"] != "resp_1" || span.attrs["gen_ai.response.model"] != "gpt-fixture-2026" ||
					!reflect.DeepEqual(span.attrs["gen_ai.response.finish_reasons"], []string{"stop"}) ||
					span.attrs["gen_ai.usage.input_tokens"] != 12 || span.attrs["gen_ai.usage.output_tokens"] != 5 ||
					span.attrs["gen_ai.usage.cache_read.input_tokens"] != 2 || span.attrs["gen_ai.usage.reasoning.output_tokens"] != 3 {
					t.Fatalf("span attrs = %v", span.attrs)
				}
				if _, ok := span.attrs["gen_ai.response.time_to_first_chunk"]; ok != tt.stream {
					t.Fatalf("time_to_first_chunk present = %v", ok)
				}
			}
		})
	}
}

func TestTraceModelWithoutDescription(t *testing.T) {
	t.Parallel()
	tracer := &recordingTracer{}
	m := middleware.Wrap(modeltest.New(modeltest.Respond(&model.Response{Message: model.AssistantText("x")})), middleware.TraceModel(tracer))
	if _, err := m.Generate(t.Context(), &model.Request{Model: "custom"}); err != nil {
		t.Fatal(err)
	}
	span := tracer.find(t, "chat custom")
	if _, ok := span.attrs["gen_ai.provider.name"]; ok || !reflect.DeepEqual(span.attrs["gen_ai.response.finish_reasons"], []string{"error"}) {
		t.Fatalf("attrs = %v", span.attrs)
	}
	// Wrappers built with Funcs forward the description.
	if d := model.Describe(middleware.Wrap(described{m, fixtureDescription}, middleware.TraceModel(nil))); d != fixtureDescription {
		t.Fatalf("description = %+v", d)
	}
}

func TestTraceTools(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		err       error
		result    *tool.Result
		errorType string
	}{
		{name: "success", result: tool.Text("sunny")},
		{name: "error result", result: tool.Errorf("no such city"), errorType: "tool_error"},
		{name: "failure", err: context.DeadlineExceeded, errorType: "timeout"},
		{name: "panic", err: &tool.PanicError{Tool: "weather", Value: "boom"}, errorType: "panic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tracer := &recordingTracer{}
			invoke := middleware.TraceTools(tracer, middleware.RecordContent())(func(ctx context.Context, _ *tool.Call) (*tool.Result, error) {
				if ctx.Value(spanKey{}) == nil {
					t.Error("span context missing")
				}
				return tc.result, tc.err
			})
			weather := tool.New("weather", "Look up the weather.", nil, nil)
			result, err := invoke(t.Context(), &tool.Call{Name: "weather", ID: "call_1", Arguments: []byte(`{"city":"Kyoto"}`), Tool: weather})
			if result != tc.result || !errors.Is(err, tc.err) {
				t.Fatalf("tracing changed tool result: %+v, %v", result, err)
			}
			s := tracer.find(t, "execute_tool weather")
			if s.kind != middleware.SpanKindInternal || s.ended != 1 || s.attrs["gen_ai.operation.name"] != "execute_tool" ||
				s.attrs["gen_ai.tool.name"] != "weather" || s.attrs["gen_ai.tool.call.id"] != "call_1" || s.attrs["gen_ai.tool.type"] != "function" ||
				s.attrs["gen_ai.tool.description"] != "Look up the weather." || s.attrs["gen_ai.tool.call.arguments"] != `{"city":"Kyoto"}` {
				t.Fatalf("span = %+v", s)
			}
			if tc.errorType == "" {
				if len(s.errors) != 0 || s.attrs["gen_ai.tool.call.result"] != "sunny" {
					t.Fatalf("success span = %+v", s)
				}
				return
			}
			if len(s.errors) != 1 || s.attrs["error.type"] != tc.errorType {
				t.Fatalf("errors = %v, error.type = %v", s.errors, s.attrs["error.type"])
			}
			if _, ok := s.attrs["gen_ai.tool.call.result"]; ok {
				t.Fatal("result of a failed call recorded")
			}
		})
	}
	// Content is opt-in.
	tracer := &recordingTracer{}
	if _, err := middleware.TraceTools(tracer)(func(context.Context, *tool.Call) (*tool.Result, error) { return tool.Text("x"), nil })(t.Context(), &tool.Call{Name: "t", Arguments: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	s := tracer.find(t, "execute_tool t")
	if _, ok := s.attrs["gen_ai.tool.call.arguments"]; ok {
		t.Fatal("arguments recorded without RecordContent")
	}
	if _, ok := s.attrs["gen_ai.tool.call.id"]; ok {
		t.Fatal("empty call ID recorded")
	}
	if _, err := middleware.TraceTools(nil)(func(context.Context, *tool.Call) (*tool.Result, error) { return nil, nil })(t.Context(), &tool.Call{}); err != nil {
		t.Fatal(err)
	}
	base := modeltest.New(modeltest.Reply("noop"))
	if _, err := middleware.Wrap(base, middleware.TraceModel(nil)).Generate(t.Context(), &model.Request{}); err != nil {
		t.Fatal(err)
	}
}

func TestTracingAgentRun(t *testing.T) {
	t.Parallel()
	tracer := &recordingTracer{}
	echo := tool.Func("echo", "Echo text.", func(_ context.Context, in struct {
		Text string `json:"text"`
	}) (string, error) {
		return in.Text, nil
	})
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c", Name: "echo", Args: map[string]string{"text": "hi"}}), modeltest.Reply("done"))
	a := agent.MustNew("assistant", agent.WithModel(described{m, fixtureDescription}), agent.WithTools(echo), agent.WithMiddleware(middleware.Tracing(tracer)))
	runner := agent.NewRunner(a, nil)
	res, err := runner.RunText(t.Context(), "session-1", "say hi")
	if err != nil || res.Status != loop.StatusCompleted {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	root := tracer.find(t, "invoke_agent assistant")
	if root.parent != nil || root.kind != middleware.SpanKindInternal || root.ended != 1 ||
		root.attrs["gen_ai.operation.name"] != "invoke_agent" || root.attrs["gen_ai.agent.name"] != "assistant" ||
		root.attrs["gen_ai.conversation.id"] != "session-1" || root.attrs["agentx.run.status"] != "completed" ||
		root.attrs["gen_ai.usage.input_tokens"] != 20 || root.attrs["agentx.run_id"] != res.RunID {
		t.Fatalf("agent span = %+v", root)
	}
	tracer.mu.Lock()
	spans := tracer.spans
	tracer.mu.Unlock()
	if len(spans) != 4 {
		t.Fatalf("got %d spans", len(spans))
	}
	for _, s := range spans[1:] {
		if s.parent != root || s.attrs["gen_ai.conversation.id"] != "session-1" || s.attrs["gen_ai.agent.name"] != "assistant" {
			t.Errorf("span %q: parent = %v, attrs = %v", s.name, s.parent, s.attrs)
		}
	}
	if tool := tracer.find(t, "execute_tool echo"); tool.attrs["agentx.turn"] != 1 {
		t.Errorf("tool span turn = %v", tool.attrs["agentx.turn"])
	}
}

func TestTracingFailedRun(t *testing.T) {
	t.Parallel()
	tracer := &recordingTracer{}
	failure := &model.APIError{Provider: "openai", StatusCode: http.StatusInternalServerError}
	a := agent.MustNew("assistant", agent.WithModel(modeltest.New(modeltest.Fail(failure))), agent.WithMiddleware(middleware.Tracing(tracer)))
	if _, err := a.Ask(t.Context(), "q"); !errors.Is(err, failure) {
		t.Fatalf("Ask = %v", err)
	}
	root := tracer.find(t, "invoke_agent assistant")
	if root.ended != 1 || len(root.errors) != 1 || root.attrs["error.type"] != "500" || root.attrs["agentx.run.status"] != "failed" {
		t.Fatalf("agent span = %+v", root)
	}
}
