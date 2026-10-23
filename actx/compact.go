package actx

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/nostosx/agentx/model"
)

// ClearedPlaceholder replaces the content of cleared tool results.
const ClearedPlaceholder = "[tool result cleared to save context]"

// ClearedArguments replaces the arguments of cleared tool calls. It is a JSON
// object, so every provider accepts it as tool input.
const ClearedArguments = `{"cleared":"[tool call arguments cleared to save context]"}`

// ClearToolResults replaces the content of older tool results with a
// placeholder, keeping the most recent KeepRecent tool result messages
// intact. It is lossless with respect to conversation structure and usually
// the cheapest strategy.
//
// Results the model has not seen yet, those after the last assistant
// message, are never cleared, so a large batch of parallel calls is read by
// the model before any of it is cleared. The trade-off is that no strategy
// in the default chain shrinks such a batch: Truncate also keeps the last
// step of a turn. If one unseen batch alone exceeds the window, compaction
// cannot fit the conversation, the middleware logs a warning and the
// request may fail. Bound tool output, for example with tool.TruncateOutput,
// when tools can return results that large.
type ClearToolResults struct {
	// KeepRecent is the number of recent tool result messages kept intact.
	// All results of one batch of parallel calls share a message. Defaults
	// to 3.
	KeepRecent int
	// MinBytes skips results shorter than this. Defaults to 256.
	MinBytes int
}

// Compact implements Strategy.
func (s ClearToolResults) Compact(_ context.Context, in *CompactInput) (*CompactOutput, error) {
	keep := s.KeepRecent
	if keep <= 0 {
		keep = 3
	}
	minBytes := s.MinBytes
	if minBytes <= 0 {
		minBytes = 256
	}
	unseen := lastAssistant(in.Messages) + 1
	seen := 0
	out := slices.Clone(in.Messages)
	for i, m := range slices.Backward(out) {
		if len(m.ToolResults()) == 0 {
			continue
		}
		seen++
		if i >= unseen || seen <= keep {
			continue
		}
		var cloned *model.Message
		for j, p := range m.Parts {
			if p.Type != model.PartToolResult || p.ToolResult == nil {
				continue
			}
			if (len(p.ToolResult.Content) < minBytes && len(p.ToolResult.Parts) == 0) || cleared(p.ToolResult) {
				continue
			}
			if cloned == nil {
				cloned = m.Clone()
			}
			clearResult(cloned.Parts[j].ToolResult)
		}
		if cloned != nil {
			out[i] = cloned
		}
	}
	return &CompactOutput{Messages: out}, nil
}

// ClearToolArguments replaces the large arguments of older, already
// answered tool calls with ClearedArguments, keeping the calls of the most
// recent KeepRecent assistant messages intact. It shrinks long agentic runs
// whose size comes from what the model wrote, such as file contents passed
// to a write tool, rather than from tool results.
//
// Calls without a result are never changed, so tool calls and results stay
// paired. Assistant messages with any signed part, such as a signed tool
// call (Gemini signs the first call of a parallel step) or signed or
// redacted reasoning (Anthropic), are left whole, so no call in a signed
// step is rewritten.
type ClearToolArguments struct {
	// KeepRecent is the number of recent assistant messages with tool calls
	// kept intact. Defaults to 3.
	KeepRecent int
	// MinBytes skips arguments shorter than this. Defaults to 1024.
	MinBytes int
}

// Compact implements Strategy.
func (s ClearToolArguments) Compact(_ context.Context, in *CompactInput) (*CompactOutput, error) {
	keep := s.KeepRecent
	if keep <= 0 {
		keep = 3
	}
	minBytes := s.MinBytes
	if minBytes <= 0 {
		minBytes = 1024
	}
	answered := map[string]bool{}
	for _, m := range in.Messages {
		for _, r := range m.ToolResults() {
			answered[r.CallID] = true
		}
	}
	seen := 0
	out := slices.Clone(in.Messages)
	for i, m := range slices.Backward(out) {
		if m.Role != model.RoleAssistant || len(m.ToolCalls()) == 0 {
			continue
		}
		seen++
		if seen <= keep || isSigned(m) {
			continue
		}
		var cloned *model.Message
		for j, p := range m.Parts {
			if p.Type != model.PartToolCall || p.ToolCall == nil || !answered[p.ToolCall.ID] {
				continue
			}
			if len(p.ToolCall.Arguments) < minBytes || string(p.ToolCall.Arguments) == ClearedArguments {
				continue
			}
			if cloned == nil {
				cloned = m.Clone()
			}
			cloned.Parts[j].ToolCall.Arguments = []byte(ClearedArguments)
		}
		if cloned != nil {
			out[i] = cloned
		}
	}
	return &CompactOutput{Messages: out}, nil
}

// isSigned reports whether m has a part a provider signed, or redacted
// reasoning, which the provider verifies when it is sent back.
func isSigned(m *model.Message) bool {
	return slices.ContainsFunc(m.Parts, func(p model.Part) bool {
		return p.Signature != "" || p.Redacted
	})
}

// hasSignedReasoning reports whether a message of step carries signed or
// redacted reasoning.
func hasSignedReasoning(step []*model.Message) bool {
	return slices.ContainsFunc(step, func(m *model.Message) bool {
		return m.Role == model.RoleAssistant && slices.ContainsFunc(m.Parts, func(p model.Part) bool {
			return p.Type == model.PartReasoning && (p.Signature != "" || p.Redacted)
		})
	})
}

// inProgress reports whether turn ends with tool results, so the model
// continues it in the next request.
func inProgress(turn []*model.Message) bool {
	return len(turn) > 0 && len(turn[len(turn)-1].ToolResults()) > 0
}

// clearResults returns step with the content of its tool results replaced
// by ClearedPlaceholder, cloning the messages it changes.
func clearResults(step []*model.Message) ([]*model.Message, bool) {
	out := slices.Clone(step)
	changed := false
	for i, m := range out {
		var cloned *model.Message
		for j, p := range m.Parts {
			if p.Type != model.PartToolResult || p.ToolResult == nil || cleared(p.ToolResult) {
				continue
			}
			if cloned == nil {
				cloned = m.Clone()
			}
			clearResult(cloned.Parts[j].ToolResult)
		}
		if cloned != nil {
			out[i], changed = cloned, true
		}
	}
	return out, changed
}

// cleared reports whether r has been cleared.
func cleared(r *model.ToolResult) bool {
	return r.Content == ClearedPlaceholder && len(r.Parts) == 0
}

// clearResult replaces the content of r, including its parts, such as
// images, with ClearedPlaceholder.
func clearResult(r *model.ToolResult) {
	r.Content, r.Parts = ClearedPlaceholder, nil
}

// lastAssistant returns the index of the last assistant message, or -1.
func lastAssistant(msgs []*model.Message) int {
	for i, m := range slices.Backward(msgs) {
		if m.Role == model.RoleAssistant {
			return i
		}
	}
	return -1
}

// Truncate drops the oldest turns until the conversation fits the target.
// The most recent KeepRecent turns are not dropped as a whole, but if they
// still do not fit, it drops the oldest steps inside them, where a step is
// an assistant message together with the tool results that answer it. The
// user message that starts each turn and the last step of each turn are
// kept, and a tool call is never separated from its result.
//
// In the turn still in progress, the last one when it ends with tool
// results, the first step with signed or redacted reasoning is kept too:
// with Anthropic extended thinking the current assistant turn must start
// with its thinking block. If the conversation still does not fit, that
// step's tool results are cleared instead.
//
// System messages are never dropped.
type Truncate struct {
	// KeepRecent defaults to 1.
	KeepRecent int
}

// Compact implements Strategy.
func (s Truncate) Compact(_ context.Context, in *CompactInput) (*CompactOutput, error) {
	keep := max(s.KeepRecent, 1)
	system, rest := splitSystem(in.Messages)
	turns := Turns(rest)
	total := in.Counter.CountMessages(in.Messages)
	dropped := 0
	for len(turns)-dropped > keep && total > in.Target {
		total -= in.Counter.CountMessages(turns[dropped])
		dropped++
	}
	kept := slices.Clone(turns[dropped:])
	removedSteps, changed := 0, false
	for i := 0; i < len(kept) && total > in.Target; i++ {
		head, st := steps(kept[i])
		pin := -1
		if i == len(kept)-1 && inProgress(kept[i]) {
			pin = slices.IndexFunc(st, hasSignedReasoning)
		}
		var keepSteps [][]*model.Message
		n := 0
		for j, step := range st {
			if j < len(st)-1 && j != pin && total > in.Target {
				total -= in.Counter.CountMessages(step)
				n++
				continue
			}
			keepSteps = append(keepSteps, step)
		}
		if pin >= 0 && pin < len(st)-1 && total > in.Target {
			// The pinned step is never the last one, so the model has seen
			// its results.
			if cleared, ok := clearResults(st[pin]); ok {
				total += in.Counter.CountMessages(cleared) - in.Counter.CountMessages(st[pin])
				keepSteps[0] = cleared
				changed = true
			}
		}
		if n > 0 || changed {
			kept[i] = slices.Concat(head, flatten(keepSteps))
			removedSteps += n
		}
	}
	if dropped == 0 && removedSteps == 0 {
		if changed {
			return &CompactOutput{Messages: slices.Concat(system, flatten(kept))}, nil
		}
		return &CompactOutput{Messages: in.Messages}, nil
	}
	var note string
	switch {
	case removedSteps == 0:
		note = fmt.Sprintf("[%d earlier turns were removed to fit the context window]", dropped)
	case dropped == 0:
		note = fmt.Sprintf("[%d earlier steps were removed to fit the context window]", removedSteps)
	default:
		note = fmt.Sprintf("[%d earlier turns and %d earlier steps were removed to fit the context window]", dropped, removedSteps)
	}
	return &CompactOutput{Messages: slices.Concat(system, prependNote(flatten(kept), note))}, nil
}

// DefaultSummaryPrompt instructs the model how to summarize a conversation.
const DefaultSummaryPrompt = `You are compacting the history of an agent session so work can continue in a fresh context window.
Write a concise but complete summary of the conversation transcript below. Preserve:
- the user's goals, explicit requests and constraints, quoted exactly where wording matters
- decisions made and their reasons
- files, identifiers, commands, URLs and other concrete details that later steps depend on
- tool results that are still relevant, and errors encountered with how they were resolved
- the current state of the work and the next steps
Do not invent information. Write the summary as plain text without preamble.`

// Summarize replaces older turns with a model-written summary, keeping the
// most recent KeepRecent turns verbatim. System messages are kept verbatim
// and are not summarized.
type Summarize struct {
	Model model.Model
	// KeepRecent defaults to 2.
	KeepRecent int
	// Prompt defaults to DefaultSummaryPrompt.
	Prompt string
	// MaxTokens bounds the summary. Defaults to 4096.
	MaxTokens int
	// MaxResultBytes truncates each tool result in the transcript. Defaults
	// to 2000.
	MaxResultBytes int
}

// Compact implements Strategy.
func (s Summarize) Compact(ctx context.Context, in *CompactInput) (*CompactOutput, error) {
	if s.Model == nil {
		return nil, errors.New("actx: Summarize requires a model")
	}
	keep := s.KeepRecent
	if keep <= 0 {
		keep = 2
	}
	system, rest := splitSystem(in.Messages)
	turns := Turns(rest)
	if len(turns) <= keep {
		keep = len(turns) - 1
	}
	if keep < 0 || len(turns) == 0 {
		return &CompactOutput{Messages: in.Messages}, nil
	}
	old, recent := turns[:len(turns)-keep], turns[len(turns)-keep:]
	if len(old) == 0 {
		return &CompactOutput{Messages: in.Messages}, nil
	}
	summary, err := s.summarize(ctx, flatten(old))
	if err != nil {
		return nil, fmt.Errorf("summarize conversation: %w", err)
	}
	note := "This session continues from an earlier conversation that was compacted. " +
		"Summary of the earlier conversation:\n<summary>\n" + summary + "\n</summary>"
	return &CompactOutput{Messages: slices.Concat(system, prependNote(flatten(recent), note)), Summary: summary}, nil
}

func (s Summarize) summarize(ctx context.Context, msgs []*model.Message) (string, error) {
	prompt := s.Prompt
	if prompt == "" {
		prompt = DefaultSummaryPrompt
	}
	maxTokens := s.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	limit := s.MaxResultBytes
	if limit <= 0 {
		limit = 2000
	}
	req := &model.Request{
		System:    prompt,
		Messages:  []*model.Message{model.UserText("<transcript>\n" + Transcript(msgs, limit) + "\n</transcript>")},
		MaxTokens: maxTokens,
	}
	resp, err := s.Model.Generate(ctx, req)
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(resp.Text())
	if text == "" {
		return "", errors.New("model returned an empty summary")
	}
	return text, nil
}

// Transcript renders messages as plain text. Tool results longer than
// maxResultBytes are truncated.
func Transcript(msgs []*model.Message, maxResultBytes int) string {
	var b strings.Builder
	for _, m := range msgs {
		for _, p := range m.Parts {
			switch p.Type {
			case model.PartText:
				if p.Text == "" {
					continue
				}
				fmt.Fprintf(&b, "%s: %s\n", roleLabel(m.Role), p.Text)
			case model.PartImage, model.PartDocument, model.PartAudio, model.PartVideo:
				fmt.Fprintf(&b, "%s: %s\n", roleLabel(m.Role), mediaLabel(p))
			case model.PartToolCall:
				if p.ToolCall != nil {
					fmt.Fprintf(&b, "Assistant called tool %s with %s\n", p.ToolCall.Name, p.ToolCall.Arguments)
				}
			case model.PartToolResult:
				if p.ToolResult != nil {
					content := p.ToolResult.Content
					if maxResultBytes > 0 && len(content) > maxResultBytes {
						content = truncateUTF8(content, maxResultBytes) + " ...[truncated]"
					}
					status := "result"
					if p.ToolResult.IsError {
						status = "error"
					}
					for _, rp := range p.ToolResult.Parts {
						if rp.IsMedia() {
							content += " " + mediaLabel(rp)
						}
					}
					fmt.Fprintf(&b, "Tool %s %s: %s\n", p.ToolResult.Name, status, content)
				}
			}
		}
	}
	return b.String()
}

// mediaLabel describes a media part in a transcript, such as "[image]" or
// "[document report.pdf]".
func mediaLabel(p model.Part) string {
	if p.Filename != "" {
		return "[" + string(p.Type) + " " + p.Filename + "]"
	}
	return "[" + string(p.Type) + "]"
}

// truncateUTF8 returns at most n bytes of s without splitting a rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func roleLabel(r model.Role) string {
	switch r {
	case model.RoleUser:
		return "User"
	case model.RoleAssistant:
		return "Assistant"
	case model.RoleSystem:
		return "System"
	}
	return string(r)
}

// prependNote adds note to the first message when it is a user message, or
// inserts a new user message otherwise, so the history still starts with the
// user.
func prependNote(msgs []*model.Message, note string) []*model.Message {
	out := make([]*model.Message, 0, len(msgs)+1)
	if len(msgs) > 0 && msgs[0].Role == model.RoleUser {
		first := msgs[0].Clone()
		first.Parts = slices.Insert(first.Parts, 0, model.TextPart(note))
		out = append(out, first)
		return append(out, msgs[1:]...)
	}
	out = append(out, model.UserText(note))
	return append(out, msgs...)
}
