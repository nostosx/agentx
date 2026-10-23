package workflow_test

import (
	"context"
	"errors"
	"testing"

	"github.com/nostosx/agentx/workflow"
)

type terminalFailureCheckpointer struct {
	workflow.Checkpointer
	saveErr   error
	deleteErr error
}

func (c *terminalFailureCheckpointer) Save(ctx context.Context, cp *workflow.Checkpoint) error {
	if cp.Status != workflow.StatusRunning && c.saveErr != nil {
		return c.saveErr
	}
	return c.Checkpointer.Save(ctx, cp)
}

func (c *terminalFailureCheckpointer) Delete(ctx context.Context, id string) error {
	if c.deleteErr != nil {
		return c.deleteErr
	}
	return c.Checkpointer.Delete(ctx, id)
}

func TestTerminalCheckpointErrors(t *testing.T) {
	t.Parallel()
	storageErr := errors.New("checkpoint storage unavailable")
	nodeErr := errors.New("node failed")
	for _, tc := range []struct {
		name          string
		node          func(context.Context, *workflow.State) error
		failDelete    bool
		wantNodeError bool
	}{
		{name: "completed", node: noop, failDelete: true},
		{name: "suspended", node: func(ctx context.Context, _ *workflow.State) error {
			_, err := workflow.Suspend(ctx, "approve?")
			return err
		}},
		{name: "failed", node: func(context.Context, *workflow.State) error { return nodeErr }, wantNodeError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cp := &terminalFailureCheckpointer{Checkpointer: workflow.NewMemoryCheckpointer()}
			if tc.failDelete {
				cp.deleteErr = storageErr
			} else {
				cp.saveErr = storageErr
			}
			w := workflow.NewGraph().AddFunc("work", tc.node).
				AddEdge(workflow.Start, "work").MustCompile(workflow.WithCheckpointer(cp))
			var end *workflow.Event
			var final error
			for ev, err := range w.Stream(t.Context(), nil, workflow.WithRunID("terminal")) {
				if err != nil {
					final = err
				} else if ev.Type == workflow.RunEnd {
					end = ev
				}
			}
			if end == nil || end.Status != workflow.StatusFailed || !errors.Is(end.Err, storageErr) || !errors.Is(final, storageErr) {
				t.Errorf("stream end = %+v, final error = %v", end, final)
			}
			if tc.wantNodeError && (!errors.Is(final, nodeErr) || end != nil && !errors.Is(end.Err, nodeErr)) {
				t.Error("checkpoint failure hid the original node failure")
			}
			_, err := w.Run(t.Context(), nil)
			if !errors.Is(err, storageErr) || errors.Is(err, workflow.ErrSuspended) {
				t.Errorf("Run error = %v, want checkpoint error", err)
			}
			_, err = w.Resume(t.Context(), "terminal", nil)
			if !errors.Is(err, storageErr) || errors.Is(err, workflow.ErrSuspended) {
				t.Errorf("Resume error = %v, want checkpoint error", err)
			}
		})
	}
}
