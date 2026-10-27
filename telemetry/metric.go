package telemetry

import "context"

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
// middleware.Metrics and middleware.MeterModel record these instruments of
// the OpenTelemetry GenAI semantic conventions
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
