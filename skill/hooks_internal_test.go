package skill

import (
	"context"
	"errors"
	"testing"

	"github.com/nostosx/agentx/hook"
)

func TestOnceHookRetriesUntilSuccess(t *testing.T) {
	t.Parallel()
	results := []error{errors.New("failed"), nil, nil}
	decisions := []hook.Decision{hook.Continue, hook.Deny, hook.Continue}
	runs := 0
	inner := hook.NewManager()
	_ = inner.OnFunc(hook.PreToolUse, "", func(context.Context, *hook.Event) (*hook.Result, error) {
		i := runs
		runs++
		if results[i] != nil {
			return nil, results[i]
		}
		return &hook.Result{Decision: decisions[i]}, nil
	})
	o := &onceHook{m: inner}
	for range 5 {
		_, _ = o.Handle(t.Context(), &hook.Event{Type: hook.PreToolUse})
	}
	// A failure and a block leave the hook in place; the first success
	// removes it.
	if runs != 3 {
		t.Errorf("runs = %d, want 3", runs)
	}
}

func TestSessionEndForgetsSkillHooks(t *testing.T) {
	t.Parallel()
	for _, endHook := range []bool{false, true} {
		meta := Metadata{Name: "h", Description: "d", Hooks: map[string]any{"PreToolUse": []any{}}}
		if endHook {
			meta.Hooks["SessionEnd"] = []any{}
		}
		s, err := Define(meta, "x")
		if err != nil {
			t.Fatal(err)
		}
		m := NewManager(NewRegistry(s), Config{})
		outer := hook.NewManager()
		if err := m.AttachHooks(outer); err != nil {
			t.Fatal(err)
		}
		m.sessions["s"] = &sessionHooks{skills: map[string]bool{"h": true}, m: hook.NewManager()}
		m.sessions["run:r"] = &sessionHooks{skills: map[string]bool{"h": true}, m: hook.NewManager()}
		if _, err := outer.Dispatch(t.Context(), &hook.Event{Type: hook.PreToolUse, SessionID: "other"}); err != nil {
			t.Fatal(err)
		}
		if _, err := outer.Dispatch(t.Context(), &hook.Event{Type: hook.SessionEnd, SessionID: "s"}); err != nil {
			t.Fatal(err)
		}
		if _, ok := m.sessions["s"]; ok || len(m.sessions) != 1 {
			t.Errorf("sessions after SessionEnd = %v", m.sessions)
		}
	}
	// A registry without hooks attaches nothing.
	outer := hook.NewManager()
	if err := NewManager(nil, Config{}).AttachHooks(outer); err != nil || outer.Has(hook.SessionEnd) {
		t.Errorf("AttachHooks without skill hooks = %v", err)
	}
}
