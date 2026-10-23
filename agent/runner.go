package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/session"
)

// PendingRunKey is the session state key holding the ID of an interrupted
// run.
const PendingRunKey = "agentx.pending_run"

// FailedRunKey is the session state key holding the ID of the session's last
// run if it failed, for example on a provider outage. Unlike an interrupted
// run, it does not block new runs: Runner.Resume continues it, and the next
// Run gives it up.
const FailedRunKey = "agentx.failed_run"

// SessionValuesKey is the session state key holding the session-scoped
// run values, those whose keys start with loop.SessionValuePrefix. The
// Runner saves them when a run ends and passes them to the session's next
// runs, so middleware can keep state for a session, such as remembered
// permission grants.
const SessionValuesKey = "agentx.session_values"

// discardedResult answers the tool calls of a discarded run.
const discardedResult = "The run was discarded, so this tool call did not run."

// Runner runs an agent against persistent sessions. Runs on the same
// session are serialized.
//
// If the agent may not checkpoint its runs, the Runner keeps a copy of the
// state of each session's interrupted run in memory, so Resume can continue
// it. A copy is kept until the session's next run ends or the run is
// discarded; sessions that are abandoned while interrupted keep theirs for
// the Runner's lifetime.
//
// If the agent has hooks (see HookProvider), the Runner fires SessionStart
// once per session: with source "startup" for a new session, "resume" the
// first time it runs or resumes an existing one, and "clear" or "fork" for
// sessions made by Clear and Fork. EndSession fires SessionEnd, after
// which the session starts again. The Runner remembers the sessions it
// started until they end.
type Runner struct {
	agent Agent
	store session.Store

	mu    sync.Mutex
	locks map[string]*sessionLock
	// states holds, by session ID, a copy of the state of interrupted runs,
	// so agents without a checkpointer can resume them with ResumeState. It
	// is kept in memory only.
	states map[string]*loop.State
	// sessions tracks the session lifecycle hooks; see session_hooks.go.
	sessions sessionTracker
}

// sessionLock serializes the runs of one session. refs counts the holders
// and waiters, so the entry is removed once nobody uses it.
type sessionLock struct {
	mu   sync.Mutex
	refs int
}

// NewRunner returns a runner. A nil store keeps sessions in memory.
func NewRunner(a Agent, store session.Store) *Runner {
	if store == nil {
		store = session.NewMemoryStore()
	}
	return &Runner{agent: a, store: store, locks: map[string]*sessionLock{}, states: map[string]*loop.State{}}
}

// Store returns the session store.
func (r *Runner) Store() session.Store { return r.store }

func (r *Runner) lock(id string) func() {
	r.mu.Lock()
	l, ok := r.locks[id]
	if !ok {
		l = &sessionLock{}
		r.locks[id] = l
	}
	l.refs++
	r.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		r.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(r.locks, id)
		}
		r.mu.Unlock()
	}
}

// Run sends a message in a session and returns the run's events. An empty
// sessionID starts a new session; its ID is reported in the events. Options
// such as RunLimits apply to this run only. The message goes to the agent
// the session was last handed off to, if any; see ActiveAgentKey. The
// run's tools and instruction functions reach the session's state with
// SessionState.
//
// The session is saved when the run ends, including when it is
// interrupted. If the caller stops iterating early, the run is cancelled
// and the session is saved once it has ended.
func (r *Runner) Run(ctx context.Context, sessionID string, msg *model.Message, opts ...RunOption) iter.Seq2[*loop.Event, error] {
	return func(yield func(*loop.Event, error) bool) {
		id := sessionID
		if id == "" {
			id = session.NewID()
		}
		unlock := r.lock(id)
		defer unlock()
		sess, created, err := session.GetOrCreate(ctx, r.store, id)
		if err != nil {
			yield(nil, err)
			return
		}
		if id, ok := sess.State().Get(PendingRunKey); ok && id != "" {
			yield(nil, fmt.Errorf("session %s has an interrupted run %v; resume it with Runner.Resume or discard it with Runner.Discard", sess.ID(), id))
			return
		}
		r.startSession(ctx, sess.ID(), created, msg.Text())
		runCtx, cancel := context.WithCancel(ContextWithSessionState(ctx, sess.State()))
		defer cancel()
		in := &Input{
			Message:    msg,
			History:    sess.Messages(),
			SessionID:  sess.ID(),
			NewSession: created,
			Values:     sessionValues(sess),
			Agent:      runIDOf(sess, ActiveAgentKey),
		}
		for _, opt := range opts {
			opt(in)
		}
		cp, err := r.beginCheckpoint(ctx, sess.ID(), in)
		if err != nil {
			yield(nil, err)
			return
		}
		events := r.agent.Run(runCtx, in)
		r.forward(ctx, cancel, sess, events, yield, cp)
	}
}

// Resume continues the interrupted run of a session or, if it has none, its
// failed run; see FailedRunKey. Failed runs are resumed from the agent's
// checkpoint. Interrupted runs are too, or, if the agent has no
// checkpointer, from the state this Runner kept in memory when the run was
// interrupted.
//
// Of the options, only RunDeps applies: dependencies are not saved with
// the run, so pass them again. The run keeps its other settings.
func (r *Runner) Resume(ctx context.Context, sessionID string, approvals map[string]loop.Approval, opts ...RunOption) iter.Seq2[*loop.Event, error] {
	return func(yield func(*loop.Event, error) bool) {
		res, canResume := r.agent.(Resumer)
		sr, canResumeState := r.agent.(StateResumer)
		if !canResume && !canResumeState {
			yield(nil, fmt.Errorf("agent %s cannot resume runs", r.agent.Name()))
			return
		}
		unlock := r.lock(sessionID)
		defer unlock()
		sess, err := r.store.Get(ctx, sessionID)
		if err != nil {
			yield(nil, err)
			return
		}
		runID, kind, err := resumableRun(sess)
		if err != nil {
			yield(nil, err)
			return
		}
		st := r.state(sessionID, runID)
		r.startSession(ctx, sessionID, false, "")
		var in Input
		for _, opt := range opts {
			opt(&in)
		}
		runCtx, cancel := context.WithCancel(withDeps(ContextWithSessionState(ctx, sess.State()), in.Deps))
		defer cancel()
		events := func(yield func(*loop.Event, error) bool) {
			fromState := func() {
				for ev, err := range sr.ResumeState(runCtx, st, approvals) {
					if !yield(ev, err) {
						return
					}
				}
			}
			if !canResume {
				if st == nil {
					yield(nil, fmt.Errorf("session %s: %s run %s cannot be resumed: its state was not kept; discard it with Runner.Discard", sessionID, kind, runID))
					return
				}
				fromState()
				return
			}
			for ev, err := range res.Resume(runCtx, runID, approvals) {
				if errors.Is(err, loop.ErrNoCheckpointer) && st != nil && canResumeState {
					// The agent has no checkpointer; resume from the state
					// kept at the interrupt.
					fromState()
					return
				}
				if errors.Is(err, loop.ErrCheckpointNotFound) || errors.Is(err, loop.ErrNoCheckpointer) {
					err = fmt.Errorf("session %s: %s run %s cannot be resumed: %w; discard it with Runner.Discard", sessionID, kind, runID, err)
				}
				if !yield(ev, err) {
					return
				}
			}
		}
		r.forward(ctx, cancel, sess, events, yield, nil)
	}
}

// checkpointed reports whether the agent is known to checkpoint its runs,
// so the Runner need not keep their states.
func (r *Runner) checkpointed() bool {
	a, ok := r.agent.(*LLMAgent)
	return ok && a.checkpointed()
}

// state returns the kept state of a session's interrupted run runID.
func (r *Runner) state(sessionID, runID string) *loop.State {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.states[sessionID]; st != nil && st.RunID == runID {
		return st
	}
	return nil
}

// keep records a copy of the state of a session's run if it was
// interrupted and the agent may not checkpoint it, and forgets it
// otherwise. The copy is not shared with the consumer, who may modify the
// result.
func (r *Runner) keep(sessionID string, res *loop.Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if res != nil && res.Status == loop.StatusInterrupted && res.Interrupt != nil && res.Interrupt.State != nil && !r.checkpointed() {
		r.states[sessionID] = res.Interrupt.State.Clone()
		return
	}
	delete(r.states, sessionID)
}

// Discard gives up the interrupted run of a session, for example when its
// checkpoint was lost, or else its failed run. Its pending tool calls are
// answered as not run, so the session can continue with Run, and the agent's
// checkpoint of the run is deleted if the agent implements Discarder.
func (r *Runner) Discard(ctx context.Context, sessionID string) error {
	unlock := r.lock(sessionID)
	defer unlock()
	sess, err := r.store.Get(ctx, sessionID)
	if err != nil {
		return err
	}
	runID, _, err := resumableRun(sess)
	if err != nil {
		return err
	}
	msgs := sess.Messages()
	if msg := loop.CloseToolCalls(msgs, discardedResult); msg != nil {
		sess.Replace(append(msgs, msg))
	}
	sess.State().Delete(PendingRunKey)
	sess.State().Delete(FailedRunKey)
	r.keep(sessionID, nil)
	if d, ok := r.agent.(Discarder); ok {
		if err := d.Discard(ctx, runID); err != nil {
			return fmt.Errorf("discard run %s: %w", runID, err)
		}
	}
	return r.store.Save(ctx, sess)
}

// resumableRun returns the ID of the session's interrupted run or, if it
// has none, of its failed run, and which of the two it is.
func resumableRun(sess session.Session) (runID, kind string, err error) {
	if runID = runIDOf(sess, PendingRunKey); runID != "" {
		return runID, "interrupted", nil
	}
	if runID = runIDOf(sess, FailedRunKey); runID != "" {
		return runID, "failed", nil
	}
	return "", "", fmt.Errorf("session %s has no interrupted run or failed run to resume", sess.ID())
}

func runIDOf(sess session.Session, key string) string {
	runID, _, err := sess.State().Value[string](key)
	if err != nil {
		return ""
	}
	return runID
}

// forward passes events to yield and saves the session when the run ends,
// with the run's checkpoint cp, which is nil for a resumed run. If yield
// stops early, the run is cancelled and its remaining events are drained,
// so the session is still saved with the conversation so far.
func (r *Runner) forward(ctx context.Context, cancel context.CancelFunc, sess session.Session, events iter.Seq2[*loop.Event, error], yield func(*loop.Event, error) bool, cp *checkpointRecord) {
	saved, forwarding := false, true
	for ev, err := range events {
		if err != nil {
			// An error ends the run; a failed run has already been saved
			// from its final event.
			if forwarding {
				yield(nil, err)
			}
			return
		}
		if ev.Type == loop.EventRunEnd && ev.Depth == 0 && ev.Result != nil && !saved {
			if err := r.save(ctx, sess, ev.Result, cp); err != nil {
				if forwarding {
					yield(nil, err)
				}
				return
			}
			saved = true
		}
		if forwarding && !yield(ev, nil) {
			if saved {
				return
			}
			forwarding = false
			cancel()
		}
	}
	if !saved && forwarding {
		yield(nil, errors.New("agent: run ended without a result"))
	}
}

func (r *Runner) save(ctx context.Context, sess session.Session, res *loop.Result, cp *checkpointRecord) error {
	sess.Replace(res.Messages)
	addCheckpoint(sess, cp, res)
	state := sess.State()
	if failed := runIDOf(sess, FailedRunKey); failed != "" && failed != res.RunID {
		// A new run gives up the failed one. Deleting its checkpoint is best
		// effort: the run is no longer referenced either way.
		if d, ok := r.agent.(Discarder); ok {
			_ = d.Discard(context.WithoutCancel(ctx), failed)
		}
	}
	state.Delete(PendingRunKey)
	state.Delete(FailedRunKey)
	saveSessionValues(sess, res.Values)
	if a, ok := r.agent.(*LLMAgent); ok && len(a.cfg.Handoffs) > 0 && res.Agent != "" {
		// The session's next message goes to the agent that ran last.
		if res.Agent == a.Name() {
			state.Delete(ActiveAgentKey)
		} else {
			state.Set(ActiveAgentKey, res.Agent)
		}
	}
	switch {
	case res.Status == loop.StatusInterrupted:
		state.Set(PendingRunKey, res.RunID)
	case res.Status == loop.StatusFailed && len(res.NewMessages) > 0:
		// A run that failed before its input was added cannot be resumed.
		state.Set(FailedRunKey, res.RunID)
	}
	r.keep(sess.ID(), res)
	// Save even if the caller cancelled, so the conversation is kept.
	if err := r.store.Save(context.WithoutCancel(ctx), sess); err != nil {
		return fmt.Errorf("save session: %w", err)
	}
	return nil
}

// RunText sends text in a session and returns the result. Options apply as
// for Run.
func (r *Runner) RunText(ctx context.Context, sessionID, text string, opts ...RunOption) (*loop.Result, error) {
	return loop.Collect(r.Run(ctx, sessionID, model.UserText(text), opts...), nil)
}

// sessionValues returns the session-scoped run values saved with sess.
func sessionValues(sess session.Session) map[string]string {
	values, _, err := sess.State().Value[map[string]string](SessionValuesKey)
	if err != nil || len(values) == 0 {
		return nil
	}
	return values
}

// saveSessionValues keeps the session-scoped values of a run's final
// values with sess. A run without values, such as one that failed before
// it started, leaves the saved ones as they are.
func saveSessionValues(sess session.Session, values map[string]string) {
	if values == nil {
		return
	}
	kept := map[string]string{}
	for k, v := range values {
		if strings.HasPrefix(k, loop.SessionValuePrefix) && v != "" {
			kept[k] = v
		}
	}
	if len(kept) == 0 {
		sess.State().Delete(SessionValuesKey)
		return
	}
	sess.State().Set(SessionValuesKey, kept)
}
