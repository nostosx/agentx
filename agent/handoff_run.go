package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/skill"
)

// part describes how a part of a run that may be handed off runs.
type part struct {
	// cont is set for a part that continues a handed-off run: its
	// EventRunStart is not forwarded.
	cont bool
	// prior counts the messages the run added before the part.
	prior int
	// maxTurns and limits are the run's overrides, which later parts keep.
	maxTurns int
	limits   *loop.Limits
}

// run starts a run of a, or of the agent in.Agent names, and continues
// it on the agents it is handed off to.
func (a *LLMAgent) run(ctx context.Context, in *Input) iter.Seq2[*loop.Event, error] {
	ctx = withDeps(ctx, in.Deps)
	var cur Agent = a
	if in.Agent != "" && in.Agent != a.Name() && len(a.cfg.Handoffs) > 0 {
		if t := a.lookup(in.Agent); t != nil {
			cur = t
		}
	}
	t, ok := cur.(*LLMAgent)
	if !ok {
		c := *in
		c.Agent, c.Deps = "", nil
		return cur.Run(ctx, &c)
	}
	req := &loop.Request{
		RunID:      in.RunID,
		SessionID:  in.SessionID,
		History:    in.History,
		Input:      in.Message,
		NewSession: in.NewSession,
		MaxTurns:   in.MaxTurns,
		Limits:     in.Limits,
		Values:     in.Values,
	}
	first := withDeps(ctx, t.cfg.Deps)
	if in.Skill != nil {
		first = skill.WithCall(first, *in.Skill)
	}
	if len(a.cfg.Handoffs) == 0 {
		return t.rt.Run(first, req)
	}
	ch := a.newChain(nil)
	first = context.WithValue(first, chainKey{}, ch)
	ctx = context.WithValue(ctx, chainKey{}, ch)
	return a.drive(ctx, ch, t, t.rt.Run(first, req), part{maxTurns: in.MaxTurns, limits: in.Limits})
}

// newChain returns the chain of a run started on a with values.
func (a *LLMAgent) newChain(values map[string]string) *chain {
	limit := a.cfg.MaxHandoffs
	if limit <= 0 {
		limit = DefaultMaxHandoffs
	}
	return &chain{root: a, limit: limit, count: atoi(values, handoffCountKey)}
}

// resumeOn resumes the run of st on owner, an agent of a's handoff graph,
// and continues it on the agents it is handed off to.
func (a *LLMAgent) resumeOn(ctx context.Context, owner *LLMAgent, st *loop.State, approvals map[string]loop.Approval) iter.Seq2[*loop.Event, error] {
	ch := a.newChain(st.Values)
	ctx = context.WithValue(ctx, chainKey{}, ch)
	events := owner.rt.ResumeState(withDeps(ctx, owner.cfg.Deps), st, approvals)
	return a.drive(ctx, ch, owner, events, part{prior: atoi(st.Values, handoffPriorKey), maxTurns: st.MaxTurns, limits: st.Limits})
}

// drive forwards the events of a part of a run on cur and, if it hands
// the run off, continues the run on the agent it hands off to. The
// EventRunEnd of a part that hands off is replaced by an EventHandoff.
func (a *LLMAgent) drive(ctx context.Context, ch *chain, cur Agent, events iter.Seq2[*loop.Event, error], p part) iter.Seq2[*loop.Event, error] {
	return func(yield func(*loop.Event, error) bool) {
		// carry is the usage and turns of earlier parts that an agent
		// other than an LLMAgent did not count.
		var carry model.Usage
		var carryHistory []*model.Message
		carryTurns := 0
		for {
			llm, _ := cur.(*LLMAgent)
			var (
				done      *loop.Result
				next      *handoffCall
				sessionID string
			)
			for ev, err := range events {
				if err != nil {
					yield(nil, err)
					return
				}
				if ev.Depth == 0 {
					switch ev.Type {
					case loop.EventRunStart:
						if p.cont {
							continue
						}
					case loop.EventRunEnd:
						if hc := llm.requestedHandoff(ev.Result); hc != nil && ev.Result != nil {
							done, next, sessionID = ev.Result, hc, ev.SessionID
							continue
						}
						if ev.Result != nil {
							finishResult(ev.Result, p.prior, carry, carryTurns, carryHistory, llm == nil)
						}
					}
				}
				if !yield(ev, nil) {
					return
				}
			}
			if next == nil {
				return
			}
			target := ch.target(next.h)
			if target == nil {
				yield(nil, fmt.Errorf("agent %s: handoff target %s is not available", cur.Name(), next.h.Agent.Name()))
				return
			}
			finishResult(done, p.prior, carry, carryTurns, carryHistory, llm == nil)
			ch.count++
			ho := &loop.Handoff{From: cur.Name(), To: target.Name(), Tool: next.call.Name, CallID: next.call.ID}
			if args := strings.TrimSpace(string(next.call.Arguments)); args != "" && args != "{}" && args != "null" {
				ho.Input = next.call.Arguments
			}
			if !yield(&loop.Event{Type: loop.EventHandoff, RunID: done.RunID, SessionID: sessionID, Agent: cur.Name(), Turn: done.Turns, Handoff: ho}, nil) {
				return
			}
			values := maps.Clone(done.Values)
			if values == nil {
				values = map[string]string{}
			}
			values[handoffFromKey] = cur.Name()
			values[handoffToolKey] = next.call.Name
			values[handoffCountKey] = strconv.Itoa(ch.count)
			values[handoffPriorKey] = strconv.Itoa(len(done.NewMessages))
			values[skipInputKey] = "true"
			note := fmt.Sprintf(handoffNote, cur.Name(), target.Name(), "a call of "+next.call.Name)
			if ho.Input != nil {
				note += "\n\nHandoff input: " + string(ho.Input)
			}
			input := model.UserText(model.WrapReminder(note))
			p = part{cont: true, prior: len(done.NewMessages), maxTurns: p.maxTurns, limits: p.limits}
			cur = target
			if t, ok := target.(*LLMAgent); ok {
				events = t.rt.Run(withDeps(ctx, t.cfg.Deps), &loop.Request{
					RunID:     done.RunID,
					SessionID: sessionID,
					History:   done.Messages,
					Input:     input,
					MaxTurns:  p.maxTurns,
					Limits:    p.limits,
					Values:    values,
					Usage:     done.Usage,
					Turns:     done.Turns,
				})
				continue
			}
			// Another kind of agent cannot apply the input filter as
			// middleware, nor count the run's usage; filter its history and
			// add the usage to its result instead.
			history := done.Messages
			if next.h.InputFilter != nil {
				history = next.h.InputFilter(append([]*model.Message(nil), history...))
			}
			carry, carryTurns, carryHistory = done.Usage, done.Turns, done.Messages
			events = target.Run(ctx, &Input{Message: input, History: history, SessionID: sessionID, RunID: done.RunID, Values: values})
		}
	}
}

// finishResult completes the result of a run's last part: NewMessages
// covers the whole run, Values omits the handoff bookkeeping and, for an
// agent other than an LLMAgent, the earlier usage is added and Messages
// holds the conversation before its history was filtered.
func finishResult(res *loop.Result, prior int, carry model.Usage, turns int, history []*model.Message, foreign bool) {
	if foreign && history != nil {
		res.Messages = slices.Concat(history, res.NewMessages)
	}
	if prior > 0 {
		n := min(prior+len(res.NewMessages), len(res.Messages))
		res.NewMessages = res.Messages[len(res.Messages)-n:]
	}
	for k := range res.Values {
		if strings.HasPrefix(k, handoffPrefix) || k == skipInputKey {
			delete(res.Values, k)
		}
	}
	if foreign {
		res.Usage.Add(carry)
		res.Turns += turns
	}
}

// resume resumes the checkpointed run runID on the agent of a's handoff
// graph that was running it.
func (a *LLMAgent) resume(ctx context.Context, runID string, approvals map[string]loop.Approval) iter.Seq2[*loop.Event, error] {
	return func(yield func(*loop.Event, error) bool) {
		var (
			st       *loop.State
			owner    *LLMAgent
			missing  bool
			firstErr error
		)
		a.walk(func(x Agent) bool {
			t, ok := x.(*LLMAgent)
			if !ok {
				return true
			}
			cp := t.rt.Config().Checkpointer
			if cp == nil {
				missing = true
				return true
			}
			s, err := cp.Load(ctx, runID)
			if err != nil {
				if firstErr == nil || !errors.Is(err, loop.ErrCheckpointNotFound) {
					firstErr = err
				}
				return true
			}
			st, owner = s, t
			return false
		})
		switch {
		case st != nil:
		case firstErr != nil && !errors.Is(firstErr, loop.ErrCheckpointNotFound):
			yield(nil, firstErr)
			return
		case missing:
			// An agent without a checkpointer may have run it; resume it
			// from its state instead.
			yield(nil, loop.ErrNoCheckpointer)
			return
		default:
			yield(nil, firstErr)
			return
		}
		if t, ok := a.lookup(st.Agent).(*LLMAgent); ok {
			owner = t
		}
		for ev, err := range a.resumeOn(ctx, owner, st, approvals) {
			if !yield(ev, err) {
				return
			}
		}
	}
}

// resumeState resumes the run of st on the agent of a's handoff graph
// that was running it.
func (a *LLMAgent) resumeState(ctx context.Context, st *loop.State, approvals map[string]loop.Approval) iter.Seq2[*loop.Event, error] {
	owner := a
	if st != nil {
		if t, ok := a.lookup(st.Agent).(*LLMAgent); ok {
			owner = t
		}
	}
	if st == nil {
		return a.rt.ResumeState(ctx, st, approvals)
	}
	return a.resumeOn(ctx, owner, st, approvals)
}

// discard deletes the checkpoint of runID in the checkpointers of a's
// handoff graph.
func (a *LLMAgent) discard(ctx context.Context, runID string) error {
	var errs []error
	a.walk(func(x Agent) bool {
		if t, ok := x.(*LLMAgent); ok {
			errs = append(errs, t.rt.Discard(ctx, runID))
		}
		return true
	})
	return errors.Join(errs...)
}

// checkpointed reports whether a and the LLMAgents it hands off to all
// checkpoint their runs.
func (a *LLMAgent) checkpointed() bool {
	all := true
	a.walk(func(x Agent) bool {
		if t, ok := x.(*LLMAgent); ok && t.rt.Config().Checkpointer == nil {
			all = false
		}
		return all
	})
	return all
}
