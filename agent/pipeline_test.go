package agent_test

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/guard"
	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/tool"
)

// pipelineRecorder records the order in which the mechanisms of the tool
// call pipeline see a call.
type pipelineRecorder struct {
	mu    sync.Mutex
	steps []string
}

func (r *pipelineRecorder) add(step string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, step)
}

func (r *pipelineRecorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.steps)
}

// stageRecorder is loop middleware that records its tool stages under
// name and denies every call when deny is set.
func (r *pipelineRecorder) stageRecorder(name string, deny bool) loop.Middleware {
	return &loop.Funcs{
		MiddlewareName: name,
		BeforeToolFunc: func(_ context.Context, _ *loop.Run, plan *loop.ToolPlan) error {
			r.add(name + ".before")
			if deny {
				plan.Deny("denied by " + name)
			}
			return nil
		},
		WrapToolFunc: func(_ *loop.Run, next tool.Invoker) tool.Invoker {
			return func(ctx context.Context, call *tool.Call) (*tool.Result, error) {
				r.add(name + ".wrap")
				return next(ctx, call)
			}
		},
		AfterToolFunc: func(_ context.Context, _ *loop.Run, _ *loop.ToolPlan, _ *model.ToolResult) error {
			r.add(name + ".after")
			return nil
		},
	}
}

// pipelineAgent builds an agent that calls the echo tool once with every
// mechanism of the tool call pipeline recording into r, plus extra.
func pipelineAgent(r *pipelineRecorder, extra ...agent.Option) agent.Agent {
	echo := tool.Func("echo", "", func(context.Context, struct{}) (string, error) {
		r.add("tool")
		return "ok", nil
	})
	opts := []agent.Option{
		agent.WithModel(modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c", Name: "echo"}), modeltest.Reply("done"))),
		agent.WithTools(echo),
		agent.WithHook(hook.PreToolUse, "", hook.Func(func(context.Context, *hook.Event) (*hook.Result, error) {
			r.add("hook.pre")
			return &hook.Result{Decision: hook.Allow}, nil
		})),
		agent.WithHook(hook.PostToolUse, "", hook.Func(func(context.Context, *hook.Event) (*hook.Result, error) {
			r.add("hook.post")
			return nil, nil
		})),
		agent.WithToolGuardrail(guard.ToolGuardrail{
			Name: "recorder",
			Before: func(context.Context, *loop.Run, *model.ToolCall) (guard.Result, error) {
				r.add("guard.before")
				return guard.Result{}, nil
			},
			After: func(context.Context, *loop.Run, *model.ToolCall, *model.ToolResult) (guard.Result, error) {
				r.add("guard.after")
				return guard.Result{}, nil
			},
		}),
		agent.WithToolMiddleware(func(next tool.Invoker) tool.Invoker {
			return func(ctx context.Context, call *tool.Call) (*tool.Result, error) {
				r.add("toolmw")
				return next(ctx, call)
			}
		}),
		agent.WithMiddleware(r.stageRecorder("mw", false)),
	}
	return agent.MustNew("assistant", append(opts, extra...)...)
}

// The order documented in the package comment under "Tool call pipeline".
func TestToolCallPipelineOrder(t *testing.T) {
	t.Parallel()
	r := &pipelineRecorder{}
	res, err := loop.Collect(pipelineAgent(r).Run(t.Context(), agent.Text("q")), nil)
	if err != nil || res.Output != "done" {
		t.Fatalf("run = %+v, %v", res, err)
	}
	want := []string{
		"hook.pre", "mw.before", "guard.before",
		"mw.wrap", "toolmw", "tool", "guard.after",
		"mw.after", "hook.post",
	}
	if got := r.got(); !slices.Equal(got, want) {
		t.Errorf("pipeline = %v, want %v", got, want)
	}
}

// A hook's allow does not lift a later denial, the denial skips the
// remaining BeforeTool stages, and denied calls pass AfterTool without
// firing hooks.
func TestToolCallPipelineDenial(t *testing.T) {
	t.Parallel()
	r := &pipelineRecorder{}
	a := pipelineAgent(r, agent.WithMiddleware(r.stageRecorder("deny", true)))
	res, err := loop.Collect(a.Run(t.Context(), agent.Text("q")), nil)
	if err != nil || res.Output != "done" {
		t.Fatalf("run = %+v, %v", res, err)
	}
	want := []string{"hook.pre", "mw.before", "deny.before", "deny.after", "mw.after"}
	if got := r.got(); !slices.Equal(got, want) {
		t.Errorf("pipeline = %v, want %v", got, want)
	}
	var results []model.ToolResult
	for _, m := range res.Messages {
		for _, tr := range m.ToolResults() {
			results = append(results, *tr)
		}
	}
	if len(results) != 1 || !results[0].IsError || results[0].Content != "denied by deny" {
		t.Errorf("tool results = %+v", results)
	}
}
