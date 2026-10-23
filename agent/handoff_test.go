package agent_test

import (
	"context"
	"errors"
	"iter"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/guard"
	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/tool"
)

func transfer(id, name string) modeltest.Call {
	return modeltest.Call{ID: id, Name: agent.HandoffToolPrefix + name}
}

func toolNames(req *model.Request) []string {
	var names []string
	for _, s := range req.Tools {
		names = append(names, s.Name)
	}
	slices.Sort(names)
	return names
}

func TestHandoffContinuesRun(t *testing.T) {
	t.Parallel()
	lookup := tool.Func("lookup_invoice", "", func(context.Context, struct{}) (string, error) { return "invoice 7", nil })
	bm := modeltest.New(modeltest.Reply("billing here"))
	billing := agent.MustNew("billing", agent.WithModel(bm), agent.WithDescription("Handles invoices."),
		agent.WithInstructions("You handle billing."), agent.WithTools(lookup))
	tm := modeltest.New(modeltest.CallTools(transfer("h", "billing")))
	triage := agent.MustNew("triage", agent.WithModel(tm), agent.WithInstructions("You route."), agent.WithHandoffs(billing))

	var types []loop.EventType
	var handoff *loop.Event
	res, err := loop.Collect(triage.Run(t.Context(), agent.Text("my invoice is wrong")), func(ev *loop.Event) {
		types = append(types, ev.Type)
		if ev.Type == loop.EventHandoff {
			handoff = ev
		}
	})
	if err != nil || res.Status != loop.StatusCompleted || res.Output != "billing here" || res.Agent != "billing" {
		t.Fatalf("run = %+v, %v", res, err)
	}
	count := func(et loop.EventType) int {
		n := 0
		for _, t := range types {
			if t == et {
				n++
			}
		}
		return n
	}
	if count(loop.EventRunStart) != 1 || count(loop.EventRunEnd) != 1 || count(loop.EventHandoff) != 1 || types[0] != loop.EventRunStart || types[len(types)-1] != loop.EventRunEnd {
		t.Fatalf("events = %v", types)
	}
	if h := handoff.Handoff; h.From != "triage" || h.To != "billing" || h.CallID != "h" || h.Tool != "transfer_to_billing" || h.Input != nil || handoff.RunID != res.RunID || handoff.Agent != "triage" {
		t.Fatalf("handoff event = %+v %+v", handoff, handoff.Handoff)
	}
	if res.Usage.Requests != 2 || res.Turns != 2 || len(res.NewMessages) != 5 || len(res.Values) != 0 {
		t.Fatalf("result usage %+v, turns %d, new messages %d, values %v", res.Usage, res.Turns, len(res.NewMessages), res.Values)
	}
	// The agent handed off to runs with its own instructions and tools,
	// on the whole conversation and a note.
	req := bm.Requests()[0]
	if req.System != "You handle billing." || !slices.Equal(toolNames(req), []string{"lookup_invoice"}) || len(req.Messages) != 4 ||
		req.Messages[1].ToolCalls()[0].Name != "transfer_to_billing" || !strings.Contains(req.Messages[3].Text(), "triage agent handed this conversation off to you") {
		t.Fatalf("billing request = %+v", req)
	}
	if tools := toolNames(tm.Requests()[0]); !slices.Equal(tools, []string{"transfer_to_billing"}) || !strings.Contains(tm.Requests()[0].Tools[0].Description, "Handles invoices.") {
		t.Fatalf("triage tools = %v", tm.Requests()[0].Tools)
	}
}

func TestRunnerKeepsActiveAgent(t *testing.T) {
	t.Parallel()
	bm := modeltest.New(modeltest.Reply("billing 1"), modeltest.Reply("billing 2"), modeltest.CallTools(transfer("back", "triage")))
	billing := agent.MustNew("billing", agent.WithModel(bm), agent.WithHandoffs(agent.Ref("triage", "Routes other topics.")))
	tm := modeltest.New(modeltest.CallTools(transfer("h", "billing")), modeltest.Reply("triage again"))
	triage := agent.MustNew("triage", agent.WithModel(tm), agent.WithHandoffs(billing))
	runner := agent.NewRunner(triage, nil)

	if res, err := runner.RunText(t.Context(), "s", "invoice"); err != nil || res.Output != "billing 1" {
		t.Fatalf("first run = %+v, %v", res, err)
	}
	sess, _ := runner.Store().Get(t.Context(), "s")
	if v, _ := sess.State().Get(agent.ActiveAgentKey); v != "billing" {
		t.Fatalf("active agent = %v", v)
	}
	forked, err := runner.Fork(t.Context(), "s")
	if err != nil {
		t.Fatal(err)
	}
	// The next message goes to billing, which sees the conversation.
	if res, err := runner.RunText(t.Context(), "s", "and another"); err != nil || res.Output != "billing 2" || res.Agent != "billing" || len(tm.Requests()) != 1 {
		t.Fatalf("second run = %+v, %v; triage calls %d", res, err, len(tm.Requests()))
	}
	if n := len(bm.Requests()[1].Messages); n != 6 {
		t.Fatalf("billing saw %d messages", n)
	}
	// Billing hands back to triage through a reference; the session
	// returns to the Runner's agent.
	if res, err := runner.RunText(t.Context(), forked, "something else"); err != nil || res.Output != "triage again" || res.Agent != "triage" {
		t.Fatalf("forked run = %+v, %v", res, err)
	}
	fs, _ := runner.Store().Get(t.Context(), forked)
	if _, ok := fs.State().Get(agent.ActiveAgentKey); ok {
		t.Fatal("active agent kept after handing back")
	}
}

type reason struct {
	Reason string `json:"reason"`
}

func TestHandoffInputAndFilter(t *testing.T) {
	t.Parallel()
	lookup := tool.Func("lookup", "", func(context.Context, struct{}) (string, error) { return "found", nil })
	bm := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "l", Name: "lookup"}), modeltest.Reply("done"))
	billing := agent.MustNew("billing", agent.WithModel(bm), agent.WithTools(lookup))
	var reasons []string
	tm := modeltest.New(
		modeltest.CallTools(modeltest.Call{ID: "s", Name: "search"}),
		modeltest.CallTools(modeltest.Call{ID: "h1", Name: "escalate", Args: reason{Reason: ""}}),
		modeltest.CallTools(modeltest.Call{ID: "h2", Name: "escalate", Args: reason{Reason: "refund"}}),
	)
	search := tool.Func("search", "", func(context.Context, struct{}) (string, error) { return "secret notes", nil })
	triage := agent.MustNew("triage", agent.WithModel(tm), agent.WithTools(search),
		agent.WithHandoff(billing, agent.HandoffName("escalate"), agent.HandoffDescription("Escalate to billing."),
			agent.HandoffInputFilter(agent.RemoveToolCalls),
			agent.HandoffInput(func(_ context.Context, in reason) error {
				if in.Reason == "" {
					return errors.New("a reason is required")
				}
				reasons = append(reasons, in.Reason)
				return nil
			})))
	runner := agent.NewRunner(triage, nil)
	var input string
	res, err := loop.Collect(runner.Run(t.Context(), "s", model.UserText("refund me")), func(ev *loop.Event) {
		if ev.Type == loop.EventHandoff {
			input = string(ev.Handoff.Input)
		}
	})
	if err != nil || res.Output != "done" || !slices.Equal(reasons, []string{"refund"}) || input != `{"reason":"refund"}` {
		t.Fatalf("run = %+v, %v; reasons %v, input %q", res, err, reasons, input)
	}
	if !strings.Contains(tm.Requests()[2].Messages[4].ToolResults()[0].Content, "a reason is required") {
		t.Fatalf("refused handoff result = %+v", tm.Requests()[2].Messages[4])
	}
	// Both of billing's requests see the conversation before the handoff
	// without tool calls, then its own turns in full.
	for i, req := range bm.Requests() {
		for _, msg := range req.Messages[:2] {
			if msg.Role == model.RoleTool || len(msg.ToolCalls()) > 0 || strings.Contains(msg.Text(), "secret") {
				t.Fatalf("request %d not filtered: %+v", i, req.Messages)
			}
		}
		if len(req.Messages) != 2+i*2 || !strings.Contains(req.Messages[1].Text(), `Handoff input: {"reason":"refund"}`) {
			t.Fatalf("request %d = %d messages", i, len(req.Messages))
		}
	}
	// The session keeps the whole conversation.
	sess, _ := runner.Store().Get(t.Context(), "s")
	if n := len(sess.Messages()); n != 11 || !strings.Contains(sess.Messages()[2].ToolResults()[0].Content, "secret") {
		t.Fatalf("session has %d messages", n)
	}
}

func TestHandoffLimits(t *testing.T) {
	t.Parallel()
	bm := modeltest.New(modeltest.CallTools(transfer("b1", "triage")), modeltest.Reply("billing answers"))
	billing := agent.MustNew("billing", agent.WithModel(bm), agent.WithHandoffs(agent.Ref("triage", ""), agent.Ref("nobody", "")))
	support := agent.MustNew("support", agent.WithModel(modeltest.New()))
	tm := modeltest.New(modeltest.CallTools(transfer("t1", "billing"), transfer("t2", "support")))
	triage := agent.MustNew("triage", agent.WithModel(tm), agent.WithHandoffs(billing, support), agent.WithMaxHandoffs(1))
	res, err := loop.Collect(triage.Run(t.Context(), agent.Text("q")), nil)
	if err != nil || res.Output != "billing answers" || res.Agent != "billing" {
		t.Fatalf("run = %+v, %v", res, err)
	}
	results := res.Messages[2].ToolResults()
	if len(results) != 2 || results[0].IsError || !results[1].IsError || !strings.Contains(results[1].Content, "one handoff") {
		t.Fatalf("handoff results = %+v", results)
	}
	if r := bm.Requests()[1].Messages[5].ToolResults()[0]; !r.IsError || !strings.Contains(r.Content, "limit of 1 handoffs") {
		t.Fatalf("handoff over the limit = %+v", r)
	}

	// A reference to an agent outside the graph is refused.
	bm.Append(modeltest.CallTools(transfer("n", "nobody")), modeltest.Reply("still billing"))
	res, err = loop.Collect(billing.Run(t.Context(), agent.Text("q")), nil)
	if err != nil || res.Output != "still billing" || !strings.Contains(res.Messages[2].ToolResults()[0].Content, "not available") {
		t.Fatalf("unresolved reference = %+v, %v", res, err)
	}
	if _, err := agent.New("bad", agent.WithModel(tm), agent.WithHandoffs(nil)); err == nil {
		t.Fatal("New accepted a nil handoff")
	}
	if _, err := agent.New("dup", agent.WithModel(tm), agent.WithHandoffs(billing, billing)); err == nil {
		t.Fatal("New accepted duplicate handoffs")
	}
}

func TestHandoffApprovalsResume(t *testing.T) {
	t.Parallel()
	for _, checkpointed := range []bool{true, false} {
		cp := loop.NewMemoryCheckpointer()
		ran := 0
		refund := tool.Func("refund", "", func(context.Context, struct{}) (string, error) { ran++; return "refunded", nil }, tool.RequireApproval())
		bm := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "r", Name: "refund"}), modeltest.Reply("refund done"))
		tm := modeltest.New(modeltest.CallTools(transfer("h", "billing")))
		var bopts, topts []agent.Option
		if checkpointed {
			bopts = append(bopts, agent.WithCheckpointer(cp))
			topts = append(topts, agent.WithCheckpointer(cp))
		}
		billing := agent.MustNew("billing", append(bopts, agent.WithModel(bm), agent.WithTools(refund))...)
		triage := agent.MustNew("triage", append(topts, agent.WithModel(tm), agent.WithHandoffs(billing))...)
		runner := agent.NewRunner(triage, nil)
		res, err := runner.RunText(t.Context(), "s", "refund")
		if err != nil || res.Status != loop.StatusInterrupted || res.Agent != "billing" || res.Interrupt.Requests[0].Agent != "billing" {
			t.Fatalf("checkpointed=%v: run = %+v, %v", checkpointed, res, err)
		}
		res, err = loop.Collect(runner.Resume(t.Context(), "s", map[string]loop.Approval{"r": loop.Approve}), nil)
		if err != nil || res.Output != "refund done" || ran != 1 || res.Agent != "billing" || res.Usage.Requests != 3 || len(res.Values) != 0 {
			t.Fatalf("checkpointed=%v: resume = %+v, %v", checkpointed, res, err)
		}
		sess, _ := runner.Store().Get(t.Context(), "s")
		if v, _ := sess.State().Get(agent.ActiveAgentKey); v != "billing" {
			t.Fatalf("checkpointed=%v: active agent = %v", checkpointed, v)
		}
	}

	// Resuming the run through the first agent finds the agent that ran it.
	cp := loop.NewMemoryCheckpointer()
	refund := tool.Func("refund", "", func(context.Context, struct{}) (string, error) { return "refunded", nil }, tool.RequireApproval())
	billing := agent.MustNew("billing", agent.WithCheckpointer(cp), agent.WithTools(refund),
		agent.WithModel(modeltest.New(modeltest.CallTools(modeltest.Call{ID: "r", Name: "refund"}), modeltest.Reply("refund done"))))
	triage := agent.MustNew("triage", agent.WithCheckpointer(loop.NewMemoryCheckpointer()), agent.WithHandoffs(billing),
		agent.WithModel(modeltest.New(modeltest.CallTools(transfer("h", "billing")))))
	res, err := loop.Collect(triage.Run(t.Context(), agent.Text("refund")), nil)
	if err != nil || res.Status != loop.StatusInterrupted {
		t.Fatalf("run = %+v, %v", res, err)
	}
	if _, err := loop.Collect(triage.Resume(t.Context(), "missing", nil), nil); !errors.Is(err, loop.ErrCheckpointNotFound) {
		t.Fatalf("resume of a missing run = %v", err)
	}
	res, err = loop.Collect(triage.Resume(t.Context(), res.RunID, map[string]loop.Approval{"r": loop.Approve}), nil)
	if err != nil || res.Output != "refund done" || res.Agent != "billing" || len(res.NewMessages) != 7 {
		t.Fatalf("resume = %+v, %v", res, err)
	}
	if err := triage.Discard(t.Context(), res.RunID); err != nil {
		t.Fatal(err)
	}
}

func TestHandoffGuardrailsDepsAndHooks(t *testing.T) {
	t.Parallel()
	type tenant string
	type region string
	var triageChecks, billingChecks atomic.Int32
	var seen []string
	whoami := tool.Func("whoami", "", func(ctx context.Context, _ struct{}) (string, error) {
		tn, _ := loop.Deps[tenant](ctx)
		rg, _ := loop.Deps[region](ctx)
		seen = append(seen, string(tn)+"/"+string(rg))
		return "ok", nil
	})
	outputs := 0
	bm := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "w", Name: "whoami"}), modeltest.Reply("billing answer"))
	billing := agent.MustNew("billing", agent.WithModel(bm), agent.WithTools(whoami), agent.WithDeps(region("eu")),
		agent.WithInputGuardrail(inputGuard("billing_input", "never", nil, &billingChecks)),
		agent.WithOutputGuardrail(guard.OutputGuardrail{Name: "out", Check: func(context.Context, *loop.Run, *model.Message) (guard.Result, error) {
			outputs++
			return guard.Result{}, nil
		}}))
	denied := 0
	tm := modeltest.New(modeltest.CallTools(transfer("h1", "billing")), modeltest.CallTools(transfer("h2", "billing")))
	triage := agent.MustNew("triage", agent.WithModel(tm), agent.WithHandoffs(billing),
		agent.WithInputGuardrail(inputGuard("triage_input", "never", nil, &triageChecks)),
		agent.WithHook(hook.PreToolUse, "transfer_to_.*", hook.Func(func(context.Context, *hook.Event) (*hook.Result, error) {
			denied++
			if denied == 1 {
				return &hook.Result{Decision: hook.Deny, Reason: "not yet"}, nil
			}
			return nil, nil
		})))
	res, err := loop.Collect(triage.Run(loop.WithDeps(t.Context(), tenant("acme")), agent.Text("q")), nil)
	if err != nil || res.Output != "billing answer" || res.Agent != "billing" {
		t.Fatalf("run = %+v, %v", res, err)
	}
	if triageChecks.Load() != 1 || billingChecks.Load() != 0 || outputs != 1 || denied != 2 || !slices.Equal(seen, []string{"acme/eu"}) {
		t.Fatalf("input checks %d/%d, outputs %d, hook calls %d, deps %v", triageChecks.Load(), billingChecks.Load(), outputs, denied, seen)
	}
}

// echoAgent is an agent other than an LLMAgent that answers with the
// number of messages it got.
type echoAgent struct{ got *agent.Input }

func (e *echoAgent) Name() string        { return "echo" }
func (e *echoAgent) Description() string { return "" }
func (e *echoAgent) Run(_ context.Context, in *agent.Input) iter.Seq2[*loop.Event, error] {
	e.got = in
	return func(yield func(*loop.Event, error) bool) {
		yield(&loop.Event{Type: loop.EventRunStart, RunID: in.RunID}, nil)
		msgs := append(slices.Clone(in.History), in.Message, model.AssistantText("echo"))
		yield(&loop.Event{Type: loop.EventRunEnd, RunID: in.RunID, Result: &loop.Result{
			RunID: in.RunID, Status: loop.StatusCompleted, Output: "echo", Agent: "echo", Turns: 1,
			Messages: msgs, NewMessages: msgs[len(in.History):], Usage: model.Usage{Requests: 1},
		}}, nil)
	}
}

func TestHandoffToOtherAgent(t *testing.T) {
	t.Parallel()
	e := &echoAgent{}
	tm := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "s", Name: "search"}), modeltest.CallTools(transfer("h", "echo")))
	search := tool.Func("search", "", func(context.Context, struct{}) (string, error) { return "x", nil })
	triage := agent.MustNew("triage", agent.WithModel(tm), agent.WithTools(search), agent.WithHandoff(e, agent.HandoffInputFilter(agent.NestHandoffHistory)))
	res, err := loop.Collect(triage.Run(t.Context(), agent.Text("q")), nil)
	if err != nil || res.Output != "echo" || res.Usage.Requests != 3 || res.Turns != 3 || len(res.NewMessages) != 7 {
		t.Fatalf("run = %+v, %v", res, err)
	}
	if len(e.got.History) != 1 || !strings.Contains(e.got.History[0].Text(), "called tool search") || e.got.Values["agentx.handoff.from"] != "triage" {
		t.Fatalf("echo input = %+v", e.got)
	}
	if res.Values != nil && res.Values["agentx.handoff.from"] != "" {
		t.Fatalf("values = %v", res.Values)
	}
}

func TestHandoffFilters(t *testing.T) {
	t.Parallel()
	call := &model.Message{Role: model.RoleAssistant, Parts: []model.Part{
		{Type: model.PartReasoning, Text: "thinking"},
		{Type: model.PartToolCall, ToolCall: &model.ToolCall{ID: "c", Name: "search", Arguments: []byte(`{"q":"x"}`)}},
	}}
	mixed := &model.Message{Role: model.RoleAssistant, Parts: []model.Part{model.TextPart("let me check"), {Type: model.PartToolCall, ToolCall: &model.ToolCall{ID: "d", Name: "f"}}}}
	msgs := []*model.Message{
		model.UserText("hi"), call,
		model.ToolResults(&model.ToolResult{CallID: "c", Name: "search", Content: "found", IsError: true}),
		mixed, model.NewMessage(model.RoleUser, model.ImageURLPart("u")), model.AssistantText("answer"),
	}
	got := agent.RemoveToolCalls(msgs)
	if len(got) != 4 || got[0] != msgs[0] || got[1].Text() != "let me check" || len(got[1].Parts) != 1 || len(mixed.Parts) != 2 || got[3] != msgs[5] {
		t.Fatalf("RemoveToolCalls = %+v", got)
	}
	nested := agent.NestHandoffHistory(msgs)
	text := nested[0].Text()
	for _, want := range []string{"<CONVERSATION HISTORY>", "1. user: hi", `assistant called tool search with {"q":"x"}`, "tool search error: found", "user: [image]", "assistant: answer", "</CONVERSATION HISTORY>"} {
		if !strings.Contains(text, want) {
			t.Errorf("nested history lacks %q:\n%s", want, text)
		}
	}
	if len(nested) != 1 || nested[0].Role != model.RoleUser || agent.NestHandoffHistory(nil) != nil {
		t.Fatalf("nested = %+v", nested)
	}
}

func TestHandoffInsideSubAgentAndStreaming(t *testing.T) {
	t.Parallel()
	billing := agent.MustNew("billing", agent.WithStreaming(), agent.WithModel(modeltest.New(modeltest.Reply("billing result"))))
	triage := agent.MustNew("triage", agent.WithStreaming(), agent.WithHandoffs(billing), agent.WithModel(modeltest.New(modeltest.CallTools(transfer("h", "billing")))))
	parent := agent.MustNew("parent", agent.WithSubAgents(triage), agent.WithSubAgentEvents(),
		agent.WithModel(modeltest.New(modeltest.CallTools(delegate("d", "triage", "sort this out")), modeltest.Reply("all done"))))
	var nestedHandoffs, nestedEnds int
	var deltas strings.Builder
	res, err := loop.Collect(parent.Run(t.Context(), agent.Text("q")), func(ev *loop.Event) {
		if ev.Depth == 1 {
			switch ev.Type {
			case loop.EventHandoff:
				nestedHandoffs++
			case loop.EventRunEnd:
				nestedEnds++
			case loop.EventDelta:
				deltas.WriteString(ev.Text())
			}
		}
	})
	if err != nil || res.Output != "all done" || nestedHandoffs != 1 || nestedEnds != 1 || deltas.String() != "billing result" {
		t.Fatalf("run = %+v, %v; nested handoffs %d, ends %d, deltas %q", res, err, nestedHandoffs, nestedEnds, deltas.String())
	}
	if got := res.Messages[2].ToolResults()[0].Content; got != "billing result" || res.Usage.Requests != 4 {
		t.Fatalf("sub-agent result %q, usage %+v", got, res.Usage)
	}

	// A consumer that stops at the handoff ends the run there.
	triage2 := agent.MustNew("triage", agent.WithHandoffs(billing), agent.WithModel(modeltest.New(modeltest.CallTools(transfer("h", "billing")))))
	var last loop.EventType
	for ev, err := range triage2.Run(t.Context(), agent.Text("q")) {
		if err != nil {
			t.Fatal(err)
		}
		last = ev.Type
		if ev.Type == loop.EventHandoff {
			break
		}
	}
	if last != loop.EventHandoff {
		t.Fatalf("last event = %s", last)
	}
}
