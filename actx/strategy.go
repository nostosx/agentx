package actx

import (
	"context"
	"slices"
	"strings"

	"github.com/nostosx/agentx/model"
)

// CompactInput is the input to a compaction strategy.
type CompactInput struct {
	System   string
	Messages []*model.Message
	// Target is the desired size of the messages in tokens.
	Target  int
	Counter TokenCounter
}

// CompactOutput is the result of a compaction strategy.
type CompactOutput struct {
	Messages []*model.Message
	// Summary is set by summarizing strategies.
	Summary string
}

// Strategy reduces the size of a conversation.
//
// Strategies must not modify the input messages; they return new slices and
// clone any message they change.
type Strategy interface {
	Compact(ctx context.Context, in *CompactInput) (*CompactOutput, error)
}

// StrategyFunc adapts a function to Strategy.
type StrategyFunc func(ctx context.Context, in *CompactInput) (*CompactOutput, error)

// Compact calls f.
func (f StrategyFunc) Compact(ctx context.Context, in *CompactInput) (*CompactOutput, error) {
	return f(ctx, in)
}

// Chain applies strategies in order until the conversation fits the target.
// Cheaper, lossless strategies should come first.
func Chain(strategies ...Strategy) Strategy {
	return StrategyFunc(func(ctx context.Context, in *CompactInput) (*CompactOutput, error) {
		out := &CompactOutput{Messages: in.Messages}
		for _, s := range strategies {
			if in.Counter.CountMessages(out.Messages) <= in.Target {
				break
			}
			next := *in
			next.Messages = out.Messages
			res, err := s.Compact(ctx, &next)
			if err != nil {
				return nil, err
			}
			out.Messages = res.Messages
			if res.Summary != "" {
				out.Summary = res.Summary
			}
		}
		return out, nil
	})
}

// Turns splits a conversation into turns. A turn starts with a user message
// and contains the assistant and tool messages that follow it, so tool calls
// and their results always stay in the same turn.
//
// A user message that holds nothing but <system-reminder> blocks (see
// model.WrapReminder) does not start a turn. The application, not the user,
// adds such messages to continue the current task, for example when a Stop
// or SubagentStop hook blocks the model from finishing, so they stay in the
// turn of the request they continue and compaction keeps that request.
func Turns(msgs []*model.Message) [][]*model.Message {
	var turns [][]*model.Message
	for _, m := range msgs {
		if len(turns) == 0 || (m.Role == model.RoleUser && !isReminder(m)) {
			turns = append(turns, nil)
		}
		turns[len(turns)-1] = append(turns[len(turns)-1], m)
	}
	return turns
}

// isReminder reports whether m is a user message whose content is only
// <system-reminder> blocks.
func isReminder(m *model.Message) bool {
	if m.Role != model.RoleUser || len(m.Parts) == 0 {
		return false
	}
	for _, p := range m.Parts {
		if p.Type != model.PartText {
			return false
		}
		s := strings.TrimSpace(p.Text)
		if s == "" {
			return false
		}
		for s != "" {
			rest, ok := strings.CutPrefix(s, "<system-reminder>")
			if !ok {
				return false
			}
			_, after, ok := strings.Cut(rest, "</system-reminder>")
			if !ok {
				return false
			}
			s = strings.TrimSpace(after)
		}
	}
	return true
}

func flatten(turns [][]*model.Message) []*model.Message { return slices.Concat(turns...) }

// splitSystem separates system messages, which compaction never drops or
// summarizes, from the rest of the conversation. Both keep their order.
func splitSystem(msgs []*model.Message) (system, rest []*model.Message) {
	for _, m := range msgs {
		if m.Role == model.RoleSystem {
			system = append(system, m)
		} else {
			rest = append(rest, m)
		}
	}
	return system, rest
}

// steps splits a turn into its head, the messages before the first
// assistant message, and its steps. A step starts with an assistant message
// and holds the messages that follow it up to the next assistant message
// that starts while no tool call is waiting for its result, so a tool call
// and its result always share a step.
func steps(turn []*model.Message) (head []*model.Message, st [][]*model.Message) {
	pending := map[string]bool{}
	for _, m := range turn {
		if m.Role == model.RoleAssistant && len(pending) == 0 {
			st = append(st, nil)
		}
		for _, c := range m.ToolCalls() {
			pending[c.ID] = true
		}
		for _, r := range m.ToolResults() {
			delete(pending, r.CallID)
		}
		if len(st) == 0 {
			head = append(head, m)
		} else {
			st[len(st)-1] = append(st[len(st)-1], m)
		}
	}
	return head, st
}
