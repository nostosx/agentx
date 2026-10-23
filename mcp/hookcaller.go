package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nostosx/agentx/hook"
)

// HookCaller returns a hook.MCPCaller for hook.MCPTool hooks, and for
// mcp_tool handlers in hook settings, that calls tools on clients by
// server name. As for Claude Code, the hook reads the text content of the
// result.
func HookCaller(clients map[string]*Client) hook.MCPCaller {
	return hook.MCPCallerFunc(func(ctx context.Context, server, tool string, args json.RawMessage) (string, bool, error) {
		c := clients[server]
		if c == nil {
			return "", false, fmt.Errorf("mcp: no server %q", server)
		}
		res, err := c.CallTool(ctx, tool, args)
		if err != nil {
			return "", false, err
		}
		var texts []string
		for _, content := range res.Content {
			if content.Type == "text" {
				texts = append(texts, content.Text)
			}
		}
		return strings.Join(texts, "\n"), res.IsError, nil
	})
}
