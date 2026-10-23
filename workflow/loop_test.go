package workflow_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nostosx/agentx/workflow"
)

// loopGraph builds gen -> check, where check loops back to gen until
// count reaches limit and then continues to done.
func loopGraph(limit int, gen func(context.Context, *workflow.State) error) *workflow.Graph {
	return workflow.NewGraph().
		AddFunc("gen", gen).
		AddFunc("check", noop).
		AddFunc("done", func(_ context.Context, st *workflow.State) error { st.Set("finished", true); return nil }).
		AddChain(workflow.Start, "gen", "check").
		AddCondition("check", func(_ context.Context, st *workflow.State) (string, error) {
			n, _, err := st.Value[int]("count")
			if err != nil {
				return "", err
			}
			if n < limit {
				return "again", nil
			}
			return "ok", nil
		}, map[string]string{"again": "gen", "ok": "done"}).
		AddEdge("done", workflow.End)
}

func countNode(visits *[]workflow.Info, mu *sync.Mutex) func(context.Context, *workflow.State) error {
	return func(ctx context.Context, st *workflow.State) error {
		info, _ := workflow.NodeInfo(ctx)
		mu.Lock()
		*visits = append(*visits, info)
		mu.Unlock()
		n, _, err := st.Value[int]("count")
		if err != nil {
			return err
		}
		st.Set("count", n+1)
		st.Set("log", n)
		return nil
	}
}

func TestLoopBackThroughCondition(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var visits []workflow.Info
	var skipped []string
	w := loopGraph(3, countNode(&visits, &mu)).MustCompile(workflow.Reducer("log", workflow.Append()),
		workflow.OnEvent(func(ev workflow.Event) {
			if ev.Type == workflow.NodeSkipped {
				skipped = append(skipped, ev.Node)
			}
		}))
	st, err := w.Run(t.Context(), workflow.NewState(map[string]any{"count": 0}))
	if err != nil {
		t.Fatal(err)
	}
	if st.String("count") != "3" || st.String("finished") != "true" || st.String("log") != "[0 1 2]" {
		t.Fatalf("state = %v", st.Snapshot())
	}
	if len(visits) != 3 || visits[2].Visit != 3 || visits[2].Attempt != 3 || visits[2].Try != 1 {
		t.Fatalf("visits = %+v", visits)
	}
	// Nodes after a loop are not skipped while it loops.
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v", skipped)
	}
}

func TestLoopStepLimit(t *testing.T) {
	t.Parallel()
	w := loopGraph(1000, func(context.Context, *workflow.State) error { return nil }).MustCompile(workflow.MaxSteps(5))
	_, err := w.Run(t.Context(), workflow.NewState(map[string]any{"count": 0}))
	var nodeErr *workflow.NodeError
	if !errors.Is(err, workflow.ErrStepLimit) || !errors.As(err, &nodeErr) || nodeErr.Node != "check" {
		t.Fatalf("Run = %v", err)
	}
	// Resuming with a higher limit continues the loop.
	cp := workflow.NewMemoryCheckpointer()
	var n atomic.Int32
	w = loopGraph(4, func(_ context.Context, st *workflow.State) error { st.Set("count", int(n.Add(1))); return nil }).
		MustCompile(workflow.MaxSteps(3), workflow.WithCheckpointer(cp))
	if _, err := w.Run(t.Context(), workflow.NewState(map[string]any{"count": 0}), workflow.WithRunID("steps")); !errors.Is(err, workflow.ErrStepLimit) {
		t.Fatalf("limited run = %v", err)
	}
	st, err := w.Resume(t.Context(), "steps", nil, workflow.MaxSteps(100))
	if err != nil || st.String("count") != "4" || st.String("finished") != "true" {
		t.Fatalf("Resume = %v, %v", st.Snapshot(), err)
	}
}

func TestLoopValidation(t *testing.T) {
	t.Parallel()
	_, err := workflow.NewGraph().AddFunc("a", noop).AddFunc("b", noop).AddChain(workflow.Start, "a", "b").
		AddBranch("b", func(context.Context, *workflow.State) ([]string, error) { return nil, nil }, "a").
		AddEdge("b", "a").Compile()
	if err == nil || !strings.Contains(err.Error(), "cycle a -> b -> a") {
		t.Fatalf("static cycle = %v", err)
	}
	w := workflow.NewGraph().AddFunc("a", noop).AddFunc("b", noop).AddFunc("c", noop).AddChain(workflow.Start, "a", "b").
		AddBranch("b", func(context.Context, *workflow.State) ([]string, error) { return []string{"a", "c"}, nil }, "a", "c").
		MustCompile()
	if _, err := w.Run(t.Context(), nil); err == nil || !strings.Contains(err.Error(), "routes back to a loop target and continues") {
		t.Fatalf("mixed routes = %v", err)
	}
}

func TestLoopWaitsForRunningNodesOfItsRegion(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var timeline []string
	record := func(s string) {
		mu.Lock()
		timeline = append(timeline, s)
		mu.Unlock()
	}
	slowRelease := make(chan struct{})
	var iter atomic.Int32
	w := workflow.NewGraph().
		AddFunc("head", func(_ context.Context, st *workflow.State) error {
			record("head")
			st.Set("items", "h")
			return nil
		}).
		AddFunc("fast", func(_ context.Context, st *workflow.State) error {
			record("fast")
			st.Set("x", iter.Add(1))
			return nil
		}).
		AddFunc("slow", func(context.Context, *workflow.State) error {
			if iter.Load() <= 1 {
				<-slowRelease
			}
			record("slow")
			return nil
		}).
		AddFunc("check", func(context.Context, *workflow.State) error {
			if iter.Load() == 1 {
				close(slowRelease)
			}
			return nil
		}).
		AddEdge(workflow.Start, "head").AddEdge("head", "fast").AddEdge("head", "slow").AddEdge("fast", "check").
		AddBranch("check", func(context.Context, *workflow.State) ([]string, error) {
			if iter.Load() < 2 {
				return []string{"head"}, nil
			}
			return nil, nil
		}, "head").
		AddEdge("slow", workflow.End).
		MustCompile(workflow.Reducer("items", workflow.Append()))
	st, err := w.Run(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(timeline, ",") != "head,fast,slow,head,fast,slow" && strings.Join(timeline, ",") != "head,fast,slow,head,slow,fast" {
		t.Fatalf("timeline = %v", timeline)
	}
	if st.String("items") != "[h h]" || st.String("x") != "2" {
		t.Fatalf("state = %v", st.Snapshot())
	}
}

func TestLoopCrashAndResume(t *testing.T) {
	t.Parallel()
	inner := workflow.NewMemoryCheckpointer()
	cp := &crashCheckpointer{Checkpointer: inner}
	var mu sync.Mutex
	var visits []workflow.Info
	block := make(chan struct{})
	gen := func(crash bool) func(context.Context, *workflow.State) error {
		count := countNode(&visits, &mu)
		return func(ctx context.Context, st *workflow.State) error {
			info, _ := workflow.NodeInfo(ctx)
			if crash && info.Visit == 2 {
				close(block)
				<-ctx.Done()
				return ctx.Err()
			}
			return count(ctx, st)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := loopGraph(3, gen(true)).MustCompile(workflow.WithCheckpointer(cp), workflow.Reducer("log", workflow.Append())).
			Run(ctx, workflow.NewState(map[string]any{"count": 0}), workflow.WithRunID("loop"))
		done <- err
	}()
	<-block
	cp.crashed.Store(true)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted run: %v, want context.Canceled", err)
	}
	cp.crashed.Store(false)
	st, err := loopGraph(3, gen(false)).MustCompile(workflow.WithCheckpointer(cp), workflow.Reducer("log", workflow.Append())).
		Resume(t.Context(), "loop", nil)
	if err != nil || st.String("count") != "3" || st.String("log") != "[0 1 2]" {
		t.Fatalf("Resume = %v, %v", st.Snapshot(), err)
	}
	if len(visits) != 3 || visits[1].Visit != 2 || visits[1].Attempt != 2 || visits[2].Visit != 3 {
		t.Fatalf("visits = %+v", visits)
	}
}
