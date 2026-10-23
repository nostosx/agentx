package loop_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
)

func TestMemoryCheckpointer(t *testing.T) {
	t.Parallel()
	var cp loop.MemoryCheckpointer
	testCheckpointer(t, &cp)
}

func testCheckpointer(t *testing.T, cp loop.Checkpointer) {
	t.Helper()
	if _, err := cp.Load(t.Context(), "missing"); !errors.Is(err, loop.ErrCheckpointNotFound) {
		t.Fatalf("Load missing = %v", err)
	}
	st := &loop.State{RunID: "run-1", Status: loop.StatusInterrupted, Messages: []*model.Message{model.UserText("original")}, Values: map[string]string{"key": "value"}, Pending: []*loop.PendingCall{{Call: &model.ToolCall{ID: "call", Name: "echo", Arguments: []byte(`{"text":"original"}`)}, NeedsApproval: true}}}
	want := st.Clone()
	if err := cp.Save(t.Context(), st); err != nil {
		t.Fatal(err)
	}
	st.Messages[0].Parts[0].Text = "caller mutation"
	st.Values["key"] = "caller mutation"
	st.Pending[0].Call.Arguments[0] = 'x'
	got, err := cp.Load(t.Context(), "run-1")
	if err != nil || !reflect.DeepEqual(got.Clone(), want) {
		t.Fatalf("Load = %+v, %v; want %+v", got, err, want)
	}
	got.Messages[0].Parts[0].Text = "reader mutation"
	got.Values["key"] = "reader mutation"
	again, err := cp.Load(t.Context(), "run-1")
	if err != nil || !reflect.DeepEqual(again.Clone(), want) {
		t.Fatalf("checkpoint changed through reader: %+v, %v", again, err)
	}
	again.Status = loop.StatusStopped
	if err := cp.Save(t.Context(), again); err != nil {
		t.Fatal(err)
	}
	if updated, err := cp.Load(t.Context(), "run-1"); err != nil || updated.Status != loop.StatusStopped {
		t.Fatalf("overwrite = %+v, %v", updated, err)
	}
	if err := cp.Delete(t.Context(), "run-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := cp.Load(t.Context(), "run-1"); !errors.Is(err, loop.ErrCheckpointNotFound) {
		t.Fatalf("Load deleted = %v", err)
	}
}

func TestRuntimeResumeCheckpointErrors(t *testing.T) {
	t.Parallel()
	cp := loop.NewMemoryCheckpointer()
	r := newRuntime(t, loop.Config{Model: modeltest.New(), Checkpointer: cp})
	if _, err := loop.Collect(r.Resume(t.Context(), "missing", nil), nil); !errors.Is(err, loop.ErrCheckpointNotFound) {
		t.Fatalf("Resume missing = %v", err)
	}
	if err := cp.Save(t.Context(), &loop.State{RunID: "completed", Status: loop.StatusCompleted}); err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Collect(r.Resume(t.Context(), "completed", nil), nil); err == nil {
		t.Fatal("Resume accepted completed checkpoint")
	}
}
