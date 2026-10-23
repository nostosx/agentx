package actx

import (
	"context"
	"slices"

	// The package's middleware type takes the package name.
	"github.com/nostosx/agentx/loop"
	trace "github.com/nostosx/agentx/middleware"
	"github.com/nostosx/agentx/model"
)

// MiddlewareOption configures Middleware.
type MiddlewareOption func(*middleware)

// WithCompactGuard calls guard before each automatic compaction; returning
// false skips it. hook.PreCompactGuard adapts PreCompact hooks.
func WithCompactGuard(guard func(ctx context.Context, run *loop.Run, reason string) bool) MiddlewareOption {
	return func(mw *middleware) { mw.guard = guard }
}

// WithCompactObserver calls observe after each automatic compaction that
// replaced the conversation, with the reason passed to the guard ("auto")
// and the compaction. hook.PostCompactObserver adapts PostCompact hooks.
func WithCompactObserver(observe func(ctx context.Context, run *loop.Run, reason string, c *loop.Compaction)) MiddlewareOption {
	return func(mw *middleware) { mw.observe = observe }
}

// Middleware returns runtime middleware that engineers the context of each
// run with m: it adds the providers' sections to the system prompt and the
// input, compacts the conversation when it outgrows the window and
// calibrates token estimates against reported usage. A nil m uses the
// defaults of NewManager.
//
// Provider and compaction failures are logged and do not stop the run. A
// compaction that does not shrink the conversation is discarded without an
// EventCompacted. When the conversation still exceeds the usable window
// after compaction, a warning is logged and the request is sent anyway; the
// provider may reject it.
//
// Inside a run traced by middleware.Tracing, an automatic compaction gets a
// "compact_context" span, and the run's later model spans carry
// gen_ai.conversation.compacted.
func Middleware(m *Manager, opts ...MiddlewareOption) loop.Middleware {
	mw := &middleware{m: m}
	for _, opt := range opts {
		opt(mw)
	}
	return mw
}

type middleware struct {
	m       *Manager
	guard   func(ctx context.Context, run *loop.Run, reason string) bool
	observe func(ctx context.Context, run *loop.Run, reason string, c *loop.Compaction)
}

func (mw *middleware) Name() string { return "context" }

func (mw *middleware) BeforeRun(ctx context.Context, run *loop.Run, in *loop.RunInput) error {
	prepared, err := mw.m.Prepare(ctx, in.System, &Input{
		SessionID: run.SessionID(),
		Agent:     run.Agent(),
		Messages:  run.Messages(),
		Query:     in.Query,
	})
	if err != nil {
		run.Logger().Warn("context provider failed", "run_id", run.ID(), "error", err)
	}
	in.System = prepared.System
	if prepared.Reminder != "" {
		in.Message.Parts = slices.Insert(in.Message.Parts, 0, model.TextPart(prepared.Reminder))
	}
	return nil
}

func (mw *middleware) BeforeTurn(ctx context.Context, run *loop.Run) error {
	specs := run.Tools()
	if need, _ := mw.m.NeedsCompaction(run.System(), run.Messages(), specs); !need {
		return nil
	}
	// The span covers the guard's PreCompact hooks, the compaction and its
	// observer's PostCompact hooks.
	ctx, span := trace.StartSpan(ctx, "compact_context", trace.SpanKindInternal,
		trace.Attr{Key: "agentx.compaction.reason", Value: "auto"})
	defer span.End()
	if mw.guard != nil && !mw.guard(ctx, run, "auto") {
		span.SetAttributes(trace.Attr{Key: "agentx.compaction.skipped", Value: true})
		return nil
	}
	res, err := mw.m.Compact(ctx, run.System(), run.Messages(), specs)
	if err != nil {
		trace.RecordSpanError(span, err)
		run.Logger().Warn("compaction failed", "run_id", run.ID(), "error", err)
		return nil
	}
	span.SetAttributes(
		trace.Attr{Key: "agentx.compaction.tokens_before", Value: res.Before},
		trace.Attr{Key: "agentx.compaction.tokens_after", Value: res.After},
	)
	usable := mw.m.Window().Usable()
	if res.After >= res.Before {
		// Nothing could be removed; keep the conversation and do not report
		// a compaction that did not happen.
		if res.Before > usable {
			run.Logger().Warn("compaction cannot fit the conversation in the context window", "run_id", run.ID(), "tokens", res.Before, "usable", usable)
		} else {
			run.Logger().Debug("compaction did not shrink the conversation", "run_id", run.ID(), "tokens", res.Before)
		}
		return nil
	}
	if res.After > usable {
		run.Logger().Warn("conversation still exceeds the context window after compaction", "run_id", run.ID(), "before", res.Before, "after", res.After, "usable", usable)
	}
	run.ReplaceMessages(res.Messages)
	span.SetAttributes(trace.Attr{Key: "agentx.compaction.applied", Value: true})
	trace.MarkCompacted(ctx, run)
	compaction := &loop.Compaction{
		Messages: res.Messages,
		Before:   res.Before,
		After:    res.After,
		Summary:  res.Summary,
	}
	run.Emit(&loop.Event{Type: loop.EventCompacted, Compaction: compaction})
	if mw.observe != nil {
		mw.observe(ctx, run, "auto", compaction)
	}
	return nil
}

func (mw *middleware) AfterModel(_ context.Context, _ *loop.Run, req *model.Request, resp *model.Response) error {
	mw.m.Observe(req.System, req.Messages, req.Tools, resp.Usage)
	return nil
}
