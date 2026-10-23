package loop_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/tool"
)

type tenant struct{ ID string }

func TestDepsByType(t *testing.T) {
	t.Parallel()
	ctx := loop.WithDeps(t.Context(), tenant{ID: "a"})
	ctx = loop.WithDeps(ctx, 42)
	if v, ok := loop.Deps[tenant](ctx); !ok || v.ID != "a" {
		t.Fatalf("Deps[tenant] = %+v, %v", v, ok)
	}
	if v, ok := loop.Deps[int](ctx); !ok || v != 42 {
		t.Fatalf("Deps[int] = %v, %v", v, ok)
	}
	if _, ok := loop.Deps[*tenant](ctx); ok {
		t.Fatal("Deps of an unset type found a value")
	}
	inner := loop.WithDeps(ctx, tenant{ID: "b"})
	if v, _ := loop.Deps[tenant](inner); v.ID != "b" {
		t.Fatalf("overridden dependency = %+v", v)
	}
	if v, _ := loop.Deps[tenant](ctx); v.ID != "a" {
		t.Fatalf("outer dependency changed to %+v", v)
	}

	// Tools and middleware of a run see the dependencies, which are not
	// saved with the checkpoint.
	var fromTool, fromMW string
	lookup := tool.Func("lookup", "", func(ctx context.Context, _ struct{}) (string, error) {
		v, _ := loop.Deps[tenant](ctx)
		fromTool = v.ID
		return "ok", nil
	})
	mw := &loop.Funcs{MiddlewareName: "deps", BeforeModelFunc: func(ctx context.Context, _ *loop.Run, _ *model.Request) error {
		v, _ := loop.Deps[tenant](ctx)
		fromMW = v.ID
		return nil
	}}
	cp := loop.NewMemoryCheckpointer()
	rt, err := loop.New(loop.Config{
		Model:        modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c", Name: "lookup"}), modeltest.Reply("done")),
		Tools:        registry(t, lookup),
		Middlewares:  []loop.Middleware{mw},
		Checkpointer: cp,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := loop.Collect(rt.Run(loop.WithDeps(t.Context(), tenant{ID: "t1"}), &loop.Request{Input: model.UserText("q")}), nil)
	if err != nil || res.Output != "done" || fromTool != "t1" || fromMW != "t1" {
		t.Fatalf("run = %+v, %v; tool saw %q, middleware %q", res, err, fromTool, fromMW)
	}
}

func registry(t *testing.T, tools ...tool.Tool) *tool.Registry {
	t.Helper()
	reg := &tool.Registry{}
	if err := reg.Register(tools...); err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestTypedRunValuesSurviveCheckpoints(t *testing.T) {
	t.Parallel()
	type progress struct {
		Done  []string `json:"done"`
		Count int      `json:"count"`
	}
	cp := loop.NewMemoryCheckpointer()
	var restored progress
	var getErr error
	mw := &loop.Funcs{
		MiddlewareName: "progress",
		BeforeRunFunc: func(_ context.Context, run *loop.Run, _ *loop.RunInput) error {
			if _, ok, _ := run.DecodeValue[progress]("progress"); ok {
				t.Error("value before it was set")
			}
			if err := run.EncodeValue("bad", func() {}); err == nil {
				t.Error("EncodeValue encoded a function")
			}
			run.SetValue("text", "not json")
			_, _, getErr = run.DecodeValue[progress]("text")
			run.SetValue("text", "")
			return run.EncodeValue("progress", progress{Done: []string{"a"}, Count: 1})
		},
		AfterToolFunc: func(_ context.Context, run *loop.Run, _ *loop.ToolPlan, _ *model.ToolResult) error {
			p, ok, err := run.DecodeValue[progress]("progress")
			if !ok || err != nil {
				t.Errorf("DecodeValue = %v, %v", ok, err)
			}
			restored = p
			if err := run.EncodeValue("gone", nil); err != nil {
				t.Error(err)
			}
			return nil
		},
	}
	write := tool.Func("write", "", func(context.Context, struct{}) (string, error) { return "w", nil }, tool.RequireApproval())
	rt, err := loop.New(loop.Config{
		Model:        modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c", Name: "write"}), modeltest.Reply("done")),
		Tools:        registry(t, write),
		Middlewares:  []loop.Middleware{mw},
		Checkpointer: cp,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := loop.Collect(rt.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil)
	if err != nil || res.Status != loop.StatusInterrupted || res.Values["progress"] != `{"done":["a"],"count":1}` {
		t.Fatalf("run = %+v, %v", res, err)
	}
	if getErr == nil || !strings.Contains(getErr.Error(), "run value text") {
		t.Fatalf("DecodeValue of invalid JSON = %v", getErr)
	}
	res, err = loop.Collect(rt.Resume(t.Context(), res.RunID, map[string]loop.Approval{"c": loop.Approve}), nil)
	if err != nil || res.Output != "done" || restored.Count != 1 || restored.Done[0] != "a" {
		t.Fatalf("resume = %+v, %v; restored %+v", res, err, restored)
	}
}

func TestGuardrailErrorBlocksRun(t *testing.T) {
	t.Parallel()
	trip := &loop.GuardrailError{Guardrail: loop.Guardrail{Name: "pii", Kind: "output", Message: "contains PII", Info: map[string]any{"field": "ssn"}}}
	mw := &loop.Funcs{MiddlewareName: "guard", BeforeFinishFunc: func(context.Context, *loop.Run, *model.Message) (bool, error) {
		return false, trip
	}}
	rt, err := loop.New(loop.Config{Name: "a", Model: modeltest.New(modeltest.Reply("my ssn is 123")), Middlewares: []loop.Middleware{mw}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := loop.Collect(rt.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil)
	if err != nil || res.Status != loop.StatusBlocked || res.Output != "" || res.Guardrail == nil ||
		res.Guardrail.Name != "pii" || res.Agent != "a" || !strings.Contains(res.Reason, "contains PII") || res.Usage.Requests != 1 {
		t.Fatalf("run = %+v, %v", res, err)
	}

	// A model wrapper that trips reports the call's usage.
	wrap := &loop.Funcs{MiddlewareName: "guard", WrapModelFunc: func(_ *loop.Run, next model.Model) model.Model {
		return model.Func(func(ctx context.Context, req *model.Request) (*model.Response, error) {
			resp, err := next.Generate(ctx, req)
			if err != nil {
				return nil, err
			}
			return nil, &loop.GuardrailError{Guardrail: loop.Guardrail{Name: "late"}, Usage: resp.Usage}
		})
	}}
	rt, err = loop.New(loop.Config{Model: modeltest.New(modeltest.Reply("x")), Middlewares: []loop.Middleware{wrap}})
	if err != nil {
		t.Fatal(err)
	}
	res, err = loop.Collect(rt.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil)
	if err != nil || res.Status != loop.StatusBlocked || res.Guardrail.Name != "late" || res.Usage.OutputTokens != 5 {
		t.Fatalf("model trip = %+v, %v", res, err)
	}
	if !errors.As(error(trip), new(*loop.GuardrailError)) || !strings.Contains(trip.Error(), "pii") {
		t.Fatal(trip.Error())
	}
}

func TestRequestCarriesEarlierUsage(t *testing.T) {
	t.Parallel()
	rt, err := loop.New(loop.Config{Model: modeltest.New(modeltest.Reply("x")), MaxTurns: 3})
	if err != nil {
		t.Fatal(err)
	}
	res, err := loop.Collect(rt.Run(t.Context(), &loop.Request{
		Input: model.UserText("q"), Usage: model.Usage{InputTokens: 100, Requests: 2}, Turns: 2,
	}), nil)
	if err != nil || res.Turns != 3 || res.Usage.InputTokens != 110 || res.Usage.Requests != 3 {
		t.Fatalf("run = %+v, %v", res, err)
	}
	// The earlier turns count toward the turn limit.
	res, err = loop.Collect(rt.Run(t.Context(), &loop.Request{Input: model.UserText("q"), Turns: 3}), nil)
	if err != nil || res.Status != loop.StatusMaxTurns {
		t.Fatalf("run past the limit = %+v, %v", res, err)
	}
}
