package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
)

// sampleEvents returns one event of each type, as a run that calls a tool
// needing approval would emit them.
func sampleEvents() []*loop.Event {
	call := &model.ToolCall{ID: "call_1", Name: "deploy", Arguments: json.RawMessage(`{"env":"prod"}`)}
	stamp := func(ev *loop.Event) *loop.Event {
		ev.RunID, ev.SessionID, ev.Agent = "run_1", "s1", "chat"
		return ev
	}
	msg := &model.Message{Role: model.RoleAssistant, Parts: []model.Part{
		{Type: model.PartReasoning, Text: "Deploying.", Signature: "sig"},
		model.TextPart("Deploying now."),
		{Type: model.PartToolCall, ToolCall: call},
	}}
	intr := &loop.Interrupt{RunID: "run_1", Requests: []*loop.ApprovalRequest{{Call: call, Reason: "Deploys need approval.", Agent: "chat"}}}
	usage := model.Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 2, ReasoningTokens: 1, Requests: 1}
	return []*loop.Event{
		stamp(&loop.Event{Type: loop.EventRunStart}),
		stamp(&loop.Event{Type: loop.EventTurnStart, Turn: 1}),
		stamp(&loop.Event{Type: loop.EventDelta, Turn: 1, Delta: &model.StreamEvent{Type: model.StreamDelta, Index: 1, Delta: &model.Part{Type: model.PartText, Text: "Deploying"}}}),
		stamp(&loop.Event{Type: loop.EventMessage, Turn: 1, Message: msg, Usage: usage}),
		stamp(&loop.Event{Type: loop.EventToolCall, Turn: 1, ToolCall: call}),
		stamp(&loop.Event{Type: loop.EventToolResult, Turn: 1, ToolCall: call, ToolResult: &model.ToolResult{CallID: "call_1", Name: "deploy", Content: "deployed"}}),
		stamp(&loop.Event{Type: loop.EventCompacted, Turn: 1, Compaction: &loop.Compaction{Messages: []*model.Message{model.UserText("summary")}, Before: 900, After: 100, Summary: "summary"}}),
		stamp(&loop.Event{Type: loop.EventOutputRetry, Turn: 1, OutputRetry: &loop.OutputRetry{Output: "{}", Problems: []string{"missing name"}, Attempt: 1, Retries: 2}}),
		stamp(&loop.Event{Type: loop.EventToolResult, Turn: 1, ToolResult: &model.ToolResult{CallID: "call_2", Name: "screenshot", Content: "screen",
			Parts: []model.Part{model.ImagePart("image/png", []byte{0x89, 'P', 'N', 'G'}), model.DocumentPart("application/pdf", []byte("%PDF"), "r.pdf")}, Metadata: map[string]any{"width": 1280.0}}}),
		stamp(&loop.Event{Type: loop.EventHandoff, Turn: 1, Handoff: &loop.Handoff{From: "chat", To: "billing", Tool: "transfer_to_billing", CallID: "call_3", Input: json.RawMessage(`{"reason":"invoice"}`)}}),
		stamp(&loop.Event{Type: loop.EventMessage, Turn: 1, Depth: 1, ParentCallID: "call_0", Message: model.AssistantText("nested")}),
		stamp(&loop.Event{Type: loop.EventInterrupted, Turn: 1, Interrupt: intr}),
		stamp(&loop.Event{Type: loop.EventRunEnd, Turn: 1, Result: &loop.Result{
			RunID: "run_1", Status: loop.StatusInterrupted, Reason: "awaiting tool approval",
			FinishReason: model.FinishToolCalls, Output: "Deploying now.",
			NewMessages: []*model.Message{model.UserText("Deploy"), msg}, Usage: usage, Turns: 1, Interrupt: intr,
		}}),
		stamp(&loop.Event{Type: loop.EventRunEnd, Turn: 2, Result: &loop.Result{
			RunID: "run_2", Status: loop.StatusFailed, Reason: "provider unavailable", Err: errors.New("provider unavailable"),
		}}),
		stamp(&loop.Event{Type: loop.EventRunEnd, Turn: 3, Result: &loop.Result{
			RunID: "run_3", Status: loop.StatusBlocked, Reason: "guardrail pii tripped: card number", Turns: 3, Agent: "billing",
			Guardrail: &loop.Guardrail{Name: "pii", Kind: "output", Message: "card number", Info: map[string]any{"field": "card"}},
		}}),
	}
}

func TestEventWireFormatGolden(t *testing.T) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, ev := range sampleEvents() {
		if err := enc.Encode(ev); err != nil {
			t.Fatal(err)
		}
	}
	golden(t, "events.golden.jsonl", buf.Bytes())
}

func TestEventJSONRoundTrip(t *testing.T) {
	for _, ev := range sampleEvents() {
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		var got loop.Event
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		if got.Result != nil && ev.Result.Err != nil {
			if got.Result.Err == nil || got.Result.Err.Error() != ev.Result.Err.Error() {
				t.Fatalf("Err = %v, want %v", got.Result.Err, ev.Result.Err)
			}
			got.Result.Err = ev.Result.Err
		}
		if !reflect.DeepEqual(&got, ev) {
			t.Errorf("round trip of %s:\n got %+v\nwant %+v", ev.Type, &got, ev)
		}
	}
}

func TestResultJSONValue(t *testing.T) {
	// A Result value, not only a pointer, encodes its error.
	b, err := json.Marshal(loop.Result{RunID: "r", Status: loop.StatusFailed, Err: errors.New("boom")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"error":"boom"`) {
		t.Fatalf("got %s", b)
	}
	if strings.Contains(string(b), "Err") {
		t.Fatalf("Err field leaked: %s", b)
	}
}

func TestWireEventStripsStateAndConversation(t *testing.T) {
	state := &loop.State{RunID: "run_1", System: "secret system prompt"}
	intr := &loop.Interrupt{RunID: "run_1", State: state}
	ev := &loop.Event{Type: loop.EventRunEnd, Interrupt: intr, Result: &loop.Result{
		RunID: "run_1", Messages: []*model.Message{model.UserText("old")}, NewMessages: []*model.Message{model.UserText("new")}, Interrupt: intr,
		Values: map[string]string{"permission.grants": `["secret grant"]`},
	}}
	got := wireEvent(ev)
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "secret") || strings.Contains(string(b), `"old"`) || strings.Contains(string(b), "values") {
		t.Fatalf("wire event leaks state or history: %s", b)
	}
	if !strings.Contains(string(b), `"new"`) {
		t.Fatalf("wire event lost new messages: %s", b)
	}
	if ev.Interrupt.State == nil || ev.Result.Messages == nil || ev.Result.Values == nil {
		t.Fatal("wireEvent modified the event")
	}
	plain := &loop.Event{Type: loop.EventTurnStart}
	if wireEvent(plain) != plain {
		t.Fatal("events without results should not be copied")
	}
}

func TestWriteJSONL(t *testing.T) {
	events := sampleEvents()[:4]
	final := &loop.Event{Type: loop.EventRunEnd, Result: &loop.Result{RunID: "run_1", Status: loop.StatusCompleted, Output: "ok"}}
	seq := func(yield func(*loop.Event, error) bool) {
		for _, ev := range append(events, final) {
			if !yield(ev, nil) {
				return
			}
		}
	}
	var buf bytes.Buffer
	res, err := WriteJSONL(&buf, seq)
	if err != nil || res.Output != "ok" {
		t.Fatalf("WriteJSONL = %+v, %v", res, err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 5 {
		t.Fatalf("got %d lines: %s", len(lines), buf.String())
	}
	for i, line := range lines {
		var ev loop.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
	}

	boom := errors.New("boom")
	failing := func(yield func(*loop.Event, error) bool) {
		if !yield(events[0], nil) {
			return
		}
		yield(nil, boom)
	}
	buf.Reset()
	res, err = WriteJSONL(&buf, failing)
	if !errors.Is(err, boom) || res == nil || res.Status != loop.StatusFailed {
		t.Fatalf("WriteJSONL = %+v, %v", res, err)
	}
	lines = strings.Split(strings.TrimSpace(buf.String()), "\n")
	if want := `{"type":"error","code":"run_failed","message":"boom"}`; lines[len(lines)-1] != want {
		t.Fatalf("last line = %s, want %s", lines[len(lines)-1], want)
	}

	empty := func(func(*loop.Event, error) bool) {}
	if _, err := WriteJSONL(&buf, empty); err == nil {
		t.Fatal("a run without a result should fail")
	}
}

func TestWriteJSONLWriteError(t *testing.T) {
	seq := func(yield func(*loop.Event, error) bool) { yield(sampleEvents()[0], nil) }
	if _, err := WriteJSONL(failWriter{}, seq); err == nil {
		t.Fatal("expected the write error")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }
