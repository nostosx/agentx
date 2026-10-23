package loop_test

import (
	"context"
	"strings"
	"testing"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/tool"
)

type decision struct {
	d      loop.Decision
	reason string
}

func observe(p *loop.ToolPlan) decision {
	d, reason := p.Decision()
	return decision{d, reason}
}

func TestToolPlanDecision(t *testing.T) {
	t.Parallel()
	var seen []decision
	record := func(p *loop.ToolPlan) { seen = append(seen, observe(p)) }
	executed := false
	echo := tool.Func("echo", "echo", func(context.Context, struct{}) (string, error) { executed = true; return "ran", nil })
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "call", Name: "echo"}), modeltest.Reply("done"))
	r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, echo), Middlewares: []loop.Middleware{
		&loop.Funcs{MiddlewareName: "allow", BeforeToolFunc: func(_ context.Context, _ *loop.Run, p *loop.ToolPlan) error {
			record(p)
			p.Allow()
			record(p)
			return nil
		}},
		&loop.Funcs{MiddlewareName: "ask", BeforeToolFunc: func(_ context.Context, _ *loop.Run, p *loop.ToolPlan) error {
			p.Ask("check first")
			record(p)
			// A less restrictive decision does not override a stricter one.
			p.Allow()
			record(p)
			return nil
		}},
		&loop.Funcs{MiddlewareName: "deny", BeforeToolFunc: func(_ context.Context, _ *loop.Run, p *loop.ToolPlan) error {
			p.Deny("")
			record(p)
			// The same decision fills in a missing reason but keeps one.
			p.Deny("policy")
			record(p)
			p.Deny("other")
			p.Ask("ignored")
			record(p)
			return nil
		}},
		&loop.Funcs{MiddlewareName: "after", BeforeToolFunc: func(context.Context, *loop.Run, *loop.ToolPlan) error {
			t.Error("BeforeTool ran after a denial")
			return nil
		}},
	}})
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil)
	if err != nil || res.Status != loop.StatusCompleted || executed {
		t.Fatalf("Run = %+v, %v; executed = %v", res, err, executed)
	}
	want := []decision{
		{loop.NoDecision, ""}, {loop.Allowed, ""},
		{loop.Asked, "check first"}, {loop.Asked, "check first"},
		{loop.Denied, ""}, {loop.Denied, "policy"}, {loop.Denied, "policy"},
	}
	if len(seen) != len(want) {
		t.Fatalf("decisions = %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("decision %d = %v, want %v", i, seen[i], want[i])
		}
	}
	result := m.Requests()[1].Messages[2].ToolResults()[0]
	if !result.IsError || !strings.Contains(result.Content, "policy") {
		t.Fatalf("denied tool result = %+v", result)
	}
}

func TestToolPlanAllowSkipsApproval(t *testing.T) {
	t.Parallel()
	executed := false
	echo := tool.Func("echo", "echo", func(context.Context, struct{}) (string, error) { executed = true; return "ran", nil }, tool.RequireApproval())
	var got decision
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "call", Name: "echo"}), modeltest.Reply("done"))
	r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, echo), Middlewares: []loop.Middleware{
		&loop.Funcs{MiddlewareName: "allow", BeforeToolFunc: func(_ context.Context, _ *loop.Run, p *loop.ToolPlan) error {
			p.Allow()
			got = observe(p)
			return nil
		}},
	}})
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil)
	if err != nil || res.Status != loop.StatusCompleted || !executed || got != (decision{loop.Allowed, ""}) {
		t.Fatalf("Run = %+v, %v; executed = %v, decision = %v", res, err, executed, got)
	}
}

func TestStopErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		err    error
		text   string
		status loop.Status
	}{
		{"block", loop.Block("bad input"), "run blocked: bad input", loop.StatusBlocked},
		{"stop", loop.Stop("enough"), "run stopped: enough", loop.StatusStopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.err.Error(); got != tc.text {
				t.Fatalf("Error() = %q, want %q", got, tc.text)
			}
			m := modeltest.New(modeltest.Reply("unexpected"))
			r := newRuntime(t, loop.Config{Model: m, Middlewares: []loop.Middleware{
				&loop.Funcs{MiddlewareName: "gate", BeforeRunFunc: func(context.Context, *loop.Run, *loop.RunInput) error { return tc.err }},
			}})
			res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil)
			// The stop error ends the run with its status and reason; it is
			// not reported as a failure.
			if err != nil || res.Err != nil || res.Status != tc.status || !strings.Contains(tc.text, res.Reason) || res.Reason == "" || len(m.Requests()) != 0 {
				t.Fatalf("Run = %+v, %v", res, err)
			}
		})
	}
}
