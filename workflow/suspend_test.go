package workflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nostosx/agentx/workflow"
)

// approvalNode suspends for a decision and stores it under key.
func approvalNode(runs *atomic.Int32, key string) func(context.Context, *workflow.State) error {
	return func(ctx context.Context, st *workflow.State) error {
		runs.Add(1)
		v, err := workflow.Suspend(ctx, map[string]string{"question": "approve " + key + "?"})
		if err != nil {
			return err
		}
		var answer string
		if err := json.Unmarshal(v, &answer); err != nil {
			return err
		}
		st.Set(key, answer)
		return nil
	}
}

func suspension(t *testing.T, err error) *workflow.Suspension {
	t.Helper()
	s, ok := errors.AsType[*workflow.Suspension](err)
	if !ok || !errors.Is(err, workflow.ErrSuspended) || !strings.Contains(err.Error(), "suspended at") {
		t.Fatalf("error = %v, want a suspension", err)
	}
	return s
}

func TestSuspendAndResume(t *testing.T) {
	t.Parallel()
	for _, persisted := range []bool{true, false} {
		t.Run(map[bool]string{true: "checkpointer", false: "memory"}[persisted], func(t *testing.T) {
			t.Parallel()
			var runs, after atomic.Int32
			var opts []workflow.Option
			cp := workflow.NewMemoryCheckpointer()
			if persisted {
				opts = append(opts, workflow.WithCheckpointer(cp))
			}
			w := workflow.NewGraph().
				AddFunc("ask", approvalNode(&runs, "answer"), workflow.Retry(3, 0), workflow.OnError("never")).
				AddFunc("never", func(context.Context, *workflow.State) error { t.Error("suspension fell back"); return nil }).
				AddFunc("after", func(_ context.Context, st *workflow.State) error {
					after.Add(1)
					st.Set("final", st.String("answer"))
					return nil
				}).
				AddChain(workflow.Start, "ask", "after", workflow.End).MustCompile(opts...)
			st, err := w.Run(t.Context(), workflow.NewState(map[string]any{"in": 1}), workflow.WithRunID("hitl"))
			s := suspension(t, err)
			if s.RunID != "hitl" || len(s.Pending) != 1 || s.Pending[0].Key != "ask" || s.Pending[0].Node != "ask" || s.Pending[0].Index != -1 || s.Pending[0].Interrupt != nil {
				t.Fatalf("suspension = %+v", s)
			}
			var payload map[string]string
			if err := s.Pending[0].Decode(&payload); err != nil || payload["question"] != "approve answer?" {
				t.Fatalf("payload = %v, %v", payload, err)
			}
			if _, ok := st.Get("answer"); ok || after.Load() != 0 || runs.Load() != 1 {
				t.Fatalf("suspended run went on: %v", st.Snapshot())
			}
			if persisted {
				c, err := cp.Load(t.Context(), "hitl")
				if err != nil || c.Status != workflow.StatusSuspended || !slices.Equal(c.Suspended, []string{"ask"}) {
					t.Fatalf("checkpoint = %+v, %v", c, err)
				}
			}
			if _, err := w.Resume(t.Context(), "hitl", map[string]any{"other": 1}); err == nil || !strings.Contains(err.Error(), `no pending suspension "other"`) {
				t.Fatalf("Resume with unknown key = %v", err)
			}
			st, err = w.Resume(t.Context(), "hitl", map[string]any{"ask": "yes"})
			if err != nil || st.String("final") != "yes" || st.String("in") != "1" || runs.Load() != 2 || after.Load() != 1 {
				t.Fatalf("Resume = %v, %v (runs %d)", st.Snapshot(), err, runs.Load())
			}
			if _, err := w.Resume(t.Context(), "hitl", nil); !errors.Is(err, workflow.ErrCheckpointNotFound) {
				t.Fatalf("completed run still resumable: %v", err)
			}
		})
	}
}

func TestSuspendSeveralTimesInOneNode(t *testing.T) {
	t.Parallel()
	var runs atomic.Int32
	w := workflow.NewGraph().AddFunc("form", func(ctx context.Context, st *workflow.State) error {
		runs.Add(1)
		name, err := workflow.Suspend(ctx, "name?")
		if err != nil {
			return err
		}
		age, err := workflow.Suspend(ctx, "age?")
		if err != nil {
			return err
		}
		st.Set("form", string(name)+"/"+string(age))
		return nil
	}).AddEdge(workflow.Start, "form").MustCompile(workflow.WithCheckpointer(workflow.NewMemoryCheckpointer()))
	_, err := w.Run(t.Context(), nil, workflow.WithRunID("form"))
	if s := suspension(t, err); string(s.Pending[0].Payload) != `"name?"` {
		t.Fatalf("first suspension = %+v", s.Pending[0])
	}
	_, err = w.Resume(t.Context(), "form", map[string]any{"form": json.RawMessage(`"Ada"`)})
	if s := suspension(t, err); string(s.Pending[0].Payload) != `"age?"` {
		t.Fatalf("second suspension = %+v", s.Pending[0])
	}
	st, err := w.Resume(t.Context(), "form", map[string]any{"form": 36})
	if err != nil || st.String("form") != `"Ada"/36` || runs.Load() != 3 {
		t.Fatalf("Resume = %v, %v", st.Snapshot(), err)
	}
}

func TestParallelSuspensionsAndPartialResume(t *testing.T) {
	t.Parallel()
	var r1, r2, joined atomic.Int32
	w := workflow.NewGraph().
		AddFunc("s1", approvalNode(&r1, "one")).
		AddFunc("s2", approvalNode(&r2, "two")).
		AddFunc("free", func(_ context.Context, st *workflow.State) error { st.Set("free", true); return nil }).
		AddFunc("join", func(_ context.Context, st *workflow.State) error { joined.Add(1); return nil }).
		AddEdge(workflow.Start, "s1").AddEdge(workflow.Start, "s2").AddEdge(workflow.Start, "free").
		AddEdge("s1", "join").AddEdge("s2", "join").AddEdge("free", "join").
		MustCompile(workflow.WithCheckpointer(workflow.NewMemoryCheckpointer()))
	st, err := w.Run(t.Context(), nil, workflow.WithRunID("par"))
	s := suspension(t, err)
	if len(s.Pending) != 2 || s.Pending[0].Key != "s1" || s.Pending[1].Key != "s2" {
		t.Fatalf("pending = %+v", s.Pending)
	}
	if st.String("free") != "true" {
		t.Fatal("independent branch did not finish before the run suspended")
	}
	st, err = w.Resume(t.Context(), "par", map[string]any{"s1": "a"})
	s = suspension(t, err)
	if len(s.Pending) != 1 || s.Pending[0].Key != "s2" || st.String("one") != "a" || joined.Load() != 0 || r2.Load() != 1 {
		t.Fatalf("partial resume = %+v, %v", s.Pending, st.Snapshot())
	}
	st, err = w.Resume(t.Context(), "par", map[string]any{"s2": "b"})
	if err != nil || st.String("two") != "b" || st.String("one") != "a" || joined.Load() != 1 || r1.Load() != 2 {
		t.Fatalf("final resume = %v, %v", st.Snapshot(), err)
	}
}

func TestSuspendErrors(t *testing.T) {
	t.Parallel()
	if _, err := workflow.Suspend(t.Context(), "x"); err == nil {
		t.Fatal("Suspend outside a workflow succeeded")
	}
	if workflow.Emit(t.Context(), "x") {
		t.Fatal("Emit outside a workflow succeeded")
	}
	if _, ok := workflow.NodeInfo(t.Context()); ok {
		t.Fatal("NodeInfo outside a workflow")
	}
	w := workflow.NewGraph().AddFunc("a", func(ctx context.Context, _ *workflow.State) error {
		_, err := workflow.Suspend(ctx, make(chan int))
		return err
	}).AddEdge(workflow.Start, "a").MustCompile()
	if _, err := w.Run(t.Context(), nil); err == nil || errors.Is(err, workflow.ErrSuspended) || !strings.Contains(err.Error(), "suspend payload") {
		t.Fatalf("non-JSON payload = %v", err)
	}
	// A node that ignores ErrSuspended is still suspended.
	w = workflow.NewGraph().AddFunc("a", func(ctx context.Context, st *workflow.State) error {
		_, _ = workflow.Suspend(ctx, "x")
		st.Set("leaked", true)
		return nil
	}).AddEdge(workflow.Start, "a").MustCompile()
	st, err := w.Run(t.Context(), nil, workflow.WithRunID("ignore"))
	_ = suspension(t, err)
	if _, ok := st.Get("leaked"); ok {
		t.Fatal("writes of a suspended node were committed")
	}
	if err := w.Discard(t.Context(), "ignore"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Resume(t.Context(), "ignore", map[string]any{"a": 1}); !errors.Is(err, workflow.ErrCheckpointNotFound) {
		t.Fatalf("Resume after Discard = %v", err)
	}
}
