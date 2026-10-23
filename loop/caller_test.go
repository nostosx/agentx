package loop_test

import (
	"context"
	"testing"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/tool"
)

func TestRunCaller(t *testing.T) {
	t.Parallel()
	type seen struct {
		run, caller, callerSession string
		callerTurn                 int
		outerCaller                bool
	}
	var got seen
	inner := tool.Func("inner", "inner", func(ctx context.Context, _ struct{}) (string, error) {
		run := loop.CallingRun(ctx)
		got.run = run.ID()
		if c := run.Caller(); c != nil {
			got.caller, got.callerSession, got.callerTurn = c.ID(), c.SessionID(), c.Turn()
			got.outerCaller = c.Caller() != nil
		}
		return "ok", nil
	})
	innerReg := &tool.Registry{}
	if err := innerReg.Register(inner); err != nil {
		t.Fatal(err)
	}
	innerRT := newRuntime(t, loop.Config{
		Model: modeltest.New(modeltest.CallTools(modeltest.Call{ID: "i", Name: "inner"}), modeltest.Reply("inner done")),
		Tools: innerReg,
	})
	outer := tool.Func("outer", "outer", func(ctx context.Context, _ struct{}) (string, error) {
		res, err := loop.Collect(innerRT.Run(ctx, &loop.Request{RunID: "inner-run", Input: model.UserText("go")}), nil)
		if err != nil {
			return "", err
		}
		return res.Output, nil
	})
	outerReg := &tool.Registry{}
	if err := outerReg.Register(outer); err != nil {
		t.Fatal(err)
	}
	outerRT := newRuntime(t, loop.Config{
		Model: modeltest.New(modeltest.CallTools(modeltest.Call{ID: "o", Name: "outer"}), modeltest.Reply("done")),
		Tools: outerReg,
	})
	res, err := loop.Collect(outerRT.Run(t.Context(), &loop.Request{RunID: "outer-run", SessionID: "s", Input: model.UserText("go")}), nil)
	if err != nil || res.Status != loop.StatusCompleted {
		t.Fatalf("run = %+v, %v", res, err)
	}
	if want := (seen{run: "inner-run", caller: "outer-run", callerSession: "s", callerTurn: 1}); got != want {
		t.Fatalf("seen = %+v, want %+v", got, want)
	}
}

func TestResumedRunCaller(t *testing.T) {
	t.Parallel()
	var caller string
	gated := tool.Func("gated", "gated", func(ctx context.Context, _ struct{}) (string, error) {
		if c := loop.CallingRun(ctx).Caller(); c != nil {
			caller = c.ID()
		}
		return "ok", nil
	}, tool.RequireApproval())
	innerReg := &tool.Registry{}
	if err := innerReg.Register(gated); err != nil {
		t.Fatal(err)
	}
	innerRT := newRuntime(t, loop.Config{
		Model: modeltest.New(modeltest.CallTools(modeltest.Call{ID: "g", Name: "gated"}), modeltest.Reply("inner done")),
		Tools: innerReg,
	})
	res, err := loop.Collect(innerRT.Run(t.Context(), &loop.Request{Input: model.UserText("go")}), nil)
	if err != nil || res.Status != loop.StatusInterrupted {
		t.Fatalf("inner run = %+v, %v", res, err)
	}
	st := res.Interrupt.State
	outer := tool.Func("outer", "outer", func(ctx context.Context, _ struct{}) (string, error) {
		res, err := loop.Collect(innerRT.ResumeState(ctx, st, map[string]loop.Approval{"g": loop.Approve}), nil)
		if err != nil {
			return "", err
		}
		return res.Output, nil
	})
	outerReg := &tool.Registry{}
	if err := outerReg.Register(outer); err != nil {
		t.Fatal(err)
	}
	outerRT := newRuntime(t, loop.Config{
		Model: modeltest.New(modeltest.CallTools(modeltest.Call{ID: "o", Name: "outer"}), modeltest.Reply("done")),
		Tools: outerReg,
	})
	if _, err := loop.Collect(outerRT.Run(t.Context(), &loop.Request{RunID: "outer-run", Input: model.UserText("go")}), nil); err != nil {
		t.Fatal(err)
	}
	if caller != "outer-run" {
		t.Fatalf("caller of the resumed run = %q", caller)
	}
}
