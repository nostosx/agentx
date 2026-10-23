package actx_test

import (
	"testing"

	"github.com/nostosx/agentx/actx"
	"github.com/nostosx/agentx/model"
)

func TestEstimateTokens(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, text string
		want       int
	}{
		{"empty", "", 0},
		{"ascii rounds up", "abcde", 2},
		{"cjk", "上下文", 3},
		{"mixed unicode", "hello世界🙂", 5},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := actx.EstimateTokens(tt.text); got != tt.want {
				t.Errorf("EstimateTokens(%q) = %d, want %d", tt.text, got, tt.want)
			}
		})
	}
}

func TestEstimateCounterCountMessages(t *testing.T) {
	t.Parallel()
	msg := model.NewMessage(model.RoleAssistant,
		model.TextPart("abcd"),
		model.Part{Type: model.PartReasoning, Text: "分析", Signature: "12345678"},
		model.ImageURLPart("https://example.invalid/image"),
		model.Part{Type: model.PartToolCall, ToolCall: &model.ToolCall{Name: "read", Arguments: []byte("{}")}},
		model.Part{Type: model.PartToolResult, ToolResult: &model.ToolResult{Content: "done"}},
		model.Part{Type: model.PartRaw, Raw: []byte("12345678")},
		model.Part{Type: model.PartToolCall},
		model.Part{Type: model.PartToolResult},
	)
	// Include framing, reasoning signatures, image cost, and tool overhead.
	if got, want := (actx.EstimateCounter{}).CountMessages([]*model.Message{msg}), 1029; got != want {
		t.Errorf("CountMessages() = %d, want %d", got, want)
	}
}

func TestWindow(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name           string
		window         actx.Window
		usable, target int
		threshold      int
	}{
		{"configured", actx.Window{Limit: 100, ReserveOutput: 20, Threshold: 0.75, TargetRatio: 0.5}, 80, 40, 60},
		{"zero defaults", actx.Window{}, 200000, 80000, 160000},
		{"invalid reserve", actx.Window{Limit: 100, ReserveOutput: 100}, 90, 36, 72},
		{"invalid ratios", actx.Window{Limit: 100, Threshold: 2, TargetRatio: 3}, 100, 40, 80},
		{"target below threshold", actx.Window{Limit: 100, Threshold: 0.2, TargetRatio: 0.3}, 100, 10, 20},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.window.Usable(); got != tt.usable {
				t.Errorf("Usable() = %d, want %d", got, tt.usable)
			}
			if got := tt.window.Target(); got != tt.target {
				t.Errorf("Target() = %d, want %d", got, tt.target)
			}
			if tt.window.ShouldCompact(tt.threshold-1) || !tt.window.ShouldCompact(tt.threshold) {
				t.Errorf("compaction boundary must be inclusive at %d", tt.threshold)
			}
		})
	}
}

func TestEstimateMediaTokens(t *testing.T) {
	t.Parallel()
	twoPages := []byte("%PDF-1.7\n<< /Type /Pages >>\n<< /Type /Page >>\n<</Type/Page>>")
	for _, tc := range []struct {
		name string
		part model.Part
		want int
	}{
		{"image", model.ImagePart("image/png", make([]byte, 50_000)), 1000},
		{"pdf pages", model.DocumentPart("application/pdf", twoPages, ""), 5000},
		{"pdf by size", model.DocumentPart("application/pdf", make([]byte, 120_000), ""), 7500},
		{"text document", model.TextDocumentPart("t", "abcdefgh"), 2},
		{"document by URL", model.DocumentURLPart("https://example.com/a.pdf"), 2500},
		{"audio", model.AudioPart("audio/mpeg", make([]byte, 16_000)), 32},
		{"audio by URL", model.AudioURLPart("gs://b/a.mp3"), 1920},
		{"video", model.VideoPart("video/mp4", make([]byte, 4000)), 10},
		{"video by URL", model.VideoURLPart("https://youtu.be/x"), 6000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Four tokens frame each message.
			if got := (actx.EstimateCounter{}).CountMessages([]*model.Message{model.NewMessage(model.RoleUser, tc.part)}) - 4; got != tc.want {
				t.Errorf("tokens = %d, want %d", got, tc.want)
			}
		})
	}
	result := model.ToolResults(&model.ToolResult{Content: "abcd", Parts: []model.Part{model.ImagePart("image/png", []byte{1})}})
	if got := (actx.EstimateCounter{}).CountMessages([]*model.Message{result}); got != 4+8+1+1000 {
		t.Errorf("tool result tokens = %d", got)
	}
}
