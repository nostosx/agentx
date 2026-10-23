package agent

import (
	"fmt"
	"strings"

	"github.com/nostosx/agentx/model"
)

// RemoveToolCalls is a handoff input filter that removes tool calls and
// their results from the conversation, and assistant messages left empty,
// like the OpenAI Agents SDK's remove_all_tools. Use it when the agent
// handed off to should not see the work of the previous one, or has no
// tools, which some providers require when the conversation holds tool
// calls.
func RemoveToolCalls(msgs []*model.Message) []*model.Message {
	out := make([]*model.Message, 0, len(msgs))
	for _, msg := range msgs {
		if msg == nil || msg.Role == model.RoleTool {
			continue
		}
		var parts []model.Part
		changed := false
		for _, p := range msg.Parts {
			if p.Type == model.PartToolCall || p.Type == model.PartToolResult {
				changed = true
				continue
			}
			parts = append(parts, p)
		}
		if !changed {
			out = append(out, msg)
			continue
		}
		if !hasContent(parts) {
			continue
		}
		c := *msg
		c.Parts = parts
		out = append(out, &c)
	}
	return out
}

// hasContent reports whether parts hold more than reasoning, which
// providers reject on its own.
func hasContent(parts []model.Part) bool {
	for _, p := range parts {
		if p.Type != model.PartReasoning {
			return true
		}
	}
	return false
}

// NestHandoffHistory is a handoff input filter that replaces the
// conversation with a single user message holding a transcript of it in a
// <CONVERSATION HISTORY> block, like the OpenAI Agents SDK's
// nest_handoff_history. The agent handed off to then sees the earlier
// turns, including tool calls and results, as context rather than as its
// own. Images and other binary content are noted, not copied; the
// transcript is not redacted.
func NestHandoffHistory(msgs []*model.Message) []*model.Message {
	var b strings.Builder
	b.WriteString("For context, here is the conversation so far, before it was handed off to you:\n<CONVERSATION HISTORY>\n")
	n := 0
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		for _, p := range msg.Parts {
			line := ""
			switch p.Type {
			case model.PartText:
				if strings.TrimSpace(p.Text) != "" {
					line = fmt.Sprintf("%s: %s", msg.Role, p.Text)
				}
			case model.PartImage:
				line = fmt.Sprintf("%s: [image]", msg.Role)
			case model.PartToolCall:
				if p.ToolCall != nil {
					line = fmt.Sprintf("%s called tool %s with %s", msg.Role, p.ToolCall.Name, p.ToolCall.Arguments)
				}
			case model.PartToolResult:
				if p.ToolResult != nil {
					kind := "result"
					if p.ToolResult.IsError {
						kind = "error"
					}
					line = fmt.Sprintf("tool %s %s: %s", p.ToolResult.Name, kind, p.ToolResult.Content)
				}
			}
			if line != "" {
				n++
				fmt.Fprintf(&b, "%d. %s\n", n, line)
			}
		}
	}
	if n == 0 {
		return nil
	}
	b.WriteString("</CONVERSATION HISTORY>")
	return []*model.Message{model.UserText(b.String())}
}
