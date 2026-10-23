package actx

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"github.com/nostosx/agentx/model"
)

// TokenCounter estimates the token size of model input.
type TokenCounter interface {
	CountText(s string) int
	CountMessages(msgs []*model.Message) int
}

// EstimateCounter is a dependency-free heuristic counter. ASCII text counts
// as one token per four bytes and every other rune as one token, which is a
// reasonable upper estimate for CJK text. Use it when exact counts are not
// required; Manager calibrates it against reported usage.
type EstimateCounter struct{}

// CountText estimates the tokens in s.
func (EstimateCounter) CountText(s string) int { return EstimateTokens(s) }

// CountMessages estimates the tokens in msgs, including per-message framing.
func (EstimateCounter) CountMessages(msgs []*model.Message) int {
	n := 0
	for _, m := range msgs {
		n += 4
		for _, p := range m.Parts {
			n += partTokens(p)
		}
	}
	return n
}

func partTokens(p model.Part) int {
	switch p.Type {
	case model.PartText:
		return EstimateTokens(p.Text) + len(p.Signature)/8
	case model.PartReasoning:
		// Redacted reasoning (Anthropic) is carried in Data and encrypted
		// reasoning items (OpenAI Responses) in Raw. Both are sent back to the
		// provider, so they count like opaque text.
		return EstimateTokens(p.Text) + len(p.Signature)/8 + len(p.Data)/4 + len(p.Raw)/4
	case model.PartImage, model.PartDocument, model.PartAudio, model.PartVideo:
		return mediaTokens(p)
	case model.PartToolCall:
		if p.ToolCall == nil {
			return 0
		}
		// Gemini thought signatures on function calls can be large.
		return 8 + EstimateTokens(p.ToolCall.Name) + EstimateTokens(string(p.ToolCall.Arguments)) + len(p.Signature)/8
	case model.PartToolResult:
		if p.ToolResult == nil {
			return 0
		}
		n := 8 + EstimateTokens(p.ToolResult.Content)
		for _, rp := range p.ToolResult.Parts {
			n += partTokens(rp)
		}
		return n
	case model.PartRaw:
		return len(p.Raw) / 4
	}
	return 0
}

// Media estimates. Images cost roughly a thousand tokens on most
// providers. Document pages cost about 1,500 to 3,000 tokens of text plus
// an image. Gemini bills audio at 32 tokens a second, about 500 bytes of
// compressed audio, and video at about 300 tokens a second, about 400
// bytes of compressed video. Media referenced by URL or file ID assume a
// page, a minute of audio or 20 seconds of video.
const (
	imageTokens     = 1000
	pageTokens      = 2500
	pageBytes       = 50_000
	audioBytesToken = 500
	videoBytesToken = 400
	audioTokens     = 32 * 60
	videoTokens     = 300 * 20
)

func mediaTokens(p model.Part) int {
	n := len(p.Data)
	switch p.Type {
	case model.PartImage:
		return imageTokens
	case model.PartAudio:
		if n == 0 {
			return audioTokens
		}
		return max(n/audioBytesToken, 1)
	case model.PartVideo:
		if n == 0 {
			return videoTokens
		}
		return max(n/videoBytesToken, 1)
	}
	if n == 0 {
		return pageTokens
	}
	if strings.HasPrefix(p.MediaType, "text/") {
		return EstimateTokens(string(p.Data))
	}
	// Count the pages of a PDF by their page objects; compressed object
	// streams hide them, so fall back to the size.
	pages := bytes.Count(p.Data, []byte("/Type /Page")) - bytes.Count(p.Data, []byte("/Type /Pages")) +
		bytes.Count(p.Data, []byte("/Type/Page")) - bytes.Count(p.Data, []byte("/Type/Pages"))
	if pages <= 0 {
		pages = n/pageBytes + 1
	}
	return pages * pageTokens
}

// EstimateTokens estimates the tokens in s.
func EstimateTokens(s string) int {
	ascii, other := 0, 0
	for _, r := range s {
		if r < utf8.RuneSelf {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+3)/4 + other
}

// Window describes the model's context window and when to compact.
type Window struct {
	// Limit is the model's context window in tokens.
	Limit int
	// ReserveOutput is kept free for the response.
	ReserveOutput int
	// Threshold is the fraction of the usable window at which automatic
	// compaction starts. Defaults to 0.8.
	Threshold float64
	// TargetRatio is the fraction of the usable window that compaction aims
	// for. Defaults to 0.5.
	TargetRatio float64
}

// DefaultWindow suits models with a 200K-token context window.
var DefaultWindow = Window{Limit: 200_000, ReserveOutput: 16_000, Threshold: 0.8, TargetRatio: 0.5}

func (w Window) normalized() Window {
	if w.Limit <= 0 {
		w.Limit = DefaultWindow.Limit
	}
	if w.ReserveOutput < 0 || w.ReserveOutput >= w.Limit {
		w.ReserveOutput = w.Limit / 10
	}
	if w.Threshold <= 0 || w.Threshold > 1 {
		w.Threshold = DefaultWindow.Threshold
	}
	if w.TargetRatio <= 0 || w.TargetRatio >= w.Threshold {
		w.TargetRatio = min(DefaultWindow.TargetRatio, w.Threshold/2)
	}
	return w
}

// Usable returns the tokens available for input.
func (w Window) Usable() int {
	w = w.normalized()
	return w.Limit - w.ReserveOutput
}

// ShouldCompact reports whether input of the given size should be compacted.
func (w Window) ShouldCompact(tokens int) bool {
	n := w.normalized()
	return float64(tokens) >= float64(n.Usable())*n.Threshold
}

// Target returns the input size compaction aims for.
func (w Window) Target() int {
	n := w.normalized()
	return int(float64(n.Usable()) * n.TargetRatio)
}
