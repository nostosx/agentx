package loop

import (
	"context"
	"fmt"
)

// Limits bounds the usage of a run, including the usage that nested runs,
// such as sub-agents, report with ReportUsage. Zero fields are unlimited.
//
// Requests is checked before each model call, ToolCalls before each batch
// of tool calls, and the token limits before both: once tokens are spent,
// the run does no further work. A run that reaches a limit ends with
// StatusLimitExceeded; tool calls that did not run are answered as such,
// so the conversation stays valid. A final answer is kept even if its
// model call crossed a token limit.
type Limits struct {
	// Requests bounds the model calls.
	Requests int `json:"requests,omitempty"`
	// ToolCalls bounds the executed tool calls. A batch that would exceed
	// it does not start; denied calls do not count.
	ToolCalls int `json:"tool_calls,omitempty"`
	// InputTokens bounds model.Usage.PromptTokens, the input tokens
	// including cache reads and writes.
	InputTokens int `json:"input_tokens,omitempty"`
	// OutputTokens bounds model.Usage.OutputTokens.
	OutputTokens int `json:"output_tokens,omitempty"`
	// TotalTokens bounds model.Usage.TotalTokens.
	TotalTokens int `json:"total_tokens,omitempty"`
}

// limitResult answers the tool calls of a run that reached a limit.
const limitResult = "The run reached a usage limit before this tool call ran."

// limits returns the limits of the run: its request's, else the
// configuration's.
func (l *execution) limits() Limits {
	if l.st.Limits != nil {
		return *l.st.Limits
	}
	return l.r.cfg.Limits
}

// modelLimit returns the reason the run may not call the model, or "".
func (l *execution) modelLimit() string {
	u, lim := l.st.Usage, l.limits()
	if lim.Requests > 0 && u.Requests >= lim.Requests {
		return fmt.Sprintf("reached the limit of %d requests", lim.Requests)
	}
	return l.spent()
}

// toolLimit returns the reason the run may not execute its pending calls,
// or "".
func (l *execution) toolLimit() string {
	u, lim := l.st.Usage, l.limits()
	if lim.ToolCalls > 0 {
		n := 0
		for _, p := range l.st.Pending {
			if !p.Denied {
				n++
			}
		}
		if n > 0 && u.ToolCalls+n > lim.ToolCalls {
			return fmt.Sprintf("running %d more tool calls would exceed the limit of %d tool calls (%d used)", n, lim.ToolCalls, u.ToolCalls)
		}
	}
	return l.spent()
}

// spent returns the reason the run's token limits or budget are used up,
// or "".
func (l *execution) spent() string {
	u, lim := l.st.Usage, l.limits()
	for _, c := range []struct {
		name       string
		max, total int
	}{
		{"input tokens", lim.InputTokens, u.PromptTokens()},
		{"output tokens", lim.OutputTokens, u.OutputTokens},
		{"total tokens", lim.TotalTokens, u.TotalTokens()},
	} {
		if c.max > 0 && c.total >= c.max {
			return fmt.Sprintf("reached the limit of %d %s (%d used)", c.max, c.name, c.total)
		}
	}
	if b := l.r.cfg.Budget; b != nil {
		if err := b(u); err != nil {
			return "budget: " + err.Error()
		}
	}
	return ""
}

// exceed ends the run with StatusLimitExceeded.
func (l *execution) exceed(ctx context.Context, reason string) {
	l.closeCalls(limitResult)
	l.finish(ctx, StatusLimitExceeded, reason)
}

// runTools runs the pending calls of the turn, as tools does, unless they
// would exceed a limit. It counts the calls that ran in the usage and then
// applies the stop conditions. It returns false when the run ended or was
// interrupted.
func (l *execution) runTools(ctx context.Context, approvals map[string]Approval) bool {
	if reason := l.toolLimit(); reason != "" {
		l.exceed(ctx, reason)
		return false
	}
	pending := l.st.Pending
	if !l.tools(ctx, approvals) {
		return false
	}
	for _, p := range pending {
		if !p.Denied {
			l.st.Usage.ToolCalls++
		}
	}
	if l.halt {
		// The caller ends the run as stopped.
		return true
	}
	return !l.stopAfterTools(ctx, pending)
}
