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
	"github.com/nostosx/agentx/tool"
)

// Instrument describes a metric instrument.
type Instrument struct {
	Name        string
	Description string
	// Unit is a UCUM unit, such as "s" or "{token}".
	Unit string
	// Buckets are the explicit bucket boundaries the conventions advise for
	// a histogram. Adapters may ignore them.
	Buckets []float64
}

// Int64Counter adds to a monotonic sum.
type Int64Counter interface {
	Add(ctx context.Context, n int64, attrs ...Attr)
}

// Int64Histogram records a distribution of integers.
type Int64Histogram interface {
	Record(ctx context.Context, v int64, attrs ...Attr)
}

// Float64Histogram records a distribution of floats.
type Float64Histogram interface {
	Record(ctx context.Context, v float64, attrs ...Attr)
}

// Meter creates metric instruments. Adapt OpenTelemetry or another metrics
// library to it; agentx itself has no metrics dependency. Implementations
// and their instruments must be safe for concurrent use. The context of a
// measurement holds the span of the measured operation, for exemplars.
//
// Metrics and MeterModel record these instruments of the OpenTelemetry
// GenAI semantic conventions
// (https://github.com/open-telemetry/semantic-conventions-genai):
//
//   - gen_ai.client.inference.duration (s), and for streaming calls
//     gen_ai.client.inference.time_to_first_chunk and
//     gen_ai.client.inference.time_per_output_chunk;
//   - the gen_ai.client.inference.usage.input_tokens, .output_tokens,
//     .cache_read.input_tokens, .cache_write.input_tokens and
//     .reasoning.output_tokens counters, with gen_ai.token.modality
//     "unknown", as providers do not break usage down by modality;
//   - the gen_ai.client.inference.operation.input_tokens and
//     .output_tokens histograms;
//   - gen_ai.execute_tool.duration (s), whose count is the number of tool
//     calls;
//   - gen_ai.invoke_agent.duration (s), with agentx.run.status, and the
//     gen_ai.invoke_agent.inference_calls and .tool_calls histograms, which
//     count only a run's own calls, not those of its sub-agents.
//
// Model metrics carry gen_ai.operation.name, gen_ai.provider.name,
// gen_ai.request.model, gen_ai.response.model and server.address and
// server.port; run and tool metrics carry gen_ai.agent.name. Failed
// operations add error.type as spans do.
type Meter interface {
	Int64Counter(Instrument) Int64Counter
	Int64Histogram(Instrument) Int64Histogram
	Float64Histogram(Instrument) Float64Histogram
}

// NoopMeter discards measurements.
type NoopMeter struct{}

// Int64Counter implements Meter.
func (NoopMeter) Int64Counter(Instrument) Int64Counter { return noopInstrument{} }

// Int64Histogram implements Meter.
func (NoopMeter) Int64Histogram(Instrument) Int64Histogram { return noopInstrument{} }

// Float64Histogram implements Meter.
func (NoopMeter) Float64Histogram(Instrument) Float64Histogram { return noopFloat{} }

type noopInstrument struct{}

func (noopInstrument) Add(context.Context, int64, ...Attr)    {}
func (noopInstrument) Record(context.Context, int64, ...Attr) {}

type noopFloat struct{}

func (noopFloat) Record(context.Context, float64, ...Attr) {}

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
	meter  Meter
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
	inferenceDuration, timeToFirstChunk, timePerChunk Float64Histogram
	inputTokens, outputTokens, cacheReadTokens        Int64Counter
	cacheWriteTokens, reasoningTokens                 Int64Counter
	operationInput, operationOutput                   Int64Histogram
	toolDuration, agentDuration                       Float64Histogram
	agentInferenceCalls, agentToolCalls               Int64Histogram
	// Legacy instruments; nil unless LegacyMetrics is set.
	tokenUsage        Int64Histogram
	operationDuration Float64Histogram
}

func newInstruments(m Meter, opts []MetricsOption) *instruments {
	c := &metricsConfig{meter: m}
	for _, opt := range opts {
		opt(c)
	}
	if c.meter == nil {
		c.meter = NoopMeter{}
	}
	m = c.meter
	seconds := func(name, desc string, buckets []float64) Float64Histogram {
		return m.Float64Histogram(Instrument{Name: name, Description: desc, Unit: "s", Buckets: buckets})
	}
	tokens := func(name, desc string) Int64Counter {
		return m.Int64Counter(Instrument{Name: name, Description: desc, Unit: "{token}"})
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
		operationInput: m.Int64Histogram(Instrument{Name: "gen_ai.client.inference.operation.input_tokens",
			Description: "The number of input (prompt) tokens used per inference operation.", Unit: "{token}", Buckets: tokenBuckets}),
		operationOutput: m.Int64Histogram(Instrument{Name: "gen_ai.client.inference.operation.output_tokens",
			Description: "The number of output (completion) tokens used per inference operation.", Unit: "{token}", Buckets: tokenBuckets}),
		toolDuration:  seconds("gen_ai.execute_tool.duration", "The duration of a single tool execution.", durationBuckets),
		agentDuration: seconds("gen_ai.invoke_agent.duration", "The end-to-end duration of a single agent invocation.", agentBuckets),
		agentInferenceCalls: m.Int64Histogram(Instrument{Name: "gen_ai.invoke_agent.inference_calls",
			Description: "The number of inference calls an agent makes during a single invocation.", Unit: "{inference_call}", Buckets: callBuckets}),
		agentToolCalls: m.Int64Histogram(Instrument{Name: "gen_ai.invoke_agent.tool_calls",
			Description: "The number of tool calls an agent makes during a single invocation.", Unit: "{tool_call}", Buckets: callBuckets}),
	}
	if c.legacy {
		in.tokenUsage = m.Int64Histogram(Instrument{Name: "gen_ai.client.token.usage", Description: "Number of input and output tokens used.", Unit: "{token}", Buckets: tokenBuckets})
		in.operationDuration = seconds("gen_ai.client.operation.duration", "GenAI operation duration.", durationBuckets)
	}
	return in
}

// Metrics returns runtime middleware that records the metrics described at
// Meter for each run and its model and tool calls.
//
// Register it after Tracing, so measurements carry the spans, and before
// retries, so a model call's duration includes them.
func Metrics(m Meter, opts ...MetricsOption) loop.Middleware {
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
	attrs := with(agent, Attr{"agentx.run.status", string(res.Status)})
	if res.Err != nil {
		attrs = append(attrs, Attr{"error.type", ErrorType(res.Err)})
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

func agentAttr(run *loop.Run) []Attr {
	if run.Agent() == "" {
		return nil
	}
	return []Attr{{"gen_ai.agent.name", run.Agent()}}
}

// MeterModel records the model metrics described at Meter for each call.
func MeterModel(m Meter, opts ...MetricsOption) Model {
	return meterModel(newInstruments(m, opts), nil, nil)
}

// MeterTools records gen_ai.execute_tool.duration for each tool call.
func MeterTools(m Meter, opts ...MetricsOption) tool.Middleware {
	return meterTools(newInstruments(m, opts), nil, nil)
}

// meterModel measures model calls; onCall, if non-nil, runs at the start of
// each call.
func meterModel(in *instruments, extra []Attr, onCall func()) Model {
	return func(next model.Model) model.Model {
		begin := func(req *model.Request) []Attr {
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
func metricAttrs(d model.Description, req *model.Request) []Attr {
	attrs := []Attr{{"gen_ai.operation.name", cmp.Or(d.Operation, "chat")}}
	if d.Provider != "" {
		attrs = append(attrs, Attr{"gen_ai.provider.name", d.Provider})
	}
	if m := cmp.Or(req.Model, d.Model); m != "" {
		attrs = append(attrs, Attr{"gen_ai.request.model", m})
	}
	if d.ServerAddress != "" {
		attrs = append(attrs, Attr{"server.address", d.ServerAddress}, Attr{"server.port", d.ServerPort})
	}
	return attrs
}

// recordInference records the duration and token usage of a model call.
// As the conventions ask, usage is recorded for failed calls too, as zero.
func (in *instruments) recordInference(ctx context.Context, attrs []Attr, resp *model.Response, err error, d time.Duration) {
	var u model.Usage
	if resp != nil {
		u = resp.Usage
		if resp.Model != "" {
			attrs = append(attrs, Attr{"gen_ai.response.model", resp.Model})
		}
	}
	failed := attrs
	if err != nil {
		failed = with(attrs, Attr{"error.type", ErrorType(err)})
	}
	in.inferenceDuration.Record(ctx, d.Seconds(), failed...)
	in.operationInput.Record(ctx, int64(u.PromptTokens()), failed...)
	in.operationOutput.Record(ctx, int64(u.OutputTokens), failed...)

	modality := with(attrs, Attr{"gen_ai.token.modality", "unknown"})
	in.inputTokens.Add(ctx, int64(u.PromptTokens()), modality...)
	in.outputTokens.Add(ctx, int64(u.OutputTokens), modality...)
	in.cacheReadTokens.Add(ctx, int64(u.CacheReadTokens), modality...)
	in.cacheWriteTokens.Add(ctx, int64(u.CacheWriteTokens), modality...)
	in.reasoningTokens.Add(ctx, int64(u.ReasoningTokens), modality...)

	if in.tokenUsage != nil {
		in.operationDuration.Record(ctx, d.Seconds(), failed...)
		if err == nil && resp != nil {
			in.tokenUsage.Record(ctx, int64(u.PromptTokens()), with(attrs, Attr{"gen_ai.token.type", "input"})...)
			in.tokenUsage.Record(ctx, int64(u.OutputTokens), with(attrs, Attr{"gen_ai.token.type", "output"})...)
		}
	}
}

// with returns attrs and extra in a new slice.
func with(attrs []Attr, extra ...Attr) []Attr { return append(slices.Clip(attrs), extra...) }

// meterTools measures tool calls; onCall, if non-nil, runs at the start of
// each call.
func meterTools(in *instruments, extra []Attr, onCall func()) tool.Middleware {
	return func(next tool.Invoker) tool.Invoker {
		return func(ctx context.Context, call *tool.Call) (*tool.Result, error) {
			if onCall != nil {
				onCall()
			}
			start := time.Now()
			res, err := next(ctx, call)
			attrs := append([]Attr{{"gen_ai.tool.name", call.Name}, {"gen_ai.tool.type", "function"}}, extra...)
			switch {
			case err != nil:
				attrs = append(attrs, Attr{"error.type", ErrorType(err)})
			case res != nil && res.IsError:
				attrs = append(attrs, Attr{"error.type", "tool_error"})
			}
			in.toolDuration.Record(ctx, time.Since(start).Seconds(), attrs...)
			return res, err
		}
	}
}
