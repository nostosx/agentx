package agent

import (
	"cmp"
	"context"
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"strings"
	"uuid"

	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/tool"
)

// DefaultMaxDepth bounds how deeply sub-agents can be nested: a sub-agent of
// a top-level agent is at depth 1, its own sub-agents at depth 2.
const DefaultMaxDepth = 5

// ToolOptions configures the tool that runs an agent as a sub-agent; see
// AsToolWith. The zero value gives the defaults of AsTool.
type ToolOptions struct {
	// Name is the tool name. It defaults to the agent's name. Hooks match
	// the agent's name either way.
	Name string
	// Description is the tool description. It defaults to the agent's
	// description, with a note that the agent cannot see the conversation.
	Description string
	// Hooks receives SubagentStart and SubagentStop events.
	Hooks *hook.Manager
	// Events forwards the sub-agent's events to the consumer of the calling
	// run, tagged with loop.Event.Depth and ParentCallID. Sub-agents of
	// the sub-agent then forward theirs too. See loop.ForwardEvent.
	Events bool
	// Concurrent lets calls run in parallel with adjacent calls of
	// concurrent or read-only tools; see tool.Concurrent. SubagentStart
	// and SubagentStop hooks of sub-agents running concurrently may run
	// concurrently.
	Concurrent bool
	// MaxDepth bounds how deeply the sub-agent may be nested; zero means
	// DefaultMaxDepth. The smallest bound of the agents above a sub-agent
	// applies. A call that would exceed it returns an error result.
	MaxDepth int
}

// nesting describes the sub-agent runs a context belongs to.
type nesting struct {
	// depth is the nesting depth of the sub-agent run, limit the bound of
	// the agents above it.
	depth, limit int
	// events is set when the events of nested sub-agents are forwarded.
	events bool
}

type nestingKey struct{}

type taskArgs struct {
	Prompt string `json:"prompt" jsonschema:"A complete, self-contained description of the task. The agent cannot see this conversation."`
}

// AsToolWith is like AsTool with options. Before the sub-agent starts a
// task, SubagentStart hooks run; their AdditionalContext is added in front
// of the task.
func AsToolWith(a Agent, opts ToolOptions) tool.Tool {
	desc := opts.Description
	if desc == "" {
		desc = cmp.Or(a.Description(), "Delegate a task to the "+a.Name()+" agent.") +
			" The agent starts with no knowledge of this conversation, so include all needed context in the prompt."
	}
	var topts []tool.Option
	if opts.Concurrent {
		topts = append(topts, tool.Concurrent())
	}
	hooks := opts.Hooks
	return tool.Func(cmp.Or(opts.Name, a.Name()), desc, func(ctx context.Context, in taskArgs) (*tool.Result, error) {
		outer, _ := ctx.Value(nestingKey{}).(nesting)
		limit := opts.MaxDepth
		if limit <= 0 {
			limit = DefaultMaxDepth
		}
		if outer.limit > 0 {
			limit = min(limit, outer.limit)
		}
		if outer.depth >= limit {
			return tool.Errorf("agent %s was not started: sub-agents can be nested at most %d levels deep", a.Name(), limit), nil
		}
		inner := nesting{depth: outer.depth + 1, limit: limit, events: opts.Events || outer.events}
		ctx = context.WithValue(ctx, nestingKey{}, inner)
		collect := func(events iter.Seq2[*loop.Event, error]) (*loop.Result, error) {
			var forward func(*loop.Event)
			if inner.events {
				forward = func(ev *loop.Event) { loop.ForwardEvent(ctx, ev) }
			}
			return loop.Collect(events, forward)
		}

		var (
			res   *loop.Result
			err   error
			prior model.Usage // usage reported before an interrupt
		)
		if intr, approvals, ok := loop.ResumedCall(ctx); ok {
			if intr.State != nil {
				prior = intr.State.Usage
			}
			res, err = resumeSubAgent(ctx, a, intr, approvals, collect)
		} else {
			if strings.TrimSpace(in.Prompt) == "" {
				return tool.Errorf("prompt is empty"), nil
			}
			runID := "run_" + uuid.NewV7().String()
			msg := model.UserText(in.Prompt)
			if extra := subagentStart(ctx, a, hooks, runID, in.Prompt); extra != "" {
				msg.Parts = slices.Insert(msg.Parts, 0, model.TextPart(model.WrapReminder(extra)))
			}
			res, err = collect(a.Run(ctx, &Input{Message: msg, RunID: runID}))
		}
		// blocks counts the consecutive continuations SubagentStop hooks
		// caused, and continued records that there was one; as for Stop, a
		// continuation that called tools resets the count.
		blocks, continued := 0, false
		for {
			if res != nil {
				loop.ReportUsage(ctx, usageSince(res.Usage, prior))
			}
			prior = model.Usage{}
			if err == nil && res.Status == loop.StatusInterrupted && loop.InterruptCall(ctx, res.Interrupt) {
				// The calling run pauses for the sub-agent's approvals.
				return tool.Text(""), nil
			}
			if continued && calledTools(res) {
				blocks = 0
			}
			next := subagentStop(ctx, a, hooks, res, continued)
			if next == "" || err != nil || res.Status != loop.StatusCompleted {
				break
			}
			if limit := hooks.MaxStopBlocks(); limit > 0 && blocks >= limit {
				if run := loop.CallingRun(ctx); run != nil {
					run.Logger().Warn("subagent stop hooks continued the sub-agent too often in a row; stopping", "agent", a.Name(), "run_id", res.RunID, "limit", limit)
				}
				break
			}
			blocks++
			continued = true
			// A hook blocked the stop; the sub-agent continues with the
			// feedback.
			res, err = collect(a.Run(ctx, &Input{
				Message: model.UserText(model.WrapReminder(next)),
				History: res.Messages,
			}))
		}
		if err != nil {
			return nil, err
		}
		switch res.Status {
		case loop.StatusCompleted:
			return tool.Text(res.Output), nil
		case loop.StatusInterrupted:
			return tool.Errorf("agent %s needs approval for a tool call and cannot continue as a sub-agent here", a.Name()), nil
		default:
			out := fmt.Sprintf("agent %s ended with status %s", a.Name(), res.Status)
			if res.Reason != "" {
				out += ": " + res.Reason
			}
			if res.Output != "" {
				out += "\n\nPartial answer:\n" + res.Output
			}
			return tool.Errorf("%s", out), nil
		}
	}, topts...)
}

// hookEvent returns a hook event of a sub-agent, with the calling run's
// session and turn, and the logger for hook failures.
func hookEvent(ctx context.Context, t hook.EventType, a Agent) (*hook.Event, *slog.Logger) {
	ev := &hook.Event{Type: t, Agent: a.Name(), Reason: a.Name()}
	log := slog.New(slog.DiscardHandler)
	if run := loop.CallingRun(ctx); run != nil {
		ev.SessionID, ev.Turn, log = run.SessionID(), run.Turn(), run.Logger()
	}
	return ev, log
}

// subagentStart dispatches the SubagentStart event of a sub-agent run about
// to start and returns the context the hooks added.
func subagentStart(ctx context.Context, a Agent, hooks *hook.Manager, runID, prompt string) string {
	if !hooks.Has(hook.SubagentStart) {
		return ""
	}
	ev, log := hookEvent(ctx, hook.SubagentStart, a)
	ev.RunID, ev.Prompt = runID, prompt
	out, err := hooks.Dispatch(ctx, ev)
	if err != nil {
		log.Warn("hook failed", "event", ev.Type, "run_id", ev.RunID, "error", err)
	}
	return out.AdditionalContext
}

// subagentStop dispatches the SubagentStop event of a sub-agent run that
// ended and returns the input that continues it, if a hook blocked the
// stop or added context; see hook.StopFeedback.
func subagentStop(ctx context.Context, a Agent, hooks *hook.Manager, res *loop.Result, active bool) string {
	if !hooks.Has(hook.SubagentStop) {
		return ""
	}
	ev, log := hookEvent(ctx, hook.SubagentStop, a)
	ev.StopHookActive = active
	if res != nil {
		ev.RunID, ev.Message, ev.Messages = res.RunID, res.Output, res.Messages
	}
	out, err := hooks.Dispatch(ctx, ev)
	if err != nil {
		log.Warn("hook failed", "event", ev.Type, "run_id", ev.RunID, "error", err)
	}
	return hook.StopFeedback(hook.SubagentStop, out)
}

// calledTools reports whether the last run of a sub-agent called tools.
func calledTools(res *loop.Result) bool {
	if res == nil {
		return false
	}
	for _, msg := range res.NewMessages {
		if len(msg.ToolCalls()) > 0 {
			return true
		}
	}
	return false
}

// resumeSubAgent resumes the interrupted run of a sub-agent, from the
// interrupt's state when possible so no checkpointer is needed.
func resumeSubAgent(ctx context.Context, a Agent, intr *loop.Interrupt, approvals map[string]loop.Approval, collect func(iter.Seq2[*loop.Event, error]) (*loop.Result, error)) (*loop.Result, error) {
	if sr, ok := a.(StateResumer); ok && intr.State != nil {
		return collect(sr.ResumeState(ctx, intr.State, approvals))
	}
	if r, ok := a.(Resumer); ok {
		return collect(r.Resume(ctx, intr.RunID, approvals))
	}
	return nil, fmt.Errorf("agent %s cannot resume runs", a.Name())
}

// usageSince returns the usage in total beyond prior, the part of a
// resumed run's cumulative usage that was not reported yet.
func usageSince(total, prior model.Usage) model.Usage {
	sub := func(a, b int) int { return max(a-b, 0) }
	return model.Usage{
		InputTokens:      sub(total.InputTokens, prior.InputTokens),
		OutputTokens:     sub(total.OutputTokens, prior.OutputTokens),
		CacheReadTokens:  sub(total.CacheReadTokens, prior.CacheReadTokens),
		CacheWriteTokens: sub(total.CacheWriteTokens, prior.CacheWriteTokens),
		ReasoningTokens:  sub(total.ReasoningTokens, prior.ReasoningTokens),
		Requests:         sub(total.Requests, prior.Requests),
		ToolCalls:        sub(total.ToolCalls, prior.ToolCalls),
	}
}
