package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/tool"
)

type deployArgs struct {
	Target string `json:"target"`
}

// newDeployer returns a sub-agent whose deploy tool needs approval, and a
// counter of deployments.
func newDeployer(t *testing.T) (*agent.LLMAgent, *atomic.Int32) {
	t.Helper()
	var deploys atomic.Int32
	deploy := tool.Func("deploy", "deploy", func(_ context.Context, in deployArgs) (string, error) {
		deploys.Add(1)
		return "deployed " + in.Target, nil
	}, tool.RequireApproval())
	sub := agent.MustNew("deployer",
		agent.WithDescription("Deploys builds."),
		agent.WithModel(modeltest.New(
			modeltest.CallTools(modeltest.Call{ID: "sub_call", Name: "deploy", Args: deployArgs{Target: "prod"}}),
			modeltest.Reply("prod is live"),
		)),
		agent.WithTools(deploy))
	return sub, &deploys
}

func parentModel() *modeltest.Model {
	return modeltest.New(
		modeltest.CallTools(modeltest.Call{ID: "parent_call", Name: "deployer", Args: map[string]string{"prompt": "deploy prod"}}),
		modeltest.Reply("all done"),
	)
}

func TestSubAgentInterruptReachesParent(t *testing.T) {
	t.Parallel()
	sub, deploys := newDeployer(t)
	cp := loop.NewMemoryCheckpointer()
	parent := agent.MustNew("coordinator", agent.WithModel(parentModel()), agent.WithSubAgents(sub), agent.WithCheckpointer(cp))
	runner := agent.NewRunner(parent, nil)

	res, err := runner.RunText(t.Context(), "s", "ship it")
	if err != nil || res.Status != loop.StatusInterrupted || deploys.Load() != 0 {
		t.Fatalf("Run = %+v, %v; deploys = %d", res, err, deploys.Load())
	}
	if reqs := res.Interrupt.Requests; len(reqs) != 1 || reqs[0].Call.ID != "sub_call" || reqs[0].Agent != "deployer" {
		t.Fatalf("requests = %+v", res.Interrupt.Requests)
	}
	// The sub-agent has no checkpointer; it resumes from the state carried
	// in the parent's checkpoint.
	res, err = loop.Collect(runner.Resume(t.Context(), "s", map[string]loop.Approval{"sub_call": loop.Approve}), nil)
	if err != nil || res.Status != loop.StatusCompleted || res.Output != "all done" || deploys.Load() != 1 {
		t.Fatalf("Resume = %+v, %v; deploys = %d", res, err, deploys.Load())
	}
	results := res.Messages[len(res.Messages)-2].ToolResults()
	if len(results) != 1 || results[0].Content != "prod is live" {
		t.Fatalf("sub-agent result = %+v", results)
	}
	// Usage covers both models: 2 parent calls and 2 sub-agent calls of 15
	// tokens each, counted once.
	if got := res.Usage.TotalTokens(); got != 60 {
		t.Fatalf("usage = %d, want 60", got)
	}
	// It counts the 4 requests, and 2 tool calls: the parent's call of the
	// sub-agent and the sub-agent's deploy.
	if u := res.Usage; u.Requests != 4 || u.ToolCalls != 2 {
		t.Fatalf("usage = %+v", u)
	}
}

func TestSubAgentDenied(t *testing.T) {
	t.Parallel()
	sub, deploys := newDeployer(t)
	parent := agent.MustNew("coordinator", agent.WithModel(parentModel()), agent.WithSubAgents(sub))
	res, err := loop.Collect(parent.Run(t.Context(), agent.Text("ship it")), nil)
	if err != nil || res.Status != loop.StatusInterrupted {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	res, err = loop.Collect(parent.ResumeState(t.Context(), res.Interrupt.State, nil), nil)
	if err != nil || res.Status != loop.StatusCompleted || deploys.Load() != 0 {
		t.Fatalf("Resume = %+v, %v; deploys = %d", res, err, deploys.Load())
	}
}

func TestParentApproverDecidesForSubAgent(t *testing.T) {
	t.Parallel()
	sub, deploys := newDeployer(t)
	var asked []string
	approver := loop.ApproverFunc(func(_ context.Context, req *loop.ApprovalRequest) (loop.Approval, error) {
		asked = append(asked, req.Agent+"/"+req.Call.Name)
		return loop.Approve, nil
	})
	parent := agent.MustNew("coordinator", agent.WithModel(parentModel()), agent.WithSubAgents(sub), agent.WithApprover(approver))
	res, err := loop.Collect(parent.Run(t.Context(), agent.Text("ship it")), nil)
	if err != nil || res.Status != loop.StatusCompleted || deploys.Load() != 1 || strings.Join(asked, ",") != "deployer/deploy" {
		t.Fatalf("Run = %+v, %v; deploys = %d; asked = %v", res, err, deploys.Load(), asked)
	}
}

func TestSubAgentUsageReachesParent(t *testing.T) {
	t.Parallel()
	sub := agent.MustNew("helper", agent.WithModel(modeltest.New(modeltest.Reply("help"))))
	parent := agent.MustNew("coordinator", agent.WithModel(modeltest.New(
		modeltest.CallTools(modeltest.Call{ID: "c", Name: "helper", Args: map[string]string{"prompt": "help"}}),
		modeltest.Reply("done"),
	)), agent.WithSubAgents(sub))
	res, err := loop.Collect(parent.Run(t.Context(), agent.Text("q")), nil)
	if err != nil || res.Usage.TotalTokens() != 45 {
		t.Fatalf("Run = %+v, %v", res, err)
	}
}

func TestRunnerSavesWhenConsumerStops(t *testing.T) {
	t.Parallel()
	slow := tool.Func("slow", "slow", func(ctx context.Context, _ struct{}) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c", Name: "slow"}), modeltest.Reply("next answer"))
	runner := agent.NewRunner(agent.MustNew("worker", agent.WithModel(m), agent.WithTools(slow)), nil)
	for ev, err := range runner.Run(t.Context(), "s", model.UserText("go")) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == loop.EventToolCall {
			break
		}
	}
	sess, err := runner.Store().Get(t.Context(), "s")
	if err != nil {
		t.Fatalf("session not saved: %v", err)
	}
	msgs := sess.Messages()
	if len(msgs) != 3 || msgs[0].Text() != "go" || len(msgs[2].ToolResults()) != 1 {
		t.Fatalf("saved messages = %d", len(msgs))
	}
	// The session continues normally.
	if res, err := runner.RunText(t.Context(), "s", "again"); err != nil || res.Output != "next answer" {
		t.Fatalf("next run = %+v, %v", res, err)
	}
}

func TestRunnerDiscard(t *testing.T) {
	t.Parallel()
	deploy := tool.Func("deploy", "deploy", func(context.Context, struct{}) (string, error) { return "ok", nil }, tool.RequireApproval())
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c", Name: "deploy"}), modeltest.Reply("moving on"))
	cp := loop.NewMemoryCheckpointer()
	runner := agent.NewRunner(agent.MustNew("ops", agent.WithModel(m), agent.WithTools(deploy), agent.WithCheckpointer(cp)), nil)
	res, err := runner.RunText(t.Context(), "s", "deploy")
	if err != nil || res.Status != loop.StatusInterrupted {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	// Losing the checkpoint used to leave the session stuck.
	if err := cp.Delete(t.Context(), res.RunID); err != nil {
		t.Fatal(err)
	}
	_, err = loop.Collect(runner.Resume(t.Context(), "s", nil), nil)
	if !errors.Is(err, loop.ErrCheckpointNotFound) || !strings.Contains(err.Error(), "Runner.Discard") {
		t.Fatalf("Resume without checkpoint = %v", err)
	}
	if err := runner.Discard(t.Context(), "s"); err != nil {
		t.Fatal(err)
	}
	if err := runner.Discard(t.Context(), "s"); err == nil {
		t.Fatal("second Discard succeeded")
	}
	res, err = runner.RunText(t.Context(), "s", "something else")
	if err != nil || res.Output != "moving on" {
		t.Fatalf("Run after Discard = %+v, %v", res, err)
	}
	discarded := m.Requests()[1].Messages[2].ToolResults()
	if len(discarded) != 1 || !discarded[0].IsError || !strings.Contains(discarded[0].Content, "discarded") {
		t.Fatalf("discarded call = %+v", discarded)
	}
}

func TestRunnerResumesWithoutCheckpointer(t *testing.T) {
	t.Parallel()
	var deploys atomic.Int32
	deploy := tool.Func("deploy", "deploy", func(context.Context, struct{}) (string, error) { deploys.Add(1); return "ok", nil }, tool.RequireApproval())
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c", Name: "deploy"}), modeltest.Reply("deployed"))
	runner := agent.NewRunner(agent.MustNew("ops", agent.WithModel(m), agent.WithTools(deploy)), nil)
	res, err := runner.RunText(t.Context(), "s", "deploy")
	if err != nil || res.Status != loop.StatusInterrupted {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	res, err = loop.Collect(runner.Resume(t.Context(), "s", map[string]loop.Approval{"c": loop.Approve}), nil)
	if err != nil || res.Status != loop.StatusCompleted || res.Output != "deployed" || deploys.Load() != 1 {
		t.Fatalf("Resume = %+v, %v; deploys = %d", res, err, deploys.Load())
	}
	if _, err := loop.Collect(runner.Resume(t.Context(), "s", nil), nil); err == nil || !strings.Contains(err.Error(), "no interrupted run") {
		t.Fatalf("second Resume = %v", err)
	}
}

func TestRunnerResumesFailedRun(t *testing.T) {
	t.Parallel()
	m := modeltest.New(modeltest.Fail(errors.New("overloaded")), modeltest.Reply("recovered"), modeltest.Fail(errors.New("overloaded")), modeltest.Reply("fresh"))
	cp := loop.NewMemoryCheckpointer()
	runner := agent.NewRunner(agent.MustNew("assistant", agent.WithModel(m), agent.WithCheckpointer(cp)), nil)
	res, err := runner.RunText(t.Context(), "s", "q")
	if err == nil || res.Status != loop.StatusFailed {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	sess, err := runner.Store().Get(t.Context(), "s")
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := sess.State().Get(agent.FailedRunKey); id != res.RunID {
		t.Fatalf("failed run = %v, want %s", id, res.RunID)
	}
	res, err = loop.Collect(runner.Resume(t.Context(), "s", nil), nil)
	if err != nil || res.Status != loop.StatusCompleted || res.Output != "recovered" {
		t.Fatalf("Resume = %+v, %v", res, err)
	}
	// A failed run does not block new runs, which give it up.
	failed, err := runner.RunText(t.Context(), "s", "again")
	if err == nil || failed.Status != loop.StatusFailed {
		t.Fatalf("Run = %+v, %v", failed, err)
	}
	if res, err := runner.RunText(t.Context(), "s", "new question"); err != nil || res.Output != "fresh" {
		t.Fatalf("Run after failure = %+v, %v", res, err)
	}
	if _, err := cp.Load(t.Context(), failed.RunID); !errors.Is(err, loop.ErrCheckpointNotFound) {
		t.Fatalf("checkpoint of the given-up run: %v", err)
	}
}

func TestSubagentStopHookKeepsSubAgentWorking(t *testing.T) {
	t.Parallel()
	subModel := modeltest.New(modeltest.Reply("draft"), modeltest.Reply("checked"))
	sub := agent.MustNew("writer", agent.WithModel(subModel))
	var events []hook.Event
	hooks := hook.NewManager()
	if err := hooks.OnFunc(hook.SubagentStop, "writer", func(_ context.Context, ev *hook.Event) (*hook.Result, error) {
		events = append(events, *ev)
		if !ev.StopHookActive {
			return &hook.Result{Decision: hook.Deny, Reason: "check your draft"}, nil
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	// Hook errors are logged, not returned as the delegation's error.
	if err := hooks.OnFunc(hook.SubagentStop, "", func(context.Context, *hook.Event) (*hook.Result, error) {
		return nil, errors.New("broken hook")
	}); err != nil {
		t.Fatal(err)
	}
	parentModel := modeltest.New(
		modeltest.CallTools(modeltest.Call{ID: "c", Name: "writer", Args: map[string]string{"prompt": "write"}}),
		modeltest.Reply("done"),
	)
	parent := agent.MustNew("coordinator", agent.WithModel(parentModel), agent.WithSubAgents(sub), agent.WithHooks(hooks))
	res, err := agent.NewRunner(parent, nil).RunText(t.Context(), "s", "go")
	if err != nil || res.Status != loop.StatusCompleted {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if out := parentModel.Requests()[1].Messages[2].ToolResults()[0]; out.IsError || out.Content != "checked" {
		t.Fatalf("sub-agent result = %+v", out)
	}
	if len(events) != 2 || events[0].SessionID != "s" || events[0].Turn != 1 || events[0].Message != "draft" || events[1].Message != "checked" {
		t.Fatalf("SubagentStop events = %+v", events)
	}
	if last := subModel.Requests()[1].Messages; len(last) != 3 || !strings.Contains(last[2].Text(), "check your draft") {
		t.Fatalf("continued sub-agent request = %+v", last)
	}
}

func TestSubagentStopHookIsBounded(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		opts  []hook.Option
		calls int
	}{
		{"default cap", nil, 1 + hook.DefaultStopBlockCap},
		{"configured cap", []hook.Option{hook.StopBlockCap(2)}, 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			steps := make([]modeltest.Step, 12)
			for i := range steps {
				steps[i] = modeltest.Reply("answer")
			}
			subModel := modeltest.New(steps...)
			hooks := hook.NewManager(tt.opts...)
			if err := hooks.OnFunc(hook.SubagentStop, "", func(context.Context, *hook.Event) (*hook.Result, error) {
				return &hook.Result{Decision: hook.Deny, Reason: "never done"}, nil
			}); err != nil {
				t.Fatal(err)
			}
			wrapped := agent.AsTool(agent.MustNew("writer", agent.WithModel(subModel)), agent.ToolOptions{Hooks: hooks})
			res, err := wrapped.Call(t.Context(), json.RawMessage(`{"prompt":"task"}`))
			if err != nil || res.IsError || len(subModel.Requests()) != tt.calls {
				t.Fatalf("Call = %+v, %v; sub-agent calls = %d, want %d", res, err, len(subModel.Requests()), tt.calls)
			}
		})
	}
}

func TestSubagentStopContextContinuesAndToolCallsResetCap(t *testing.T) {
	t.Parallel()
	ok := tool.Func("ok", "Succeed.", func(context.Context, struct{}) (string, error) { return "fine", nil })
	// The first continuation calls a tool, which resets the count, so the
	// cap of 1 allows a second continuation.
	subModel := modeltest.New(
		modeltest.Reply("draft"),
		modeltest.CallTools(modeltest.Call{ID: "c", Name: "ok"}), modeltest.Reply("tested"),
		modeltest.Reply("final"),
		modeltest.Reply("unreachable"),
	)
	var active []bool
	hooks := hook.NewManager(hook.StopBlockCap(1))
	if err := hooks.OnFunc(hook.SubagentStop, "", func(_ context.Context, ev *hook.Event) (*hook.Result, error) {
		active = append(active, ev.StopHookActive)
		return &hook.Result{AdditionalContext: "run the tests"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	wrapped := agent.AsTool(agent.MustNew("writer", agent.WithModel(subModel), agent.WithTools(ok)), agent.ToolOptions{Hooks: hooks})
	res, err := wrapped.Call(t.Context(), json.RawMessage(`{"prompt":"task"}`))
	if err != nil || res.IsError || res.Content != "final" || len(subModel.Requests()) != 4 {
		t.Fatalf("Call = %+v, %v; sub-agent calls = %d", res, err, len(subModel.Requests()))
	}
	if !slices.Equal(active, []bool{false, true, true}) {
		t.Fatalf("StopHookActive = %v", active)
	}
	if got := subModel.Requests()[1].Messages[2].Text(); !strings.Contains(got, "SubagentStop hook feedback:\nrun the tests") {
		t.Fatalf("feedback = %q", got)
	}
}

// The Runner keeps its own copy of an interrupted run's state, so a caller
// modifying the result does not change how the run resumes.
func TestRunnerKeepsACopyOfTheState(t *testing.T) {
	t.Parallel()
	var deploys atomic.Int32
	deploy := tool.Func("deploy", "deploy", func(context.Context, struct{}) (string, error) { deploys.Add(1); return "ok", nil }, tool.RequireApproval())
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c", Name: "deploy"}), modeltest.Reply("deployed"))
	runner := agent.NewRunner(agent.MustNew("ops", agent.WithModel(m), agent.WithTools(deploy)), nil)
	res, err := runner.RunText(t.Context(), "s", "deploy")
	if err != nil || res.Status != loop.StatusInterrupted {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	res.Interrupt.State.Pending = nil
	res.Interrupt.State.Messages = nil
	res, err = loop.Collect(runner.Resume(t.Context(), "s", map[string]loop.Approval{"c": loop.Approve}), nil)
	if err != nil || res.Status != loop.StatusCompleted || res.Output != "deployed" || deploys.Load() != 1 || len(res.Messages) != 4 {
		t.Fatalf("Resume = %+v, %v; deploys = %d", res, err, deploys.Load())
	}
}
