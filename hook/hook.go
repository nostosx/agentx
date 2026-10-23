// Package hook implements lifecycle hooks that observe and control the agent
// loop, in the style of Claude Code hooks.
package hook

import (
	"cmp"
	"context"
	"encoding/json"
)

// Decision is a hook's verdict on the action that triggered it.
type Decision string

const (
	// Continue expresses no opinion.
	Continue Decision = ""
	// Allow approves the action and skips further approval.
	Allow Decision = "allow"
	// Ask requires approval before the action proceeds.
	Ask Decision = "ask"
	// Defer pauses a PreToolUse call so it can be decided later, like
	// Claude Code's "defer". agentx treats it like Ask: the call needs
	// approval, so without an Approver the run is interrupted and can be
	// resumed. It ranks between Ask and Deny.
	Defer Decision = "defer"
	// Deny blocks the action.
	Deny Decision = "deny"
)

// rank orders decisions by restrictiveness: deny > defer > ask > allow.
func (d Decision) rank() int {
	switch d {
	case Allow:
		return 1
	case Ask:
		return 2
	case Defer:
		return 3
	case Deny:
		return 4
	}
	return 0
}

// Result is the output of a hook. A nil result means Continue.
type Result struct {
	Decision Decision
	// Reason explains the decision. For Deny it is shown to the model.
	Reason string
	// AdditionalContext is added to the conversation; where depends on the
	// event type.
	AdditionalContext string
	// UpdatedInput replaces the tool arguments for PreToolUse.
	UpdatedInput json.RawMessage
	// UpdatedOutput replaces the tool result content for PostToolUse.
	UpdatedOutput *string
	// Halt stops the run after the current step.
	Halt bool
	// HaltReason explains Halt.
	HaltReason string
	// SystemMessage is a warning for the user, not the model. The hook
	// middleware logs it.
	SystemMessage string
}

// Hook handles lifecycle events.
type Hook interface {
	Handle(ctx context.Context, ev *Event) (*Result, error)
}

// Func adapts a function to Hook.
type Func func(ctx context.Context, ev *Event) (*Result, error)

// Handle calls f.
func (f Func) Handle(ctx context.Context, ev *Event) (*Result, error) { return f(ctx, ev) }

// merge combines a later hook result into r. The most restrictive decision
// wins, contexts accumulate and later updates replace earlier ones.
func (r *Result) merge(other *Result) {
	if other == nil {
		return
	}
	if other.Decision.rank() > r.Decision.rank() {
		r.Decision = other.Decision
		r.Reason = other.Reason
	} else if other.Decision == r.Decision {
		r.Reason = cmp.Or(r.Reason, other.Reason)
	}
	if other.AdditionalContext != "" {
		if r.AdditionalContext != "" {
			r.AdditionalContext += "\n"
		}
		r.AdditionalContext += other.AdditionalContext
	}
	if other.UpdatedInput != nil {
		r.UpdatedInput = other.UpdatedInput
	}
	if other.UpdatedOutput != nil {
		r.UpdatedOutput = other.UpdatedOutput
	}
	if other.Halt {
		r.Halt = true
		r.HaltReason = cmp.Or(r.HaltReason, other.HaltReason)
	}
	if other.SystemMessage != "" {
		if r.SystemMessage != "" {
			r.SystemMessage += "\n"
		}
		r.SystemMessage += other.SystemMessage
	}
}
