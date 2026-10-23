package workflow_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/workflow"
)

func TestRetryPolicyBackoff(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var starts []time.Time
		w := workflow.NewGraph().AddFunc("flaky", func(context.Context, *workflow.State) error {
			starts = append(starts, time.Now())
			if len(starts) < 4 {
				return &model.APIError{StatusCode: 503}
			}
			return nil
		}, workflow.WithRetry(workflow.RetryPolicy{MaxAttempts: 4, InitialBackoff: time.Second, MaxBackoff: 3 * time.Second, Jitter: -1, RetryIf: workflow.Transient})).
			AddEdge(workflow.Start, "flaky").MustCompile()
		if _, err := w.Run(t.Context(), nil); err != nil {
			t.Fatal(err)
		}
		want := []time.Duration{time.Second, 2 * time.Second, 3 * time.Second}
		for i, d := range want {
			if got := starts[i+1].Sub(starts[i]); got != d {
				t.Errorf("wait %d = %s, want %s", i+1, got, d)
			}
		}
	})
}

func TestRetryPolicyJitterAndRetryAfter(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var starts []time.Time
		w := workflow.NewGraph().AddFunc("limited", func(context.Context, *workflow.State) error {
			starts = append(starts, time.Now())
			switch len(starts) {
			case 1:
				return &model.APIError{StatusCode: 429, RetryAfter: 5 * time.Second}
			case 2:
				return errors.New("plain")
			}
			return nil
		}, workflow.WithRetry(workflow.RetryPolicy{MaxAttempts: 3, InitialBackoff: time.Second, Jitter: 0.5})).
			AddEdge(workflow.Start, "limited").MustCompile()
		if _, err := w.Run(t.Context(), nil); err != nil {
			t.Fatal(err)
		}
		if d := starts[1].Sub(starts[0]); d != 5*time.Second {
			t.Errorf("Retry-After wait = %s", d)
		}
		if d := starts[2].Sub(starts[1]); d < time.Second || d > 3*time.Second {
			t.Errorf("jittered wait = %s", d)
		}
	})
}

func TestRetryIfStopsPermanentErrors(t *testing.T) {
	t.Parallel()
	attempts := 0
	w := workflow.NewGraph().AddFunc("bad", func(context.Context, *workflow.State) error {
		attempts++
		return model.Permanent(errors.New("invalid request"))
	}, workflow.WithRetry(workflow.RetryPolicy{MaxAttempts: 5, InitialBackoff: time.Millisecond, RetryIf: workflow.Transient})).
		AddEdge(workflow.Start, "bad").MustCompile()
	if _, err := w.Run(t.Context(), nil); err == nil || attempts != 1 {
		t.Fatalf("Run = %v after %d attempts", err, attempts)
	}
	if !workflow.Transient(fmt.Errorf("wrapped: %w", workflow.ErrNodeTimeout)) || workflow.Transient(context.Canceled) {
		t.Fatal("Transient classification")
	}
}

func TestTimeoutIsTransient(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		attempts := 0
		var errs []error
		w := workflow.NewGraph().AddFunc("slow", func(ctx context.Context, _ *workflow.State) error {
			attempts++
			if attempts == 1 {
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		}, workflow.Timeout(time.Second), workflow.WithRetry(workflow.RetryPolicy{MaxAttempts: 2, RetryIf: workflow.Transient})).
			AddEdge(workflow.Start, "slow").MustCompile(workflow.OnEvent(func(ev workflow.Event) {
			if ev.Type == workflow.NodeFailed {
				errs = append(errs, ev.Err)
			}
		}))
		if _, err := w.Run(t.Context(), nil); err != nil || attempts != 2 {
			t.Fatalf("Run = %v after %d attempts", err, attempts)
		}
		if len(errs) != 1 || !errors.Is(errs[0], workflow.ErrNodeTimeout) || !errors.Is(errs[0], context.DeadlineExceeded) {
			t.Fatalf("errors = %v", errs)
		}
	})
}

func TestOnErrorRoutesToFallback(t *testing.T) {
	t.Parallel()
	w := workflow.NewGraph().
		AddFunc("primary", func(_ context.Context, st *workflow.State) error {
			st.Set("partial", true)
			return errors.New("primary down")
		}, workflow.Retry(2, 0), workflow.OnError("fallback")).
		AddFunc("next", func(context.Context, *workflow.State) error { t.Error("normal successor ran"); return nil }).
		AddFunc("fallback", func(_ context.Context, st *workflow.State) error {
			st.Set("answer", "fallback after: "+st.String(workflow.ErrorKey("primary")))
			return nil
		}).
		AddFunc("join", func(_ context.Context, st *workflow.State) error { st.Set("joined", true); return nil }).
		AddChain(workflow.Start, "primary", "next", "join").AddEdge("fallback", "join").MustCompile()
	st, err := w.Run(t.Context(), nil)
	if err != nil || st.String("answer") != "fallback after: primary down" || st.String("joined") != "true" {
		t.Fatalf("Run = %v, %v", st.Snapshot(), err)
	}
	if _, ok := st.Get("partial"); ok {
		t.Fatal("failed node's writes were committed")
	}
	// Errors from a fallback node itself fail the run.
	w = workflow.NewGraph().
		AddFunc("a", func(context.Context, *workflow.State) error { return errors.New("a") }, workflow.OnError("b")).
		AddFunc("b", func(context.Context, *workflow.State) error { return errors.New("b failed too") }).
		AddEdge(workflow.Start, "a").MustCompile()
	if _, err := w.Run(t.Context(), nil); err == nil || !strings.Contains(err.Error(), "b failed too") {
		t.Fatalf("fallback failure = %v", err)
	}
	if _, err := workflow.NewGraph().AddFunc("a", noop, workflow.OnError("missing")).AddEdge(workflow.Start, "a").Compile(); err == nil || !strings.Contains(err.Error(), "falls back to unknown node") {
		t.Fatalf("unknown fallback = %v", err)
	}
	if _, err := workflow.NewGraph().AddFunc("a", noop).AddFunc("b", noop, workflow.OnError("a")).AddChain(workflow.Start, "a", "b").Compile(); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("fallback cycle = %v", err)
	}
}
