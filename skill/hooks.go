package skill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/loop"
)

// sessionHooks holds the hooks of the skills invoked in one session.
type sessionHooks struct {
	skills map[string]bool
	m      *hook.Manager
}

// sessionKey identifies the scope of skill hooks: the session, or the run
// for runs without one.
func sessionKey(sessionID, runID string) string {
	if sessionID != "" {
		return sessionID
	}
	return "run:" + runID
}

// noMatcher lists the events whose matcher Claude Code ignores.
var noMatcher = map[hook.EventType]bool{
	hook.UserPromptSubmit: true,
	hook.PreModel:         true,
	hook.PostModel:        true,
	hook.Stop:             true,
}

// HookEvents returns the events the hooks of the registry's skills handle,
// in sorted order.
func (r *Registry) HookEvents() []hook.EventType {
	set := map[hook.EventType]bool{}
	for _, s := range r.List() {
		for ev := range s.Metadata().Hooks {
			set[hook.EventType(ev)] = true
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// AttachHooks makes h dispatch the hooks of the skills invoked in a
// session or run to them, for the events that skills registered in the
// registry at this time handle. Pass the hook manager of the agent whose
// runs use the Middleware; context that asynchronous skill hooks produce
// is queued on h. The agent package attaches its own manager.
//
// As in Claude Code (https://code.claude.com/docs/en/hooks#hooks-in-skills-and-agents),
// the hooks of a skill are registered when it is invoked and run for the
// rest of the session, and a handler with "once": true is removed after
// its first run that neither fails nor blocks. Hooks are registered once
// per skill and session, held in memory and registered again from the
// conversation when a run of the session starts, for example after a
// restart; a once handler may then run again.
func (m *Manager) AttachHooks(h *hook.Manager) error {
	if h == nil {
		return errors.New("skill: AttachHooks needs a hook manager")
	}
	events := m.reg.HookEvents()
	if len(events) == 0 {
		return nil
	}
	m.mu.Lock()
	m.outer = h
	m.mu.Unlock()
	for _, ev := range events {
		if err := h.On(ev, "", hook.Func(m.dispatch)); err != nil {
			return err
		}
	}
	if slices.Contains(events, hook.SessionEnd) {
		return nil // dispatch forgets the session
	}
	return h.On(hook.SessionEnd, "", hook.Func(func(_ context.Context, ev *hook.Event) (*hook.Result, error) {
		m.forget(sessionKey(ev.SessionID, ev.RunID))
		return nil, nil
	}))
}

// dispatch runs the hooks of the skills invoked in the event's session.
func (m *Manager) dispatch(ctx context.Context, ev *hook.Event) (*hook.Result, error) {
	m.mu.Lock()
	sh := m.sessions[sessionKey(ev.SessionID, ev.RunID)]
	m.mu.Unlock()
	if sh == nil {
		return nil, nil
	}
	res, err := sh.m.Dispatch(ctx, ev)
	if ev.Type == hook.SessionEnd {
		m.forget(sessionKey(ev.SessionID, ev.RunID))
	}
	return res, err
}

// forget drops the skill hooks of a session or run.
func (m *Manager) forget(key string) {
	m.mu.Lock()
	sh := m.sessions[key]
	delete(m.sessions, key)
	m.mu.Unlock()
	if sh != nil {
		sh.m.Close()
	}
}

// registerHooks registers the hooks of s for the run's session, once.
// Problems are logged with the run's logger.
func (m *Manager) registerHooks(run *loop.Run, s Skill) {
	meta := s.Metadata()
	if len(meta.Hooks) == 0 {
		return
	}
	key := sessionKey(run.SessionID(), run.ID())
	m.mu.Lock()
	defer m.mu.Unlock()
	sh := m.sessions[key]
	if sh == nil {
		outer := m.outer
		var inner *hook.Manager
		inner = hook.NewManager(hook.OnAsyncResult(func(r hook.AsyncResult) {
			// Context from asynchronous skill hooks reaches the agent's
			// runs through its own manager.
			if outer != nil && r.Context != "" {
				outer.AddContext(r.SessionID, r.Context)
			}
			inner.DropContext(r.SessionID)
		}))
		sh = &sessionHooks{skills: map[string]bool{}, m: inner}
		m.sessions[key] = sh
	}
	if sh.skills[meta.Name] {
		return
	}
	sh.skills[meta.Name] = true
	warnings, err := m.addHooks(sh.m, meta)
	for _, w := range warnings {
		run.Logger().Warn("skill hook skipped", "skill", meta.Name, "warning", w)
	}
	if err != nil {
		run.Logger().Warn("skill hooks invalid", "skill", meta.Name, "error", err)
	}
}

// SkillHooks returns a manager with the hooks of a skill's frontmatter,
// built with opts, and warnings about handlers that cannot run; see
// hook.SettingsOptions.Parse. Handlers with "once": true run until their
// first run that neither fails nor blocks.
func SkillHooks(meta Metadata, opts hook.SettingsOptions) (*hook.Manager, []string, error) {
	m := hook.NewManager()
	warnings, err := (&Manager{cfg: Config{HookOptions: opts}}).addHooks(m, meta)
	return m, warnings, err
}

// addHooks adds the hooks of meta to dst, each handler on its own so a
// once handler can be dropped alone.
func (m *Manager) addHooks(dst *hook.Manager, meta Metadata) ([]string, error) {
	var warnings []string
	var errs []error
	for _, event := range slices.Sorted(maps.Keys(meta.Hooks)) {
		groups, ok := meta.Hooks[event].([]any)
		if !ok {
			warnings = append(warnings, fmt.Sprintf("hooks.%s: not a list; skipped", event))
			continue
		}
		for gi, g := range groups {
			group, ok := g.(map[string]any)
			if !ok {
				warnings = append(warnings, fmt.Sprintf("hooks.%s[%d]: not a mapping; skipped", event, gi))
				continue
			}
			matcher, _ := group["matcher"].(string)
			handlers, _ := group["hooks"].([]any)
			for hi, h := range handlers {
				handler, ok := h.(map[string]any)
				if !ok {
					warnings = append(warnings, fmt.Sprintf("hooks.%s[%d].hooks[%d]: not a mapping; skipped", event, gi, hi))
					continue
				}
				handler = maps.Clone(handler)
				once, _ := handler["once"].(bool)
				delete(handler, "once")
				data, err := json.Marshal(map[string]any{"hooks": map[string]any{
					event: []any{map[string]any{"matcher": matcher, "hooks": []any{handler}}},
				}})
				if err != nil {
					errs = append(errs, err)
					continue
				}
				single, ws, err := m.cfg.HookOptions.Parse(data)
				if err != nil {
					errs = append(errs, fmt.Errorf("hooks.%s[%d].hooks[%d]: %w", event, gi, hi, err))
					continue
				}
				for _, w := range ws {
					warnings = append(warnings, fmt.Sprintf("hooks.%s[%d].hooks[%d]: %s", event, gi, hi, w))
				}
				if !once {
					dst.Merge(single)
					continue
				}
				ev := hook.EventType(event)
				if noMatcher[ev] {
					matcher = ""
				}
				if !single.Has(ev) {
					continue
				}
				if err := dst.On(ev, matcher, &onceHook{m: single}); err != nil {
					warnings = append(warnings, fmt.Sprintf("hooks.%s[%d].hooks[%d]: %v; skipped", event, gi, hi, err))
				}
			}
		}
	}
	return warnings, errors.Join(errs...)
}

// onceHook runs the hooks of m until a run neither fails nor denies, as a
// Claude Code skill hook with "once": true.
type onceHook struct {
	mu   sync.Mutex
	m    *hook.Manager
	done bool
}

func (o *onceHook) Handle(ctx context.Context, ev *hook.Event) (*hook.Result, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.done {
		return nil, nil
	}
	res, err := o.m.Dispatch(ctx, ev)
	if err == nil && res.Decision != hook.Deny {
		o.done = true
	}
	return res, err
}
