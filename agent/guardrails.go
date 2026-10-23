package agent

import "github.com/nostosx/agentx/guard"

// WithInputGuardrail checks the input of the agent's new runs with g; a
// tripped guardrail ends the run with loop.StatusBlocked and
// loop.Result.Guardrail set. Guardrails run one after another before
// the run starts, before hooks see the input; guard.Parallel runs g
// concurrently with the first model call instead. Input guardrails do not
// run for the agent a run is handed off to, whose input was checked.
func WithInputGuardrail(g guard.InputGuardrail, opts ...guard.InputOption) Option {
	return func(c *Config) {
		for _, opt := range opts {
			opt(&g)
		}
		c.Guardrails.Input = append(c.Guardrails.Input, g)
	}
}

// WithOutputGuardrail checks the final answer of the agent's runs with g,
// after Stop hooks let the run finish. A tripped guardrail ends the run
// with loop.StatusBlocked and an empty Output, and the conversation
// keeps loop.WithheldAnswer in place of the answer. guard.HoldOutput
// also holds the answer's events until the guardrails pass. When a run is
// handed off, the output guardrails of the agent that answers apply.
func WithOutputGuardrail(g guard.OutputGuardrail, opts ...guard.OutputOption) Option {
	return func(c *Config) {
		for _, opt := range opts {
			opt(&g)
		}
		c.Guardrails.Output = append(c.Guardrails.Output, g)
	}
}

// WithToolGuardrail checks the agent's tool calls before they run and
// their results after; see guard.ToolGuardrail. A tripped guardrail ends
// the run with loop.StatusBlocked.
func WithToolGuardrail(g guard.ToolGuardrail) Option {
	return func(c *Config) { c.Guardrails.Tool = append(c.Guardrails.Tool, g) }
}
