package hook

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/nostosx/agentx/loop"
)

// DefaultStopBlockCap is the default number of consecutive times Stop or
// SubagentStop hooks may keep the agent working, like Claude Code's
// 8-consecutive-continuation cap.
const DefaultStopBlockCap = 8

// maxModelHooks bounds how many Prompt and Agent hooks of one event run at
// once, as in Claude Code; the others start as running ones finish.
const maxModelHooks = 12

// Manager dispatches events to registered hooks.
//
// A nil *Manager is valid and has no hooks. The zero value is an empty
// manager with the default options.
type Manager struct {
	mu          sync.RWMutex
	entries     map[EventType][]entry
	failClosed  bool
	stopCap     *int
	parallelism int
	endTimeout  time.Duration
	onAsync     func(AsyncResult)
	modeOf      func(*loop.Run) string

	// queue holds, by session ID, the context hooks produced outside of
	// the conversation's flow: results of Async hooks and AddContext.
	queueMu sync.Mutex
	queue   map[string][]string
	// bgCancel holds, by ID, the cancel functions of the running Async
	// hooks; bgIdle is closed when the last one finishes.
	bgMu     sync.Mutex
	bgCancel map[int]context.CancelFunc
	bgIdle   chan struct{}
	bgNext   int
}

type entry struct {
	matcher *matcher
	hook    Hook
}

// Option configures a Manager.
type Option func(*Manager)

// FailClosed makes hook errors deny the action instead of being ignored.
// Use it when hooks enforce security policy.
func FailClosed() Option { return func(m *Manager) { m.failClosed = true } }

// StopBlockCap sets how many consecutive times Stop or SubagentStop hooks
// may keep the agent working before the stop is let through anyway, like
// Claude Code's CLAUDE_CODE_STOP_HOOK_BLOCK_CAP. Zero disables the cap; a
// negative n restores the default, DefaultStopBlockCap.
func StopBlockCap(n int) Option {
	return func(m *Manager) {
		if n < 0 {
			m.stopCap = nil
			return
		}
		m.stopCap = &n
	}
}

// Parallelism bounds how many hooks of one event run at once. Zero, the
// default, runs all matching hooks at once, as Claude Code does, except
// that at most 12 Prompt and Agent hooks run at a time. 1 runs them one at
// a time in registration order.
func Parallelism(n int) Option { return func(m *Manager) { m.parallelism = max(n, 0) } }

// SessionEndTimeout sets the time budget of the SessionEnd hooks of one
// dispatch, which is also the timeout of each Command, HTTP and MCPTool
// hook without its own, like Claude Code's
// CLAUDE_CODE_SESSIONEND_HOOKS_TIMEOUT_MS. By default the budget is 1.5
// seconds, raised to the longest timeout set on a SessionEnd hook, up to a
// minute, and hooks without their own timeout get 1.5 seconds.
func SessionEndTimeout(d time.Duration) Option { return func(m *Manager) { m.endTimeout = max(d, 0) } }

// OnAsyncResult registers a function called when an Async hook finishes,
// after its context was queued for the session. Use it to wake an idle
// session when AsyncResult.Rewake is set, for example by starting a run;
// the queued context is added to that run's input. f runs on the hook's
// goroutine.
func OnAsyncResult(f func(AsyncResult)) Option { return func(m *Manager) { m.onAsync = f } }

// NewManager returns an empty manager.
func NewManager(opts ...Option) *Manager {
	m := &Manager{entries: map[EventType][]entry{}}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// On registers a hook for an event.
//
// matcher follows Claude Code's rules. An empty matcher or "*" matches
// everything. A matcher made only of letters, digits, '_', '-', spaces,
// ',' and '|' lists exact names separated by '|' or ','; for example
// "Edit|Write" or "Edit, Write". Any other matcher is a regular expression
// (Go RE2 syntax) that may match anywhere in the value, so "^Edit$"
// matches only Edit while "Edit.*" also matches NotebookEdit.
//
// For tool events the value is the tool name. Claude Code's built-in tool
// names also select the agentx built-ins with the same role: Bash (bash),
// Read (read_file), Write (write_file), Edit and MultiEdit (edit_file),
// Glob (glob), Grep (grep), WebFetch (web_fetch) and TodoWrite
// (todo_write); Edit and Write also select apply_patch. For Notification the value is Event.Reason or Claude
// Code's notification type, such as permission_prompt for "approval"; for
// other events it is Event.Reason.
func (m *Manager) On(event EventType, matcher string, h Hook) error {
	re, err := compileMatcher(matcher)
	if err != nil {
		return fmt.Errorf("hook matcher %q: %w", matcher, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries = map[EventType][]entry{}
	}
	m.entries[event] = append(m.entries[event], entry{matcher: re, hook: h})
	return nil
}

// OnFunc registers a function hook.
func (m *Manager) OnFunc(event EventType, matcher string, f func(context.Context, *Event) (*Result, error)) error {
	return m.On(event, matcher, Func(f))
}

// Has reports whether any hook is registered for event.
func (m *Manager) Has(event EventType) bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.entries[event]) > 0
}

// MaxStopBlocks returns how many consecutive times Stop or SubagentStop
// hooks may keep the agent working; zero means no limit. See StopBlockCap.
func (m *Manager) MaxStopBlocks() int {
	if m == nil {
		return DefaultStopBlockCap
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.stopCap == nil {
		return DefaultStopBlockCap
	}
	return *m.stopCap
}

// Merge registers all hooks from other into m. FailClosed carries over,
// and so do the other options of other that m does not set.
func (m *Manager) Merge(other *Manager) {
	if other == nil {
		return
	}
	other.mu.RLock()
	defer other.mu.RUnlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries = map[EventType][]entry{}
	}
	for ev, es := range other.entries {
		m.entries[ev] = append(m.entries[ev], es...)
	}
	m.failClosed = m.failClosed || other.failClosed
	if m.stopCap == nil && other.stopCap != nil {
		m.stopCap = new(*other.stopCap)
	}
	if m.parallelism == 0 {
		m.parallelism = other.parallelism
	}
	if m.endTimeout == 0 {
		m.endTimeout = other.endTimeout
	}
	if m.onAsync == nil {
		m.onAsync = other.onAsync
	}
	if m.modeOf == nil {
		m.modeOf = other.modeOf
	}
}

// SetPermissionMode sets how the manager learns a run's permission mode,
// which events report as Event.PermissionMode and command hooks receive
// as permission_mode. agent.New sets it from a middleware with a
// PermissionMode(*loop.Run) string method, such as permission.Policy.
// Without it the mode is "default".
func (m *Manager) SetPermissionMode(f func(*loop.Run) string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.modeOf = f
}

// event returns an event of a run.
func (m *Manager) event(run *loop.Run, t EventType) *Event {
	ev := &Event{Type: t, SessionID: run.SessionID(), RunID: run.ID(), Agent: run.Agent(), Turn: run.Turn()}
	m.mu.RLock()
	modeOf := m.modeOf
	m.mu.RUnlock()
	if modeOf != nil {
		ev.PermissionMode = modeOf(run)
	}
	return ev
}

// Dispatch runs the matching hooks and merges their results. It always
// returns a non-nil result.
//
// As in Claude Code, the matching hooks run in parallel (see Parallelism),
// each with its own copy of ev, and identical Command, HTTP, MCPTool,
// Prompt and Agent hooks registered more than once run once. The results
// are merged in registration order, whatever order the hooks finish in:
// the most restrictive decision wins (deny > defer > ask > allow), with
// the Reason of the first hook that returned it; AdditionalContext and
// SystemMessage accumulate; the last UpdatedInput and UpdatedOutput in
// registration order win. For PreToolUse the merged UpdatedInput is then
// applied to ev.ToolCall. PreModel hooks, which may modify ev.Request, run
// one at a time in registration order on ev itself, and so does a lone
// hook.
//
// Async hooks start in the background and do not contribute to the
// result. SessionEnd hooks share a time budget; see SessionEndTimeout.
// Hook errors and panics are returned joined; with FailClosed they also
// produce Deny. If ctx is done, hooks that have not started are skipped
// and its error is returned.
//
// Inside a run traced by middleware.Tracing, hooks that run get a
// "hook {event}" span.
func (m *Manager) Dispatch(ctx context.Context, ev *Event) (res *Result, err error) {
	merged := &Result{}
	if m == nil {
		return merged, nil
	}
	m.mu.RLock()
	entries := slices.Clone(m.entries[ev.Type])
	failClosed, limit, endTimeout := m.failClosed, m.parallelism, m.endTimeout
	m.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return merged, err
	}

	values := matchValues(ev)
	var hooks []Hook
	seen := map[string]bool{}
	for _, e := range entries {
		if !e.matcher.match(values...) {
			continue
		}
		if k := hookKey(e.hook); k != "" {
			if seen[k] {
				continue
			}
			seen[k] = true
		}
		if a, ok := e.hook.(*Async); ok {
			m.startAsync(ctx, ev, a)
			continue
		}
		hooks = append(hooks, e.hook)
	}
	if len(hooks) == 0 {
		return merged, nil
	}
	ctx, span := startSpan(ctx, ev, len(hooks))
	defer func() { endSpan(span, res, err) }()
	if ev.Type == SessionEnd {
		var cancel context.CancelFunc
		ctx, cancel = sessionEndContext(ctx, endTimeout, hooks)
		defer cancel()
	}

	results := make([]*Result, len(hooks))
	errs := make([]error, len(hooks))
	if ev.Type == PreModel || len(hooks) == 1 {
		for i, h := range hooks {
			if ctx.Err() != nil {
				errs[i] = ctx.Err()
				continue
			}
			results[i], errs[i] = handle(ctx, h, ev)
		}
	} else {
		m.parallel(ctx, ev, hooks, limit, results, errs)
	}

	var all []error
	for i := range hooks {
		if err := errs[i]; err != nil {
			if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
				// Skipped or cut short by ctx; reported once below.
				continue
			}
			all = append(all, fmt.Errorf("%s hook: %w", ev.Type, err))
			if failClosed {
				merged.merge(&Result{Decision: Deny, Reason: "hook failed: " + err.Error()})
			}
			continue
		}
		merged.merge(results[i])
	}
	if err := ctx.Err(); err != nil {
		all = append(all, err)
		if failClosed {
			merged.merge(&Result{Decision: Deny, Reason: "hook failed: " + err.Error()})
		}
	}
	if merged.UpdatedInput != nil && ev.ToolCall != nil {
		ev.ToolCall.Arguments = merged.UpdatedInput
	}
	return merged, errors.Join(all...)
}

// parallel runs hooks concurrently, each on its own copy of ev, at most
// limit at once if limit is positive and at most maxModelHooks Prompt and
// Agent hooks at once.
func (m *Manager) parallel(ctx context.Context, ev *Event, hooks []Hook, limit int, results []*Result, errs []error) {
	var all, model chan struct{}
	if limit > 0 {
		all = make(chan struct{}, limit)
	}
	model = make(chan struct{}, maxModelHooks)
	acquire := func(sem chan struct{}) bool {
		if ctx.Err() != nil {
			return false
		}
		if sem == nil {
			return true
		}
		select {
		case sem <- struct{}{}:
			// Cancellation and an available slot may both be ready. Do not
			// let select's random choice start another waiting hook.
			if ctx.Err() != nil {
				<-sem
				return false
			}
			return true
		case <-ctx.Done():
			return false
		}
	}
	release := func(sem chan struct{}) {
		if sem != nil {
			<-sem
		}
	}
	var wg sync.WaitGroup
	for i, h := range hooks {
		own := copyEvent(ev)
		var sem chan struct{}
		if usesModel(h) {
			sem = model
		}
		// Hooks start in registration order, so with a limit the earlier
		// ones are not starved by later ones.
		if !acquire(all) {
			errs[i] = ctx.Err()
			continue
		}
		wg.Go(func() {
			defer release(all)
			if !acquire(sem) {
				errs[i] = ctx.Err()
				return
			}
			defer release(sem)
			results[i], errs[i] = handle(ctx, h, own)
		})
	}
	wg.Wait()
}

// handle runs one hook and turns a panic into an error, so a failing hook
// cannot take down the other hooks' goroutines or the run.
func handle(ctx context.Context, h Hook, ev *Event) (res *Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			res, err = nil, fmt.Errorf("panic: %v", r)
		}
	}()
	return h.Handle(ctx, ev)
}

// copyEvent returns a copy of ev that a hook running concurrently with
// others can modify: the tool call, tool result, messages slice and data
// map are copied. Request and Response are shared and must not be
// modified.
func copyEvent(ev *Event) *Event {
	c := *ev
	if ev.ToolCall != nil {
		tc := *ev.ToolCall
		tc.Arguments = slices.Clone(tc.Arguments)
		c.ToolCall = &tc
	}
	if ev.ToolResult != nil {
		tr := *ev.ToolResult
		c.ToolResult = &tr
	}
	c.Messages = slices.Clone(ev.Messages)
	c.Data = maps.Clone(ev.Data)
	return &c
}

// keyer is implemented by hooks that are identical to any other hook with
// the same key, so a hook registered twice runs once.
type keyer interface{ hookKey() string }

func hookKey(h Hook) string {
	if k, ok := h.(keyer); ok {
		return k.hookKey()
	}
	return ""
}

// timeouter is implemented by hooks with a configured timeout.
type timeouter interface{ hookTimeout() time.Duration }

// modelHook is implemented by hooks that may call a model, which Claude
// Code runs at most 12 at a time.
type modelHook interface{ usesModel() bool }

func usesModel(h Hook) bool {
	m, ok := h.(modelHook)
	return ok && m.usesModel()
}

// SessionEnd timeouts, as in Claude Code.
const (
	sessionEndBudget    = 1500 * time.Millisecond
	maxSessionEndBudget = time.Minute
)

type sessionEndKey struct{}

// sessionEndContext returns the context of a SessionEnd dispatch: its
// budget is d if set, else 1.5 seconds raised to the longest timeout set
// on hooks, up to a minute. The default timeout of each hook is d, or 1.5
// seconds.
func sessionEndContext(ctx context.Context, d time.Duration, hooks []Hook) (context.Context, context.CancelFunc) {
	budget, each := d, d
	if d == 0 {
		budget, each = sessionEndBudget, sessionEndBudget
		for _, h := range hooks {
			if t, ok := h.(timeouter); ok {
				budget = max(budget, min(t.hookTimeout(), maxSessionEndBudget))
			}
		}
	}
	return context.WithTimeout(context.WithValue(ctx, sessionEndKey{}, each), budget)
}

// defaultTimeout returns the timeout of a Command, HTTP or MCPTool hook
// without its own: 1.5 seconds or the configured budget for SessionEnd, 30
// seconds for UserPromptSubmit, and 10 minutes otherwise.
func defaultTimeout(ctx context.Context, event EventType) time.Duration {
	switch event {
	case SessionEnd:
		if d, ok := ctx.Value(sessionEndKey{}).(time.Duration); ok && d > 0 {
			return d
		}
		return sessionEndBudget
	case UserPromptSubmit:
		return promptCommandTimeout
	}
	return defaultCommandTimeout
}
