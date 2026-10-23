// Package guard implements guardrails: checks of a run's input, its final
// output and its tool calls that end the run when their tripwire
// triggers, in the style of the OpenAI Agents SDK's guardrails. They run
// as runtime middleware; agent.WithInputGuardrail, WithOutputGuardrail
// and WithToolGuardrail add them to an agent.
//
// A guardrail whose tripwire triggers ends the run with
// loop.StatusBlocked and loop.Result.Guardrail describing it. A
// guardrail that returns an error fails the run.
package guard

import (
	"context"
	"slices"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
)

// Result is the verdict of a guardrail.
type Result struct {
	// Tripwire ends the run when set.
	Tripwire bool
	// Info is any information to report, as loop.Guardrail.Info.
	Info any
	// Message explains the verdict, for example to show to the user.
	Message string
}

// Kinds of guardrails, as loop.Guardrail.Kind reports them.
const (
	KindInput      = "input"
	KindOutput     = "output"
	KindToolInput  = "tool_input"
	KindToolOutput = "tool_output"
)

// InputGuardrail checks the input of a new run. It does not run when a
// run is resumed, nor for the agent a run is handed off to.
type InputGuardrail struct {
	Name string
	// Check checks the input message. It must not modify it.
	Check func(ctx context.Context, run *loop.Run, input *model.Message) (Result, error)
	// Parallel runs the check concurrently with the run's first model
	// call instead of before the run starts; see Parallel.
	Parallel bool
}

// InputOption configures an input guardrail.
type InputOption func(*InputGuardrail)

// Parallel runs an input guardrail concurrently with the run's first
// model call, which lowers latency: the call is cancelled through its
// context when the tripwire triggers, and its response is held until all
// parallel guardrails have finished, so no tool runs and no answer is
// handled before. The usage of the call is still counted when it
// completed before a guardrail tripped; a call cut short reports none.
// With streaming, deltas of the first call may reach the consumer before
// a guardrail trips. By default input guardrails run one after another
// before the run starts, so a trip costs no tokens.
//
// A run whose process stopped before its parallel guardrails finished
// runs them again when it is resumed.
func Parallel() InputOption { return func(g *InputGuardrail) { g.Parallel = true } }

// OutputGuardrail checks the final answer of a run, once the model
// answered without tool calls and the other middleware, such as Stop
// hooks, let the run finish. A trip withholds the answer: the
// conversation keeps loop.WithheldAnswer in its place.
type OutputGuardrail struct {
	Name  string
	Check func(ctx context.Context, run *loop.Run, output *model.Message) (Result, error)
	// Hold holds the answer's events until the output guardrails pass;
	// see HoldOutput.
	Hold bool
}

// OutputOption configures an output guardrail.
type OutputOption func(*OutputGuardrail)

// HoldOutput holds the events of each model response, its deltas and its
// message, until the response turns out to call tools, when they are
// sent, or until the output guardrails pass, so a consumer never sees an
// answer they withhold, nor one they never checked because it did not end
// the run, such as an answer a Stop hook continues; see loop.AnswerHolder.
// The text of a final answer then arrives at once, after the model
// finished it. Without it an answer streams as the model writes it, and a
// trip withholds it only from the conversation, the result and the session.
func HoldOutput() OutputOption { return func(g *OutputGuardrail) { g.Hold = true } }

// ToolGuardrail checks tool calls before they run and their results
// after. Either function may be nil.
type ToolGuardrail struct {
	Name string
	// Tools, if not empty, limits the guardrail to the tools named.
	Tools []string
	// Before checks a call while the turn's calls are planned, after
	// PreToolUse hooks and before approval; calls already denied are not
	// checked. A trip ends the run without running the turn's calls.
	Before func(ctx context.Context, run *loop.Run, call *model.ToolCall) (Result, error)
	// After checks the result of a call that ran, before other middleware,
	// tracing and PostToolUse hooks see it; only the tool middleware of
	// the agent's executor runs inside it. A trip replaces the result with
	// the guardrail's message, without the tool's parts and metadata, so
	// the withheld content is neither sent nor kept in the conversation,
	// and ends the run.
	After func(ctx context.Context, run *loop.Run, call *model.ToolCall, res *model.ToolResult) (Result, error)
}

func (g *ToolGuardrail) applies(name string) bool {
	return len(g.Tools) == 0 || slices.Contains(g.Tools, name)
}

// Config lists the guardrails of an agent.
type Config struct {
	Input  []InputGuardrail
	Output []OutputGuardrail
	Tool   []ToolGuardrail
}

// Empty reports whether c has no guardrails.
func (c Config) Empty() bool { return len(c.Input)+len(c.Output)+len(c.Tool) == 0 }

// SkipInputKey is a run value that, when set, makes input guardrails skip
// the run, as the agent package sets it for the agent a run is handed off
// to: its input was checked already.
const SkipInputKey = "guard.skip_input"

// trip returns the error that ends a run for a tripped guardrail.
func trip(name, kind string, r Result) *loop.GuardrailError {
	if name == "" {
		name = kind
	}
	return &loop.GuardrailError{Guardrail: loop.Guardrail{Name: name, Kind: kind, Message: r.Message, Info: r.Info}}
}
