package loop_test

import (
	"testing"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
)

func TestStatusDone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		s    loop.Status
		done bool
	}{
		{name: "empty"},
		{name: "running", s: loop.StatusRunning},
		{name: "interrupted", s: loop.StatusInterrupted},
		{name: "completed", s: loop.StatusCompleted, done: true},
		{name: "failed", s: loop.StatusFailed, done: true},
		{name: "limited", s: loop.StatusMaxTurns, done: true},
		{name: "stopped", s: loop.StatusStopped, done: true},
		{name: "blocked", s: loop.StatusBlocked, done: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.Done(); got != tc.done {
				t.Fatalf("Done = %v, want %v", got, tc.done)
			}
		})
	}
}

func TestStateCloneAndNewMessages(t *testing.T) {
	t.Parallel()
	st := &loop.State{Start: 1, Messages: []*model.Message{model.UserText("history"), model.AssistantText("answer")}, Values: map[string]string{"key": "original"}, Pending: []*loop.PendingCall{{Call: &model.ToolCall{Arguments: []byte(`{}`)}}}}
	clone := st.Clone()
	clone.Messages[0].Parts[0].Text = "changed"
	clone.Values["key"] = "changed"
	clone.Pending[0].Call.Arguments[0] = 'x'
	if st.Messages[0].Text() != "history" || st.Values["key"] != "original" || string(st.Pending[0].Call.Arguments) != "{}" {
		t.Fatal("Clone shares mutable state")
	}
	if got := st.NewMessages(); len(got) != 1 || got[0].Text() != "answer" {
		t.Fatalf("NewMessages = %+v", got)
	}
	st.Start = len(st.Messages)
	if got := st.NewMessages(); len(got) != 0 {
		t.Fatalf("NewMessages at end = %+v", got)
	}
	st.Start++
	if got := st.NewMessages(); got != nil {
		t.Fatalf("NewMessages past end = %+v", got)
	}
}
