package middleware

import (
	"cmp"
	"context"
	"iter"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/telemetry"
	"github.com/nostosx/agentx/tool"
)

// Bucket boundaries advised by the conventions.
var (
	durationBuckets = []float64{0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92}
	agentBuckets    = []float64{0.1, 0.2, 0.4, 0.8, 1.6, 3.2, 6.4, 12.8, 25.6, 51.2, 102.4, 204.8, 409.6}
	tokenBuckets    = []float64{1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864}
	callBuckets     = []float64{1, 2, 4, 8, 16, 32, 64, 128}
)

// MetricsOption configures metrics.
type MetricsOption func(*metricsConfig)

type metricsConfig struct {
	meter  telemetry.Meter
	legacy bool
}

// LegacyMetrics also records the model metrics that earlier releases of
// the conventions defined and the current ones replaced, for dashboards
// that still query them: the gen_ai.client.token.usage histogram, with
// gen_ai.token.type "input" or "output", and
// gen_ai.client.operation.duration.
func LegacyMetrics() MetricsOption { return func(c *metricsConfig) { c.legacy = true } }

// instruments holds the instruments of the conventions.
type instruments struct {
	inferenceDuration, timeToFirstChunk, timePerChunk telemetry.Float64Histogram
	inputTokens, outputTokens, cacheReadTokens        telemetry.Int64Counter
	cacheWriteTokens, reasoningTokens                 telemetry.Int64Counter
	operationInput, operationOutput                   telemetry.Int64Histogram
	toolDuration, agentDuration                       telemetry.Float64Histogram
	agentInferenceCalls, agentToolCalls               telemetry.Int64Histogram
	// Legacy instruments; nil unless LegacyMetrics is set.
	tokenUsage        telemetry.Int64Histogram
	operationDuration telemetry.Float64Histogram
}

func newInstruments(m telemetry.Meter, opts []MetricsOption) *instruments {
	c := &metricsConfig{meter: m}
	for _, opt := range opts {
		opt(c)
	}
	if c.meter == nil {
		c.meter = telemetry.NoopMeter{}
	}
	m = c.meter
	seconds := func(name, desc string, buckets []float64) telemetry.Float64Histogram {
		return m.Float64Histogram(telemetry.Instrument{Name: name, Description: desc, Unit: "s", Buckets: buckets})
	}
	tokens := func(name, desc string) telemetry.Int64Counter {
		return m.Int64Counter(telemetry.Instrument{Name: name, Description: desc, Unit: "{token}"})
	}
	in := &instruments{
		inferenceDuration: seconds("gen_ai.client.inference.duration", "GenAI client inference operation duration.", durationBuckets),
		timeToFirstChunk:  seconds("gen_ai.client.inference.time_to_first_chunk", "Time to receive the first chunk of a streaming response.", durationBuckets),
		timePerChunk:      seconds("gen_ai.client.inference.time_per_output_chunk", "Time per output chunk after the first one.", durationBuckets),
		inputTokens:       tokens("gen_ai.client.inference.usage.input_tokens", "The number of input (prompt) tokens used, including cached tokens."),
		outputTokens:      tokens("gen_ai.client.inference.usage.output_tokens", "The number of output (completion) tokens used, including reasoning tokens."),
		cacheReadTokens:   tokens("gen_ai.client.inference.usage.cache_read.input_tokens", "The number of input tokens served from a provider-managed cache."),
		cacheWriteTokens:  tokens("gen_ai.client.inference.usage.cache_write.input_tokens", "The number of input tokens written to a provider-managed cache."),
		reasoningTokens:   tokens("gen_ai.client.inference.usage.reasoning.output_tokens", "The number of output tokens used for reasoning."),
		operationInput: m.Int64Histogram(telemetry.Instrument{Name: "gen_ai.client.inference.operation.input_tokens",
			Description: "The number of input (prompt) tokens used per inference operation.", Unit: "{token}", Buckets: tokenBuckets}),
		operationOutput: m.Int64Histogram(telemetry.Instrument{Name: "gen_ai.client.inference.operation.output_tokens",
			Description: "The number of output (completion) tokens used per inference operation.", Unit: "{token}", Buckets: tokenBuckets}),
		toolDuration:  seconds("gen_ai.execute_tool.duration", "The duration of a single tool execution.", durationBuckets),
		agentDuration: seconds("gen_ai.invoke_agent.duration", "The end-to-end duration of a single agent invocation.", agentBuckets),
		agentInferenceCalls: m.Int64Histogram(telemetry.Instrument{Name: "gen_ai.invoke_agent.inference_calls",
			Description: "The number of inference calls an agent makes during a single invocation.", Unit: "{inference_call}", Buckets: callBuckets}),
		agentToolCalls: m.Int64Histogram(telemetry.Instrument{Name: "gen_ai.invoke_agent.tool_calls",
			Description: "The number of tool calls an agent makes during a single invocation.", Unit: "{tool_call}", Buckets: callBuckets}),
	}
	if c.legacy {
		in.tokenUsage = m.Int64Histogram(telemetry.Instrument{Name: "gen_ai.client.token.usage", Description: "Number of input and output tokens used.", Unit: "{token}", Buckets: tokenBuckets})
		in.operationDuration = seconds("gen_ai.client.operation.duration", "GenAI operation duration.", durationBuckets)
	}
	return in
}

// Metrics returns runtime middleware that records the metrics described at
// telemetry.Meter for each run and its model and tool calls.
//
// Register it after Tracing, so measurements carry the spans, and before
// retries, so a model call's duration includes them.
func Metrics(m telemetry.Meter, opts ...MetricsOption) loop.Middleware {
	return &metrics{in: newInstruments(m, opts)}
}

type metrics struct {
	in *instruments
	// runs holds the stats of each run in progress.
	runs sync.Map // *loop.Run -> *runStats
}

type runStats struct {
	start           time.Time
	inference, tool atomic.Int64
}

func (*metrics) Name() string { return "metrics" }

func (mt *metrics) WrapContext(ctx context.Context, run *loop.Run) (context.Context, func()) {
	mt.runs.Store(run, &runStats{start: time.Now()})
	return ctx, func() { mt.runs.Delete(run) }
}

func (mt *metrics) stats(run *loop.Run) *runStats {
	if s, ok := mt.runs.Load(run); ok {
		return s.(*runStats)
	}
	return nil
}

func (mt *metrics) AfterRun(ctx context.Context, run *loop.Run, res *loop.Result) {
	s := mt.stats(run)
	if s == nil || res == nil {
		return
	}
	agent := agentAttr(run)
	attrs := with(agent, telemetry.Attr{Key: "agentx.run.status", Value: string(res.Status)})
	if res.Err != nil {
		attrs = append(attrs, telemetry.Attr{Key: "error.type", Value: telemetry.ErrorType(res.Err)})
	}
	mt.in.agentDuration.Record(ctx, time.Since(s.start).Seconds(), attrs...)
	mt.in.agentInferenceCalls.Record(ctx, s.inference.Load(), agent...)
	mt.in.agentToolCalls.Record(ctx, s.tool.Load(), agent...)
}

func (mt *metrics) WrapModel(run *loop.Run, next model.Model) model.Model {
	return meterModel(mt.in, agentAttr(run), func() {
		if s := mt.stats(run); s != nil {
			s.inference.Add(1)
		}
	})(next)
}

func (mt *metrics) WrapTool(run *loop.Run, next tool.Invoker) tool.Invoker {
	return meterTools(mt.in, agentAttr(run), func() {
		if s := mt.stats(run); s != nil {
			s.tool.Add(1)
		}
	})(next)
}

func agentAttr(run *loop.Run) []telemetry.Attr {
	if run.Agent() == "" {
		return nil
	}
	return []telemetry.Attr{{Key: "gen_ai.agent.name", Value: run.Agent()}}
}

// MeterModel records the model metrics described at telemetry.Meter for each
// call.
func MeterModel(m telemetry.Meter, opts ...MetricsOption) Model {
	return meterModel(newInstruments(m, opts), nil, nil)
}

// MeterTools records gen_ai.execute_tool.duration for each tool call.
func MeterTools(m telemetry.Meter, opts ...MetricsOption) tool.Middleware {
	return meterTools(newInstruments(m, opts), nil, nil)
}

// meterModel measures model calls; onCall, if non-nil, runs at the start of
// each call.
func meterModel(in *instruments, extra []telemetry.Attr, onCall func()) Model {
	return func(next model.Model) model.Model {
		begin := func(req *model.Request) []telemetry.Attr {
			if onCall != nil {
				onCall()
			}
			return append(metricAttrs(model.Describe(next), req), extra...)
		}
		return &Funcs{
			Next: next,
			GenerateFunc: func(ctx context.Context, req *model.Request) (*model.Response, error) {
				attrs, start := begin(req), time.Now()
				resp, err := next.Generate(ctx, req)
				in.recordInference(ctx, attrs, resp, err, time.Since(start))
				return resp, err
			},
			StreamFunc: func(ctx context.Context, req *model.Request) iter.Seq2[*model.StreamEvent, error] {
				return func(yield func(*model.StreamEvent, error) bool) {
					attrs, start := begin(req), time.Now()
					var (
						resp      *model.Response
						streamErr error
						last      time.Time
					)
					defer func() { in.recordInference(ctx, attrs, resp, streamErr, time.Since(start)) }()
					for ev, err := range next.Stream(ctx, req) {
						switch {
						case err != nil:
							streamErr = err
						case ev != nil && ev.Type == model.StreamDelta:
							now := time.Now()
							if last.IsZero() {
								in.timeToFirstChunk.Record(ctx, now.Sub(start).Seconds(), attrs...)
							} else {
								in.timePerChunk.Record(ctx, now.Sub(last).Seconds(), attrs...)
							}
							last = now
						case ev != nil && ev.Type == model.StreamDone:
							resp = ev.Response
						}
						if !yield(ev, err) {
							return
						}
					}
				}
			},
		}
	}
}

// metricAttrs returns the attributes of the model metrics of a call.
func metricAttrs(d model.Description, req *model.Request) []telemetry.Attr {
	attrs := []telemetry.Attr{{Key: "gen_ai.operation.name", Value: cmp.Or(d.Operation, "chat")}}
	if d.Provider != "" {
		attrs = append(attrs, telemetry.Attr{Key: "gen_ai.provider.name", Value: d.Provider})
	}
	if m := cmp.Or(req.Model, d.Model); m != "" {
		attrs = append(attrs, telemetry.Attr{Key: "gen_ai.request.model", Value: m})
	}
	if d.ServerAddress != "" {
		attrs = append(attrs, telemetry.Attr{Key: "server.address", Value: d.ServerAddress}, telemetry.Attr{Key: "server.port", Value: d.ServerPort})
	}
	return attrs
}

// recordInference records the duration and token usage of a model call.
// As the conventions ask, usage is recorded for failed calls too, as zero.
func (in *instruments) recordInference(ctx context.Context, attrs []telemetry.Attr, resp *model.Response, err error, d time.Duration) {
	var u model.Usage
	if resp != nil {
		u = resp.Usage
		if resp.Model != "" {
			attrs = append(attrs, telemetry.Attr{Key: "gen_ai.response.model", Value: resp.Model})
		}
	}
	failed := attrs
	if err != nil {
		failed = with(attrs, telemetry.Attr{Key: "error.type", Value: telemetry.ErrorType(err)})
	}
	in.inferenceDuration.Record(ctx, d.Seconds(), failed...)
	in.operationInput.Record(ctx, int64(u.PromptTokens()), failed...)
	in.operationOutput.Record(ctx, int64(u.OutputTokens), failed...)

	modality := with(attrs, telemetry.Attr{Key: "gen_ai.token.modality", Value: "unknown"})
	in.inputTokens.Add(ctx, int64(u.PromptTokens()), modality...)
	in.outputTokens.Add(ctx, int64(u.OutputTokens), modality...)
	in.cacheReadTokens.Add(ctx, int64(u.CacheReadTokens), modality...)
	in.cacheWriteTokens.Add(ctx, int64(u.CacheWriteTokens), modality...)
	in.reasoningTokens.Add(ctx, int64(u.ReasoningTokens), modality...)

	if in.tokenUsage != nil {
		in.operationDuration.Record(ctx, d.Seconds(), failed...)
		if err == nil && resp != nil {
			in.tokenUsage.Record(ctx, int64(u.PromptTokens()), with(attrs, telemetry.Attr{Key: "gen_ai.token.type", Value: "input"})...)
			in.tokenUsage.Record(ctx, int64(u.OutputTokens), with(attrs, telemetry.Attr{Key: "gen_ai.token.type", Value: "output"})...)
		}
	}
}

// with returns attrs and extra in a new slice.
func with(attrs []telemetry.Attr, extra ...telemetry.Attr) []telemetry.Attr {
	return append(slices.Clip(attrs), extra...)
}

// meterTools measures tool calls; onCall, if non-nil, runs at the start of
// each call.
func meterTools(in *instruments, extra []telemetry.Attr, onCall func()) tool.Middleware {
	return func(next tool.Invoker) tool.Invoker {
		return func(ctx context.Context, call *tool.Call) (*tool.Result, error) {
			if onCall != nil {
				onCall()
			}
			start := time.Now()
			res, err := next(ctx, call)
			attrs := append([]telemetry.Attr{{Key: "gen_ai.tool.name", Value: call.Name}, {Key: "gen_ai.tool.type", Value: "function"}}, extra...)
			switch {
			case err != nil:
				attrs = append(attrs, telemetry.Attr{Key: "error.type", Value: telemetry.ErrorType(err)})
			case res != nil && res.IsError:
				attrs = append(attrs, telemetry.Attr{Key: "error.type", Value: "tool_error"})
			}
			in.toolDuration.Record(ctx, time.Since(start).Seconds(), attrs...)
			return res, err
		}
	}
}
