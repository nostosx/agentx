package loop_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/tool"
)

// countingEcho returns an echo tool and the number of times it ran.
func countingEcho(opts ...tool.Option) (tool.Tool, *atomic.Int32) {
	var n atomic.Int32
	return tool.Func("echo", "echo", func(_ context.Context, in echoArgs) (string, error) {
		n.Add(1)
		return in.Text, nil
	}, opts...), &n
}

func echoCall(id string) modeltest.Step {
	return modeltest.CallTools(modeltest.Call{ID: id, Name: "echo", Args: echoArgs{Text: id}})
}

func TestUsageCountsRequestsAndToolCalls(t *testing.T) {
	t.Parallel()
	echo, _ := countingEcho()
	deny := &loop.Funcs{MiddlewareName: "deny", BeforeToolFunc: func(_ context.Context, _ *loop.Run, p *loop.ToolPlan) error {
		if p.Call.ID == "b" {
			p.Deny("no")
		}
		return nil
	}}
	m := modeltest.New(
		modeltest.CallTools(modeltest.Call{ID: "a", Name: "echo", Args: echoArgs{Text: "a"}}, modeltest.Call{ID: "b", Name: "echo", Args: echoArgs{Text: "b"}}),
		modeltest.Reply("done"),
	)
	r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, echo), Middlewares: []loop.Middleware{deny}})
	var messageUsage []model.Usage
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), func(ev *loop.Event) {
		if ev.Type == loop.EventMessage {
			messageUsage = append(messageUsage, ev.Usage)
		}
	})
	// The denied call does not count.
	if err != nil || res.Usage.Requests != 2 || res.Usage.ToolCalls != 1 {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if len(messageUsage) != 2 || messageUsage[0].Requests != 1 {
		t.Fatalf("message usage = %+v", messageUsage)
	}
}

func TestRequestLimit(t *testing.T) {
	t.Parallel()
	cp := loop.NewMemoryCheckpointer()
	echo, ran := countingEcho()
	m := modeltest.New(echoCall("a"), echoCall("b"), echoCall("c"))
	r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, echo), Checkpointer: cp, Limits: loop.Limits{Requests: 2}})
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{RunID: "lim", Input: model.UserText("q")}), nil)
	if err != nil || res.Status != loop.StatusLimitExceeded || res.Reason != "reached the limit of 2 requests" || len(m.Requests()) != 2 || ran.Load() != 2 {
		t.Fatalf("Run = %+v, %v; model calls = %d, tool runs = %d", res, err, len(m.Requests()), ran.Load())
	}
	requireAnswered(t, res.Messages)
	if res.Status.Resumable() || !res.Status.Done() {
		t.Fatal("limit_exceeded is resumable or not final")
	}
	if _, err := loop.Collect(r.Resume(t.Context(), "lim", nil), nil); !errors.Is(err, loop.ErrCheckpointNotFound) {
		t.Fatalf("Resume = %v", err)
	}
}

func TestToolCallLimit(t *testing.T) {
	t.Parallel()
	echo, ran := countingEcho()
	m := modeltest.New(
		echoCall("a"),
		modeltest.CallTools(modeltest.Call{ID: "b", Name: "echo", Args: echoArgs{Text: "b"}}, modeltest.Call{ID: "c", Name: "echo", Args: echoArgs{Text: "c"}}),
	)
	r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, echo), Limits: loop.Limits{ToolCalls: 2}})
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil)
	// The second batch would exceed the limit, so none of it runs.
	if err != nil || res.Status != loop.StatusLimitExceeded || ran.Load() != 1 || res.Usage.ToolCalls != 1 || !strings.Contains(res.Reason, "limit of 2 tool calls") {
		t.Fatalf("Run = %+v, %v; tool runs = %d", res, err, ran.Load())
	}
	requireAnswered(t, res.Messages)
	results := res.Messages[len(res.Messages)-1].ToolResults()
	if len(results) != 2 || !results[0].IsError || !strings.Contains(results[0].Content, "usage limit") {
		t.Fatalf("results = %+v", results)
	}
}

func TestTokenLimits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		limits loop.Limits
		reason string
	}{
		{"input", loop.Limits{InputTokens: 15}, "reached the limit of 15 input tokens (20 used)"},
		{"output", loop.Limits{OutputTokens: 10}, "reached the limit of 10 output tokens (10 used)"},
		{"total", loop.Limits{TotalTokens: 30}, "reached the limit of 30 total tokens (30 used)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			echo, ran := countingEcho()
			m := modeltest.New(echoCall("a"), echoCall("b"), echoCall("c"))
			r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, echo), Limits: tc.limits})
			res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil)
			// The second response spends the tokens, so its calls do not run.
			if err != nil || res.Status != loop.StatusLimitExceeded || res.Reason != tc.reason || ran.Load() != 1 || len(m.Requests()) != 2 {
				t.Fatalf("Run = %+v, %v; tool runs = %d", res, err, ran.Load())
			}
			requireAnswered(t, res.Messages)
		})
	}
	// A final answer that crosses a limit is kept.
	m := modeltest.New(modeltest.Reply("answer"))
	r := newRuntime(t, loop.Config{Model: m, Limits: loop.Limits{TotalTokens: 1}})
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil)
	if err != nil || res.Status != loop.StatusCompleted || res.Output != "answer" {
		t.Fatalf("Run = %+v, %v", res, err)
	}
}

func TestBudget(t *testing.T) {
	t.Parallel()
	echo, ran := countingEcho()
	var seen []model.Usage
	budget := func(u model.Usage) error {
		seen = append(seen, u)
		if u.OutputTokens >= 5 {
			return errors.New("over $0.01")
		}
		return nil
	}
	m := modeltest.New(echoCall("a"))
	r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, echo), Budget: budget})
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil)
	if err != nil || res.Status != loop.StatusLimitExceeded || res.Reason != "budget: over $0.01" || ran.Load() != 0 {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	// Checked before the model call and before the tool batch.
	if len(seen) != 2 || seen[0].Requests != 0 || seen[1].Requests != 1 {
		t.Fatalf("budget saw %+v", seen)
	}
	requireAnswered(t, res.Messages)
}

func TestRequestOverridesLimits(t *testing.T) {
	t.Parallel()
	echo, _ := countingEcho(tool.RequireApproval())
	m := modeltest.New(echoCall("a"), echoCall("b"), echoCall("c"))
	r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, echo), Limits: loop.Limits{Requests: 1}})
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q"), Limits: &loop.Limits{Requests: 5}, MaxTurns: 2}), nil)
	if err != nil || res.Status != loop.StatusInterrupted {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if st := res.Interrupt.State; st.MaxTurns != 2 || st.Limits == nil || st.Limits.Requests != 5 {
		t.Fatalf("state = %+v", st)
	}
	// The overrides survive the interrupt: the turn limit ends the run, not
	// the configured request limit.
	res, err = loop.Collect(r.ResumeState(t.Context(), res.Interrupt.State, map[string]loop.Approval{"a": loop.Approve}), nil)
	if err != nil || res.Status != loop.StatusInterrupted {
		t.Fatalf("Resume = %+v, %v", res, err)
	}
	res, err = loop.Collect(r.ResumeState(t.Context(), res.Interrupt.State, map[string]loop.Approval{"b": loop.Approve}), nil)
	if err != nil || res.Status != loop.StatusMaxTurns || res.Reason != "reached the limit of 2 turns" {
		t.Fatalf("Resume = %+v, %v", res, err)
	}
}

func TestReportedUsageCountsTowardLimits(t *testing.T) {
	t.Parallel()
	nested := tool.Func("nested", "runs a nested agent", func(ctx context.Context, _ struct{}) (string, error) {
		loop.ReportUsage(ctx, model.Usage{Requests: 3, ToolCalls: 4, InputTokens: 1})
		return "ok", nil
	})
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "a", Name: "nested"}), modeltest.Reply("done"))
	r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, nested), Limits: loop.Limits{Requests: 4}})
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil)
	if err != nil || res.Status != loop.StatusLimitExceeded || res.Usage.Requests != 4 || res.Usage.ToolCalls != 5 || len(m.Requests()) != 1 {
		t.Fatalf("Run = %+v, %v", res, err)
	}
}

func TestToolLimitOnResume(t *testing.T) {
	t.Parallel()
	echo, ran := countingEcho(tool.RequireApproval())
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "a", Name: "echo", Args: echoArgs{Text: "a"}}, modeltest.Call{ID: "b", Name: "echo", Args: echoArgs{Text: "b"}}))
	r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, echo)})
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q"), Limits: &loop.Limits{ToolCalls: 1}}), nil)
	if err != nil || res.Status != loop.StatusLimitExceeded || ran.Load() != 0 {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	// A denied call does not count toward the limit.
	m = modeltest.New(modeltest.CallTools(modeltest.Call{ID: "a", Name: "echo", Args: echoArgs{Text: "a"}}, modeltest.Call{ID: "b", Name: "echo", Args: echoArgs{Text: "b"}}), modeltest.Reply("done"))
	deny := &loop.Funcs{MiddlewareName: "deny", BeforeToolFunc: func(_ context.Context, _ *loop.Run, p *loop.ToolPlan) error {
		if p.Call.ID == "b" {
			p.Deny("no")
		}
		return nil
	}}
	r = newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, echo), Middlewares: []loop.Middleware{deny}})
	res, err = loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q"), Limits: &loop.Limits{ToolCalls: 1}}), nil)
	if err != nil || res.Status != loop.StatusInterrupted {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	res, err = loop.Collect(r.ResumeState(t.Context(), res.Interrupt.State, map[string]loop.Approval{"a": loop.Approve}), nil)
	if err != nil || res.Status != loop.StatusCompleted || ran.Load() != 1 || res.Usage.ToolCalls != 1 {
		t.Fatalf("Resume = %+v, %v; tool runs = %d", res, err, ran.Load())
	}
}
