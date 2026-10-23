package hook

import (
	"context"

	// The package's middleware type takes the package name.
	trace "github.com/nostosx/agentx/middleware"
)

// startSpan starts the span of the hooks of ev, when the run of ctx is
// traced; see middleware.StartSpan.
func startSpan(ctx context.Context, ev *Event, hooks int) (context.Context, trace.Span) {
	attrs := []trace.Attr{
		{Key: "agentx.hook.event", Value: string(ev.Type)},
		{Key: "agentx.hook.count", Value: hooks},
	}
	if name := ev.ToolName(); name != "" {
		attrs = append(attrs, trace.Attr{Key: "agentx.hook.tool", Value: name})
	}
	return trace.StartSpan(ctx, "hook "+string(ev.Type), trace.SpanKindInternal, attrs...)
}

// endSpan records the merged result of the hooks and ends span.
func endSpan(span trace.Span, res *Result, err error) {
	defer span.End()
	if res != nil {
		var attrs []trace.Attr
		if res.Decision != Continue {
			attrs = append(attrs, trace.Attr{Key: "agentx.hook.decision", Value: string(res.Decision)})
		}
		if res.Halt {
			attrs = append(attrs, trace.Attr{Key: "agentx.hook.halt", Value: true})
		}
		span.SetAttributes(attrs...)
	}
	if err != nil {
		trace.RecordSpanError(span, err)
	}
}
