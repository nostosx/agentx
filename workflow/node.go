package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/loop"
)

// Node is a unit of work in a workflow.
type Node interface {
	Run(ctx context.Context, st *State) error
}

// NodeFunc adapts a function to Node.
type NodeFunc func(ctx context.Context, st *State) error

// Run calls f.
func (f NodeFunc) Run(ctx context.Context, st *State) error { return f(ctx, st) }

// AgentNode runs an agent. The prompt is built from the state by prompt,
// and the agent's answer is stored under output.
//
// In a workflow run the agent's events are forwarded as AgentEvent, and
// its run has the ID SubRunID of the node attempt. When the agent is
// interrupted for tool approvals, the node suspends with the
// loop.Interrupt as payload (see Pending.Interrupt); resuming the node's
// key with a map[string]loop.Approval resumes the agent from the
// interrupt with those decisions, from its state if the agent implements
// agent.StateResumer and otherwise with agent.Resumer. If the agent is
// interrupted again, the node suspends again.
//
// When an attempt runs again after a crash or cancellation, an agent with
// a Runtime and a loop.Checkpointer, such as an agent.LLMAgent with
// agent.WithCheckpointer, resumes its checkpointed run instead of starting
// over, so finished tool calls are not repeated and cut-off calls with
// side effects are not run twice; a run that was interrupted suspends the
// node again with its pending requests. Other agents start over.
//
// Outside a workflow run, an interrupted agent fails the node.
func AgentNode(a agent.Agent, prompt func(*State) string, output string) Node {
	return NodeFunc(func(ctx context.Context, st *State) error {
		sc := scopeOf(ctx)
		var forward func(*loop.Event)
		var runID string
		if sc != nil {
			forward = func(ev *loop.Event) {
				sc.r.emit(Event{Type: AgentEvent, Node: sc.node, Index: sc.index, Attempt: sc.try, Agent: ev})
			}
			runID = SubRunID(sc.r.id, sc.node, sc.attempt, sc.index)
		}
		var (
			res *loop.Result
			err error
		)
		if sc != nil {
			if pt, ok := sc.lastResumed(); ok && pt.Agent {
				res, err = resumeAgent(ctx, a, pt, forward)
			} else {
				var intr *loop.Interrupt
				res, intr, err = recoverAgent(ctx, a, runID, forward)
				if intr != nil {
					return suspendAgent(sc, intr)
				}
			}
		}
		if res == nil && err == nil {
			text := prompt(st)
			if strings.TrimSpace(text) == "" {
				return fmt.Errorf("agent %s: empty prompt", a.Name())
			}
			in := agent.Text(text)
			in.RunID = runID
			res, err = loop.Collect(a.Run(ctx, in), forward)
		}
		if err != nil {
			discardAgentRun(ctx, a, runID)
			return fmt.Errorf("agent %s: %w", a.Name(), err)
		}
		if res.Status == loop.StatusInterrupted && sc != nil && res.Interrupt != nil {
			return suspendAgent(sc, res.Interrupt)
		}
		if res.Status != loop.StatusCompleted {
			discardAgentRun(ctx, a, runID)
			return fmt.Errorf("agent %s: run %s: %s", a.Name(), res.Status, res.Reason)
		}
		if output != "" {
			st.Set(output, res.Output)
		}
		return nil
	})
}

// suspendAgent suspends the node for the approvals of intr.
func suspendAgent(sc *scope, intr *loop.Interrupt) error {
	b, err := json.Marshal(intr)
	if err != nil {
		return fmt.Errorf("workflow: encode agent interrupt: %w", err)
	}
	_, err = sc.suspend(b, true)
	return err
}

// resumeAgent resumes the agent's interrupted run with the approvals of pt.
func resumeAgent(ctx context.Context, a agent.Agent, pt SuspendPoint, forward func(*loop.Event)) (*loop.Result, error) {
	var intr loop.Interrupt
	if err := json.Unmarshal(pt.Payload, &intr); err != nil {
		return nil, fmt.Errorf("decode interrupt: %w", err)
	}
	var approvals map[string]loop.Approval
	if err := json.Unmarshal(pt.Resume, &approvals); err != nil {
		return nil, fmt.Errorf("resume value must map tool call IDs to runtime.Approval: %w", err)
	}
	if approvals == nil {
		approvals = map[string]loop.Approval{}
	}
	if sr, ok := a.(agent.StateResumer); ok && intr.State != nil {
		return loop.Collect(sr.ResumeState(ctx, intr.State, approvals), forward)
	}
	if r, ok := a.(agent.Resumer); ok {
		return loop.Collect(r.Resume(ctx, intr.RunID, approvals), forward)
	}
	return nil, fmt.Errorf("agent %s cannot resume runs", a.Name())
}

// runtimeAgent is implemented by agents built on a runtime, such as
// agent.LLMAgent.
type runtimeAgent interface{ Runtime() *loop.Runtime }

// recoverAgent continues the agent's checkpointed run runID, if there is
// one: it resumes a run that was running or failed and returns the
// interrupt of a run that was interrupted. It returns nils when there is
// nothing to continue.
func recoverAgent(ctx context.Context, a agent.Agent, runID string, forward func(*loop.Event)) (*loop.Result, *loop.Interrupt, error) {
	ra, ok := a.(runtimeAgent)
	if !ok || runID == "" {
		return nil, nil, nil
	}
	rt := ra.Runtime()
	cp := rt.Config().Checkpointer
	if cp == nil {
		return nil, nil, nil
	}
	st, err := cp.Load(ctx, runID)
	if errors.Is(err, loop.ErrCheckpointNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("load checkpoint of run %s: %w", runID, err)
	}
	switch {
	case st.Status == loop.StatusInterrupted:
		return nil, interruptOf(st), nil
	case st.Status.Resumable() && len(st.Messages) > st.Start:
		res, err := loop.Collect(rt.Resume(ctx, runID, nil), forward)
		return res, nil, err
	}
	return nil, nil, nil
}

// interruptOf rebuilds the interrupt of an interrupted run from its state.
func interruptOf(st *loop.State) *loop.Interrupt {
	intr := &loop.Interrupt{RunID: st.RunID, State: st}
	for _, p := range st.Pending {
		switch {
		case p.NeedsApproval:
			intr.Requests = append(intr.Requests, &loop.ApprovalRequest{Call: p.Call, Reason: p.Reason, Agent: st.Agent})
		case p.Nested != nil && p.NestedApprovals == nil:
			intr.Requests = append(intr.Requests, p.Nested.Requests...)
		}
	}
	return intr
}

// discardAgentRun deletes the checkpoint of an agent run that ended in a
// workflow, unless the run was cancelled and may be resumed.
func discardAgentRun(ctx context.Context, a agent.Agent, runID string) {
	if runID == "" || ctx.Err() != nil {
		return
	}
	if d, ok := a.(agent.Discarder); ok {
		_ = d.Discard(context.WithoutCancel(ctx), runID)
	}
}

// Template returns a prompt function that replaces {{key}} with state
// values.
func Template(tmpl string) func(*State) string {
	return func(st *State) string {
		var pairs []string
		for k, v := range st.Snapshot() {
			pairs = append(pairs, "{{"+k+"}}", format(v))
		}
		return strings.NewReplacer(pairs...).Replace(tmpl)
	}
}

// Sequence runs nodes one after another.
func Sequence(nodes ...Node) Node {
	return NodeFunc(func(ctx context.Context, st *State) error {
		for _, n := range nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := n.Run(ctx, st); err != nil {
				return err
			}
		}
		return nil
	})
}

// ErrMaxIterations is returned by Loop when the iteration limit is reached
// while the condition still holds.
var ErrMaxIterations = errors.New("workflow: loop reached its iteration limit")

// Loop runs body while cond returns true, at most limit times. The
// condition is checked before each iteration. The current iteration,
// starting at 0, is stored in the state under "<name>.iteration" when name
// is set.
func Loop(name string, body Node, cond func(ctx context.Context, st *State) (bool, error), limit int) Node {
	return NodeFunc(func(ctx context.Context, st *State) error {
		for i := 0; ; i++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			ok, err := cond(ctx, st)
			if err != nil || !ok {
				return err
			}
			if limit > 0 && i >= limit {
				return ErrMaxIterations
			}
			if name != "" {
				st.Set(name+".iteration", i)
			}
			if err := body.Run(ctx, st); err != nil {
				return err
			}
		}
	})
}
