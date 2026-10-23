package hook

import (
	"context"
	"strings"
)

// Async runs a hook in the background, like Claude Code's "async": true.
// The action that fired the event does not wait for it, and its decision,
// Halt and updates have no effect. When it finishes, its
// AdditionalContext and SystemMessage are queued for its session and
// added to the conversation at the next tool result or the next run's
// input, whichever comes first; see Manager.AddContext.
//
// With Rewake, like Claude Code's "asyncRewake", a Deny (exit status 2 of
// a Command) also queues its Reason, the command's stderr or else its
// stdout, and sets AsyncResult.Rewake so an OnAsyncResult function can
// wake an idle session.
//
// The hook's own timeout applies; settings files give async commands no
// timeout, as Claude Code does not enforce one. Manager.Close cancels the
// hooks still running.
type Async struct {
	Hook   Hook
	Rewake bool
}

// Handle runs the hook synchronously, for an Async used outside a
// Manager. A Manager runs it in the background instead.
func (a *Async) Handle(ctx context.Context, ev *Event) (*Result, error) {
	if a.Rewake {
		ctx = context.WithValue(ctx, rewakeKey{}, true)
	}
	return a.Hook.Handle(ctx, ev)
}

func (a *Async) hookKey() string {
	if k := hookKey(a.Hook); k != "" {
		if a.Rewake {
			return "rewake\x00" + k
		}
		return "async\x00" + k
	}
	return ""
}

// AsyncResult is the outcome of an Async hook.
type AsyncResult struct {
	Event     EventType
	SessionID string
	RunID     string
	// Context is what was queued for the session: the hook's
	// AdditionalContext and SystemMessage, and for Rewake the Reason of a
	// Deny.
	Context string
	// Rewake is set when a Rewake hook denied (exited with status 2).
	Rewake bool
	// Err is the hook's error; context.Canceled if Manager.Close
	// cancelled it.
	Err error
}

type rewakeKey struct{}

func rewaking(ctx context.Context) bool {
	v, _ := ctx.Value(rewakeKey{}).(bool)
	return v
}

// startAsync runs an Async hook in the background on a copy of ev. It is
// not cancelled with ctx, which belongs to the action that fired it.
func (m *Manager) startAsync(ctx context.Context, ev *Event, a *Async) {
	own := copyEvent(ev)
	// The request may be modified once the event's dispatch returns.
	own.Request = nil
	bg, cancel := context.WithCancel(context.WithoutCancel(ctx))
	id := m.track(cancel)
	go func() {
		defer m.untrack(id)
		res, err := handle(bg, a, own)
		out := AsyncResult{Event: own.Type, SessionID: own.SessionID, RunID: own.RunID, Err: err}
		if res != nil {
			var parts []string
			if a.Rewake && res.Decision == Deny {
				out.Rewake = true
				if res.Reason != "" {
					parts = append(parts, res.Reason)
				}
			}
			for _, s := range []string{res.AdditionalContext, res.SystemMessage} {
				if s != "" {
					parts = append(parts, s)
				}
			}
			out.Context = strings.Join(parts, "\n")
		}
		m.AddContext(own.SessionID, out.Context)
		m.mu.RLock()
		notify := m.onAsync
		m.mu.RUnlock()
		if notify != nil {
			notify(out)
		}
	}()
}

// AddContext queues text for a session's conversation. The hook
// middleware adds it, as a system reminder, to the next tool result or
// the next run's input of the session, whichever comes first. Async hooks
// and agent.Runner, for the SessionStart hooks of a resumed session, use
// it. Empty text is ignored.
func (m *Manager) AddContext(sessionID, text string) {
	if m == nil || text == "" {
		return
	}
	m.queueMu.Lock()
	defer m.queueMu.Unlock()
	if m.queue == nil {
		m.queue = map[string][]string{}
	}
	m.queue[sessionID] = append(m.queue[sessionID], text)
}

// takeContext removes and returns the context queued for a session.
func (m *Manager) takeContext(sessionID string) string {
	if m == nil {
		return ""
	}
	m.queueMu.Lock()
	defer m.queueMu.Unlock()
	texts := m.queue[sessionID]
	delete(m.queue, sessionID)
	return strings.Join(texts, "\n")
}

// DropContext discards the context queued for a session, for example when
// it ends.
func (m *Manager) DropContext(sessionID string) {
	if m == nil {
		return
	}
	m.queueMu.Lock()
	defer m.queueMu.Unlock()
	delete(m.queue, sessionID)
}

// track records a running Async hook and returns its ID.
func (m *Manager) track(cancel context.CancelFunc) int {
	m.bgMu.Lock()
	defer m.bgMu.Unlock()
	if m.bgCancel == nil {
		m.bgCancel = map[int]context.CancelFunc{}
	}
	if len(m.bgCancel) == 0 {
		m.bgIdle = make(chan struct{})
	}
	m.bgNext++
	m.bgCancel[m.bgNext] = cancel
	return m.bgNext
}

// untrack forgets a finished Async hook.
func (m *Manager) untrack(id int) {
	m.bgMu.Lock()
	defer m.bgMu.Unlock()
	m.bgCancel[id]()
	delete(m.bgCancel, id)
	if len(m.bgCancel) == 0 {
		close(m.bgIdle)
	}
}

// Wait waits until the Async hooks running in the background finish, or
// ctx is done.
func (m *Manager) Wait(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.bgMu.Lock()
	if len(m.bgCancel) == 0 {
		m.bgMu.Unlock()
		return nil
	}
	idle := m.bgIdle
	m.bgMu.Unlock()
	select {
	case <-idle:
		// Hooks started meanwhile are waited for too.
		return m.Wait(ctx)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close cancels the Async hooks running in the background and waits for
// them to finish, like Claude Code at the end of a non-interactive
// session. The manager remains usable.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.bgMu.Lock()
	for _, cancel := range m.bgCancel {
		cancel()
	}
	m.bgMu.Unlock()
	_ = m.Wait(context.Background())
}
