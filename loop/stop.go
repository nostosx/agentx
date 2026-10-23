package loop

import (
	"context"
	"fmt"
	"slices"

	"github.com/nostosx/agentx/model"
)

// Step is a turn whose tool calls have run, as a StopCondition sees it.
type Step struct {
	// Turn is the turn number.
	Turn int
	// Message is the model's message of the turn.
	Message *model.Message
	// ToolResults are the results of its tool calls, in call order.
	ToolResults []*model.ToolResult
	// Usage is the run's usage so far.
	Usage model.Usage
}

// StopCondition reports whether a run should end after a step. It must
// not modify the step. A run that a condition stops ends as completed, and
// Result.Output is the text of the model's last message, which may be
// empty when the model only called tools.
type StopCondition func(step *Step) bool

// configureStops checks the stop configuration.
func configureStops(cfg *Config) error {
	for _, name := range cfg.StopAtTools {
		if _, ok := cfg.Tools.Get(name); !ok {
			return fmt.Errorf("loop: stop tool %q is not registered", name)
		}
	}
	return nil
}

// stopAfterTools ends the run if the tool calls of pending, which have
// all run, meet a stop condition: a call of a tool in Config.StopAtTools
// that succeeded, the first in call order, or a StopWhen condition. It
// reports whether the run ended.
func (l *execution) stopAfterTools(ctx context.Context, pending []*PendingCall) bool {
	cfg := &l.r.cfg
	if len(cfg.StopAtTools) == 0 && len(cfg.StopWhen) == 0 {
		return false
	}
	for _, p := range pending {
		if p.Result != nil && !p.Result.IsError && slices.Contains(cfg.StopAtTools, p.Call.Name) {
			output := p.Result.Content
			l.output = &output
			l.finish(ctx, StatusCompleted, "")
			return true
		}
	}
	if len(cfg.StopWhen) == 0 {
		return false
	}
	step := &Step{Turn: l.st.Turn, Usage: l.st.Usage}
	for _, p := range pending {
		step.ToolResults = append(step.ToolResults, p.Result)
	}
	for _, msg := range slices.Backward(l.st.Messages) {
		if msg.Role == model.RoleAssistant {
			step.Message = msg
			break
		}
	}
	for _, cond := range cfg.StopWhen {
		if cond != nil && cond(step) {
			l.finish(ctx, StatusCompleted, "")
			return true
		}
	}
	return false
}
