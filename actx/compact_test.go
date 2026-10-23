package actx_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nostosx/agentx/actx"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
)

func TestClearToolResults(t *testing.T) {
	t.Parallel()
	msgs := toolConversation()
	before := model.CloneMessages(msgs)
	out, err := (actx.ClearToolResults{KeepRecent: 1, MinBytes: 1}).Compact(t.Context(), &actx.CompactInput{Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	assertPairedTools(t, out.Messages)
	for i, m := range out.Messages {
		for _, r := range m.ToolResults() {
			if r.CallID != "recent" && r.Content != actx.ClearedPlaceholder {
				t.Errorf("old result not cleared: %+v", r)
			}
			if r.CallID == "recent" && r.Content != before[i].ToolResults()[0].Content {
				t.Errorf("recent result changed: %+v", r)
			}
		}
	}
	if !reflect.DeepEqual(msgs, before) {
		t.Error("ClearToolResults mutated original conversation")
	}
	unchanged, err := (actx.ClearToolResults{}).Compact(t.Context(), &actx.CompactInput{Messages: msgs})
	if err != nil || !reflect.DeepEqual(unchanged.Messages, before) {
		t.Errorf("default settings should preserve three recent results: %+v, %v", unchanged, err)
	}
	short, err := (actx.ClearToolResults{KeepRecent: 1, MinBytes: 1000}).Compact(t.Context(), &actx.CompactInput{Messages: msgs})
	if err != nil || !reflect.DeepEqual(short.Messages, before) {
		t.Error("short tool results must be retained")
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name                    string
		target, keep, wantTurns int
	}{
		{"keep latest whole turn", 0, 1, 1},
		{"keep two whole turns", 0, 2, 2},
		{"already fits", 10000, 1, 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			msgs := toolConversation()
			before := model.CloneMessages(msgs)
			out, err := (actx.Truncate{KeepRecent: tt.keep}).Compact(t.Context(), &actx.CompactInput{Messages: msgs, Target: tt.target, Counter: actx.EstimateCounter{}})
			if err != nil {
				t.Fatal(err)
			}
			assertPairedTools(t, out.Messages)
			if len(actx.Turns(out.Messages)) != tt.wantTurns {
				t.Errorf("got %d turns, want %d", len(actx.Turns(out.Messages)), tt.wantTurns)
			}
			if out.Messages[len(out.Messages)-1] != msgs[len(msgs)-1] {
				t.Error("latest result not preserved")
			}
			if tt.wantTurns < 3 && !strings.Contains(out.Messages[0].Text(), "earlier turns were removed") {
				t.Error("missing truncation notice")
			}
			if !reflect.DeepEqual(msgs, before) {
				t.Error("Truncate mutated original conversation")
			}
		})
	}
}

func TestSummarize(t *testing.T) {
	t.Parallel()
	msgs := toolConversation()
	before := model.CloneMessages(msgs)
	m := modeltest.New(modeltest.Reply("  Earlier work is complete. \n"))
	out, err := (actx.Summarize{Model: m, KeepRecent: 1, Prompt: "summarize carefully", MaxTokens: 42, MaxResultBytes: 4}).Compact(t.Context(), &actx.CompactInput{Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	assertPairedTools(t, out.Messages)
	if out.Summary != "Earlier work is complete." || len(out.Messages) != 3 || !strings.Contains(out.Messages[0].Text(), "<summary>\nEarlier work is complete.\n</summary>") {
		t.Errorf("Summarize() = %+v", out)
	}
	if !strings.Contains(out.Messages[0].Text(), "recent") {
		t.Error("latest user input lost")
	}
	if !reflect.DeepEqual(msgs, before) {
		t.Error("Summarize mutated original conversation")
	}
	req := m.Requests()[0]
	if req.System != "summarize carefully" || req.MaxTokens != 42 {
		t.Errorf("summary request = %+v", req)
	}
	transcript := req.Messages[0].Text()
	if !strings.Contains(transcript, "old") || !strings.Contains(transcript, "middle") || strings.Contains(transcript, "recent") || !strings.Contains(transcript, "resu ...[truncated]") {
		t.Errorf("summary transcript = %q", transcript)
	}
}

func TestSummarizeErrorsAndEmptyHistory(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("model unavailable")
	for _, tt := range []struct {
		name     string
		strategy actx.Summarize
		want     error
	}{
		{"missing model", actx.Summarize{}, nil},
		{"model error", actx.Summarize{Model: modeltest.New(modeltest.Fail(wantErr))}, wantErr},
		{"empty summary", actx.Summarize{Model: modeltest.New(modeltest.Reply(" \n"))}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.strategy.Compact(t.Context(), &actx.CompactInput{Messages: toolConversation()})
			if err == nil || (tt.want != nil && !errors.Is(err, tt.want)) {
				t.Errorf("error = %v, want failure wrapping %v", err, tt.want)
			}
		})
	}
	m := modeltest.New()
	out, err := (actx.Summarize{Model: m}).Compact(t.Context(), &actx.CompactInput{})
	if err != nil || len(out.Messages) != 0 || len(m.Requests()) != 0 {
		t.Errorf("empty history = %+v, %v", out, err)
	}
	m = modeltest.New(modeltest.Reply("single turn summary"))
	out, err = (actx.Summarize{Model: m}).Compact(t.Context(), &actx.CompactInput{Messages: []*model.Message{model.UserText("single")}})
	if err != nil || len(out.Messages) != 1 || out.Summary != "single turn summary" {
		t.Errorf("single turn = %+v, %v", out, err)
	}
	if req := m.Requests()[0]; req.System != actx.DefaultSummaryPrompt || req.MaxTokens != 4096 {
		t.Errorf("default summary request = %+v", req)
	}
}

func TestTranscript(t *testing.T) {
	t.Parallel()
	msgs := []*model.Message{
		model.SystemText("rules"),
		model.UserText("question"),
		model.NewMessage(model.RoleAssistant, model.TextPart(""), model.ImageURLPart("image"), model.Part{Type: model.PartToolCall, ToolCall: &model.ToolCall{Name: "read", Arguments: []byte("{}")}}),
		model.ToolResults(&model.ToolResult{Name: "read", Content: "failure details", IsError: true}),
		model.NewMessage("custom", model.TextPart("custom content")),
	}
	got := actx.Transcript(msgs, 7)
	for _, want := range []string{"System: rules\n", "User: question\n", "Assistant: [image]\n", "Assistant called tool read with {}\n", "Tool read error: failure ...[truncated]\n", "custom: custom content\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("Transcript() missing %q in %q", want, got)
		}
	}
	if unlimited := actx.Transcript(msgs, 0); !strings.Contains(unlimited, "failure details") {
		t.Error("non-positive limit should preserve complete results")
	}
}

// Results with media are cleared whole, even with short text, since an
// image alone can cost a thousand tokens.
func TestClearToolResultsWithMedia(t *testing.T) {
	t.Parallel()
	call := func(id string) *model.Message {
		return model.NewMessage(model.RoleAssistant, model.Part{Type: model.PartToolCall, ToolCall: &model.ToolCall{ID: id, Name: "read"}})
	}
	image := model.ImagePart("image/png", []byte{1})
	msgs := []*model.Message{
		model.UserText("look"),
		call("old"), model.ToolResults(&model.ToolResult{CallID: "old", Name: "read", Content: "img", Parts: []model.Part{image}}),
		call("new"), model.ToolResults(&model.ToolResult{CallID: "new", Name: "read", Content: "img", Parts: []model.Part{image}}),
		model.AssistantText("seen"),
	}
	out, err := (actx.ClearToolResults{KeepRecent: 1}).Compact(t.Context(), &actx.CompactInput{Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	old, recent := out.Messages[2].ToolResults()[0], out.Messages[4].ToolResults()[0]
	if old.Content != actx.ClearedPlaceholder || old.Parts != nil || len(recent.Parts) != 1 || len(msgs[2].ToolResults()[0].Parts) != 1 {
		t.Fatalf("old = %+v, recent = %+v", old, recent)
	}
	again, err := (actx.ClearToolResults{KeepRecent: 1}).Compact(t.Context(), &actx.CompactInput{Messages: out.Messages})
	if err != nil || !reflect.DeepEqual(again.Messages, out.Messages) {
		t.Fatalf("clearing twice changed the result: %v", err)
	}
	got := actx.Transcript([]*model.Message{
		model.NewMessage(model.RoleUser, model.DocumentPart("application/pdf", nil, "a.pdf"), model.AudioURLPart("x")),
		msgs[4],
	}, 0)
	for _, want := range []string{"User: [document a.pdf]\n", "User: [audio]\n", "Tool read result: img [image]\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("Transcript() missing %q in %q", want, got)
		}
	}
}
