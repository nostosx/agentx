package middleware_test

import (
	"context"
	"errors"
	"testing"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/middleware"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/telemetry"
	"github.com/nostosx/agentx/tool"
)

func newTestRuntime(t *testing.T, cfg loop.Config) *loop.Runtime {
	t.Helper()
	r, err := loop.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func echoRegistry(t *testing.T, fail error) *tool.Registry {
	t.Helper()
	echo := tool.Func("echo", "Echo text.", func(_ context.Context, in struct {
		Text string `json:"text"`
	}) (string, error) {
		return in.Text, fail
	})
	reg, err := tool.NewRegistry(echo)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestNoopTracer(t *testing.T) {
	t.Parallel()
	type key struct{}
	ctx := context.WithValue(t.Context(), key{}, "kept")
	got, span := telemetry.NoopTracer{}.Start(ctx, "span", telemetry.SpanKindClient, telemetry.Attr{Key: "k", Value: 1})
	if got != ctx || span == nil {
		t.Fatalf("Start = %v, %v", got, span)
	}
	span.SetAttributes(telemetry.Attr{Key: "k", Value: 2})
	span.RecordError(errors.New("ignored"))
	span.End()

	// A nil tracer falls back to NoopTracer in a whole run, including
	// failed tool calls and the run's end, without changing the outcome.
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c", Name: "echo", Args: map[string]string{"text": "hi"}}), modeltest.Reply("done"))
	r := newTestRuntime(t, loop.Config{Model: m, Tools: echoRegistry(t, errors.New("broken")), Middlewares: []loop.Middleware{middleware.Tracing(nil, middleware.RecordContent())}})
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil)
	if err != nil || res.Output != "done" || !m.Requests()[1].Messages[2].ToolResults()[0].IsError {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	failing := newTestRuntime(t, loop.Config{Model: modeltest.New(modeltest.Fail(&model.APIError{StatusCode: 500})), Middlewares: []loop.Middleware{middleware.Tracing(telemetry.NoopTracer{})}})
	if res, err := loop.Collect(failing.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil); err == nil || res.Status != loop.StatusFailed {
		t.Fatalf("failed Run = %+v, %v", res, err)
	}
}
