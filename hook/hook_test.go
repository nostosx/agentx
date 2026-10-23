package hook_test

import (
	"context"
	"testing"

	"github.com/nostosx/agentx/hook"
)

func TestResultSameDecisionKeepsFirstReason(t *testing.T) {
	t.Parallel()
	m := hook.NewManager()
	for _, reason := range []string{"", "first", "second"} {
		on(t, m, hook.Stop, "", func(context.Context, *hook.Event) (*hook.Result, error) {
			return &hook.Result{Decision: hook.Ask, Reason: reason}, nil
		})
	}
	on(t, m, hook.Stop, "", func(context.Context, *hook.Event) (*hook.Result, error) { return nil, nil })
	result, err := m.Dispatch(t.Context(), &hook.Event{Type: hook.Stop})
	if err != nil || result.Decision != hook.Ask || result.Reason != "first" {
		t.Fatalf("same decision result=%+v,%v", result, err)
	}
}

func TestResultMergeDeferAndSystemMessage(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		decisions []hook.Decision
		want      hook.Decision
	}{
		{[]hook.Decision{hook.Allow, hook.Defer, hook.Ask}, hook.Defer},
		{[]hook.Decision{hook.Defer, hook.Deny}, hook.Deny},
	} {
		m := hook.NewManager()
		for _, d := range tt.decisions {
			on(t, m, hook.PreToolUse, "", func(context.Context, *hook.Event) (*hook.Result, error) {
				return &hook.Result{Decision: d, SystemMessage: string(d)}, nil
			})
		}
		res, err := m.Dispatch(t.Context(), &hook.Event{Type: hook.PreToolUse})
		if err != nil || res.Decision != tt.want {
			t.Fatalf("%v merged to %+v, %v", tt.decisions, res, err)
		}
		if tt.want == hook.Defer && res.SystemMessage != "allow\ndefer\nask" {
			t.Fatalf("SystemMessage = %q", res.SystemMessage)
		}
	}
}
