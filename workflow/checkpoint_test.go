package workflow_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/workflow"
)

// crashCheckpointer simulates a process crash: after crash, saves and
// deletes are dropped, so the stored checkpoint stays as it was when the
// process died.
type crashCheckpointer struct {
	workflow.Checkpointer
	crashed atomic.Bool
	saves   atomic.Int32
}

func (c *crashCheckpointer) Save(ctx context.Context, cp *workflow.Checkpoint) error {
	if c.crashed.Load() {
		return nil
	}
	c.saves.Add(1)
	return c.Checkpointer.Save(ctx, cp)
}

func (c *crashCheckpointer) Delete(ctx context.Context, id string) error {
	if c.crashed.Load() {
		return nil
	}
	return c.Checkpointer.Delete(ctx, id)
}

func compact(b []byte) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		return string(b)
	}
	return buf.String()
}

func TestCheckpointers(t *testing.T) {
	t.Parallel()
	cp := workflow.NewMemoryCheckpointer()
	if _, err := cp.Load(t.Context(), "missing"); !errors.Is(err, workflow.ErrCheckpointNotFound) {
		t.Fatalf("Load missing = %v", err)
	}
	c := &workflow.Checkpoint{RunID: "run-1", Status: workflow.StatusSuspended, Done: []string{"a"},
		Suspends: map[string][]*workflow.SuspendPoint{"b": {{Payload: []byte(`{"q":1}`)}}}}
	if err := cp.Save(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	c.Done[0] = "mutated"
	got, err := cp.Load(t.Context(), "run-1")
	if err != nil || got.Done[0] != "a" || compact(got.Suspends["b"][0].Payload) != `{"q":1}` || got.Status != workflow.StatusSuspended {
		t.Fatalf("Load = %+v, %v", got, err)
	}
	if err := cp.Delete(t.Context(), "run-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := cp.Load(t.Context(), "run-1"); !errors.Is(err, workflow.ErrCheckpointNotFound) {
		t.Fatalf("Load deleted = %v", err)
	}
	if err := cp.Delete(t.Context(), "run-1"); err != nil {
		t.Fatalf("Delete missing = %v", err)
	}
	for _, id := range []string{"", "../escape", "dot.name"} {
		if cp.Save(t.Context(), &workflow.Checkpoint{RunID: id}) == nil {
			t.Errorf("Save accepted run ID %q", id)
		}
	}
}

func TestSubRunID(t *testing.T) {
	t.Parallel()
	if got := workflow.SubRunID("wf_1", "draft", 2, -1); got != "wf_1-draft-2" {
		t.Fatalf("SubRunID = %q", got)
	}
	if got := workflow.SubRunID("wf_1", "draft", 1, 3); got != "wf_1-draft-1-i3" {
		t.Fatalf("SubRunID item = %q", got)
	}
	hashed := workflow.SubRunID("wf", "write a draft", 1, -1)
	if !strings.HasPrefix(hashed, "wf-_") || strings.Count(hashed, "-") != 2 {
		t.Fatalf("hashed SubRunID = %q", hashed)
	}
	if workflow.SubRunID("wf", "_x", 1, -1) == workflow.SubRunID("wf", "_x2", 1, -1) ||
		workflow.SubRunID("wf", "a-b", 1, -1) == workflow.SubRunID("wf-a", "b", 1, -1) {
		t.Fatal("SubRunIDs collide")
	}
	long := workflow.SubRunID(strings.Repeat("r", 64), strings.Repeat("n", 100), 99, 999)
	if len(long) > 128 {
		t.Fatalf("SubRunID too long for runtime checkpointers: %d", len(long))
	}
}

func TestCheckpointAfterEachNode(t *testing.T) {
	t.Parallel()
	cp := workflow.NewMemoryCheckpointer()
	var seen []*workflow.Checkpoint
	check := func(ctx context.Context, _ *workflow.State) error {
		info, ok := workflow.NodeInfo(ctx)
		if !ok || info.RunID != "run-1" || info.Attempt != 1 || info.Try != 1 || info.Visit != 1 || info.Index != -1 {
			t.Errorf("NodeInfo = %+v, %v", info, ok)
		}
		c, err := cp.Load(ctx, "run-1")
		if err != nil {
			return err
		}
		seen = append(seen, c)
		return nil
	}
	w := workflow.NewGraph().
		AddFunc("a", func(ctx context.Context, st *workflow.State) error { st.Set("a", 1); return check(ctx, st) }).
		AddFunc("b", check).
		AddChain(workflow.Start, "a", "b", workflow.End).MustCompile(workflow.WithCheckpointer(cp), workflow.Named("pipeline"))
	if _, err := w.Run(t.Context(), workflow.NewState(map[string]any{"in": "x"}), workflow.WithRunID("run-1")); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("checkpoints = %d", len(seen))
	}
	if c := seen[0]; c.Status != workflow.StatusRunning || !slices.Equal(c.Running, []string{"a"}) || len(c.Done) != 0 || string(c.Values["in"]) != `"x"` || c.Workflow != "pipeline" || c.Attempts["a"] != 1 {
		t.Fatalf("checkpoint in a = %+v", c)
	}
	if c := seen[1]; !slices.Equal(c.Done, []string{"a"}) || !slices.Equal(c.Running, []string{"b"}) || string(c.Values["a"]) != "1" || !slices.Equal(c.Routes["a"], []string{"b"}) || c.Steps != 2 {
		t.Fatalf("checkpoint in b = %+v", c)
	}
	if _, err := cp.Load(t.Context(), "run-1"); !errors.Is(err, workflow.ErrCheckpointNotFound) {
		t.Fatalf("completed run kept its checkpoint: %v", err)
	}
}

// buildCrashGraph returns a graph a -> {b, c} -> d whose node b blocks on
// its first execution until the test crashes the run.
func buildCrashGraph(counts *sync.Map, block chan struct{}) *workflow.Graph {
	count := func(name string) int {
		v, _ := counts.LoadOrStore(name, new(atomic.Int32))
		return int(v.(*atomic.Int32).Add(1))
	}
	return workflow.NewGraph().
		AddFunc("a", func(_ context.Context, st *workflow.State) error { count("a"); st.Set("a", "done"); return nil }).
		AddFunc("b", func(ctx context.Context, st *workflow.State) error {
			if count("b") == 1 && block != nil {
				close(block)
				<-ctx.Done()
				return ctx.Err()
			}
			info, _ := workflow.NodeInfo(ctx)
			st.Set("b_attempt", info.Attempt)
			st.Set("items", "b")
			return nil
		}).
		AddFunc("c", func(_ context.Context, st *workflow.State) error { count("c"); st.Set("items", "c"); return nil }).
		AddFunc("d", func(_ context.Context, st *workflow.State) error { count("d"); st.Set("d", st.String("a")); return nil }).
		AddEdge(workflow.Start, "a").AddEdge("a", "b").AddEdge("a", "c").AddEdge("b", "d").AddEdge("c", "d")
}

func TestCrashAndResume(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		inner := workflow.NewMemoryCheckpointer()
		cp := &crashCheckpointer{Checkpointer: inner}
		var counts sync.Map
		block := make(chan struct{})
		w := buildCrashGraph(&counts, block).MustCompile(workflow.WithCheckpointer(cp), workflow.Reducer("items", workflow.Append()))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := w.Run(ctx, nil, workflow.WithRunID("crash"))
			done <- err
		}()
		<-block
		// Wait until c has been checkpointed as done, then crash.
		synctest.Wait()
		if c, err := inner.Load(t.Context(), "crash"); err != nil || !slices.Contains(c.Done, "c") {
			t.Fatalf("checkpoint before the crash = %+v, %v", c, err)
		}
		cp.crashed.Store(true)
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("crashed run = %v", err)
		}
		c, err := inner.Load(t.Context(), "crash")
		if err != nil || !slices.Equal(c.Running, []string{"b"}) || c.Status != workflow.StatusRunning {
			t.Fatalf("checkpoint at crash = %+v, %v", c, err)
		}

		// A new process compiles the workflow again and resumes the run.
		var counts2 sync.Map
		w2 := buildCrashGraph(&counts2, nil).MustCompile(workflow.WithCheckpointer(inner), workflow.Reducer("items", workflow.Append()))
		st, err := w2.Resume(t.Context(), "crash", nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{"a", "c"} {
			if _, ran := counts2.Load(n); ran {
				t.Errorf("finished node %s ran again", n)
			}
		}
		if st.String("d") != "done" || st.String("b_attempt") != "1" || st.String("items") != "[c b]" {
			t.Fatalf("state = %v", st.Snapshot())
		}
		if _, err := inner.Load(t.Context(), "crash"); !errors.Is(err, workflow.ErrCheckpointNotFound) {
			t.Fatal("checkpoint kept after the resumed run completed")
		}
	})
}

func TestResumeFailedRunRetriesFailedNode(t *testing.T) {
	t.Parallel()
	cp := workflow.NewMemoryCheckpointer()
	var fail atomic.Bool
	fail.Store(true)
	var ranA atomic.Int32
	w := workflow.NewGraph().
		AddFunc("a", func(_ context.Context, st *workflow.State) error { ranA.Add(1); st.Set("a", 1); return nil }).
		AddFunc("b", func(ctx context.Context, st *workflow.State) error {
			if fail.Load() {
				return errors.New("outage")
			}
			info, _ := workflow.NodeInfo(ctx)
			st.Set("attempt", info.Attempt)
			st.Set("try", info.Try)
			return nil
		}).
		AddChain(workflow.Start, "a", "b").MustCompile(workflow.WithCheckpointer(cp))
	_, err := w.Run(t.Context(), nil, workflow.WithRunID("failing"))
	if err == nil || !strings.Contains(err.Error(), "outage") {
		t.Fatalf("Run = %v", err)
	}
	c, err := cp.Load(t.Context(), "failing")
	if err != nil || c.Status != workflow.StatusFailed || !slices.Equal(c.Failed, []string{"b"}) || !strings.Contains(c.Error, "outage") {
		t.Fatalf("failed checkpoint = %+v, %v", c, err)
	}
	fail.Store(false)
	st, err := w.Resume(t.Context(), "failing", nil)
	if err != nil || ranA.Load() != 1 || st.String("attempt") != "2" || st.String("try") != "1" {
		t.Fatalf("Resume = %v, %v, a ran %d times", st.Snapshot(), err, ranA.Load())
	}
}

func TestRunIDValidationAndJSONValues(t *testing.T) {
	t.Parallel()
	cp := workflow.NewMemoryCheckpointer()
	w := workflow.NewGraph().AddFunc("a", func(_ context.Context, st *workflow.State) error { st.Set("ch", make(chan int)); return nil }).
		AddEdge(workflow.Start, "a").MustCompile(workflow.WithCheckpointer(cp))
	if _, err := w.Run(t.Context(), nil, workflow.WithRunID("bad id")); err == nil || !strings.Contains(err.Error(), "invalid run ID") {
		t.Fatalf("invalid ID = %v", err)
	}
	_, err := w.Run(t.Context(), nil, workflow.WithRunID("json"))
	if err == nil || !strings.Contains(err.Error(), `state value "ch" is not JSON-encodable`) {
		t.Fatalf("non-JSON value = %v", err)
	}
	if _, err := w.Run(t.Context(), nil, workflow.WithRunID("json")); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate ID = %v", err)
	}
	if err := w.Discard(t.Context(), "json"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Resume(t.Context(), "json", nil); !errors.Is(err, workflow.ErrCheckpointNotFound) {
		t.Fatalf("Resume discarded = %v", err)
	}
	// Without a Checkpointer values need not be JSON.
	w = workflow.NewGraph().AddFunc("a", func(_ context.Context, st *workflow.State) error { st.Set("ch", make(chan int)); return nil }).
		AddEdge(workflow.Start, "a").MustCompile()
	if _, err := w.Run(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Resume(t.Context(), "unknown", nil); !errors.Is(err, workflow.ErrCheckpointNotFound) {
		t.Fatalf("Resume unknown = %v", err)
	}
	// Failed runs are not kept in memory.
	w = workflow.NewGraph().AddFunc("a", func(context.Context, *workflow.State) error { return errors.New("x") }).
		AddEdge(workflow.Start, "a").MustCompile()
	if _, err := w.Run(t.Context(), nil, workflow.WithRunID("failed")); err == nil {
		t.Fatal("failing run succeeded")
	}
	if _, err := w.Resume(t.Context(), "failed", nil); !errors.Is(err, workflow.ErrCheckpointNotFound) {
		t.Fatalf("Resume failed run without checkpointer = %v", err)
	}
}

// A missing workflow checkpoint matches loop.ErrCheckpointNotFound, but a
// missing agent run checkpoint does not match the workflow error.
func TestCheckpointNotFoundWrapsLoop(t *testing.T) {
	missing := fmt.Errorf("resume: %w", workflow.ErrCheckpointNotFound)
	runMissing := fmt.Errorf("resume: %w", loop.ErrCheckpointNotFound)
	if !errors.Is(missing, loop.ErrCheckpointNotFound) || errors.Is(runMissing, workflow.ErrCheckpointNotFound) {
		t.Error("workflow.ErrCheckpointNotFound must wrap loop.ErrCheckpointNotFound, not the other way round")
	}
}
