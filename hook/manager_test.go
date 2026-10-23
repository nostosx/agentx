package hook_test

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/model"
)

func on(t *testing.T, m *hook.Manager, event hook.EventType, matcher string, f hook.Func) {
	t.Helper()
	if err := m.On(event, matcher, f); err != nil {
		t.Fatal(err)
	}
}

func TestManagerMatchersAndNil(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name            string
		event           hook.EventType
		matcher, target string
		want            bool
	}{
		{"tool alternatives", hook.PreToolUse, "Edit|Write", "Write", true},
		{"whole tool name", hook.PreToolUse, "Edit|Write", "WriteFile", false},
		{"reason", hook.PreCompact, "auto", "auto", true},
		{"reason mismatch", hook.PreCompact, "auto", "manual", false},
		{"empty", hook.Stop, "", "anything", true},
		{"wildcard", hook.Stop, "*", "anything", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := hook.NewManager()
			called := false
			on(t, m, tt.event, tt.matcher, func(context.Context, *hook.Event) (*hook.Result, error) { called = true; return nil, nil })
			ev := &hook.Event{Type: tt.event, Reason: tt.target, ToolCall: &model.ToolCall{Name: tt.target}}
			res, err := m.Dispatch(t.Context(), ev)
			if err != nil || res == nil || called != tt.want {
				t.Fatalf("Dispatch=%+v,%v called=%v", res, err, called)
			}
		})
	}
	var m *hook.Manager
	if m.Has(hook.Stop) {
		t.Fatal("nil manager has hooks")
	}
	res, err := m.Dispatch(t.Context(), &hook.Event{})
	if res == nil || err != nil {
		t.Fatalf("nil Dispatch=%v,%v", res, err)
	}
	if err := hook.NewManager().On(hook.Stop, "[", hook.Func(func(context.Context, *hook.Event) (*hook.Result, error) { return nil, nil })); err == nil {
		t.Fatal("invalid matcher accepted")
	}
}

func TestManagerDecisionMergeAndUpdatedInput(t *testing.T) {
	t.Parallel()
	m := hook.NewManager()
	first := json.RawMessage(`{"value":1}`)
	second := json.RawMessage(`{"value":2}`)
	out1, out2 := "first", "second"
	// Hooks run in parallel on their own copy of the event; results merge
	// in registration order, whatever order they finish in.
	var ran sync.WaitGroup
	ran.Add(5)
	wait := func() { ran.Done(); ran.Wait() }
	on(t, m, hook.PreToolUse, "*", func(_ context.Context, ev *hook.Event) (*hook.Result, error) {
		wait()
		ev.ToolCall.Arguments = json.RawMessage(`{"mutated":true}`)
		return &hook.Result{Decision: hook.Allow, Reason: "approved", AdditionalContext: "one", UpdatedInput: first, UpdatedOutput: &out1, Halt: true, HaltReason: "first halt"}, nil
	})
	on(t, m, hook.PreToolUse, "*", func(_ context.Context, ev *hook.Event) (*hook.Result, error) {
		wait()
		if string(ev.ToolCall.Arguments) != `{"value":0}` {
			t.Errorf("hook saw arguments %s", ev.ToolCall.Arguments)
		}
		return &hook.Result{Decision: hook.Ask, Reason: "ask", AdditionalContext: "two", UpdatedInput: second, UpdatedOutput: &out2, Halt: true, HaltReason: "second halt"}, nil
	})
	on(t, m, hook.PreToolUse, "*", func(context.Context, *hook.Event) (*hook.Result, error) {
		wait()
		return &hook.Result{Decision: hook.Allow, Reason: "less restrictive"}, nil
	})
	on(t, m, hook.PreToolUse, "*", func(context.Context, *hook.Event) (*hook.Result, error) {
		wait()
		return &hook.Result{Decision: hook.Deny, Reason: "blocked"}, nil
	})
	on(t, m, hook.PreToolUse, "*", func(context.Context, *hook.Event) (*hook.Result, error) {
		wait()
		return &hook.Result{Decision: hook.Deny, Reason: "blocked later", AdditionalContext: "three"}, nil
	})
	ev := &hook.Event{Type: hook.PreToolUse, ToolCall: &model.ToolCall{Name: "Edit", Arguments: json.RawMessage(`{"value":0}`)}}
	res, err := m.Dispatch(t.Context(), ev)
	if err != nil || res.Decision != hook.Deny || res.Reason != "blocked" || res.AdditionalContext != "one\ntwo\nthree" || string(res.UpdatedInput) != string(second) || string(ev.ToolCall.Arguments) != string(second) || res.UpdatedOutput == nil || *res.UpdatedOutput != "second" || !res.Halt || res.HaltReason != "first halt" {
		t.Fatalf("merged=%+v err=%v args=%s", res, err, ev.ToolCall.Arguments)
	}
}

func TestManagerMergeOrderIsRegistrationOrder(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m := hook.NewManager()
		for i := range 6 {
			on(t, m, hook.PostToolUse, "", func(context.Context, *hook.Event) (*hook.Result, error) {
				// Later hooks finish first.
				time.Sleep(time.Duration(6-i) * 20 * time.Millisecond)
				return &hook.Result{AdditionalContext: strconv.Itoa(i), UpdatedOutput: new(strconv.Itoa(i))}, nil
			})
		}
		start := time.Now()
		res, err := m.Dispatch(t.Context(), &hook.Event{Type: hook.PostToolUse, ToolCall: &model.ToolCall{Name: "x"}})
		if err != nil || res.AdditionalContext != "0\n1\n2\n3\n4\n5" || *res.UpdatedOutput != "5" {
			t.Fatalf("merged=%+v err=%v", res, err)
		}
		// In parallel they take as long as the slowest, 120ms of the fake
		// clock; one at a time they would take 420ms.
		if elapsed := time.Since(start); elapsed != 120*time.Millisecond {
			t.Errorf("hooks did not run in parallel: %v", elapsed)
		}
	})
}

func TestManagerParallelismAndModelHookLimit(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		opts []hook.Option
		hook func(f hook.Func) hook.Hook
		n    int
		want int32
	}{
		{"unbounded", nil, func(f hook.Func) hook.Hook { return f }, 20, 20},
		{"bounded", []hook.Option{hook.Parallelism(3)}, func(f hook.Func) hook.Hook { return f }, 10, 3},
		{"sequential", []hook.Option{hook.Parallelism(1)}, func(f hook.Func) hook.Hook { return f }, 5, 1},
		{"model hooks", nil, func(f hook.Func) hook.Hook {
			return &hook.Prompt{Model: funcModel(func(ctx context.Context) { _, _ = f(ctx, nil) }), Prompt: "p"}
		}, 20, 12},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				m := hook.NewManager(tt.opts...)
				var running, peak atomic.Int32
				var order []int
				var mu sync.Mutex
				for i := range tt.n {
					if err := m.On(hook.Stop, "", tt.hook(func(context.Context, *hook.Event) (*hook.Result, error) {
						n := running.Add(1)
						for {
							p := peak.Load()
							if n <= p || peak.CompareAndSwap(p, n) {
								break
							}
						}
						mu.Lock()
						order = append(order, i)
						mu.Unlock()
						time.Sleep(20 * time.Millisecond)
						running.Add(-1)
						return nil, nil
					})); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := m.Dispatch(t.Context(), &hook.Event{Type: hook.Stop}); err != nil {
					t.Fatal(err)
				}
				if peak.Load() != tt.want {
					t.Errorf("peak concurrency = %d, want %d", peak.Load(), tt.want)
				}
				if tt.want == 1 && !slices.IsSorted(order) {
					t.Errorf("sequential order = %v", order)
				}
			})
		})
	}
}

// funcModel is a model that calls f and answers {"ok": true}.
type funcModel func(ctx context.Context)

func (f funcModel) Generate(ctx context.Context, _ *model.Request) (*model.Response, error) {
	f(ctx)
	return &model.Response{Message: model.AssistantText(`{"ok": true}`)}, nil
}

func (f funcModel) Stream(context.Context, *model.Request) iter.Seq2[*model.StreamEvent, error] {
	return func(func(*model.StreamEvent, error) bool) {}
}

func TestManagerDeduplicatesIdenticalHandlers(t *testing.T) {
	t.Parallel()
	m := hook.NewManager()
	var calls atomic.Int32
	mdl := funcModel(func(context.Context) { calls.Add(1) })
	same := func() hook.Hook { return &hook.Prompt{Model: &mdl, Prompt: "check"} }
	for _, matcher := range []string{"Edit", "Edit|Write", "*"} {
		if err := m.On(hook.PreToolUse, matcher, same()); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.On(hook.PreToolUse, "", &hook.Prompt{Model: &mdl, Prompt: "other"}); err != nil {
		t.Fatal(err)
	}
	// Function hooks cannot be compared, so each runs.
	for range 2 {
		on(t, m, hook.PreToolUse, "", func(context.Context, *hook.Event) (*hook.Result, error) { calls.Add(1); return nil, nil })
	}
	if _, err := m.Dispatch(t.Context(), &hook.Event{Type: hook.PreToolUse, ToolCall: &model.ToolCall{Name: "Edit"}}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 {
		t.Fatalf("calls = %d, want 4", calls.Load())
	}
}

func TestManagerRecoversPanics(t *testing.T) {
	t.Parallel()
	m := hook.NewManager()
	on(t, m, hook.Stop, "", func(context.Context, *hook.Event) (*hook.Result, error) { panic("boom") })
	on(t, m, hook.Stop, "", func(context.Context, *hook.Event) (*hook.Result, error) {
		return &hook.Result{AdditionalContext: "fine"}, nil
	})
	res, err := m.Dispatch(t.Context(), &hook.Event{Type: hook.Stop})
	if err == nil || !strings.Contains(err.Error(), "panic: boom") || res.AdditionalContext != "fine" {
		t.Fatalf("Dispatch = %+v, %v", res, err)
	}
}

func TestManagerCancellationSkipsWaitingHooks(t *testing.T) {
	t.Parallel()
	m := hook.NewManager(hook.Parallelism(1))
	ctx, cancel := context.WithCancel(t.Context())
	var later atomic.Bool
	on(t, m, hook.Stop, "", func(context.Context, *hook.Event) (*hook.Result, error) { cancel(); return nil, nil })
	on(t, m, hook.Stop, "", func(context.Context, *hook.Event) (*hook.Result, error) { later.Store(true); return nil, nil })
	if _, err := m.Dispatch(ctx, &hook.Event{Type: hook.Stop}); !errors.Is(err, context.Canceled) || later.Load() {
		t.Fatalf("Dispatch err = %v, later hook ran = %v", err, later.Load())
	}
}

func TestStopBlockCapOption(t *testing.T) {
	t.Parallel()
	if got := hook.NewManager().MaxStopBlocks(); got != hook.DefaultStopBlockCap {
		t.Fatalf("default = %d", got)
	}
	var nilManager *hook.Manager
	if nilManager.MaxStopBlocks() != hook.DefaultStopBlockCap {
		t.Fatal("nil manager cap")
	}
	m := hook.NewManager(hook.StopBlockCap(0))
	if m.MaxStopBlocks() != 0 {
		t.Fatal("cap not disabled")
	}
	merged := hook.NewManager()
	merged.Merge(hook.NewManager(hook.StopBlockCap(3)))
	if merged.MaxStopBlocks() != 3 {
		t.Fatalf("merged cap = %d", merged.MaxStopBlocks())
	}
	if hook.NewManager(hook.StopBlockCap(3), hook.StopBlockCap(-1)).MaxStopBlocks() != hook.DefaultStopBlockCap {
		t.Fatal("negative cap does not restore the default")
	}
}

func TestManagerErrorPoliciesAndMerge(t *testing.T) {
	t.Parallel()
	for _, closed := range []bool{false, true} {
		name := "open"
		if closed {
			name = "closed"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := hook.NewManager()
			other := hook.NewManager()
			if closed {
				other = hook.NewManager(hook.FailClosed())
			}
			sentinel := errors.New("hook unavailable")
			on(t, other, hook.Stop, "", func(context.Context, *hook.Event) (*hook.Result, error) { return nil, sentinel })
			m.Merge(other)
			m.Merge(nil)
			called := false
			on(t, m, hook.Stop, "", func(context.Context, *hook.Event) (*hook.Result, error) {
				called = true
				return &hook.Result{Decision: hook.Allow}, nil
			})
			res, err := m.Dispatch(t.Context(), &hook.Event{Type: hook.Stop})
			if !errors.Is(err, sentinel) || !m.Has(hook.Stop) {
				t.Fatalf("Dispatch=%+v,%v", res, err)
			}
			// Hooks run in parallel, so the other hook runs either way.
			if closed {
				if res.Decision != hook.Deny || !called || !strings.Contains(res.Reason, "hook unavailable") {
					t.Fatalf("fail closed=%+v called=%v", res, called)
				}
			} else if res.Decision != hook.Allow || !called {
				t.Fatalf("fail open=%+v called=%v", res, called)
			}
		})
	}
	m := hook.NewManager()
	on(t, m, hook.Stop, "", func(context.Context, *hook.Event) (*hook.Result, error) {
		t.Error("called canceled hook")
		return nil, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := m.Dispatch(ctx, &hook.Event{Type: hook.Stop}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled=%v", err)
	}
}

func TestManagerConcurrentRegistrationAndDispatch(t *testing.T) {
	t.Parallel()
	var zero hook.Manager
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 20 {
				if err := zero.OnFunc(hook.Stop, "", func(context.Context, *hook.Event) (*hook.Result, error) { return nil, nil }); err != nil {
					t.Error(err)
				}
			}
		})
		wg.Go(func() {
			for range 20 {
				if _, err := zero.Dispatch(t.Context(), &hook.Event{Type: hook.Stop}); err != nil {
					t.Error(err)
				}
				zero.Has(hook.Stop)
			}
		})
	}
	wg.Wait()
}
