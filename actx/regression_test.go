package actx_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/nostosx/agentx/actx"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
)

func call(id, args string) model.Part {
	return model.Part{Type: model.PartToolCall, ToolCall: &model.ToolCall{ID: id, Name: "tool", Arguments: json.RawMessage(args)}}
}

func result(id, content string) *model.ToolResult {
	return &model.ToolResult{CallID: id, Name: "tool", Content: content}
}

func TestClearToolResultsKeepsUnseenParallelBatch(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", 500)
	msgs := []*model.Message{
		model.UserText("task"),
		model.NewMessage(model.RoleAssistant, call("a", "{}")),
		model.ToolResults(result("a", big)),
		model.NewMessage(model.RoleAssistant, call("p1", "{}"), call("p2", "{}"), call("p3", "{}"), call("p4", "{}"), call("p5", "{}")),
		model.ToolResults(result("p1", big), result("p2", big), result("p3", big), result("p4", big), result("p5", big)),
	}
	out, err := (actx.ClearToolResults{KeepRecent: 1}).Compact(t.Context(), &actx.CompactInput{Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range out.Messages[4].ToolResults() {
		if r.Content == actx.ClearedPlaceholder {
			t.Errorf("unseen result %s cleared", r.CallID)
		}
	}
	if got := out.Messages[2].ToolResults()[0].Content; got != actx.ClearedPlaceholder {
		t.Errorf("seen older result kept: %q", got)
	}

	// The batch is one message for KeepRecent even once the model has seen it.
	seen := append(slices.Clone(msgs), model.AssistantText("done"))
	out, err = (actx.ClearToolResults{KeepRecent: 1}).Compact(t.Context(), &actx.CompactInput{Messages: seen})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range out.Messages[4].ToolResults() {
		if r.Content == actx.ClearedPlaceholder {
			t.Errorf("result %s of the most recent batch cleared", r.CallID)
		}
	}
}

func TestClearToolArguments(t *testing.T) {
	t.Parallel()
	big := `{"content":"` + strings.Repeat("y", 2000) + `"}`
	msgs := []*model.Message{
		model.UserText("task"),
		model.NewMessage(model.RoleAssistant, call("old", big)),
		model.ToolResults(result("old", "ok")),
		model.NewMessage(model.RoleAssistant, model.Part{Type: model.PartToolCall, Signature: "sig", ToolCall: &model.ToolCall{ID: "signed", Name: "tool", Arguments: json.RawMessage(big)}}),
		model.ToolResults(result("signed", "ok")),
		model.NewMessage(model.RoleAssistant, call("recent", big)),
		model.ToolResults(result("recent", "ok")),
	}
	out, err := (actx.ClearToolArguments{KeepRecent: 1}).Compact(t.Context(), &actx.CompactInput{Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	assertPairedTools(t, out.Messages)
	if got := string(out.Messages[1].ToolCalls()[0].Arguments); got != actx.ClearedArguments || !json.Valid([]byte(got)) {
		t.Errorf("old arguments = %.40q", got)
	}
	if string(out.Messages[3].ToolCalls()[0].Arguments) != big {
		t.Error("signed call changed")
	}
	if string(out.Messages[5].ToolCalls()[0].Arguments) != big {
		t.Error("recent call changed")
	}
	if string(msgs[1].ToolCalls()[0].Arguments) != big {
		t.Error("input mutated")
	}
}

// longSingleTurn mimics an agent writing many large files within one task.
func longSingleTurn(n int) []*model.Message {
	msgs := []*model.Message{model.SystemText("Always answer in French"), model.UserText("write the project")}
	for i := range n {
		id := fmt.Sprintf("w%d", i)
		args := `{"path":"f` + id + `","content":"` + strings.Repeat("z", 40_000) + `"}`
		msgs = append(msgs, model.NewMessage(model.RoleAssistant, call(id, args)), model.ToolResults(result(id, "written")))
	}
	return msgs
}

func TestManagerCompactsLongSingleTurn(t *testing.T) {
	t.Parallel()
	m := actx.NewManager(actx.Config{})
	msgs := longSingleTurn(20)
	res, err := m.Compact(t.Context(), "", msgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.After >= res.Before || res.After > m.Window().Target() {
		t.Fatalf("compaction %d -> %d, target %d", res.Before, res.After, m.Window().Target())
	}
	assertPairedTools(t, res.Messages)
	if res.Messages[0].Text() != "Always answer in French" || !strings.Contains(res.Messages[1].Text(), "write the project") {
		t.Errorf("system or task message lost: %q, %q", res.Messages[0].Text(), res.Messages[1].Text())
	}
}

func TestTruncateDropsStepsWithinTurn(t *testing.T) {
	t.Parallel()
	msgs := longSingleTurn(5)
	out, err := (actx.Truncate{}).Compact(t.Context(), &actx.CompactInput{Messages: msgs, Target: 25_000, Counter: actx.EstimateCounter{}})
	if err != nil {
		t.Fatal(err)
	}
	assertPairedTools(t, out.Messages)
	if n := (actx.EstimateCounter{}).CountMessages(out.Messages); n > 25_000 {
		t.Errorf("result has %d tokens", n)
	}
	if out.Messages[0].Role != model.RoleSystem || out.Messages[1].Role != model.RoleUser || !strings.Contains(out.Messages[1].Text(), "earlier steps were removed") || !strings.Contains(out.Messages[1].Text(), "write the project") {
		t.Errorf("unexpected head: %+v %+v", out.Messages[0], out.Messages[1])
	}
	if out.Messages[len(out.Messages)-1] != msgs[len(msgs)-1] {
		t.Error("latest step not kept")
	}
}

func TestStrategiesPinSystemMessages(t *testing.T) {
	t.Parallel()
	msgs := append([]*model.Message{model.SystemText("Always answer in French")}, toolConversation()...)
	out, err := (actx.Truncate{}).Compact(t.Context(), &actx.CompactInput{Messages: msgs, Target: 0, Counter: actx.EstimateCounter{}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Messages[0].Text() != "Always answer in French" {
		t.Errorf("Truncate dropped the system message: %+v", out.Messages[0])
	}
	m := modeltest.New(modeltest.Reply("summary"))
	out, err = (actx.Summarize{Model: m, KeepRecent: 1}).Compact(t.Context(), &actx.CompactInput{Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	if out.Messages[0].Text() != "Always answer in French" || out.Messages[1].Role != model.RoleUser {
		t.Errorf("Summarize dropped the system message: %+v", out.Messages)
	}
	if strings.Contains(m.Requests()[0].Messages[0].Text(), "French") {
		t.Error("system message was summarized")
	}
}

func TestTranscriptKeepsUTF8(t *testing.T) {
	t.Parallel()
	msgs := []*model.Message{model.ToolResults(&model.ToolResult{Name: "read", Content: "日本語のテキスト"})}
	for n := 1; n < 10; n++ {
		if got := actx.Transcript(msgs, n); !utf8.ValidString(got) {
			t.Errorf("Transcript(%d) = %q is not valid UTF-8", n, got)
		}
	}
}

func TestRedactedReasoningCounts(t *testing.T) {
	t.Parallel()
	msg := model.NewMessage(model.RoleAssistant, model.Part{Type: model.PartReasoning, Redacted: true, Data: []byte(strings.Repeat("e", 4000))})
	if n := (actx.EstimateCounter{}).CountMessages([]*model.Message{msg}); n < 1000 {
		t.Errorf("redacted reasoning counted as %d tokens", n)
	}
}

func TestNewManagerKeepsWindowFields(t *testing.T) {
	t.Parallel()
	w := actx.NewManager(actx.Config{Window: actx.Window{ReserveOutput: 50_000, Threshold: 0.6}}).Window()
	if w.Limit != actx.DefaultWindow.Limit || w.ReserveOutput != 50_000 || w.Threshold != 0.6 || w.TargetRatio != actx.DefaultWindow.TargetRatio {
		t.Errorf("Window() = %+v", w)
	}
}

func TestMiddlewareSkipsFutileCompaction(t *testing.T) {
	t.Parallel()
	manager := actx.NewManager(actx.Config{Window: actx.Window{Limit: 100, Threshold: 0.5}, Strategy: actx.StrategyFunc(func(_ context.Context, in *actx.CompactInput) (*actx.CompactOutput, error) {
		return &actx.CompactOutput{Messages: append(model.CloneMessages(in.Messages), model.UserText("padding"))}, nil
	})})
	m := modeltest.New(modeltest.Reply("answer"))
	r, err := loop.New(loop.Config{Model: m, Middlewares: []loop.Middleware{actx.Middleware(manager)}})
	if err != nil {
		t.Fatal(err)
	}
	history := []*model.Message{model.UserText(strings.Repeat("old ", 100)), model.AssistantText("old answer")}
	compactions := 0
	_, err = loop.Collect(r.Run(t.Context(), &loop.Request{History: history, Input: model.UserText("new")}), func(ev *loop.Event) {
		if ev.Type == loop.EventCompacted {
			compactions++
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if compactions != 0 || len(m.Requests()[0].Messages) != 3 {
		t.Errorf("futile compaction applied: events=%d messages=%d", compactions, len(m.Requests()[0].Messages))
	}
}

func TestManagerObserveSmooths(t *testing.T) {
	t.Parallel()
	m := actx.NewManager(actx.Config{Counter: byteCounter{}})
	m.Observe("1234567890", nil, nil, model.Usage{InputTokens: 20})
	m.Observe("1234567890", nil, nil, model.Usage{InputTokens: 10})
	if got := m.Count("1234567890", nil, nil); got != 15 {
		t.Errorf("Count() = %d, want 15", got)
	}
}

func signedReasoning(sig string) model.Part {
	return model.Part{Type: model.PartReasoning, Text: "thinking", Signature: sig}
}

func TestTruncateKeepsSignedReasoningOfCurrentTurn(t *testing.T) {
	t.Parallel()
	for _, reasoning := range []model.Part{
		signedReasoning("sig"),
		{Type: model.PartReasoning, Redacted: true, Data: []byte("encrypted")},
	} {
		msgs := []*model.Message{
			model.UserText("task"),
			model.NewMessage(model.RoleAssistant, reasoning, call("c1", "{}")),
			model.ToolResults(result("c1", strings.Repeat("r", 20_000))),
			model.NewMessage(model.RoleAssistant, call("c2", "{}")),
			model.ToolResults(result("c2", "ok")),
		}
		m := actx.NewManager(actx.Config{Window: actx.Window{Limit: 6000}})
		res, err := m.Compact(t.Context(), "", msgs, nil)
		if err != nil {
			t.Fatal(err)
		}
		assertPairedTools(t, res.Messages)
		var first *model.Message
		for _, msg := range res.Messages {
			if msg.Role == model.RoleAssistant {
				first = msg
				break
			}
		}
		if first == nil || first.Parts[0].Type != model.PartReasoning || first.Parts[0].Signature != reasoning.Signature || first.Parts[0].Redacted != reasoning.Redacted {
			t.Fatalf("the current turn no longer starts with its reasoning: %+v", res.Messages)
		}
		if res.After > m.Window().Target() {
			t.Errorf("compaction %d -> %d, target %d", res.Before, res.After, m.Window().Target())
		}
		if got := res.Messages[2].ToolResults()[0].Content; got != actx.ClearedPlaceholder {
			t.Errorf("large result of the reasoning step = %.20q, want it cleared", got)
		}
		if msgs[2].ToolResults()[0].Content == actx.ClearedPlaceholder {
			t.Error("input mutated")
		}
	}

	// Completed turns and turns without signed reasoning still lose steps.
	msgs := []*model.Message{
		model.UserText("task"),
		model.NewMessage(model.RoleAssistant, call("c1", "{}")),
		model.ToolResults(result("c1", strings.Repeat("r", 20_000))),
		model.NewMessage(model.RoleAssistant, call("c2", "{}")),
		model.ToolResults(result("c2", "ok")),
	}
	out, err := (actx.Truncate{}).Compact(t.Context(), &actx.CompactInput{Messages: msgs, Target: 1000, Counter: actx.EstimateCounter{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 3 {
		t.Errorf("unsigned step kept: %d messages", len(out.Messages))
	}
}

func TestEstimateCounterCountsOpaqueReasoningAndCallSignatures(t *testing.T) {
	t.Parallel()
	opaque := json.RawMessage(`{"type":"reasoning","encrypted_content":"` + strings.Repeat("e", 4000) + `"}`)
	reasoning := model.NewMessage(model.RoleAssistant, model.Part{Type: model.PartReasoning, Provider: "openai", Raw: opaque})
	if n := (actx.EstimateCounter{}).CountMessages([]*model.Message{reasoning}); n < 1000 {
		t.Errorf("encrypted reasoning in Raw counted as %d tokens", n)
	}
	signed := call("c", "{}")
	signed.Signature = strings.Repeat("s", 4000)
	if n := (actx.EstimateCounter{}).CountMessages([]*model.Message{model.NewMessage(model.RoleAssistant, signed)}); n < 500 {
		t.Errorf("tool call signature counted as %d tokens", n)
	}
}

func TestClearToolArgumentsSkipsSignedSteps(t *testing.T) {
	t.Parallel()
	big := `{"content":"` + strings.Repeat("y", 2000) + `"}`
	signed := call("g1", big)
	signed.Signature = "gemini-sig"
	msgs := []*model.Message{
		model.UserText("task"),
		// A Gemini parallel step signs only its first call.
		model.NewMessage(model.RoleAssistant, signed, call("g2", big)),
		model.ToolResults(result("g1", "ok"), result("g2", "ok")),
		// Anthropic signs the reasoning that precedes the call.
		model.NewMessage(model.RoleAssistant, signedReasoning("sig"), call("a1", big)),
		model.ToolResults(result("a1", "ok")),
		model.NewMessage(model.RoleAssistant, call("old", big)),
		model.ToolResults(result("old", "ok")),
		model.NewMessage(model.RoleAssistant, call("recent", big)),
		model.ToolResults(result("recent", "ok")),
	}
	out, err := (actx.ClearToolArguments{KeepRecent: 1}).Compact(t.Context(), &actx.CompactInput{Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{1, 3} {
		for _, c := range out.Messages[i].ToolCalls() {
			if string(c.Arguments) != big {
				t.Errorf("call %s in a signed step changed", c.ID)
			}
		}
	}
	if got := string(out.Messages[5].ToolCalls()[0].Arguments); got != actx.ClearedArguments {
		t.Errorf("unsigned old call = %.40q", got)
	}
}

func TestTruncateKeepsTaskBeforeStopReminder(t *testing.T) {
	t.Parallel()
	msgs := []*model.Message{
		model.UserText("original task"),
		model.NewMessage(model.RoleAssistant, call("c1", "{}")),
		model.ToolResults(result("c1", strings.Repeat("r", 20_000))),
		model.AssistantText("done"),
		// A Stop hook or a SubagentStop hook continues the run like this.
		model.UserText(model.WrapReminder("A Stop hook prevented you from finishing:\nrun the tests")),
		model.NewMessage(model.RoleAssistant, call("c2", "{}")),
		model.ToolResults(result("c2", "ok")),
	}
	if got := len(actx.Turns(msgs)); got != 1 {
		t.Errorf("Turns() split a reminder continuation into %d turns", got)
	}
	out, err := (actx.Truncate{KeepRecent: 1}).Compact(t.Context(), &actx.CompactInput{Messages: msgs, Target: 1000, Counter: actx.EstimateCounter{}})
	if err != nil {
		t.Fatal(err)
	}
	assertPairedTools(t, out.Messages)
	if !strings.Contains(out.Messages[0].Text(), "original task") {
		t.Errorf("original task dropped: %q", out.Messages[0].Text())
	}
	if out.Messages[len(out.Messages)-1] != msgs[len(msgs)-1] {
		t.Error("latest step not kept")
	}

	// A user message with text of its own still starts a turn.
	mixed := []*model.Message{
		model.UserText("first"),
		model.AssistantText("answer"),
		model.NewMessage(model.RoleUser, model.TextPart(model.WrapReminder("recalled memory")), model.TextPart("second")),
	}
	if got := len(actx.Turns(mixed)); got != 2 {
		t.Errorf("Turns() = %d turns, want 2", got)
	}
}

func TestMiddlewareWarnsWhenCompactionCannotFit(t *testing.T) {
	t.Parallel()
	manager := actx.NewManager(actx.Config{Window: actx.Window{Limit: 100, Threshold: 0.5}, Strategy: actx.StrategyFunc(func(_ context.Context, in *actx.CompactInput) (*actx.CompactOutput, error) {
		return &actx.CompactOutput{Messages: in.Messages[1:]}, nil
	})})
	var logs strings.Builder
	m := modeltest.New(modeltest.Reply("answer"))
	r, err := loop.New(loop.Config{Model: m, Logger: slog.New(slog.NewTextHandler(&syncWriter{w: &logs}, nil)), Middlewares: []loop.Middleware{actx.Middleware(manager)}})
	if err != nil {
		t.Fatal(err)
	}
	history := []*model.Message{model.UserText(strings.Repeat("old ", 100)), model.AssistantText(strings.Repeat("big ", 200))}
	if _, err := loop.Collect(r.Run(t.Context(), &loop.Request{History: history, Input: model.UserText("new")}), nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "context window") {
		t.Errorf("no warning logged: %s", logs.String())
	}
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
