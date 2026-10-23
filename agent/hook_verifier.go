package agent

import (
	"context"
	"fmt"
	"iter"

	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
)

// verifierMaxTurns bounds the turns of a hook verifier, as Claude Code
// bounds agent hooks to 50.
const verifierMaxTurns = 50

// verifierInstructions is the system prompt of HookVerifier agents.
const verifierInstructions = "You verify a condition for a hook of another AI agent. Investigate with your tools as needed, without changing anything, and answer exactly as the task asks."

// HookVerifier returns a hook.Verifier for hook.Agent hooks that runs a
// new agent on m for each check: it is built with opts, has instructions
// for the task and at most 50 turns, as in Claude Code, and its final
// answer is the verdict. A model name set on the hook overrides the
// request's model. Give it the tools it may use with WithTools, typically
// read-only ones such as builtin.ReadOnlyTools.
func HookVerifier(m model.Model, opts ...Option) hook.Verifier {
	return hook.VerifierFunc(func(ctx context.Context, name, prompt string) (string, error) {
		mdl := m
		if name != "" {
			mdl = namedModel{Model: m, name: name}
		}
		base := []Option{WithModel(mdl), WithInstructions(verifierInstructions), WithMaxTurns(verifierMaxTurns)}
		a, err := New("hook_verifier", append(base, opts...)...)
		if err != nil {
			return "", err
		}
		return a.Ask(ctx, prompt)
	})
}

// AgentVerifier returns a hook.Verifier for hook.Agent hooks that runs a
// on each check's prompt, without history, and returns its final answer.
// The hook's model name is ignored.
func AgentVerifier(a Agent) hook.Verifier {
	return hook.VerifierFunc(func(ctx context.Context, _, prompt string) (string, error) {
		res, err := loop.Collect(a.Run(ctx, Text(prompt)), nil)
		if err != nil {
			return "", err
		}
		if res.Status != loop.StatusCompleted {
			return res.Output, fmt.Errorf("agent %s: run %s: %s", a.Name(), res.Status, res.Reason)
		}
		return res.Output, nil
	})
}

// namedModel sends requests to Model with name as the model name.
type namedModel struct {
	model.Model
	name string
}

func (m namedModel) Generate(ctx context.Context, req *model.Request) (*model.Response, error) {
	req = req.Clone()
	req.Model = m.name
	return m.Model.Generate(ctx, req)
}

func (m namedModel) Stream(ctx context.Context, req *model.Request) iter.Seq2[*model.StreamEvent, error] {
	req = req.Clone()
	req.Model = m.name
	return m.Model.Stream(ctx, req)
}

// Describe implements model.Describer.
func (m namedModel) Describe() model.Description {
	d := model.Describe(m.Model)
	d.Model = m.name
	return d
}
