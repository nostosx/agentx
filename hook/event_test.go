package hook_test

import (
	"testing"

	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/model"
)

func TestEventToolName(t *testing.T) {
	t.Parallel()
	ev := &hook.Event{}
	if ev.ToolName() != "" {
		t.Fatal("event without tool has a name")
	}
	ev.ToolCall = &model.ToolCall{Name: "Edit"}
	if ev.ToolName() != "Edit" {
		t.Fatalf("ToolName=%q", ev.ToolName())
	}
}
