package telemetry

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"

	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/tool"
)

// Attr is a span attribute.
type Attr struct {
	Key   string
	Value any
}

// SpanKind is the kind of a span, as in OpenTelemetry.
type SpanKind int

const (
	// SpanKindInternal marks work done in the process, such as agent runs
	// and tool calls.
	SpanKindInternal SpanKind = iota
	// SpanKindClient marks calls to a remote service, such as model calls.
	SpanKindClient
)

// Span is an in-progress trace span.
type Span interface {
	SetAttributes(attrs ...Attr)
	// RecordError records err and marks the span as failed: adapters should
	// set the span status to error.
	RecordError(err error)
	End()
}

// EventSpan is a Span that records events. Spans that do not implement it
// drop the events of this package, such as the retries of a model call.
type EventSpan interface {
	Span
	AddEvent(name string, attrs ...Attr)
}

// Tracer starts spans. Adapt OpenTelemetry or another tracing library to
// it; agentx itself has no tracing dependency.
//
// Spans follow the OpenTelemetry GenAI semantic conventions
// (https://github.com/open-telemetry/semantic-conventions-genai):
//
//   - "invoke_agent {agent}" (internal) covers a run of the agent loop;
//   - "{operation} {model}" (client), such as "chat gpt-6-astra", covers a
//     model call;
//   - "execute_tool {tool}" (internal) covers a tool call. A sub-agent run
//     by agent.AsTool is a child of its execute_tool span.
//
// The middleware package records these spans; see middleware.Tracing.
//
// Work the conventions do not cover gets agentx spans: "hook {event}" for
// the hooks of a lifecycle event and "compact_context" for an automatic
// compaction. Retries of a model call are "agentx.retry" events.
//
// Attributes use the gen_ai.* names of the conventions, plus agentx.run_id,
// agentx.turn and agentx.run.status. The span of an agent a run was handed
// off to has agentx.handoff.from, the agent that handed it off; a run that
// a guardrail blocked has agentx.guardrail.name and agentx.guardrail.kind
// and an "agentx.guardrail" event, which also holds the guardrail's
// message when content is recorded. A failed operation sets error.type to
// the provider's error code, "timeout", "canceled", "panic", "tool_error"
// for a tool that returned an error result, or "_OTHER".
type Tracer interface {
	Start(ctx context.Context, name string, kind SpanKind, attrs ...Attr) (context.Context, Span)
}

// NoopTracer discards spans.
type NoopTracer struct{}

// Start implements Tracer.
func (NoopTracer) Start(ctx context.Context, _ string, _ SpanKind, _ ...Attr) (context.Context, Span) {
	return ctx, noopSpan{}
}

type noopSpan struct{}

func (noopSpan) SetAttributes(...Attr) {}
func (noopSpan) RecordError(error)     {}
func (noopSpan) End()                  {}

type (
	runKey  struct{}
	spanKey struct{}
)

// runState is the telemetry state of an agent run, carried in its context.
type runState struct {
	tracer Tracer
	runID  string
	// compacted is set once the run compacted its conversation.
	compacted atomic.Bool
}

// ContextWithRun returns ctx carrying tracer for the run runID, so that work
// done inside the run, such as hooks and compaction, starts spans with
// StartSpan and records MarkCompacted. middleware.Tracing installs it for
// each run; a nil tracer discards spans.
func ContextWithRun(ctx context.Context, tracer Tracer, runID string) context.Context {
	if tracer == nil {
		tracer = NoopTracer{}
	}
	return context.WithValue(ctx, runKey{}, &runState{tracer: tracer, runID: runID})
}

// ContextWithSpan returns ctx carrying span as its innermost span, as
// returned by SpanFromContext.
func ContextWithSpan(ctx context.Context, span Span) context.Context {
	return context.WithValue(ctx, spanKey{}, span)
}

// SpanFromContext returns the innermost span of ctx, or a span that
// discards everything.
func SpanFromContext(ctx context.Context) Span {
	if span, ok := ctx.Value(spanKey{}).(Span); ok {
		return span
	}
	return noopSpan{}
}

// AddEvent records an event on the span of ctx, if it is an EventSpan.
func AddEvent(ctx context.Context, name string, attrs ...Attr) {
	if span, ok := SpanFromContext(ctx).(EventSpan); ok {
		span.AddEvent(name, attrs...)
	}
}

// StartSpan starts a span with the tracer of the run ctx belongs to, as
// installed by ContextWithRun, for work done inside the run, such as hooks.
// The returned context carries the span. Without such a run it returns ctx
// and a span that discards everything.
func StartSpan(ctx context.Context, name string, kind SpanKind, attrs ...Attr) (context.Context, Span) {
	rs, _ := ctx.Value(runKey{}).(*runState)
	if rs == nil {
		return ctx, noopSpan{}
	}
	ctx, span := rs.tracer.Start(ctx, name, kind, attrs...)
	return ContextWithSpan(ctx, span), span
}

// RecordSpanError records err on span and sets its low-cardinality
// error.type, as returned by ErrorType.
func RecordSpanError(span Span, err error) {
	span.SetAttributes(Attr{"error.type", ErrorType(err)})
	span.RecordError(err)
}

// MarkCompacted records that the run runID compacted its conversation; ctx
// must belong to that run. Its later model spans carry
// gen_ai.conversation.compacted.
func MarkCompacted(ctx context.Context, runID string) {
	if rs, _ := ctx.Value(runKey{}).(*runState); rs != nil && rs.runID == runID {
		rs.compacted.Store(true)
	}
}

// Compacted reports whether MarkCompacted recorded a compaction of the run
// runID in ctx.
func Compacted(ctx context.Context, runID string) bool {
	rs, _ := ctx.Value(runKey{}).(*runState)
	return rs != nil && rs.runID == runID && rs.compacted.Load()
}

// ErrorType returns the low-cardinality error.type that spans and metrics
// record for err: "timeout", "canceled", the provider's
// error type or HTTP status code, "panic" or "_OTHER". Middleware that
// records errors, such as retries, uses it to stay consistent.
func ErrorType(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	if e, ok := errors.AsType[*model.APIError](err); ok {
		if e.Type != "" {
			return e.Type
		}
		return strconv.Itoa(e.StatusCode)
	}
	if _, ok := errors.AsType[*tool.PanicError](err); ok {
		return "panic"
	}
	return "_OTHER"
}
