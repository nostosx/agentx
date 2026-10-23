package model_test

import (
	"testing"

	"github.com/nostosx/agentx/model"
)

func TestRequestClone(t *testing.T) {
	t.Parallel()
	original := &model.Request{Messages: []*model.Message{model.UserText("hi")}, Tools: []*model.ToolSpec{{Name: "lookup"}}, Stop: []string{"end"}, Extra: map[string]any{"seed": 1}}
	clone := original.Clone()
	if clone.Messages[0] != original.Messages[0] || clone.Tools[0] != original.Tools[0] {
		t.Fatal("request clone should share message and tool values")
	}
	clone.Messages[0] = model.UserText("changed")
	clone.Tools[0] = &model.ToolSpec{Name: "changed"}
	clone.Stop[0] = "changed"
	clone.Extra["seed"] = 2
	if original.Messages[0].Text() != "hi" || original.Tools[0].Name != "lookup" || original.Stop[0] != "end" || original.Extra["seed"] != 1 {
		t.Fatal("request clone shares mutable containers")
	}
	var empty *model.Request
	if empty.Clone() != nil {
		t.Fatal("nil request clone must be nil")
	}
}
