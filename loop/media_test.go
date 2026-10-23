package loop_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/tool"
)

// The parts and metadata a tool returns reach events, model requests and
// checkpoints.
func TestToolResultParts(t *testing.T) {
	t.Parallel()
	png := []byte("\x89PNG\r\n\x1a\n")
	shot := tool.Func("screenshot", "Take a screenshot.", func(context.Context, struct{}) (*tool.Result, error) {
		res := tool.Media("captured", model.ImagePart("image/png", png))
		res.Metadata = map[string]any{"display": "main"}
		return res, nil
	})
	reg, err := tool.NewRegistry(shot)
	if err != nil {
		t.Fatal(err)
	}
	cp := loop.NewMemoryCheckpointer()
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c1", Name: "screenshot"}), modeltest.Reply("seen"))
	r := newRuntime(t, loop.Config{Model: m, Tools: reg, Checkpointer: cp})
	var event *model.ToolResult
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{RunID: "media", Input: model.UserText("look")}), func(ev *loop.Event) {
		if ev.Type == loop.EventToolResult {
			event = ev.ToolResult
		}
	})
	if err != nil || res.Status != loop.StatusCompleted {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	check := func(name string, r *model.ToolResult) {
		t.Helper()
		if r == nil || r.Content != "captured" || len(r.Parts) != 1 || !bytes.Equal(r.Parts[0].Data, png) || r.Metadata["display"] != "main" {
			t.Errorf("%s = %+v", name, r)
		}
	}
	check("event", event)
	check("request", m.Requests()[1].Messages[2].ToolResults()[0])
	// Completed runs delete their checkpoint; save the conversation as a
	// checkpoint to read it back from JSON.
	if err := cp.Save(t.Context(), &loop.State{RunID: "saved", Messages: res.Messages}); err != nil {
		t.Fatal(err)
	}
	st, err := cp.Load(t.Context(), "saved")
	if err != nil {
		t.Fatal(err)
	}
	check("checkpoint", st.Messages[2].ToolResults()[0])
}
