package loop_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/tool"
)

func TestToolPlanApproval(t *testing.T) {
	t.Parallel()
	write := tool.Func("write", "write", func(context.Context, struct{}) (string, error) { return "ok", nil }, tool.RequireApproval())
	read := tool.Func("read", "read", func(context.Context, struct{}) (string, error) { return "ok", nil })
	var seen []string
	observer := &loop.Funcs{MiddlewareName: "observer", AfterToolFunc: func(_ context.Context, _ *loop.Run, plan *loop.ToolPlan, _ *model.ToolResult) error {
		a, ok := plan.Approval()
		b, _ := json.Marshal(a)
		seen = append(seen, plan.Call.Name+":"+map[bool]string{true: string(b), false: "none"}[ok])
		return nil
	}}
	steps := func() *modeltest.Model {
		return modeltest.New(modeltest.CallTools(modeltest.Call{ID: "w", Name: "write"}, modeltest.Call{ID: "r", Name: "read"}), modeltest.Reply("done"))
	}
	approval := loop.Approval{Approved: true, UpdatedInput: json.RawMessage(`{}`), Remember: loop.RememberSession, RememberRules: []string{"write"}}
	want := `{"approved":true,"remember":"session","remember_rules":["write"]}`

	// Decided by the Approver.
	r := newRuntime(t, loop.Config{Model: steps(), Tools: toolRegistry(t, write, read), Middlewares: []loop.Middleware{observer},
		Approver: loop.ApproverFunc(func(context.Context, *loop.ApprovalRequest) (loop.Approval, error) { return approval, nil })})
	if _, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("go")}), nil); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != "write:"+want || seen[1] != "read:none" {
		t.Fatalf("approver: seen %q", seen)
	}

	// Decided on resume, from the interrupt's state.
	seen = nil
	r = newRuntime(t, loop.Config{Model: steps(), Tools: toolRegistry(t, write, read), Middlewares: []loop.Middleware{observer}})
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("go")}), nil)
	if err != nil || res.Status != loop.StatusInterrupted {
		t.Fatalf("run = %+v, %v", res, err)
	}
	if _, err := loop.Collect(r.ResumeState(t.Context(), res.Interrupt.State, map[string]loop.Approval{"w": approval}), nil); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != "write:"+want {
		t.Fatalf("resume: seen %q", seen)
	}
}

func TestRequestValues(t *testing.T) {
	t.Parallel()
	var got string
	mw := &loop.Funcs{MiddlewareName: "values", BeforeRunFunc: func(_ context.Context, run *loop.Run, _ *loop.RunInput) error {
		got, _ = run.Value("k")
		run.SetValue(loop.SessionValuePrefix+"x", "1")
		run.SetValue("gone", "")
		return nil
	}}
	r := newRuntime(t, loop.Config{Model: modeltest.New(modeltest.Reply("done")), Middlewares: []loop.Middleware{mw}})
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("go"), Values: map[string]string{"k": "v", "empty": ""}}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "v" || len(res.Values) != 2 || res.Values["k"] != "v" || res.Values["session.x"] != "1" {
		t.Fatalf("value %q, result values %v", got, res.Values)
	}
}
