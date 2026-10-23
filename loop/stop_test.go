package loop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/tool"
)

type finalArgs struct {
	Answer string `json:"answer"`
}

func finalTool() tool.Tool {
	return tool.Func("final", "Give the final answer.", func(_ context.Context, in finalArgs) (string, error) {
		if in.Answer == "" {
			return "", errors.New("answer is empty")
		}
		return in.Answer, nil
	})
}

func TestStopAtTools(t *testing.T) {
	t.Parallel()
	cp := loop.NewMemoryCheckpointer()
	echo, ran := countingEcho()
	var ended *loop.Result
	afterRun := &loop.Funcs{MiddlewareName: "after", AfterRunFunc: func(_ context.Context, _ *loop.Run, res *loop.Result) { ended = res }}
	m := modeltest.New(
		// A failed stop tool does not end the run.
		modeltest.CallTools(modeltest.Call{ID: "a", Name: "final", Args: finalArgs{}}),
		modeltest.CallTools(
			modeltest.Call{ID: "b", Name: "echo", Args: echoArgs{Text: "b"}},
			modeltest.Call{ID: "c", Name: "final", Args: finalArgs{Answer: "42"}},
			modeltest.Call{ID: "d", Name: "final", Args: finalArgs{Answer: "43"}},
		),
	)
	r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, echo, finalTool()), Checkpointer: cp,
		StopAtTools: []string{"final"}, Middlewares: []loop.Middleware{afterRun}})
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{RunID: "stop", Input: model.UserText("q")}), nil)
	// The whole batch runs, and the first successful stop tool's result is
	// the output.
	if err != nil || res.Status != loop.StatusCompleted || res.Output != "42" || len(m.Requests()) != 2 || ran.Load() != 1 {
		t.Fatalf("Run = %+v, %v; model calls = %d", res, err, len(m.Requests()))
	}
	if ended == nil || ended.Status != loop.StatusCompleted || ended.Output != "42" {
		t.Fatalf("AfterRun saw %+v", ended)
	}
	requireAnswered(t, res.Messages)
	if _, err := cp.Load(t.Context(), "stop"); !errors.Is(err, loop.ErrCheckpointNotFound) {
		t.Fatalf("checkpoint: %v", err)
	}
	var out finalArgs
	if err := (&loop.Result{Status: loop.StatusCompleted, Output: `{"answer":"x"}`}).Decode(&out); err != nil || out.Answer != "x" {
		t.Fatalf("Decode = %+v, %v", out, err)
	}
}

func TestStopAtToolsResumes(t *testing.T) {
	t.Parallel()
	cp := loop.NewMemoryCheckpointer()
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "a", Name: "final", Args: finalArgs{Answer: "42"}}))
	r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, finalTool()), Checkpointer: cp, StopAtTools: []string{"final"}})
	// The consumer stops at the stop tool's result, before the run ended.
	for ev, err := range r.Run(t.Context(), &loop.Request{RunID: "stop", Input: model.UserText("q")}) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == loop.EventToolResult {
			break
		}
	}
	res, err := loop.Collect(r.Resume(t.Context(), "stop", nil), nil)
	if err != nil || res.Status != loop.StatusCompleted || res.Output != "42" || len(m.Requests()) != 1 {
		t.Fatalf("Resume = %+v, %v; model calls = %d", res, err, len(m.Requests()))
	}
}

func TestStopWhen(t *testing.T) {
	t.Parallel()
	echo, ran := countingEcho()
	var steps []loop.Step
	cond := func(s *loop.Step) bool {
		steps = append(steps, *s)
		return s.Turn == 2
	}
	second := &model.Message{Role: model.RoleAssistant, Parts: []model.Part{
		{Type: model.PartText, Text: "checking again"},
		{Type: model.PartToolCall, ToolCall: &model.ToolCall{ID: "b", Name: "echo", Arguments: []byte(`{"text":"b"}`)}},
	}}
	m := modeltest.New(echoCall("a"), modeltest.Respond(&model.Response{Message: second, FinishReason: model.FinishToolCalls}), echoCall("c"))
	r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, echo), StopWhen: []loop.StopCondition{nil, cond}})
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil)
	if err != nil || res.Status != loop.StatusCompleted || res.Output != "checking again" || ran.Load() != 2 || len(m.Requests()) != 2 {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if len(steps) != 2 || steps[1].Message.Text() != "checking again" || len(steps[1].ToolResults) != 1 || steps[1].ToolResults[0].Content != "b" || steps[1].Usage.Requests != 2 || steps[1].Usage.ToolCalls != 2 {
		t.Fatalf("steps = %+v", steps)
	}
}

func TestStopAtUnknownTool(t *testing.T) {
	t.Parallel()
	if _, err := loop.New(loop.Config{Model: modeltest.New(), StopAtTools: []string{"missing"}}); err == nil {
		t.Fatal("New accepted an unknown stop tool")
	}
}
