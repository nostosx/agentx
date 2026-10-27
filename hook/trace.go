package hook

import (
	"context"

	// The package's middleware type takes the package name.
	"github.com/nostosx/agentx/telemetry"
)

// startSpan starts the span of the hooks of ev, when the run of ctx is
// traced; see telemetry.StartSpan.
func startSpan(ctx context.Context, ev *Event, hooks int) (context.Context, telemetry.Span) {
	attrs := []telemetry.Attr{
		{Key: "agentx.hook.event", Value: string(ev.Type)},
		{Key: "agentx.hook.count", Value: hooks},
	}
	if name := ev.ToolName(); name != "" {
		attrs = append(attrs, telemetry.Attr{Key: "agentx.hook.tool", Value: name})
	}
	return telemetry.StartSpan(ctx, "hook "+string(ev.Type), telemetry.SpanKindInternal, attrs...)
}

// endSpan records the merged result of the hooks and ends span.
func endSpan(span telemetry.Span, res *Result, err error) {
	defer span.End()
	if res != nil {
		var attrs []telemetry.Attr
		if res.Decision != Continue {
			attrs = append(attrs, telemetry.Attr{Key: "agentx.hook.decision", Value: string(res.Decision)})
		}
		if res.Halt {
			attrs = append(attrs, telemetry.Attr{Key: "agentx.hook.halt", Value: true})
		}
		span.SetAttributes(attrs...)
	}
	if err != nil {
		telemetry.RecordSpanError(span, err)
	}
}
