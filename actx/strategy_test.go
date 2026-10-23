package actx_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/nostosx/agentx/actx"
	"github.com/nostosx/agentx/model"
)

func toolConversation() []*model.Message {
	var msgs []*model.Message
	for _, id := range []string{"old", "middle", "recent"} {
		msgs = append(msgs, model.UserText(id),
			model.NewMessage(model.RoleAssistant, model.Part{Type: model.PartToolCall, ToolCall: &model.ToolCall{ID: id, Name: "read", Arguments: []byte("{}")}}),
			model.ToolResults(&model.ToolResult{CallID: id, Name: "read", Content: "result for " + id}))
	}
	return msgs
}

func assertPairedTools(t *testing.T, msgs []*model.Message) {
	t.Helper()
	calls, results := map[string]int{}, map[string]int{}
	for _, m := range msgs {
		for _, call := range m.ToolCalls() {
			calls[call.ID]++
		}
		for _, result := range m.ToolResults() {
			results[result.CallID]++
		}
	}
	if !reflect.DeepEqual(calls, results) {
		t.Errorf("tool calls and results diverged: calls=%v results=%v", calls, results)
	}
}

func TestTurns(t *testing.T) {
	t.Parallel()
	msgs := toolConversation()
	turns := actx.Turns(msgs)
	if len(turns) != 3 {
		t.Fatalf("Turns() has %d turns, want 3", len(turns))
	}
	for i, turn := range turns {
		if len(turn) != 3 || turn[0] != msgs[i*3] {
			t.Errorf("turn %d = %+v", i, turn)
		}
		assertPairedTools(t, turn)
	}
	if got := actx.Turns(nil); len(got) != 0 {
		t.Errorf("Turns(nil) = %v", got)
	}
	if got := actx.Turns([]*model.Message{model.SystemText("system"), model.AssistantText("hello"), model.UserText("new")}); len(got) != 2 || len(got[0]) != 2 {
		t.Errorf("leading non-user messages = %v", got)
	}
}

func TestChain(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("compact failed")
	for _, tt := range []struct {
		name        string
		target      int
		fail        bool
		wantCalls   int
		wantSummary string
	}{
		{"already fits", 100, false, 0, ""},
		{"stop once fits", 3, false, 1, "first summary"},
		{"continue shrinking", 0, false, 2, "first summary"},
		{"propagate error", 0, true, 1, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			first := actx.StrategyFunc(func(context.Context, *actx.CompactInput) (*actx.CompactOutput, error) {
				calls++
				if tt.fail {
					return nil, wantErr
				}
				return &actx.CompactOutput{Messages: []*model.Message{model.UserText("abc")}, Summary: "first summary"}, nil
			})
			second := actx.StrategyFunc(func(_ context.Context, in *actx.CompactInput) (*actx.CompactOutput, error) {
				calls++
				if in.Messages[0].Text() != "abc" {
					t.Error("second strategy did not receive first output")
				}
				return &actx.CompactOutput{}, nil
			})
			out, err := actx.Chain(first, second).Compact(t.Context(), &actx.CompactInput{Messages: []*model.Message{model.UserText("long input")}, Target: tt.target, Counter: byteCounter{}})
			if calls != tt.wantCalls {
				t.Errorf("calls = %d, want %d", calls, tt.wantCalls)
			}
			if tt.fail {
				if !errors.Is(err, wantErr) {
					t.Errorf("error = %v", err)
				}
				return
			}
			if err != nil || out.Summary != tt.wantSummary {
				t.Errorf("Chain() = %+v, %v", out, err)
			}
		})
	}
}
