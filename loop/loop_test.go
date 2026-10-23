package loop_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nostosx/agentx/actx"
	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
)

func TestRuntimeInputHooksAndContext(t *testing.T) {
	t.Parallel()
	hooks := hook.NewManager()
	for _, event := range []hook.EventType{hook.SessionStart, hook.UserPromptSubmit} {
		if err := hooks.OnFunc(event, "", func(_ context.Context, ev *hook.Event) (*hook.Result, error) {
			if ev.Prompt != "question" || ev.SessionID != "s" {
				t.Errorf("hook input = %+v", ev)
			}
			return &hook.Result{AdditionalContext: string(ev.Type)}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	m := modeltest.New(modeltest.Reply("answer"))
	r := newRuntime(t, loop.Config{Model: m, Instructions: "base", Middlewares: []loop.Middleware{hook.Middleware(hooks), actx.Middleware(actx.NewManager(actx.Config{Providers: []actx.Provider{
		actx.Static("system", "system section", actx.PlaceSystem),
		actx.Static("reminder", "provider reminder", actx.PlaceReminder),
		actx.ProviderFunc(func(context.Context, *actx.Input) ([]actx.Section, error) {
			return nil, errors.New("optional provider failed")
		}),
	}}))}})
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("question"), SessionID: "s", NewSession: true}), nil)
	if err != nil || res.Status != loop.StatusCompleted {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	req := m.Requests()[0]
	if req.System != "base\n\nsystem section" {
		t.Fatalf("system prompt = %q", req.System)
	}
	for _, want := range []string{"provider reminder", string(hook.SessionStart), string(hook.UserPromptSubmit), "question"} {
		if !strings.Contains(req.Messages[0].Text(), want) {
			t.Errorf("user message missing %q: %q", want, req.Messages[0].Text())
		}
	}
}

func TestRuntimeHookStops(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		event  hook.EventType
		result hook.Result
		status loop.Status
		calls  int
	}{
		{name: "blocked prompt", event: hook.UserPromptSubmit, result: hook.Result{Decision: hook.Deny, Reason: "policy"}, status: loop.StatusBlocked},
		{name: "session halt", event: hook.SessionStart, result: hook.Result{Halt: true, HaltReason: "policy"}, status: loop.StatusStopped},
		{name: "denied model", event: hook.PreModel, result: hook.Result{Decision: hook.Deny, Reason: "policy"}, status: loop.StatusStopped},
		{name: "post model halt", event: hook.PostModel, result: hook.Result{Halt: true, HaltReason: "policy"}, status: loop.StatusStopped, calls: 1},
		{name: "stop halt", event: hook.Stop, result: hook.Result{Halt: true, HaltReason: "policy"}, status: loop.StatusStopped, calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hooks := hook.NewManager()
			if err := hooks.OnFunc(tc.event, "", func(context.Context, *hook.Event) (*hook.Result, error) { return &tc.result, nil }); err != nil {
				t.Fatal(err)
			}
			m := modeltest.New(modeltest.Reply("answer"))
			r := newRuntime(t, loop.Config{Model: m, Middlewares: []loop.Middleware{hook.Middleware(hooks)}})
			res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("question"), NewSession: true}), nil)
			if err != nil || res.Status != tc.status || res.Reason != "policy" || len(m.Requests()) != tc.calls {
				t.Fatalf("Run = %+v, %v; model calls = %d", res, err, len(m.Requests()))
			}
		})
	}
}

func TestRuntimeStopHookCanContinue(t *testing.T) {
	t.Parallel()
	hooks := hook.NewManager()
	stops := 0
	if err := hooks.OnFunc(hook.Stop, "", func(_ context.Context, ev *hook.Event) (*hook.Result, error) {
		stops++
		if stops == 1 {
			if ev.StopHookActive {
				t.Error("first Stop hook marked active")
			}
			return &hook.Result{Decision: hook.Deny, Reason: "include details"}, nil
		}
		if !ev.StopHookActive {
			t.Error("second Stop hook did not see continuation flag")
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	m := modeltest.New(modeltest.Reply("partial"), modeltest.Reply("detailed answer"))
	r := newRuntime(t, loop.Config{Model: m, Middlewares: []loop.Middleware{hook.Middleware(hooks)}})
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("question")}), nil)
	if err != nil || res.Output != "detailed answer" || res.Turns != 2 || stops != 2 {
		t.Fatalf("Run = %+v, %v; stops = %d", res, err, stops)
	}
	if got := m.Requests()[1].Messages[2].Text(); !strings.Contains(got, "include details") {
		t.Fatalf("continuation reminder = %q", got)
	}
}

func TestRuntimePauseAndMaxTurns(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{1, 2} {
		t.Run(map[int]string{1: "limit", 2: "continue"}[limit], func(t *testing.T) {
			t.Parallel()
			m := modeltest.New(modeltest.Respond(&model.Response{Message: model.AssistantText("partial"), FinishReason: model.FinishPause}), modeltest.Reply("finished"))
			r := newRuntime(t, loop.Config{Model: m, MaxTurns: limit})
			res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("question")}), nil)
			if err != nil || res.Turns != limit {
				t.Fatalf("Run = %+v, %v", res, err)
			}
			if limit == 1 && (res.Status != loop.StatusMaxTurns || res.Output != "partial" || res.Reason == "") {
				t.Fatalf("limited result = %+v", res)
			}
			if limit == 2 && (res.Status != loop.StatusCompleted || res.Output != "finished") {
				t.Fatalf("continued result = %+v", res)
			}
		})
	}
}

func TestRuntimeModelFailureAndCancellation(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("model unavailable")
	for _, tc := range []struct {
		name string
		step modeltest.Step
		err  error
	}{
		{name: "provider error", step: modeltest.Fail(wantErr), err: wantErr},
		{name: "nil response", step: modeltest.Respond(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cp := loop.NewMemoryCheckpointer()
			r := newRuntime(t, loop.Config{Model: modeltest.New(tc.step), Checkpointer: cp})
			res, err := loop.Collect(r.Run(t.Context(), &loop.Request{RunID: "failed", Input: model.UserText("question")}), nil)
			if err == nil || res.Status != loop.StatusFailed || !errors.Is(res.Err, err) || (tc.err != nil && !errors.Is(err, tc.err)) {
				t.Fatalf("failure = %+v, %v", res, err)
			}
			st, loadErr := cp.Load(t.Context(), "failed")
			if loadErr != nil || st.Status != loop.StatusFailed || st.Reason != err.Error() {
				t.Fatalf("failed checkpoint = %+v, %v", st, loadErr)
			}
		})
	}
	t.Run("cancelled context", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		m := modeltest.New(modeltest.Reply("unexpected"))
		r := newRuntime(t, loop.Config{Model: m})
		res, err := loop.Collect(r.Run(ctx, &loop.Request{Input: model.UserText("question")}), nil)
		if !errors.Is(err, context.Canceled) || res.Status != loop.StatusFailed || len(m.Requests()) != 0 {
			t.Fatalf("cancelled Run = %+v, %v; calls = %d", res, err, len(m.Requests()))
		}
	})
}

func TestRuntimeStreamReconstructionAndErrors(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("stream failure")
	for _, tc := range []struct {
		name    string
		events  []*model.StreamEvent
		err     error
		wantErr string
	}{
		{name: "reconstruct", events: []*model.StreamEvent{nil, {Type: model.StreamDelta, Delta: &model.Part{Type: model.PartText, Text: "hello"}}, {Type: model.StreamDone, Response: &model.Response{}}}},
		{name: "missing done", events: []*model.StreamEvent{{Type: model.StreamDelta, Delta: &model.Part{Type: model.PartText, Text: "hello"}}}, wantErr: "without a done event"},
		{name: "provider error", err: wantErr, wantErr: "stream failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := streamModel{events: func(yield func(*model.StreamEvent, error) bool) {
				for _, ev := range tc.events {
					if !yield(ev, nil) {
						return
					}
				}
				if tc.err != nil {
					yield(nil, tc.err)
				}
			}}
			r := newRuntime(t, loop.Config{Model: m, Stream: true})
			res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("question")}), nil)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || res.Status != loop.StatusFailed {
					t.Fatalf("Run = %+v, %v; want %q", res, err, tc.wantErr)
				}
			} else if err != nil || res.Output != "hello" || res.Status != loop.StatusCompleted {
				t.Fatalf("Run = %+v, %v", res, err)
			}
		})
	}
}

func TestRuntimeCompaction(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		deny bool
		fail bool
	}{
		{name: "compacts old history"},
		{name: "hook denies compaction", deny: true},
		{name: "strategy failure is nonfatal", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hooks := hook.NewManager()
			if tc.deny {
				if err := hooks.OnFunc(hook.PreCompact, "auto", func(context.Context, *hook.Event) (*hook.Result, error) {
					return &hook.Result{Decision: hook.Deny}, nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			strategyCalls := 0
			manager := actx.NewManager(actx.Config{Window: actx.Window{Limit: 100, Threshold: 0.5}, Strategy: actx.StrategyFunc(func(_ context.Context, in *actx.CompactInput) (*actx.CompactOutput, error) {
				strategyCalls++
				if tc.fail {
					return nil, errors.New("cannot compact")
				}
				return &actx.CompactOutput{Messages: in.Messages[len(in.Messages)-1:], Summary: "compacted"}, nil
			})})
			m := modeltest.New(modeltest.Reply("answer"))
			r := newRuntime(t, loop.Config{Model: m, Middlewares: []loop.Middleware{hook.Middleware(hooks), actx.Middleware(manager, actx.WithCompactGuard(hook.PreCompactGuard(hooks)))}})
			compactions := 0
			history := []*model.Message{model.UserText(strings.Repeat("old ", 100)), model.AssistantText("old answer")}
			res, err := loop.Collect(r.Run(t.Context(), &loop.Request{History: history, Input: model.UserText("new question")}), func(ev *loop.Event) {
				if ev.Type == loop.EventCompacted {
					compactions++
					if ev.Compaction.Before <= ev.Compaction.After || ev.Compaction.Summary != "compacted" {
						t.Errorf("compaction event = %+v", ev.Compaction)
					}
				}
			})
			if err != nil || res.Output != "answer" || len(res.NewMessages) != 2 {
				t.Fatalf("Run = %+v, %v", res, err)
			}
			switch {
			case tc.deny:
				if strategyCalls != 0 || compactions != 0 || len(m.Requests()[0].Messages) != 3 {
					t.Fatalf("denied compaction: calls=%d events=%d", strategyCalls, compactions)
				}
			case tc.fail:
				if strategyCalls != 1 || compactions != 0 || len(m.Requests()[0].Messages) != 3 {
					t.Fatalf("failed compaction: calls=%d events=%d", strategyCalls, compactions)
				}
			default:
				if strategyCalls != 1 || compactions != 1 || len(m.Requests()[0].Messages) != 1 || len(res.Messages) != 2 {
					t.Fatalf("successful compaction: calls=%d events=%d result=%+v", strategyCalls, compactions, res)
				}
			}
			if history[0].Text() != strings.Repeat("old ", 100) || len(history) != 2 {
				t.Fatal("compaction mutated caller history")
			}
		})
	}
}
