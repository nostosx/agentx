package mcp

import (
	"encoding/json"
	"testing"

	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/model"
)

func TestHookCaller(t *testing.T) {
	c := stdioServer(t, func(req rpcRequest, write func(string)) {
		if req.Method != "tools/call" {
			return
		}
		switch req.Params.Name {
		case "check":
			if string(req.Params.Arguments) != `{"path":"/etc/passwd"}` {
				write(result(req.ID, `{"content":[{"type":"text","text":"unexpected"}],"isError":true}`))
				return
			}
			write(result(req.ID, `{"content":[{"type":"text","text":"{\"decision\":"},{"type":"image","data":"eA=="},{"type":"text","text":"\"block\",\"reason\":\"protected\"}"}]}`))
		default:
			write(result(req.ID, `{"content":[{"type":"text","text":"failed"}],"isError":true}`))
		}
	})
	caller := HookCaller(map[string]*Client{"guard": c})
	ev := &hook.Event{Type: hook.PreToolUse, ToolCall: &model.ToolCall{Name: "write_file", Arguments: json.RawMessage(`{"file_path":"/etc/passwd"}`)}}
	h := &hook.MCPTool{Caller: caller, Server: "guard", Tool: "check", Input: map[string]any{"path": "${tool_input.file_path}"}}
	res, err := h.Handle(t.Context(), ev)
	if err != nil || res.Decision != hook.Deny || res.Reason != "protected" {
		t.Fatalf("Handle = %+v, %v", res, err)
	}
	if text, isError, err := caller.CallTool(t.Context(), "guard", "other", nil); err != nil || !isError || text != "failed" {
		t.Fatalf("error result = %q, %v, %v", text, isError, err)
	}
	if _, _, err := caller.CallTool(t.Context(), "missing", "check", nil); err == nil {
		t.Fatal("unknown server accepted")
	}
}
