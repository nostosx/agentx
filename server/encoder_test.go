package server

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
)

func ev(typ loop.EventType, turn int, set func(*loop.Event)) item {
	e := &loop.Event{Type: typ, RunID: "run_1", SessionID: "s1", Agent: "chat", Turn: turn}
	if set != nil {
		set(e)
	}
	return item{ev: e}
}

func delta(turn, idx int, p model.Part) item {
	return ev(loop.EventDelta, turn, func(e *loop.Event) {
		e.Delta = &model.StreamEvent{Type: model.StreamDelta, Index: idx, Delta: &p}
	})
}

var (
	deployCall  = &model.ToolCall{ID: "call_1", Name: "deploy", Arguments: json.RawMessage(`{"env":"prod"}`)}
	handoffCall = &model.ToolCall{ID: "call_h", Name: "transfer_to_billing", Arguments: json.RawMessage(`{}`)}
	shotCall    = &model.ToolCall{ID: "call_s", Name: "screenshot", Arguments: json.RawMessage(`{}`)}
)

// billingEv is an event of the agent a run was handed off to.
func billingEv(typ loop.EventType, turn int, set func(*loop.Event)) item {
	return ev(typ, turn, func(e *loop.Event) {
		e.Agent = "billing"
		if set != nil {
			set(e)
		}
	})
}

// scenarios are event sequences the encoders are checked against.
var scenarios = map[string][]item{
	// A streamed turn with reasoning, text and a tool call that needs
	// approval.
	"interrupt": {
		ev(loop.EventRunStart, 0, nil),
		ev(loop.EventTurnStart, 1, nil),
		delta(1, 0, model.Part{Type: model.PartReasoning, Text: "Think"}),
		delta(1, 0, model.Part{Type: model.PartReasoning, Text: "ing"}),
		delta(1, 1, model.Part{Type: model.PartText, Text: "Deploy"}),
		delta(1, 1, model.Part{Type: model.PartText, Text: "ing"}),
		delta(1, 2, model.Part{Type: model.PartToolCall, ToolCall: &model.ToolCall{ID: "call_1", Name: "deploy", Arguments: json.RawMessage(`{"env"`)}}),
		delta(1, 2, model.Part{Type: model.PartToolCall, ToolCall: &model.ToolCall{Arguments: json.RawMessage(`:"prod"}`)}}),
		ev(loop.EventMessage, 1, func(e *loop.Event) {
			e.Message = &model.Message{Role: model.RoleAssistant, Parts: []model.Part{
				{Type: model.PartReasoning, Text: "Thinking"}, model.TextPart("Deploying"), {Type: model.PartToolCall, ToolCall: deployCall},
			}}
		}),
		ev(loop.EventInterrupted, 1, func(e *loop.Event) {
			e.Interrupt = &loop.Interrupt{RunID: "run_1", Requests: []*loop.ApprovalRequest{{Call: deployCall, Reason: "Deploys need approval.", Agent: "chat"}}}
		}),
		ev(loop.EventRunEnd, 1, func(e *loop.Event) {
			intr := &loop.Interrupt{RunID: "run_1", Requests: []*loop.ApprovalRequest{{Call: deployCall, Reason: "Deploys need approval.", Agent: "chat"}}}
			e.Result = &loop.Result{RunID: "run_1", Status: loop.StatusInterrupted, Reason: "awaiting tool approval", FinishReason: model.FinishToolCalls, Output: "Deploying", Turns: 1, Interrupt: intr, Usage: model.Usage{InputTokens: 10, OutputTokens: 5}}
		}),
	},
	// A resumed run whose tool calls were approved and denied, followed by
	// an answer that was not streamed.
	"resume": {
		ev(loop.EventRunStart, 1, nil),
		ev(loop.EventToolCall, 1, func(e *loop.Event) { e.ToolCall = deployCall }),
		ev(loop.EventToolResult, 1, func(e *loop.Event) {
			e.ToolCall, e.ToolResult = deployCall, &model.ToolResult{CallID: "call_1", Name: "deploy", Content: "deployed to prod"}
		}),
		ev(loop.EventToolResult, 1, func(e *loop.Event) {
			e.ToolResult = &model.ToolResult{CallID: "call_2", Name: "deploy", Content: "The user denied this tool call.", IsError: true}
		}),
		ev(loop.EventToolResult, 1, func(e *loop.Event) {
			e.ToolResult = &model.ToolResult{CallID: "call_3", Name: "deploy", Content: "timeout", IsError: true}
		}),
		ev(loop.EventTurnStart, 2, nil),
		ev(loop.EventMessage, 2, func(e *loop.Event) {
			e.Message = &model.Message{Role: model.RoleAssistant, Parts: []model.Part{
				{Type: model.PartReasoning, Text: "Done thinking"}, model.TextPart("Deployed."),
			}}
		}),
		ev(loop.EventRunEnd, 2, func(e *loop.Event) {
			e.Result = &loop.Result{RunID: "run_1", Status: loop.StatusCompleted, FinishReason: model.FinishStop, Output: "Deployed.", Turns: 2,
				Usage: model.Usage{InputTokens: 20, OutputTokens: 7, CacheReadTokens: 4, ReasoningTokens: 2}}
		}),
	},
	// A run that fails while streaming.
	"failure": {
		ev(loop.EventRunStart, 0, nil),
		ev(loop.EventTurnStart, 1, nil),
		delta(1, 0, model.Part{Type: model.PartText, Text: "Partial"}),
		ev(loop.EventRunEnd, 1, func(e *loop.Event) {
			e.Result = &loop.Result{RunID: "run_1", Status: loop.StatusFailed, Reason: "provider unavailable", Turns: 1, Err: errors.New("provider unavailable")}
		}),
		{err: errors.New("provider unavailable")},
	},
	// A run handed off to another agent, whose tool returns media and
	// whose first answer is sent back for not matching the output schema;
	// a guardrail blocks the second.
	"handoff": {
		ev(loop.EventRunStart, 0, nil),
		ev(loop.EventTurnStart, 1, nil),
		ev(loop.EventMessage, 1, func(e *loop.Event) {
			e.Message = &model.Message{Role: model.RoleAssistant, Parts: []model.Part{model.TextPart("Transferring."), {Type: model.PartToolCall, ToolCall: handoffCall}}}
		}),
		ev(loop.EventToolCall, 1, func(e *loop.Event) { e.ToolCall = handoffCall }),
		ev(loop.EventToolResult, 1, func(e *loop.Event) {
			e.ToolCall, e.ToolResult = handoffCall, &model.ToolResult{CallID: "call_h", Name: "transfer_to_billing", Content: "Transferred to billing."}
		}),
		ev(loop.EventHandoff, 1, func(e *loop.Event) {
			e.Handoff = &loop.Handoff{From: "chat", To: "billing", Tool: "transfer_to_billing", CallID: "call_h"}
		}),
		billingEv(loop.EventTurnStart, 2, nil),
		billingEv(loop.EventMessage, 2, func(e *loop.Event) {
			e.Message = &model.Message{Role: model.RoleAssistant, Parts: []model.Part{{Type: model.PartToolCall, ToolCall: shotCall}}}
		}),
		billingEv(loop.EventToolResult, 2, func(e *loop.Event) {
			e.ToolCall, e.ToolResult = shotCall, &model.ToolResult{CallID: "call_s", Name: "screenshot", Content: "screen",
				Parts: []model.Part{model.ImagePart("image/png", []byte{0x89, 'P', 'N', 'G'}), model.FilePart(model.PartDocument, "openai", "file_1", "")}}
		}),
		billingEv(loop.EventTurnStart, 3, nil),
		billingEv(loop.EventMessage, 3, func(e *loop.Event) { e.Message = model.AssistantText("{bad") }),
		billingEv(loop.EventOutputRetry, 3, func(e *loop.Event) {
			e.OutputRetry = &loop.OutputRetry{Output: "{bad", Problems: []string{"invalid JSON"}, Attempt: 1, Retries: 1}
		}),
		billingEv(loop.EventTurnStart, 4, nil),
		billingEv(loop.EventMessage, 4, func(e *loop.Event) { e.Message = model.AssistantText(`{"card":"4242"}`) }),
		billingEv(loop.EventRunEnd, 4, func(e *loop.Event) {
			e.Result = &loop.Result{RunID: "run_1", Status: loop.StatusBlocked, Reason: "guardrail pii tripped: card number", Turns: 4, Agent: "billing",
				Guardrail: &loop.Guardrail{Name: "pii", Kind: "output", Message: "card number"}}
		}),
	},
	// A run that delegates to a sub-agent whose events are forwarded.
	"subagent": {
		ev(loop.EventRunStart, 0, nil),
		ev(loop.EventTurnStart, 1, nil),
		ev(loop.EventMessage, 1, func(e *loop.Event) {
			e.Message = &model.Message{Role: model.RoleAssistant, Parts: []model.Part{{Type: model.PartToolCall, ToolCall: &model.ToolCall{ID: "call_0", Name: "research", Arguments: json.RawMessage(`{"task":"look"}`)}}}}
		}),
		nested(loop.EventRunStart, 0, nil),
		nested(loop.EventTurnStart, 1, nil),
		nested(loop.EventMessage, 1, func(e *loop.Event) { e.Message = model.AssistantText("Found it.") }),
		nested(loop.EventRunEnd, 1, func(e *loop.Event) {
			e.Result = &loop.Result{RunID: "run_sub", Status: loop.StatusCompleted, Output: "Found it.", Turns: 1}
		}),
		ev(loop.EventToolResult, 1, func(e *loop.Event) {
			e.ToolResult = &model.ToolResult{CallID: "call_0", Name: "research", Content: "Found it."}
		}),
		ev(loop.EventTurnStart, 2, nil),
		ev(loop.EventMessage, 2, func(e *loop.Event) { e.Message = model.AssistantText("Here it is.") }),
		ev(loop.EventRunEnd, 2, func(e *loop.Event) {
			e.Result = &loop.Result{RunID: "run_1", Status: loop.StatusCompleted, Output: "Here it is.", Turns: 2}
		}),
	},
}

func nested(typ loop.EventType, turn int, set func(*loop.Event)) item {
	return ev(typ, turn, func(e *loop.Event) {
		e.RunID, e.Agent, e.Depth, e.ParentCallID = "run_sub", "research", 1, "call_0"
		if set != nil {
			set(e)
		}
	})
}

func encode(t *testing.T, enc encoder, items []item) string {
	t.Helper()
	rec := httptest.NewRecorder()
	sw := newSSEWriter(rec)
	for _, it := range items {
		if it.err != nil {
			enc.fail(sw, it.err)
		} else {
			enc.event(sw, it.ev)
		}
	}
	enc.end(sw)
	if err := sw.flush(); err != nil {
		t.Fatal(err)
	}
	return rec.Body.String()
}

func TestEncodersGolden(t *testing.T) {
	encoders := map[string]func() encoder{
		"agentx":    func() encoder { return &nativeEncoder{} },
		"jsonl":     func() encoder { return jsonlEncoder{} },
		"vercel-ui": func() encoder { return newVercelEncoder("", map[string]bool{"call_2": true}) },
		"ag-ui":     func() encoder { return newAGUIEncoder("thread_1", "agui_run_1") },
	}
	for name, newEnc := range encoders {
		for scenario, items := range scenarios {
			t.Run(name+"/"+scenario, func(t *testing.T) {
				out := encode(t, newEnc(), items)
				ext := ".sse"
				if name == "jsonl" {
					ext = ".jsonl"
				}
				golden(t, name+"_"+scenario+ext, []byte(out))
				if name != "jsonl" {
					events, _ := parseSSE(t, strings.NewReader(out))
					checkStream(t, name, events)
				}
			})
		}
	}
}

// checkStream checks the protocol invariants of a stream.
func checkStream(t *testing.T, protocol string, events []sseEvent) {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("empty event stream")
	}
	switch protocol {
	case "agentx":
		for i, e := range events {
			if e.ID != strconv.Itoa(i+1) {
				t.Errorf("event %d has ID %q", i, e.ID)
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(e.Data), &m); err != nil {
				t.Fatal(err)
			}
			if m["type"] != e.Event {
				t.Errorf("event name %q, type %v", e.Event, m["type"])
			}
		}
	case "vercel-ui":
		if last := events[len(events)-1]; last.Data != "[DONE]" {
			t.Fatalf("stream does not end with [DONE]: %q", last.Data)
		}
		objs := fields(t, events[:len(events)-1])
		ts := types(objs)
		if len(ts) == 0 {
			t.Fatal("stream contains no protocol events")
		}
		if ts[0] != "start" || ts[len(ts)-1] != "finish" {
			t.Fatalf("stream is not framed by start and finish: %v", ts)
		}
		balance(t, objs, "id", "text-start", "text-end")
		balance(t, objs, "id", "reasoning-start", "reasoning-end")
		balance(t, objs, "", "start-step", "finish-step")
	case "ag-ui":
		objs := fields(t, events)
		ts := types(objs)
		if ts[0] != "RUN_STARTED" && ts[0] != "RUN_ERROR" {
			t.Fatalf("first event %s", ts[0])
		}
		if last := ts[len(ts)-1]; last != "RUN_FINISHED" && last != "RUN_ERROR" {
			t.Fatalf("last event %s", last)
		}
		balance(t, objs, "messageId", "TEXT_MESSAGE_START", "TEXT_MESSAGE_END")
		balance(t, objs, "messageId", "REASONING_MESSAGE_START", "REASONING_MESSAGE_END")
		balance(t, objs, "toolCallId", "TOOL_CALL_START", "TOOL_CALL_END")
		balance(t, objs, "stepName", "STEP_STARTED", "STEP_FINISHED")
	}
}

// balance checks that every start event is ended once, after it, and
// nothing is ended that did not start.
func balance(t *testing.T, objs []map[string]any, key, start, end string) {
	t.Helper()
	open := map[string]int{}
	for _, o := range objs {
		k := ""
		if key != "" {
			k, _ = o[key].(string)
			if sub, ok := o["subagentRunId"].(string); ok {
				k = sub + "/" + k
			}
		}
		switch o["type"] {
		case start:
			open[k]++
		case end:
			if open[k] == 0 {
				t.Errorf("%s %q without %s", end, k, start)
			}
			open[k]--
		}
	}
	for k, n := range open {
		if n != 0 {
			t.Errorf("%s %q is not ended", start, k)
		}
	}
}
