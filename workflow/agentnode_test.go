package workflow_test

import (
	"context"
	"errors"
	"iter"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/tool"
	"github.com/nostosx/agentx/workflow"
)

// resumableAgent interrupts its first run and completes when resumed from
// the interrupt's state.
type resumableAgent struct {
	scriptedAgent
	runIDs  []string
	resumed map[string]loop.Approval
}

func (a *resumableAgent) ResumeState(_ context.Context, st *loop.State, approvals map[string]loop.Approval) iter.Seq2[*loop.Event, error] {
	a.resumed = approvals
	return func(yield func(*loop.Event, error) bool) {
		out := "denied"
		if approvals["c1"].Approved && st.RunID == a.runIDs[0] {
			out = "approved"
		}
		yield(&loop.Event{Type: loop.EventRunEnd, Result: &loop.Result{RunID: st.RunID, Status: loop.StatusCompleted, Output: out}}, nil)
	}
}

func TestAgentNodeSuspendsForApprovals(t *testing.T) {
	t.Parallel()
	a := &resumableAgent{}
	a.run = func(_ context.Context, in *agent.Input) iter.Seq2[*loop.Event, error] {
		a.runIDs = append(a.runIDs, in.RunID)
		return func(yield func(*loop.Event, error) bool) {
			st := &loop.State{RunID: in.RunID, Status: loop.StatusInterrupted}
			intr := &loop.Interrupt{RunID: in.RunID, Requests: []*loop.ApprovalRequest{{Call: &model.ToolCall{ID: "c1", Name: "deploy"}}}, State: st}
			if !yield(&loop.Event{Type: loop.EventInterrupted, Interrupt: intr}, nil) {
				return
			}
			yield(&loop.Event{Type: loop.EventRunEnd, Result: &loop.Result{RunID: in.RunID, Status: loop.StatusInterrupted, Interrupt: intr}}, nil)
		}
	}
	var forwarded atomic.Int32
	w := workflow.NewGraph().
		AddNode("ops", workflow.AgentNode(a, workflow.Template("deploy {{env}}"), "result")).
		AddEdge(workflow.Start, "ops").MustCompile(workflow.WithCheckpointer(workflow.NewMemoryCheckpointer()))
	var s *workflow.Suspension
	for ev, err := range w.Stream(t.Context(), workflow.NewState(map[string]any{"env": "prod"}), workflow.WithRunID("deploy")) {
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Type {
		case workflow.AgentEvent:
			if ev.Node != "ops" || ev.Agent == nil {
				t.Errorf("agent event = %+v", ev)
			}
			forwarded.Add(1)
		case workflow.RunEnd:
			if ev.Status != workflow.StatusSuspended {
				t.Fatalf("run end = %+v", ev)
			}
			s = &workflow.Suspension{RunID: ev.RunID, Pending: ev.Suspended}
		}
	}
	if forwarded.Load() != 2 || s == nil || len(s.Pending) != 1 || s.Pending[0].Interrupt == nil || s.Pending[0].Interrupt.Requests[0].Call.ID != "c1" {
		t.Fatalf("suspension = %+v, forwarded %d", s, forwarded.Load())
	}
	if want := workflow.SubRunID("deploy", "ops", 1, -1); a.runIDs[0] != want || s.Pending[0].Interrupt.RunID != want {
		t.Fatalf("agent run ID = %v, want %s", a.runIDs, want)
	}
	st, err := w.Resume(t.Context(), "deploy", map[string]any{"ops": map[string]loop.Approval{"c1": loop.Approve}})
	if err != nil || st.String("result") != "approved" || len(a.runIDs) != 1 {
		t.Fatalf("Resume = %v, %v, runs %v", st.Snapshot(), err, a.runIDs)
	}
}

func TestAgentNodeWithLLMAgent(t *testing.T) {
	t.Parallel()
	var deploys atomic.Int32
	deploy := tool.Func("deploy", "Deploy", func(context.Context, struct{}) (string, error) {
		deploys.Add(1)
		return "deployed", nil
	}, tool.RequireApproval())
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c1", Name: "deploy"}), modeltest.Reply("all done"))
	a := agent.MustNew("ops", agent.WithModel(m), agent.WithTools(deploy))
	w := workflow.NewGraph().
		AddNode("ops", workflow.AgentNode(a, workflow.Template("ship it"), "result")).
		AddEdge(workflow.Start, "ops").MustCompile()
	_, err := w.Run(t.Context(), nil, workflow.WithRunID("llm"))
	s := suspension(t, err)
	if intr := s.Pending[0].Interrupt; intr == nil || len(intr.Requests) != 1 || intr.Requests[0].Call.Name != "deploy" || deploys.Load() != 0 {
		t.Fatalf("pending = %+v", s.Pending[0])
	}
	st, err := w.Resume(t.Context(), "llm", map[string]any{"ops": map[string]loop.Approval{"c1": loop.Approve}})
	if err != nil || st.String("result") != "all done" || deploys.Load() != 1 {
		t.Fatalf("Resume = %v, %v, deploys %d", st.Snapshot(), err, deploys.Load())
	}
}

func TestAgentNodeResumesCheckpointedRunAfterCrash(t *testing.T) {
	t.Parallel()
	var deploys atomic.Int32
	deploy := tool.Func("deploy", "Deploy", func(context.Context, struct{}) (string, error) {
		deploys.Add(1)
		return "deployed", nil
	})
	reached, release := make(chan struct{}), make(chan struct{})
	m := modeltest.New(
		modeltest.CallTools(modeltest.Call{ID: "c1", Name: "deploy"}),
		func(*model.Request) (*model.Response, error) {
			close(reached)
			<-release
			return nil, errors.New("process died")
		},
	)
	agentCP := loop.NewMemoryCheckpointer()
	a := agent.MustNew("ops", agent.WithModel(m), agent.WithTools(deploy), agent.WithCheckpointer(agentCP))
	cp := &crashCheckpointer{Checkpointer: workflow.NewMemoryCheckpointer()}
	build := func() *workflow.Workflow {
		return workflow.NewGraph().
			AddNode("ops", workflow.AgentNode(a, workflow.Template("ship it"), "result")).
			AddEdge(workflow.Start, "ops").MustCompile(workflow.WithCheckpointer(cp))
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := build().Run(ctx, nil, workflow.WithRunID("crash"))
		done <- err
	}()
	<-reached
	cp.crashed.Store(true)
	cancel()
	close(release)
	if err := <-done; err == nil {
		t.Fatal("crashed run succeeded")
	}
	if deploys.Load() != 1 {
		t.Fatalf("deploys = %d", deploys.Load())
	}
	if _, err := agentCP.Load(t.Context(), workflow.SubRunID("crash", "ops", 1, -1)); err != nil {
		t.Fatalf("agent checkpoint under the sub-run ID: %v", err)
	}
	m.Append(modeltest.Reply("recovered"))
	cp.crashed.Store(false)
	st, err := build().Resume(t.Context(), "crash", nil)
	if err != nil || st.String("result") != "recovered" || deploys.Load() != 1 {
		t.Fatalf("Resume = %v, %v, deploys %d", st.Snapshot(), err, deploys.Load())
	}
	// The resumed agent run continued the conversation instead of starting
	// over: its last request holds the tool result.
	reqs := m.Requests()
	last := reqs[len(reqs)-1]
	if len(reqs) != 3 || last.Messages[len(last.Messages)-1].Role != model.RoleTool {
		t.Fatalf("requests = %d, last messages %+v", len(reqs), last.Messages)
	}
}

func TestAgentNodeRecoversInterruptAfterCrash(t *testing.T) {
	t.Parallel()
	deploy := tool.Func("deploy", "Deploy", func(context.Context, struct{}) (string, error) { return "deployed", nil }, tool.RequireApproval())
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c2", Name: "deploy"}), modeltest.Reply("shipped"))
	agentCP := loop.NewMemoryCheckpointer()
	a := agent.MustNew("ops", agent.WithModel(m), agent.WithTools(deploy), agent.WithCheckpointer(agentCP))
	// Simulate a crash right after the agent was interrupted and before the
	// workflow saved the suspension: the agent checkpoint is interrupted
	// while the workflow checkpoint shows the node running.
	res, err := loop.Collect(a.Run(t.Context(), &agent.Input{Message: model.UserText("ship it"), RunID: workflow.SubRunID("late", "ops", 1, -1)}), nil)
	if err != nil || res.Status != loop.StatusInterrupted {
		t.Fatalf("agent run = %+v, %v", res, err)
	}
	cp := workflow.NewMemoryCheckpointer()
	if err := cp.Save(t.Context(), &workflow.Checkpoint{RunID: "late", Status: workflow.StatusRunning, Running: []string{"ops"},
		Attempts: map[string]int{"ops": 1}, Tries: map[string]int{"ops": 1}, Visits: map[string]int{"ops": 1}, Steps: 1,
		Routes: map[string][]string{workflow.Start: {"ops"}}}); err != nil {
		t.Fatal(err)
	}
	w := workflow.NewGraph().
		AddNode("ops", workflow.AgentNode(a, workflow.Template("ship it"), "result")).
		AddEdge(workflow.Start, "ops").MustCompile(workflow.WithCheckpointer(cp))
	_, err = w.Resume(t.Context(), "late", nil)
	s := suspension(t, err)
	if intr := s.Pending[0].Interrupt; intr == nil || len(intr.Requests) != 1 || intr.Requests[0].Call.ID != "c2" {
		t.Fatalf("recovered interrupt = %+v", s.Pending[0])
	}
	st, err := w.Resume(t.Context(), "late", map[string]any{"ops": map[string]loop.Approval{"c2": loop.Approve}})
	if err != nil || st.String("result") != "shipped" {
		t.Fatalf("Resume = %v, %v", st.Snapshot(), err)
	}
}

// checkpointResumer resumes by run ID only.
type checkpointResumer struct {
	scriptedAgent
	resumedID string
	discarded []string
}

func (a *checkpointResumer) Resume(_ context.Context, runID string, approvals map[string]loop.Approval) iter.Seq2[*loop.Event, error] {
	a.resumedID = runID
	return func(yield func(*loop.Event, error) bool) {
		if !approvals["c1"].Approved {
			yield(&loop.Event{Type: loop.EventRunEnd, Result: &loop.Result{RunID: runID, Status: loop.StatusFailed, Reason: "denied"}}, nil)
			return
		}
		yield(&loop.Event{Type: loop.EventRunEnd, Result: &loop.Result{RunID: runID, Status: loop.StatusCompleted, Output: "resumed"}}, nil)
	}
}

func (a *checkpointResumer) Discard(_ context.Context, runID string) error {
	a.discarded = append(a.discarded, runID)
	return nil
}

func TestAgentNodeResumesByRunIDAndDiscardsFailedRuns(t *testing.T) {
	t.Parallel()
	a := &checkpointResumer{}
	a.run = func(_ context.Context, in *agent.Input) iter.Seq2[*loop.Event, error] {
		return func(yield func(*loop.Event, error) bool) {
			intr := &loop.Interrupt{RunID: in.RunID, Requests: []*loop.ApprovalRequest{{Call: &model.ToolCall{ID: "c1", Name: "x"}}}}
			yield(&loop.Event{Type: loop.EventRunEnd, Result: &loop.Result{RunID: in.RunID, Status: loop.StatusInterrupted, Interrupt: intr}}, nil)
		}
	}
	w := workflow.NewGraph().AddNode("n", workflow.AgentNode(a, workflow.Template("go"), "out")).AddEdge(workflow.Start, "n").MustCompile()
	if _, err := w.Run(t.Context(), nil, workflow.WithRunID("r1")); !errors.Is(err, workflow.ErrSuspended) {
		t.Fatal(err)
	}
	st, err := w.Resume(t.Context(), "r1", map[string]any{"n": map[string]loop.Approval{"c1": loop.Approve}})
	if err != nil || st.String("out") != "resumed" || a.resumedID != "r1-n-1" {
		t.Fatalf("Resume = %v, %v, %s", st.Snapshot(), err, a.resumedID)
	}
	if _, err := w.Run(t.Context(), nil, workflow.WithRunID("r2")); !errors.Is(err, workflow.ErrSuspended) {
		t.Fatal(err)
	}
	if _, err := w.Resume(t.Context(), "r2", map[string]any{"n": map[string]loop.Approval{}}); err == nil || len(a.discarded) != 1 || a.discarded[0] != "r2-n-1" {
		t.Fatalf("denied Resume = %v, discarded %v", err, a.discarded)
	}
	if _, err := w.Run(t.Context(), nil, workflow.WithRunID("r3")); !errors.Is(err, workflow.ErrSuspended) {
		t.Fatal(err)
	}
	if _, err := w.Resume(t.Context(), "r3", map[string]any{"n": "not approvals"}); err == nil || !strings.Contains(err.Error(), "runtime.Approval") {
		t.Fatalf("bad resume value = %v", err)
	}
	// An agent that cannot resume fails the node.
	plain := scriptedAgent{run: a.run}
	w = workflow.NewGraph().AddNode("n", workflow.AgentNode(plain, workflow.Template("go"), "out")).AddEdge(workflow.Start, "n").MustCompile()
	if _, err := w.Run(t.Context(), nil, workflow.WithRunID("r4")); !errors.Is(err, workflow.ErrSuspended) {
		t.Fatal(err)
	}
	if _, err := w.Resume(t.Context(), "r4", map[string]any{"n": map[string]loop.Approval{}}); err == nil || !strings.Contains(err.Error(), "cannot resume") {
		t.Fatalf("non-resumer = %v", err)
	}
}
