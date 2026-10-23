package agent

import (
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/skill"
)

// RunOption configures a single run of a Runner.
type RunOption func(*Input)

// RunMaxTurns overrides the agent's turn limit for one run; see
// Input.MaxTurns. Resuming the run keeps it.
func RunMaxTurns(n int) RunOption { return func(in *Input) { in.MaxTurns = n } }

// RunLimits overrides the agent's usage limits for one run; see
// Input.Limits. Resuming the run keeps them.
func RunLimits(l loop.Limits) RunOption { return func(in *Input) { in.Limits = &l } }

// RunValue sets an initial value of one run, as loop.Run.SetValue
// stores it, for example permission.ModeKey to choose the run's
// permission mode. It overrides a session-scoped value of the same key.
func RunValue(key, value string) RunOption {
	return func(in *Input) {
		if in.Values == nil {
			in.Values = map[string]string{}
		}
		in.Values[key] = value
	}
}

// RunSkill invokes a skill as the user at the start of the run, as typing
// /name arguments does in Claude Code: its content is added in front of
// the message and it is active for the run, and the model may invoke it
// even with disable-model-invocation. See skill.WithCall.
func RunSkill(name, arguments string) RunOption {
	return func(in *Input) { in.Skill = &skill.Call{Name: name, Arguments: arguments} }
}
