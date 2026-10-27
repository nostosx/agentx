package telemetry_test

import (
	"context"
	"errors"
	"testing"

	"github.com/nostosx/agentx/telemetry"
)

// recordingTracer counts the spans it starts.
type recordingTracer struct{ started []string }

func (r *recordingTracer) Start(ctx context.Context, name string, _ telemetry.SpanKind, _ ...telemetry.Attr) (context.Context, telemetry.Span) {
	r.started = append(r.started, name)
	return ctx, &recordingSpan{}
}

type recordingSpan struct {
	attrs  []telemetry.Attr
	err    error
	events []string
}

func (s *recordingSpan) SetAttributes(attrs ...telemetry.Attr) { s.attrs = append(s.attrs, attrs...) }
func (s *recordingSpan) RecordError(err error)                 { s.err = err }
func (s *recordingSpan) End()                                  {}
func (s *recordingSpan) AddEvent(name string, _ ...telemetry.Attr) {
	s.events = append(s.events, name)
}

func TestStartSpanNeedsRun(t *testing.T) {
	ctx, span := telemetry.StartSpan(t.Context(), "outside", telemetry.SpanKindInternal)
	span.End()
	if ctx != t.Context() {
		t.Error("StartSpan outside a run must return ctx unchanged")
	}

	tr := &recordingTracer{}
	ctx = telemetry.ContextWithRun(t.Context(), tr, "run-1")
	ctx, span = telemetry.StartSpan(ctx, "hook PreToolUse", telemetry.SpanKindInternal)
	if len(tr.started) != 1 || telemetry.SpanFromContext(ctx) != span {
		t.Fatalf("started = %v; the returned context must carry the span", tr.started)
	}
	telemetry.AddEvent(ctx, "agentx.retry")
	if got := span.(*recordingSpan).events; len(got) != 1 {
		t.Errorf("events = %v", got)
	}
}

func TestContextWithRunNilTracer(t *testing.T) {
	ctx := telemetry.ContextWithRun(t.Context(), nil, "run-1")
	_, span := telemetry.StartSpan(ctx, "x", telemetry.SpanKindInternal)
	span.End()
}

// Compaction is recorded for the run of the context only, so a sub-agent's
// compaction does not mark its parent's model spans.
func TestMarkCompactedIsRunScoped(t *testing.T) {
	parent := telemetry.ContextWithRun(t.Context(), nil, "parent")
	child := telemetry.ContextWithRun(parent, nil, "child")
	telemetry.MarkCompacted(child, "child")
	telemetry.MarkCompacted(child, "parent")
	if !telemetry.Compacted(child, "child") || telemetry.Compacted(parent, "parent") {
		t.Error("compaction must be recorded only for the run of the context")
	}
	telemetry.MarkCompacted(t.Context(), "parent")
	if telemetry.Compacted(t.Context(), "parent") {
		t.Error("a context without a run records no compaction")
	}
}

func TestRecordSpanError(t *testing.T) {
	span := &recordingSpan{}
	telemetry.RecordSpanError(span, context.DeadlineExceeded)
	if !errors.Is(span.err, context.DeadlineExceeded) || len(span.attrs) != 1 || span.attrs[0] != (telemetry.Attr{Key: "error.type", Value: "timeout"}) {
		t.Errorf("span = %+v", span)
	}
	if telemetry.ErrorType(errors.New("x")) != "_OTHER" || telemetry.ErrorType(context.Canceled) != "canceled" {
		t.Error("ErrorType")
	}
}
