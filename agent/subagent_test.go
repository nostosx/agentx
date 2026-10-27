package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/tool"
)

func delegate(id, name, prompt string) modeltest.Call {
	return modeltest.Call{ID: id, Name: name, Args: map[string]string{"prompt": prompt}}
}

func TestSubAgentEventsAreOptIn(t *testing.T) {
	t.Parallel()
	newParent := func(opts ...agent.Option) *agent.LLMAgent {
		leaf := agent.MustNew("leaf", agent.WithModel(modeltest.New(modeltest.Reply("leaf answer"))))
		helper := agent.MustNew("helper", agent.WithStreaming(), agent.WithSubAgents(leaf), agent.WithModel(modeltest.New(
			modeltest.CallTools(delegate("helper_call", "leaf", "dig")),
			modeltest.Reply("helper answer"),
		)))
		return agent.MustNew("coordinator", append(opts, agent.WithSubAgents(helper), agent.WithModel(modeltest.New(
			modeltest.CallTools(delegate("parent_call", "helper", "help")),
			modeltest.Reply("all done"),
		)))...)
	}

	var nested int
	res, err := loop.Collect(newParent().Run(t.Context(), agent.Text("q")), func(ev *loop.Event) {
		if ev.Depth != 0 || ev.Agent != "coordinator" {
			nested++
		}
	})
	if err != nil || res.Output != "all done" || nested != 0 {
		t.Fatalf("Run = %+v, %v; nested events = %d", res, err, nested)
	}

	// Forwarded through a Runner, which saves the parent's result, not a
	// sub-agent's.
	runner := agent.NewRunner(newParent(agent.WithSubAgentEvents()), nil)
	var helperDeltas, helperEnds, leafEnds int
	for ev, err := range runner.Run(t.Context(), "s", model.UserText("q")) {
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case ev.Depth == 0:
			if ev.Agent != "coordinator" || ev.ParentCallID != "" {
				t.Fatalf("own event = %+v", ev)
			}
		case ev.Depth == 1 && ev.Agent == "helper" && ev.ParentCallID == "parent_call":
			switch ev.Type {
			case loop.EventDelta:
				helperDeltas++
			case loop.EventRunEnd:
				helperEnds++
			}
		case ev.Depth == 2 && ev.Agent == "leaf" && ev.ParentCallID == "helper_call":
			if ev.Type == loop.EventRunEnd {
				leafEnds++
				if ev.Result.Output != "leaf answer" {
					t.Fatalf("leaf result = %+v", ev.Result)
				}
			}
		default:
			t.Fatalf("forwarded event = %+v", ev)
		}
	}
	if helperDeltas == 0 || helperEnds != 1 || leafEnds != 1 {
		t.Fatalf("helper deltas = %d, helper ends = %d, leaf ends = %d", helperDeltas, helperEnds, leafEnds)
	}
	sess, err := runner.Store().Get(t.Context(), "s")
	if err != nil {
		t.Fatal(err)
	}
	if msgs := sess.Messages(); len(msgs) != 4 || msgs[3].Text() != "all done" {
		t.Fatalf("saved conversation has %d messages", len(msgs))
	}
}

func TestConcurrentSubAgents(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ready := make(chan struct{})
		var arrivals atomic.Int32
		blockingReply := func(text string) modeltest.Step {
			return func(*model.Request) (*model.Response, error) {
				if arrivals.Add(1) == 2 {
					close(ready)
				}
				select {
				case <-ready:
				case <-time.After(5 * time.Second):
					t.Error("sub-agents did not run concurrently")
				}
				return modeltest.Reply(text)(nil)
			}
		}
		researcher := agent.MustNew("researcher", agent.WithModel(modeltest.New(blockingReply("facts"))))
		writer := agent.MustNew("writer", agent.WithModel(modeltest.New(blockingReply("prose"))))
		m := modeltest.New(
			modeltest.CallTools(delegate("r", "researcher", "research"), delegate("w", "writer", "write")),
			modeltest.Reply("done"),
		)
		parent := agent.MustNew("coordinator", agent.WithModel(m), agent.WithSubAgents(researcher, writer), agent.WithConcurrentSubAgents(), agent.WithSubAgentEvents())
		var mu sync.Mutex
		starts := map[string]string{}
		res, err := loop.Collect(parent.Run(t.Context(), agent.Text("q")), func(ev *loop.Event) {
			if ev.Type == loop.EventRunStart && ev.Depth == 1 {
				mu.Lock()
				starts[ev.Agent] = ev.ParentCallID
				mu.Unlock()
			}
		})
		if err != nil || res.Status != loop.StatusCompleted || res.Usage.TotalTokens() != 60 {
			t.Fatalf("Run = %+v, %v", res, err)
		}
		if starts["researcher"] != "r" || starts["writer"] != "w" {
			t.Fatalf("forwarded starts = %v", starts)
		}
		if results := m.Requests()[1].Messages[2].ToolResults(); len(results) != 2 || results[0].Content != "facts" || results[1].Content != "prose" {
			t.Fatalf("results = %+v", results)
		}

		// Sub-agents run one at a time by default.
		sequential := agent.MustNew("coordinator", agent.WithModel(modeltest.New()), agent.WithSubAgents(researcher))
		if info := sequential.Tools().List()[0].Info(); info.Concurrent || info.ReadOnly {
			t.Fatalf("default sub-agent tool = %+v", info)
		}
	})
}

// newApprovingDeployer returns a sub-agent whose deploy tool, called with
// the given call ID, needs approval.
func newApprovingDeployer(name, callID string, deploys *atomic.Int32) *agent.LLMAgent {
	deploy := tool.Func("deploy", "deploy", func(_ context.Context, in deployArgs) (string, error) {
		deploys.Add(1)
		return "deployed " + in.Target, nil
	}, tool.RequireApproval())
	return agent.MustNew(name, agent.WithTools(deploy), agent.WithModel(modeltest.New(
		modeltest.CallTools(modeltest.Call{ID: callID, Name: "deploy", Args: deployArgs{Target: name}}),
		modeltest.Reply(name+" is live"),
	)))
}

// Concurrent sub-agents that need approval interrupt the parent together,
// and resume together with their own decisions.
func TestConcurrentSubAgentsInterruptTogether(t *testing.T) {
	t.Parallel()
	var deploys atomic.Int32
	east := newApprovingDeployer("east", "east_call", &deploys)
	west := newApprovingDeployer("west", "west_call", &deploys)
	m := modeltest.New(
		modeltest.CallTools(delegate("e", "east", "deploy"), delegate("w", "west", "deploy")),
		modeltest.Reply("all done"),
	)
	var starts atomic.Int32
	hooks := hook.NewManager()
	if err := hooks.OnFunc(hook.SubagentStart, "", func(context.Context, *hook.Event) (*hook.Result, error) {
		starts.Add(1)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	parent := agent.MustNew("coordinator", agent.WithModel(m), agent.WithSubAgents(east, west), agent.WithConcurrentSubAgents(),
		agent.WithHooks(hooks), agent.WithCheckpointer(loop.NewMemoryCheckpointer()))
	runner := agent.NewRunner(parent, nil)
	res, err := runner.RunText(t.Context(), "s", "ship")
	if err != nil || res.Status != loop.StatusInterrupted || deploys.Load() != 0 {
		t.Fatalf("Run = %+v, %v; deploys = %d", res, err, deploys.Load())
	}
	var got []string
	for _, req := range res.Interrupt.Requests {
		got = append(got, req.Agent+"/"+req.Call.ID)
	}
	if strings.Join(got, ",") != "east/east_call,west/west_call" {
		t.Fatalf("requests = %v", got)
	}
	approvals := map[string]loop.Approval{"east_call": loop.Approve, "west_call": {Reason: "not today"}}
	res, err = loop.Collect(runner.Resume(t.Context(), "s", approvals), nil)
	if err != nil || res.Status != loop.StatusCompleted || res.Output != "all done" || deploys.Load() != 1 {
		t.Fatalf("Resume = %+v, %v; deploys = %d", res, err, deploys.Load())
	}
	results := m.Requests()[1].Messages[2].ToolResults()
	if len(results) != 2 || results[0].Content != "east is live" || results[1].Content != "west is live" {
		t.Fatalf("results = %+v", results)
	}
	// SubagentStart fires when a sub-agent starts, not when it resumes.
	if starts.Load() != 2 {
		t.Fatalf("SubagentStart fired %d times", starts.Load())
	}
	// 2 parent calls and 2 calls of each sub-agent, 15 tokens each.
	if res.Usage.TotalTokens() != 90 {
		t.Fatalf("usage = %d", res.Usage.TotalTokens())
	}
}

func TestSubagentStartHook(t *testing.T) {
	t.Parallel()
	subModel := modeltest.New(modeltest.Reply("draft"))
	writer := agent.MustNew("writer", agent.WithModel(subModel))
	var mu sync.Mutex
	var events []hook.Event
	record := func(_ context.Context, ev *hook.Event) (*hook.Result, error) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, *ev)
		if ev.Type == hook.SubagentStart {
			return &hook.Result{AdditionalContext: "The style guide is terse.", Decision: hook.Deny}, nil
		}
		return nil, nil
	}
	hooks := hook.NewManager()
	for _, ev := range []hook.EventType{hook.SubagentStart, hook.SubagentStop} {
		if err := hooks.OnFunc(ev, "writer", record); err != nil {
			t.Fatal(err)
		}
	}
	if err := hooks.OnFunc(hook.SubagentStart, "other", record); err != nil {
		t.Fatal(err)
	}
	m := modeltest.New(modeltest.CallTools(delegate("c", "writer", "write it")), modeltest.Reply("done"))
	parent := agent.MustNew("coordinator", agent.WithModel(m), agent.WithSubAgents(writer), agent.WithHooks(hooks))
	res, err := agent.NewRunner(parent, nil).RunText(t.Context(), "s", "go")
	if err != nil || res.Status != loop.StatusCompleted {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %+v", events)
	}
	start, stop := events[0], events[1]
	if start.Type != hook.SubagentStart || start.Agent != "writer" || start.Reason != "writer" || start.Prompt != "write it" ||
		start.SessionID != "s" || start.Turn != 1 || start.RunID == "" || start.RunID != stop.RunID {
		t.Fatalf("SubagentStart = %+v; SubagentStop = %+v", start, stop)
	}
	// The context leads the task, and a Deny does not block the sub-agent.
	input := subModel.Requests()[0].Messages[0]
	if len(input.Parts) != 2 || !strings.Contains(input.Parts[0].Text, "The style guide is terse.") || input.Parts[1].Text != "write it" {
		t.Fatalf("sub-agent input = %+v", input.Parts)
	}
	if out := m.Requests()[1].Messages[2].ToolResults()[0]; out.IsError || out.Content != "draft" {
		t.Fatalf("sub-agent result = %+v", out)
	}
}

// chain returns the top of a chain of agents, each delegating to the next
// and answering with its result. opts apply to the top.
func chain(n int, opts ...agent.Option) (*agent.LLMAgent, []*modeltest.Model) {
	var below *agent.LLMAgent
	models := make([]*modeltest.Model, n)
	for i := n - 1; i >= 0; i-- {
		name := fmt.Sprintf("level%d", i)
		if below == nil {
			models[i] = modeltest.New(modeltest.Reply("bottom"))
			below = agent.MustNew(name, agent.WithModel(models[i]))
			continue
		}
		models[i] = modeltest.New(modeltest.CallTools(delegate(name+"_call", below.Name(), "go")), modeltest.Reply(name+" done"))
		aopts := []agent.Option{agent.WithModel(models[i]), agent.WithSubAgents(below)}
		if i == 0 {
			aopts = append(aopts, opts...)
		}
		below = agent.MustNew(name, aopts...)
	}
	return below, models
}

func TestMaxDepth(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		levels int
		opts   []agent.Option
		// failAt is the level whose sub-agent call fails, or -1.
		failAt int
	}{
		{name: "default allows 5", levels: agent.DefaultMaxDepth + 1, failAt: -1},
		{name: "default stops 6", levels: agent.DefaultMaxDepth + 2, failAt: agent.DefaultMaxDepth},
		{name: "top bounds the chain", levels: 4, opts: []agent.Option{agent.WithMaxDepth(2)}, failAt: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			top, models := chain(tc.levels, tc.opts...)
			res, err := loop.Collect(top.Run(t.Context(), agent.Text("q")), nil)
			if err != nil || res.Status != loop.StatusCompleted {
				t.Fatalf("Run = %+v, %v", res, err)
			}
			for i, m := range models[:len(models)-1] {
				reqs := m.Requests()
				if i > tc.failAt && tc.failAt >= 0 {
					if len(reqs) != 0 {
						t.Fatalf("level %d ran below the limit", i)
					}
					continue
				}
				out := reqs[1].Messages[2].ToolResults()[0]
				if fails := i == tc.failAt; out.IsError != fails || fails && !strings.Contains(out.Content, "nested at most") {
					t.Fatalf("level %d got %+v", i, out)
				}
			}
		})
	}
}

func TestAsToolOptions(t *testing.T) {
	t.Parallel()
	sub := agent.MustNew("helper", agent.WithModel(modeltest.New(modeltest.Reply("ok"))), agent.WithDescription("Helps."))
	wrapped := agent.AsTool(sub, agent.ToolOptions{Name: "ask_helper", Description: "Ask the helper.", Concurrent: true})
	if info := wrapped.Info(); info.Name != "ask_helper" || info.Description != "Ask the helper." || !info.Concurrent {
		t.Fatalf("info = %+v", info)
	}
	if info := agent.AsTool(sub, agent.ToolOptions{}).Info(); info.Name != "helper" || !strings.HasPrefix(info.Description, "Helps. ") || info.Concurrent {
		t.Fatalf("default info = %+v", info)
	}
	res, err := wrapped.Call(t.Context(), json.RawMessage(`{"prompt":"task"}`))
	if err != nil || res.IsError || res.Content != "ok" {
		t.Fatalf("Call = %+v, %v", res, err)
	}
}
