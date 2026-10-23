package workflow_test

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/nostosx/agentx/workflow"
)

func TestStreamEvents(t *testing.T) {
	t.Parallel()
	var listened []workflow.EventType
	w := workflow.NewGraph().
		AddFunc("a", func(ctx context.Context, st *workflow.State) error {
			if !workflow.Emit(ctx, map[string]int{"progress": 50}) {
				t.Error("Emit failed in a node")
			}
			st.Set("x", 1)
			st.Delete("old")
			return nil
		}).
		AddFunc("b", noop).
		AddFunc("skipped", noop).
		AddChain(workflow.Start, "a").
		AddBranch("a", func(context.Context, *workflow.State) ([]string, error) { return []string{"b"}, nil }, "b", "skipped").
		MustCompile(workflow.Named("streamed"), workflow.OnEvent(func(ev workflow.Event) { listened = append(listened, ev.Type) }))
	var types []workflow.EventType
	var runID string
	for ev, err := range w.Stream(t.Context(), workflow.NewState(map[string]any{"old": true})) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Workflow != "streamed" || ev.RunID == "" || (runID != "" && ev.RunID != runID) {
			t.Fatalf("event = %+v", ev)
		}
		runID = ev.RunID
		types = append(types, ev.Type)
		switch ev.Type {
		case workflow.CustomEvent:
			if ev.Node != "a" || ev.Custom.(map[string]int)["progress"] != 50 || ev.Index != -1 {
				t.Errorf("custom event = %+v", ev)
			}
		case workflow.StateUpdated:
			if ev.Node != "a" || ev.Delta["x"] != 1 || !slices.Equal(ev.Removed, []string{"old"}) {
				t.Errorf("state event = %+v", ev)
			}
		case workflow.NodeSkipped:
			if ev.Node != "skipped" {
				t.Errorf("skip event = %+v", ev)
			}
		case workflow.RunEnd:
			if ev.Status != workflow.StatusCompleted {
				t.Errorf("run end = %+v", ev)
			}
		}
	}
	want := []workflow.EventType{workflow.RunStart, workflow.NodeStart, workflow.CustomEvent, workflow.NodeEnd, workflow.StateUpdated, workflow.NodeSkipped, workflow.NodeStart, workflow.NodeEnd, workflow.RunEnd}
	if !slices.Equal(types, want) {
		t.Fatalf("events = %v, want %v", types, want)
	}
	if wantListened := []workflow.EventType{workflow.NodeStart, workflow.NodeEnd, workflow.NodeSkipped, workflow.NodeStart, workflow.NodeEnd}; !slices.Equal(listened, wantListened) {
		t.Fatalf("listener events = %v", listened)
	}
}

func TestStreamFailureYieldsError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom")
	w := workflow.NewGraph().AddFunc("a", func(context.Context, *workflow.State) error { return sentinel }).AddEdge(workflow.Start, "a").MustCompile()
	var end *workflow.Event
	var final error
	for ev, err := range w.Stream(t.Context(), nil) {
		if err != nil {
			final = err
			continue
		}
		if ev.Type == workflow.RunEnd {
			end = ev
		}
	}
	if end == nil || end.Status != workflow.StatusFailed || !errors.Is(end.Err, sentinel) || !errors.Is(final, sentinel) {
		t.Fatalf("end = %+v, error = %v", end, final)
	}
}

func TestStreamConsumerStopAndResume(t *testing.T) {
	t.Parallel()
	cp := workflow.NewMemoryCheckpointer()
	var runs atomic.Int32
	w := workflow.NewGraph().AddFunc("slow", func(ctx context.Context, st *workflow.State) error {
		if runs.Add(1) == 1 {
			<-ctx.Done()
			return ctx.Err()
		}
		info, _ := workflow.NodeInfo(ctx)
		st.Set("attempt", info.Attempt)
		return nil
	}).AddEdge(workflow.Start, "slow").MustCompile(workflow.WithCheckpointer(cp))
	for ev, err := range w.Stream(t.Context(), nil, workflow.WithRunID("stop")) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == workflow.NodeStart {
			break
		}
	}
	c, err := cp.Load(t.Context(), "stop")
	if err != nil || c.Status != workflow.StatusFailed || !slices.Equal(c.Running, []string{"slow"}) {
		t.Fatalf("checkpoint = %+v, %v", c, err)
	}
	// Stopping at RunStart of a resumed run leaves the checkpoint alone.
	for _, err := range w.ResumeStream(t.Context(), "stop", nil) {
		if err != nil {
			t.Fatalf("ResumeStream: %v", err)
		}
		break
	}
	if runs.Load() != 1 {
		t.Fatal("resumed run started nodes after its consumer stopped")
	}
	st, err := w.Resume(t.Context(), "stop", nil)
	if err != nil || st.String("attempt") != "1" || runs.Load() != 2 {
		t.Fatalf("Resume = %v, %v", st.Snapshot(), err)
	}
}

func TestRunOptionsApplyToOneRun(t *testing.T) {
	t.Parallel()
	var base, extra atomic.Int32
	w := workflow.NewGraph().AddFunc("a", noop).AddEdge(workflow.Start, "a").
		MustCompile(workflow.OnEvent(func(workflow.Event) { base.Add(1) }))
	if _, err := w.Run(t.Context(), nil, workflow.OnEvent(func(workflow.Event) { extra.Add(1) })); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Run(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if base.Load() != 4 || extra.Load() != 2 {
		t.Fatalf("listener calls: base %d, extra %d", base.Load(), extra.Load())
	}
}
