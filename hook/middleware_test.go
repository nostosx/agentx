package hook_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/nostosx/agentx/actx"
	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/tool"
)

func TestMiddlewareNotifiesApprovalInterrupts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		approval bool // the tool requires approval, interrupting the run
		notify   bool // a Notification hook is registered
		status   loop.Status
		want     int
	}{
		{"interrupted", true, true, loop.StatusInterrupted, 1},
		{"interrupted without notification hook", true, false, loop.StatusInterrupted, 0},
		{"completed", false, true, loop.StatusCompleted, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var events []*hook.Event
			m := hook.NewManager()
			// Another event's hook makes the manager non-empty, so only the
			// Notification check decides.
			on(t, m, hook.PostModel, "", func(context.Context, *hook.Event) (*hook.Result, error) { return nil, nil })
			if tc.notify {
				on(t, m, hook.Notification, "", func(_ context.Context, ev *hook.Event) (*hook.Result, error) {
					mu.Lock()
					defer mu.Unlock()
					events = append(events, ev)
					return nil, nil
				})
			}
			var opts []tool.Option
			if tc.approval {
				opts = append(opts, tool.RequireApproval())
			}
			reg, err := tool.NewRegistry(tool.Func("deploy", "Deploy.", func(context.Context, struct{}) (string, error) { return "ok", nil }, opts...))
			if err != nil {
				t.Fatal(err)
			}
			r, err := loop.New(loop.Config{
				Name:        "assistant",
				Model:       modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c", Name: "deploy"}), modeltest.Reply("done")),
				Tools:       reg,
				Middlewares: []loop.Middleware{hook.Middleware(m)},
			})
			if err != nil {
				t.Fatal(err)
			}
			res, err := loop.Collect(r.Run(t.Context(), &loop.Request{RunID: "run-1", SessionID: "session-1", Input: model.UserText("deploy")}), nil)
			if err != nil || res.Status != tc.status {
				t.Fatalf("Run = %+v, %v", res, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(events) != tc.want {
				t.Fatalf("notifications = %+v, want %d", events, tc.want)
			}
			if tc.want == 0 {
				return
			}
			ev := events[0]
			if ev.Type != hook.Notification || ev.Message != "Approval required for deploy" || ev.Reason != "approval" || ev.Title != "Approval required" ||
				ev.RunID != "run-1" || ev.SessionID != "session-1" || ev.Agent != "assistant" || ev.Turn != 1 {
				t.Fatalf("notification = %+v", ev)
			}
		})
	}
}

func newHookRuntime(t *testing.T, m *hook.Manager, mdl *modeltest.Model, tools ...tool.Tool) *loop.Runtime {
	t.Helper()
	reg, err := tool.NewRegistry(tools...)
	if err != nil {
		t.Fatal(err)
	}
	r, err := loop.New(loop.Config{Name: "assistant", Model: mdl, Tools: reg, Middlewares: []loop.Middleware{hook.Middleware(m)}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestMiddlewarePostToolUseFailure(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	fired := map[string]hook.EventType{}
	record := func(_ context.Context, ev *hook.Event) (*hook.Result, error) {
		mu.Lock()
		defer mu.Unlock()
		if _, dup := fired[ev.ToolName()]; dup {
			t.Errorf("second post event for %s", ev.ToolName())
		}
		fired[ev.ToolName()] = ev.Type
		out := "rewritten"
		return &hook.Result{AdditionalContext: string(ev.Type) + " context", UpdatedOutput: &out}, nil
	}
	m := hook.NewManager()
	on(t, m, hook.PostToolUse, "", record)
	on(t, m, hook.PostToolUseFailure, "", record)
	on(t, m, hook.PreToolUse, "blocked", func(context.Context, *hook.Event) (*hook.Result, error) {
		return &hook.Result{Decision: hook.Deny, Reason: "not allowed"}, nil
	})
	ok := tool.Func("ok", "Succeed.", func(context.Context, struct{}) (string, error) { return "fine", nil })
	fail := tool.Func("fail", "Fail.", func(context.Context, struct{}) (string, error) { return "", errors.New("exploded") })
	blocked := tool.Func("blocked", "Never runs.", func(context.Context, struct{}) (string, error) { return "ran", nil })
	mdl := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "1", Name: "ok"}, modeltest.Call{ID: "2", Name: "fail"}, modeltest.Call{ID: "3", Name: "blocked"}), modeltest.Reply("done"))
	res, err := loop.Collect(newHookRuntime(t, m, mdl, ok, fail, blocked).Run(t.Context(), &loop.Request{Input: model.UserText("go")}), nil)
	if err != nil || res.Status != loop.StatusCompleted {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if fired["ok"] != hook.PostToolUse || fired["fail"] != hook.PostToolUseFailure || len(fired) != 2 {
		t.Fatalf("fired = %v", fired)
	}
	results := map[string]string{}
	for _, r := range mdl.Requests()[1].Messages[2].ToolResults() {
		results[r.Name] = r.Content
	}
	// PostToolUse replaces the output; PostToolUseFailure only adds context.
	if !strings.HasPrefix(results["ok"], "rewritten") || !strings.Contains(results["ok"], "PostToolUse context") {
		t.Errorf("ok result = %q", results["ok"])
	}
	if !strings.Contains(results["fail"], "exploded") || !strings.Contains(results["fail"], "PostToolUseFailure context") {
		t.Errorf("fail result = %q", results["fail"])
	}
	if strings.Contains(results["blocked"], "context") {
		t.Errorf("blocked result = %q", results["blocked"])
	}
}

func TestMiddlewareStopContinuationCap(t *testing.T) {
	t.Parallel()
	var active []bool
	m := hook.NewManager()
	on(t, m, hook.Stop, "", func(_ context.Context, ev *hook.Event) (*hook.Result, error) {
		active = append(active, ev.StopHookActive)
		return &hook.Result{Decision: hook.Deny, Reason: "keep going"}, nil
	})
	steps := make([]modeltest.Step, 12)
	for i := range steps {
		steps[i] = modeltest.Reply("answer")
	}
	mdl := modeltest.New(steps...)
	res, err := loop.Collect(newHookRuntime(t, m, mdl).Run(t.Context(), &loop.Request{Input: model.UserText("go")}), nil)
	// The first answer and 8 continuations; the ninth block is overridden.
	if err != nil || res.Status != loop.StatusCompleted || len(mdl.Requests()) != 9 || len(active) != 9 {
		t.Fatalf("Run = %+v, %v; model calls = %d, stops = %d", res, err, len(mdl.Requests()), len(active))
	}
	if active[0] || !active[1] || !active[8] {
		t.Fatalf("StopHookActive = %v", active)
	}
}

func TestMiddlewareStopCapResetsAfterToolCalls(t *testing.T) {
	t.Parallel()
	stops := 0
	m := hook.NewManager()
	on(t, m, hook.Stop, "", func(_ context.Context, ev *hook.Event) (*hook.Result, error) {
		stops++
		// The run is still continuing because of a Stop hook after the
		// tool call, though the count was reset.
		if stops == 9 && !ev.StopHookActive {
			t.Error("StopHookActive lost after a turn with tool calls")
		}
		return &hook.Result{Decision: hook.Deny, Reason: "keep going"}, nil
	})
	var steps []modeltest.Step
	for range 8 {
		steps = append(steps, modeltest.Reply("answer"))
	}
	// After 7 continuations a tool call resets the count, so 8 more follow.
	steps = append(steps, modeltest.CallTools(modeltest.Call{ID: "c", Name: "ok"}))
	for range 9 {
		steps = append(steps, modeltest.Reply("answer"))
	}
	mdl := modeltest.New(steps...)
	ok := tool.Func("ok", "Succeed.", func(context.Context, struct{}) (string, error) { return "fine", nil })
	res, err := loop.Collect(newHookRuntime(t, m, mdl, ok).Run(t.Context(), &loop.Request{Input: model.UserText("go")}), nil)
	if err != nil || res.Status != loop.StatusCompleted || stops != 17 || len(mdl.Requests()) != 18 {
		t.Fatalf("Run = %+v, %v; stops = %d, model calls = %d", res, err, stops, len(mdl.Requests()))
	}
}

func TestMiddlewareStopAdditionalContextContinues(t *testing.T) {
	t.Parallel()
	m := hook.NewManager()
	on(t, m, hook.Stop, "", func(_ context.Context, ev *hook.Event) (*hook.Result, error) {
		if ev.StopHookActive {
			return nil, nil
		}
		return &hook.Result{AdditionalContext: "run the tests"}, nil
	})
	mdl := modeltest.New(modeltest.Reply("draft"), modeltest.Reply("tested"))
	res, err := loop.Collect(newHookRuntime(t, m, mdl).Run(t.Context(), &loop.Request{Input: model.UserText("go")}), nil)
	if err != nil || res.Output != "tested" || len(mdl.Requests()) != 2 {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if got := mdl.Requests()[1].Messages[2].Text(); !strings.Contains(got, "Stop hook feedback:\nrun the tests") {
		t.Fatalf("feedback = %q", got)
	}
}

func TestMiddlewareSessionStartSourceAndDefer(t *testing.T) {
	t.Parallel()
	m := hook.NewManager()
	var sources []string
	on(t, m, hook.SessionStart, "startup", func(_ context.Context, ev *hook.Event) (*hook.Result, error) {
		sources = append(sources, ev.Reason)
		return nil, nil
	})
	on(t, m, hook.SessionStart, "resume", func(context.Context, *hook.Event) (*hook.Result, error) {
		t.Error("resume hook ran for a new session")
		return nil, nil
	})
	on(t, m, hook.PreToolUse, "Bash", func(context.Context, *hook.Event) (*hook.Result, error) {
		return &hook.Result{Decision: hook.Defer, Reason: "decide later", AdditionalContext: "dropped"}, nil
	})
	calls := 0
	bash := tool.Func("bash", "Run.", func(context.Context, struct{}) (string, error) { calls++; return "ran", nil })
	mdl := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c", Name: "bash"}), modeltest.Reply("done"))
	res, err := loop.Collect(newHookRuntime(t, m, mdl, bash).Run(t.Context(), &loop.Request{Input: model.UserText("go"), NewSession: true}), nil)
	// Defer requests approval, so without an Approver the run interrupts.
	if err != nil || res.Status != loop.StatusInterrupted || calls != 0 || res.Interrupt.Requests[0].Reason != "decide later" {
		t.Fatalf("Run = %+v, %v; calls = %d", res, err, calls)
	}
	if len(sources) != 1 || sources[0] != "startup" {
		t.Fatalf("sources = %v", sources)
	}
}

func TestPostCompactObserver(t *testing.T) {
	t.Parallel()
	m := hook.NewManager()
	var got []*hook.Event
	on(t, m, hook.PostCompact, "auto", func(_ context.Context, ev *hook.Event) (*hook.Result, error) {
		got = append(got, ev)
		return &hook.Result{Halt: true, Decision: hook.Deny}, errors.New("ignored")
	})
	manager := actx.NewManager(actx.Config{Window: actx.Window{Limit: 100, Threshold: 0.5}, Strategy: actx.StrategyFunc(func(_ context.Context, in *actx.CompactInput) (*actx.CompactOutput, error) {
		return &actx.CompactOutput{Messages: in.Messages[len(in.Messages)-1:], Summary: "compacted"}, nil
	})})
	mdl := modeltest.New(modeltest.Reply("answer"))
	r, err := loop.New(loop.Config{Model: mdl, Middlewares: []loop.Middleware{hook.Middleware(m), actx.Middleware(manager, actx.WithCompactObserver(hook.PostCompactObserver(m)))}})
	if err != nil {
		t.Fatal(err)
	}
	history := []*model.Message{model.UserText(strings.Repeat("old ", 100)), model.AssistantText("old answer")}
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{History: history, Input: model.UserText("new question")}), nil)
	// The hook's result and error do not affect the run.
	if err != nil || res.Status != loop.StatusCompleted || res.Output != "answer" {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if len(got) != 1 || got[0].Reason != "auto" || got[0].Message != "compacted" || len(got[0].Messages) != 1 {
		t.Fatalf("PostCompact events = %+v", got)
	}
	// Without PostCompact hooks the observer does nothing.
	hook.PostCompactObserver(hook.NewManager())(t.Context(), nil, "auto", nil)
}

func TestMiddlewareLogsSystemMessage(t *testing.T) {
	t.Parallel()
	m := hook.NewManager()
	on(t, m, hook.UserPromptSubmit, "", func(context.Context, *hook.Event) (*hook.Result, error) {
		return &hook.Result{SystemMessage: "quota almost used"}, nil
	})
	var logs bytes.Buffer
	r, err := loop.New(loop.Config{Model: modeltest.New(modeltest.Reply("ok")), Middlewares: []loop.Middleware{hook.Middleware(m)}, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("go")}), nil); err != nil || res.Output != "ok" {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if !strings.Contains(logs.String(), "quota almost used") || !strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("logs = %s", logs.String())
	}
}
