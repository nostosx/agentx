package workflow_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nostosx/agentx/workflow"
)

func TestWorkflowBranchSkipsAndJoins(t *testing.T) {
	t.Parallel()
	var eventsMu sync.Mutex
	var events []workflow.Event
	var joined atomic.Int32
	w := workflow.NewGraph().
		AddFunc("left", func(_ context.Context, st *workflow.State) error { st.Set("left", true); return nil }).
		AddFunc("right", func(context.Context, *workflow.State) error { t.Error("unselected branch ran"); return nil }).
		AddFunc("join", func(_ context.Context, st *workflow.State) error {
			joined.Add(1)
			if st.String("left") != "true" {
				t.Error("join ran before selected predecessor")
			}
			return nil
		}).
		AddCondition(workflow.Start, func(context.Context, *workflow.State) (string, error) { return "left", nil }, map[string]string{"left": "left", "right": "right"}).
		AddEdge("left", "join").AddEdge("left", "join").AddEdge("right", "join").AddEdge("join", workflow.End).
		MustCompile(workflow.Named("branch"), workflow.OnEvent(func(ev workflow.Event) { eventsMu.Lock(); defer eventsMu.Unlock(); events = append(events, ev) }))
	if _, err := w.Run(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if joined.Load() != 1 {
		t.Fatalf("join ran %d times", joined.Load())
	}
	var skipped bool
	for _, ev := range events {
		if ev.Workflow != "branch" {
			t.Errorf("event name = %q", ev.Workflow)
		}
		if ev.Node == "right" && ev.Type == workflow.NodeSkipped {
			skipped = true
		}
	}
	if !skipped {
		t.Fatal("unselected branch did not emit skipped event")
	}
}

func TestWorkflowParallelJoin(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		started := make(chan struct{}, 2)
		release := make(chan struct{})
		var done atomic.Int32
		node := func(context.Context, *workflow.State) error {
			started <- struct{}{}
			<-release
			done.Add(1)
			return nil
		}
		w := workflow.NewGraph().AddFunc("a", node).AddFunc("b", node).AddFunc("join", func(context.Context, *workflow.State) error {
			if done.Load() != 2 {
				return errors.New("join ran early")
			}
			return nil
		}).AddEdge(workflow.Start, "a").AddEdge(workflow.Start, "b").AddEdge("a", "join").AddEdge("b", "join").MustCompile(workflow.MaxConcurrency(2))
		result := make(chan error, 1)
		go func() { _, err := w.Run(t.Context(), nil); result <- err }()
		<-started
		<-started
		close(release)
		if err := <-result; err != nil {
			t.Fatal(err)
		}
	})
}

func TestWorkflowRetryAndTimeout(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		attempts := 0
		var events []workflow.Event
		w := workflow.NewGraph().AddFunc("retry", func(ctx context.Context, _ *workflow.State) error {
			attempts++
			if attempts == 1 {
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		}, workflow.Retry(2, time.Second), workflow.Timeout(time.Second)).AddChain(workflow.Start, "retry", workflow.End).MustCompile(workflow.OnEvent(func(ev workflow.Event) { events = append(events, ev) }))
		if _, err := w.Run(t.Context(), nil); err != nil {
			t.Fatal(err)
		}
		if attempts != 2 || len(events) != 4 || events[1].Type != workflow.NodeFailed || !errors.Is(events[1].Err, context.DeadlineExceeded) || events[3].Attempt != 2 {
			t.Fatalf("attempts=%d events=%+v", attempts, events)
		}
	})
}

func TestWorkflowFailuresCancelPeers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		sentinel := errors.New("failure")
		started := make(chan struct{})
		canceled := make(chan struct{})
		w := workflow.NewGraph().AddFunc("wait", func(ctx context.Context, _ *workflow.State) error {
			close(started)
			<-ctx.Done()
			close(canceled)
			return ctx.Err()
		}).AddFunc("fail", func(context.Context, *workflow.State) error { <-started; return sentinel }).AddEdge(workflow.Start, "wait").AddEdge(workflow.Start, "fail").MustCompile()
		_, err := w.Run(t.Context(), nil)
		var nodeErr *workflow.NodeError
		if !errors.Is(err, sentinel) || !errors.As(err, &nodeErr) || nodeErr.Node != "fail" || !strings.Contains(err.Error(), "workflow node fail") {
			t.Fatalf("Run error=%v", err)
		}
		select {
		case <-canceled:
		default:
			t.Error("Run returned before peer cancellation completed")
		}
	})
}

func TestWorkflowPanicAndBranchErrors(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		graph func() *workflow.Graph
		want  string
	}{
		{"panic", func() *workflow.Graph {
			return workflow.NewGraph().AddFunc("a", func(context.Context, *workflow.State) error { panic("boom") }).AddEdge(workflow.Start, "a")
		}, "panic: boom"},
		{"invalid target", func() *workflow.Graph {
			return workflow.NewGraph().AddFunc("a", noop).AddBranch(workflow.Start, func(context.Context, *workflow.State) ([]string, error) { return []string{"unknown"}, nil }, "a")
		}, "not a target"},
		{"condition error", func() *workflow.Graph {
			return workflow.NewGraph().AddFunc("a", noop).AddCondition(workflow.Start, func(context.Context, *workflow.State) (string, error) { return "missing", nil }, map[string]string{"known": "a"})
		}, "no route"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := tt.graph().MustCompile().Run(t.Context(), nil)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Run error=%v, want %q", err, tt.want)
			}
		})
	}
}

func TestWorkflowCanceledBeforeRun(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	w := workflow.NewGraph().AddFunc("a", func(context.Context, *workflow.State) error { t.Error("node called after cancellation"); return nil }).AddEdge(workflow.Start, "a").MustCompile(workflow.MaxConcurrency(1))
	if _, err := w.Run(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run=%v", err)
	}
}

func TestWorkflowBranchPanicFailsRun(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var slowDone atomic.Bool
		slowStarted := make(chan struct{})
		w := workflow.NewGraph().
			// fast waits for slow to start, so the branch fails while slow
			// is running rather than before it is scheduled.
			AddFunc("fast", func(context.Context, *workflow.State) error { <-slowStarted; return nil }).
			AddFunc("slow", func(ctx context.Context, _ *workflow.State) error {
				close(slowStarted)
				<-ctx.Done()
				slowDone.Store(true)
				return ctx.Err()
			}).
			AddFunc("next", func(context.Context, *workflow.State) error { return nil }).
			AddEdge(workflow.Start, "fast").AddEdge(workflow.Start, "slow").
			AddBranch("fast", func(context.Context, *workflow.State) ([]string, error) { panic("bad condition") }, "next").
			AddEdge("slow", workflow.End).AddEdge("next", workflow.End).
			MustCompile()
		_, err := w.Run(t.Context(), nil)
		if nodeErr, ok := errors.AsType[*workflow.NodeError](err); !ok || nodeErr.Node != "fast" || !strings.Contains(err.Error(), "panic: bad condition") {
			t.Fatalf("Run error = %v", err)
		}
		if !slowDone.Load() {
			t.Error("Run returned before the running node finished")
		}
		synctest.Wait()
	})
}
