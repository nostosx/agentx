package guard_test

import (
	"context"
	"testing"

	"github.com/nostosx/agentx/guard"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
)

func TestMiddlewareOnRuntime(t *testing.T) {
	t.Parallel()
	cfg := guard.Config{Input: []guard.InputGuardrail{{Check: func(_ context.Context, _ *loop.Run, in *model.Message) (guard.Result, error) {
		return guard.Result{Tripwire: in.Text() == "bad"}, nil
	}}}}
	if cfg.Empty() || !(guard.Config{}).Empty() {
		t.Fatal("Empty")
	}
	first, last := guard.Middleware(cfg)
	rt, err := loop.New(loop.Config{Model: modeltest.New(modeltest.Reply("ok")), Middlewares: []loop.Middleware{first, last}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := loop.Collect(rt.Run(t.Context(), &loop.Request{Input: model.UserText("bad")}), nil)
	if err != nil || res.Status != loop.StatusBlocked || res.Guardrail.Name != guard.KindInput || res.Guardrail.Kind != guard.KindInput {
		t.Fatalf("tripped run = %+v, %v", res, err)
	}
	// A run marked as handed off skips input guardrails.
	res, err = loop.Collect(rt.Run(t.Context(), &loop.Request{Input: model.UserText("bad"), Values: map[string]string{guard.SkipInputKey: "true"}}), nil)
	if err != nil || res.Output != "ok" {
		t.Fatalf("skipped run = %+v, %v", res, err)
	}
}
