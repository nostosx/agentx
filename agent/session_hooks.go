package agent

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/session"
)

// HookProvider is implemented by agents with a hook manager, such as
// LLMAgent. A Runner dispatches the SessionStart and SessionEnd events of
// its sessions to it. For the context of SessionStart hooks to reach the
// model, the agent's runs must use hook.Middleware with the same manager.
type HookProvider interface {
	Hooks() *hook.Manager
}

// sessionTracker records the sessions whose SessionStart hooks a Runner
// fired, so each session gets one per Runner until it ends.
type sessionTracker struct {
	mu      sync.Mutex
	started map[string]bool
	// sources holds the SessionStart source of sessions made by Clear and
	// Fork, for their first run.
	sources map[string]string
}

// begin marks a session as started and reports the source of its
// SessionStart event, if the Runner fires one: none for a session created
// by this run, whose hook middleware fires "startup", and otherwise the
// source Clear or Fork recorded, or "resume".
func (t *sessionTracker) begin(id string, created bool) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started[id] {
		return "", false
	}
	if t.started == nil {
		t.started = map[string]bool{}
	}
	t.started[id] = true
	source := t.sources[id]
	delete(t.sources, id)
	if created && source == "" {
		return "", false
	}
	return cmp.Or(source, "resume"), true
}

// expect records the SessionStart source of a session's first run.
func (t *sessionTracker) expect(id, source string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sources == nil {
		t.sources = map[string]string{}
	}
	t.sources[id] = source
}

// end forgets a session, so its next run starts it again.
func (t *sessionTracker) end(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.started, id)
	delete(t.sources, id)
}

// hooks returns the agent's hook manager, if it has one.
func (r *Runner) hooks() *hook.Manager {
	if p, ok := r.agent.(HookProvider); ok {
		return p.Hooks()
	}
	return nil
}

// logger returns the agent's logger, if it is known.
func (r *Runner) logger() *slog.Logger {
	if a, ok := r.agent.(*LLMAgent); ok && a.cfg.Logger != nil {
		return a.cfg.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// startSession fires the SessionStart hooks of a session the first time
// this Runner runs or resumes it, unless the session was just created:
// the hook middleware fires those with source "startup". Their context is
// queued with Manager.AddContext, so the hook middleware adds it to the
// run's input, or to the next tool result of a resumed run.
func (r *Runner) startSession(ctx context.Context, id string, created bool, prompt string) {
	m := r.hooks()
	if !m.Has(hook.SessionStart) {
		return
	}
	source, fire := r.sessions.begin(id, created)
	if !fire {
		return
	}
	ev := &hook.Event{Type: hook.SessionStart, SessionID: id, Agent: r.agent.Name(), Reason: source, Prompt: prompt}
	res, err := m.Dispatch(ctx, ev)
	log := r.logger()
	if err != nil {
		log.Warn("hook failed", "event", ev.Type, "session_id", id, "error", err)
	}
	if res.SystemMessage != "" {
		log.Warn("hook message", "event", ev.Type, "session_id", id, "message", res.SystemMessage)
	}
	m.AddContext(id, res.AdditionalContext)
}

// EndSession ends a session, like Claude Code does when you exit, run
// /clear or switch sessions: it runs the agent's SessionEnd hooks with
// reason, such as "clear", "logout", "prompt_input_exit" or "other" (the
// default), and Event.Messages holding the conversation. The hooks cannot
// prevent it and share a time budget; see hook.SessionEndTimeout. Context
// still queued for the session is dropped.
//
// The session stays in the store: a later Run or Resume continues it and
// fires SessionStart with source "resume". EndSession waits for a run of
// the session in progress. It returns the hooks' errors.
func (r *Runner) EndSession(ctx context.Context, sessionID, reason string) error {
	unlock := r.lock(sessionID)
	defer unlock()
	r.sessions.end(sessionID)
	m := r.hooks()
	m.DropContext(sessionID)
	if !m.Has(hook.SessionEnd) {
		return nil
	}
	ev := &hook.Event{Type: hook.SessionEnd, SessionID: sessionID, Agent: r.agent.Name(), Reason: cmp.Or(reason, "other")}
	if sess, err := r.store.Get(ctx, sessionID); err == nil {
		ev.Messages = sess.Messages()
	}
	_, err := m.Dispatch(ctx, ev)
	return err
}

// Clear ends a session with reason "clear" and starts a new, empty one in
// its place, like Claude Code's /clear. It returns the new session's ID;
// the old session stays in the store. The first run of the new session
// fires SessionStart with source "clear". SessionEnd hook errors are
// returned with the new ID.
func (r *Runner) Clear(ctx context.Context, sessionID string) (string, error) {
	endErr := r.EndSession(ctx, sessionID, "clear")
	id, err := r.newSession(ctx, nil, nil)
	if err != nil {
		return "", err
	}
	if r.hooks().Has(hook.SessionStart) {
		r.sessions.expect(id, "clear")
	}
	return id, endErr
}

// Fork copies the conversation and state of a session into a new session
// and returns its ID, like Claude Code's --fork-session. An interrupted or
// failed run is not copied: its pending tool calls are answered as not
// run in the copy. The first run of the new session fires SessionStart
// with source "fork".
//
// The checkpoints and the agent's file history are copied too, so either
// session can be rewound to the checkpoints before the fork, and later
// changes are recorded only for the session that made them. Both sessions
// still work on the same files: rewinding the code of one changes the
// files the other sees.
func (r *Runner) Fork(ctx context.Context, sessionID string) (string, error) {
	unlock := r.lock(sessionID)
	sess, err := r.store.Get(ctx, sessionID)
	if err != nil {
		unlock()
		return "", err
	}
	msgs := sess.Messages()
	state := sess.State().All()
	unlock()
	if msg := loop.CloseToolCalls(msgs, discardedResult); msg != nil {
		msgs = append(msgs, msg)
	}
	delete(state, PendingRunKey)
	delete(state, FailedRunKey)
	id, err := r.newSession(ctx, msgs, state)
	if err != nil {
		return "", err
	}
	if err := r.forkHistory(ctx, sessionID, id); err != nil {
		return "", err
	}
	if r.hooks().Has(hook.SessionStart) {
		r.sessions.expect(id, "fork")
	}
	return id, nil
}

// newSession saves a new session with msgs and state.
func (r *Runner) newSession(ctx context.Context, msgs []*model.Message, state map[string]any) (string, error) {
	id := session.NewID()
	unlock := r.lock(id)
	defer unlock()
	sess, _, err := session.GetOrCreate(ctx, r.store, id)
	if err != nil {
		return "", err
	}
	sess.Replace(msgs)
	for k, v := range state {
		sess.State().Set(k, v)
	}
	if err := r.store.Save(ctx, sess); err != nil {
		return "", fmt.Errorf("save session: %w", err)
	}
	return id, nil
}
