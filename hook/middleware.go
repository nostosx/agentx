package hook

import (
	"cmp"
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
)

// Run state keys used by the middleware.
const (
	// stopActiveKey counts the consecutive continuations Stop hooks
	// caused.
	stopActiveKey = "hook.stop_active"
	// stopContinuedKey is set once Stop hooks continued the run.
	stopContinuedKey = "hook.stop_continued"
	contextKey       = "hook.context"
)

// Middleware returns runtime middleware that dispatches lifecycle events to
// m. Hook errors are logged and otherwise ignored unless m is FailClosed.
// Context queued for the run's session with Manager.AddContext, such as
// the results of Async hooks, is added to the run's input and to tool
// results.
//
// PreCompact and PostCompact hooks run through PreCompactGuard and
// PostCompactObserver, SubagentStart and SubagentStop hooks through
// agent.AsTool, and SessionEnd hooks through agent.Runner.EndSession.
func Middleware(m *Manager) loop.Middleware { return &middleware{m: m} }

type middleware struct{ m *Manager }

func (h *middleware) Name() string { return "hook" }

// dispatch runs the hooks for ev, logs failures and applies Halt.
func dispatch(ctx context.Context, m *Manager, run *loop.Run, ev *Event) *Result {
	res, err := m.Dispatch(ctx, ev)
	if err != nil {
		run.Logger().Warn("hook failed", "event", ev.Type, "run_id", run.ID(), "error", err)
	}
	if res.SystemMessage != "" {
		run.Logger().Warn("hook message", "event", ev.Type, "run_id", run.ID(), "message", res.SystemMessage)
	}
	if res.Halt {
		run.Halt(res.HaltReason)
	}
	return res
}

func (h *middleware) BeforeRun(ctx context.Context, run *loop.Run, in *loop.RunInput) error {
	var extra []string
	if in.NewSession && h.m.Has(SessionStart) {
		ev := h.m.event(run, SessionStart)
		ev.Prompt = in.Query
		ev.Reason = "startup"
		if res := dispatch(ctx, h.m, run, ev); res.AdditionalContext != "" {
			extra = append(extra, res.AdditionalContext)
		}
	}
	if queued := h.m.takeContext(run.SessionID()); queued != "" {
		extra = append(extra, queued)
	}
	if h.m.Has(UserPromptSubmit) {
		ev := h.m.event(run, UserPromptSubmit)
		ev.Prompt = in.Query
		res := dispatch(ctx, h.m, run, ev)
		if res.Decision == Deny {
			return loop.Block(cmp.Or(res.Reason, "prompt blocked by hook"))
		}
		if res.AdditionalContext != "" {
			extra = append(extra, res.AdditionalContext)
		}
	}
	if len(extra) > 0 {
		in.Message.Parts = slices.Insert(in.Message.Parts, 0, model.TextPart(model.WrapReminder(strings.Join(extra, "\n\n"))))
	}
	return nil
}

func (h *middleware) BeforeModel(ctx context.Context, run *loop.Run, req *model.Request) error {
	if !h.m.Has(PreModel) {
		return nil
	}
	ev := h.m.event(run, PreModel)
	ev.Request = req
	if res := dispatch(ctx, h.m, run, ev); res.Decision == Deny {
		return loop.Stop(cmp.Or(res.Reason, "model call denied by hook"))
	}
	return nil
}

func (h *middleware) AfterModel(ctx context.Context, run *loop.Run, _ *model.Request, resp *model.Response) error {
	if len(resp.ToolCalls()) > 0 {
		run.SetValue(stopActiveKey, "")
	}
	if h.m.Has(PostModel) {
		ev := h.m.event(run, PostModel)
		ev.Response = resp
		dispatch(ctx, h.m, run, ev)
	}
	return nil
}

func (h *middleware) BeforeTool(ctx context.Context, run *loop.Run, plan *loop.ToolPlan) error {
	if !h.m.Has(PreToolUse) {
		return nil
	}
	ev := h.m.event(run, PreToolUse)
	ev.ToolCall = plan.Call
	res := dispatch(ctx, h.m, run, ev)
	switch res.Decision {
	case Deny:
		plan.Deny(cmp.Or(res.Reason, "This tool call was blocked by a hook."))
	case Ask, Defer:
		plan.Ask(res.Reason)
	case Allow:
		plan.Allow()
	}
	if res.Decision != Defer {
		plan.SetValue(contextKey, res.AdditionalContext)
	}
	return nil
}

func (h *middleware) AfterTool(ctx context.Context, run *loop.Run, plan *loop.ToolPlan, res *model.ToolResult) error {
	extra, _ := plan.Value(contextKey)
	// Calls that ran fire PostToolUse, or PostToolUseFailure if they
	// failed; denied calls fire neither.
	t := PostToolUse
	if res.IsError {
		t = PostToolUseFailure
	}
	if d, _ := plan.Decision(); d != loop.Denied && h.m.Has(t) {
		ev := h.m.event(run, t)
		ev.ToolCall = &model.ToolCall{ID: plan.Call.ID, Name: plan.Call.Name, Arguments: slices.Clone(plan.Call.Arguments)}
		ev.ToolResult = res
		out := dispatch(ctx, h.m, run, ev)
		if out.UpdatedOutput != nil && t == PostToolUse {
			res.Content = *out.UpdatedOutput
		}
		if out.Decision == Deny && out.Reason != "" {
			res.Content += "\n\n" + model.WrapReminder(out.Reason)
		}
		if out.AdditionalContext != "" {
			if extra != "" {
				extra += "\n"
			}
			extra += out.AdditionalContext
		}
	}
	if queued := h.m.takeContext(run.SessionID()); queued != "" {
		if extra != "" {
			extra += "\n"
		}
		extra += queued
	}
	if extra != "" {
		res.Content += "\n\n" + model.WrapReminder(extra)
	}
	return nil
}

func (h *middleware) BeforeFinish(ctx context.Context, run *loop.Run, msg *model.Message) (bool, error) {
	if !h.m.Has(Stop) {
		return false, nil
	}
	blocks := stopBlocks(run)
	continued, _ := run.Value(stopContinuedKey)
	ev := h.m.event(run, Stop)
	ev.Messages = run.Messages()
	ev.Message = msg.Text()
	ev.StopHookActive = blocks > 0 || continued != ""
	res := dispatch(ctx, h.m, run, ev)
	next := StopFeedback(Stop, res)
	if next == "" {
		return false, nil
	}
	if limit := h.m.MaxStopBlocks(); limit > 0 && blocks >= limit {
		run.Logger().Warn("stop hooks continued the run too often in a row; stopping", "run_id", run.ID(), "limit", limit)
		return false, nil
	}
	run.SetValue(stopActiveKey, strconv.Itoa(blocks+1))
	run.SetValue(stopContinuedKey, "1")
	run.Append(model.UserText(model.WrapReminder(next)))
	return true, nil
}

// stopBlocks returns the consecutive continuations Stop hooks caused.
func stopBlocks(run *loop.Run) int {
	v, ok := run.Value(stopActiveKey)
	if !ok || v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// StopFeedback returns the input that continues an agent whose Stop or
// SubagentStop hooks, as given by event, returned res, or "" if it may
// stop: the Reason of a Deny and any AdditionalContext, which Claude Code
// delivers as hook feedback. A Halt lets the agent stop.
func StopFeedback(event EventType, res *Result) string {
	if res == nil || res.Halt {
		return ""
	}
	var parts []string
	if res.Decision == Deny && res.Reason != "" {
		parts = append(parts, "A "+string(event)+" hook prevented you from finishing:\n"+res.Reason)
	}
	if res.AdditionalContext != "" {
		parts = append(parts, string(event)+" hook feedback:\n"+res.AdditionalContext)
	}
	return strings.Join(parts, "\n\n")
}

func (h *middleware) AfterRun(ctx context.Context, run *loop.Run, res *loop.Result) {
	if res.Status != loop.StatusInterrupted || res.Interrupt == nil || len(res.Interrupt.Requests) == 0 || !h.m.Has(Notification) {
		return
	}
	ev := h.m.event(run, Notification)
	ev.Message = "Approval required for " + res.Interrupt.Requests[0].Call.Name
	ev.Title = "Approval required"
	ev.Reason = "approval"
	dispatch(ctx, h.m, run, ev)
}

// PreCompactGuard returns a function that runs the PreCompact hooks of m
// and reports whether compaction may proceed. Pass it to
// context.WithCompactGuard.
func PreCompactGuard(m *Manager) func(ctx context.Context, run *loop.Run, reason string) bool {
	return func(ctx context.Context, run *loop.Run, reason string) bool {
		if !m.Has(PreCompact) {
			return true
		}
		ev := m.event(run, PreCompact)
		ev.Messages = run.Messages()
		ev.Reason = reason
		return dispatch(ctx, m, run, ev).Decision != Deny
	}
}

// PostCompactObserver returns a function that runs the PostCompact hooks of
// m after a compaction, with the summary in Event.Message, and then its
// SessionStart hooks with source "compact", as Claude Code does. PostCompact
// results are ignored; the AdditionalContext of the SessionStart hooks is
// added to the first user message of the compacted conversation, next to
// the summary. Errors are logged. Pass it to context.WithCompactObserver.
func PostCompactObserver(m *Manager) func(ctx context.Context, run *loop.Run, reason string, c *loop.Compaction) {
	return func(ctx context.Context, run *loop.Run, reason string, c *loop.Compaction) {
		if m.Has(PostCompact) {
			ev := m.event(run, PostCompact)
			ev.Messages = c.Messages
			ev.Message = c.Summary
			ev.Reason = reason
			if _, err := m.Dispatch(ctx, ev); err != nil {
				run.Logger().Warn("hook failed", "event", ev.Type, "run_id", run.ID(), "error", err)
			}
		}
		if m.Has(SessionStart) {
			ev := m.event(run, SessionStart)
			ev.Messages = c.Messages
			ev.Reason = "compact"
			res, err := m.Dispatch(ctx, ev)
			if err != nil {
				run.Logger().Warn("hook failed", "event", ev.Type, "run_id", run.ID(), "error", err)
			}
			if res.AdditionalContext != "" {
				run.ReplaceMessages(withReminder(run.Messages(), res.AdditionalContext))
			}
		}
	}
}

// withReminder returns msgs with text added as a system reminder in front
// of the first user message, which is replaced rather than modified, or
// in a new user message after the system messages if there is none.
func withReminder(msgs []*model.Message, text string) []*model.Message {
	part := model.TextPart(model.WrapReminder(text))
	out := slices.Clone(msgs)
	for i, msg := range out {
		if msg.Role == model.RoleUser {
			c := *msg
			c.Parts = slices.Insert(slices.Clone(msg.Parts), 0, part)
			out[i] = &c
			return out
		}
	}
	i := 0
	for i < len(out) && out[i].Role == model.RoleSystem {
		i++
	}
	return slices.Insert(out, i, model.NewMessage(model.RoleUser, part))
}
