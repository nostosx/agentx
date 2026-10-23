package hook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// MCPCaller calls a tool of a connected MCP server for MCPTool hooks and
// returns the text content of the result and whether the server reported
// it as an error. mcp.HookCaller returns one for mcp.Client values.
type MCPCaller interface {
	CallTool(ctx context.Context, server, tool string, args json.RawMessage) (text string, isError bool, err error)
}

// MCPCallerFunc adapts a function to MCPCaller.
type MCPCallerFunc func(ctx context.Context, server, tool string, args json.RawMessage) (string, bool, error)

// CallTool calls f.
func (f MCPCallerFunc) CallTool(ctx context.Context, server, tool string, args json.RawMessage) (string, bool, error) {
	return f(ctx, server, tool, args)
}

// MCPTool is a hook that calls a tool of an MCP server, like Claude Code's
// mcp_tool hooks. The tool's text is read like a command's stdout on exit
// status 0, and a result marked as an error is a hook error.
type MCPTool struct {
	Caller MCPCaller
	Server string
	Tool   string
	// Input holds the tool arguments. "${path}" in its string values, such
	// as "${tool_input.file_path}", is replaced by the value at that
	// dotted path in the event's JSON input: strings as they are, other
	// values as JSON, and missing values by "".
	Input map[string]any
	// Timeout defaults to 10 minutes, to 30 seconds for UserPromptSubmit
	// and to 1.5 seconds for SessionEnd. A negative Timeout disables it.
	Timeout time.Duration
}

func (t *MCPTool) hookTimeout() time.Duration { return t.Timeout }

func (t *MCPTool) hookKey() string {
	id := identity(t.Caller)
	if id == "" {
		return ""
	}
	key, err := json.Marshal([]any{"mcp_tool", id, t.Server, t.Tool, t.Input, t.Timeout})
	if err != nil {
		return ""
	}
	return string(key)
}

// Handle calls the tool.
func (t *MCPTool) Handle(ctx context.Context, ev *Event) (*Result, error) {
	if t.Caller == nil {
		return nil, errors.New("mcp_tool hook: no MCP caller")
	}
	ctx, cancel := withTimeout(ctx, t.Timeout, ev.Type)
	defer cancel()
	input, err := eventJSON(ev)
	if err != nil {
		return nil, err
	}
	var doc any
	if err := json.Unmarshal(input, &doc); err != nil {
		return nil, err
	}
	args := json.RawMessage("{}")
	if t.Input != nil {
		if args, err = json.Marshal(substitute(t.Input, doc)); err != nil {
			return nil, fmt.Errorf("mcp_tool hook: input: %w", err)
		}
	}
	text, isError, err := t.Caller.CallTool(ctx, t.Server, t.Tool, args)
	if err != nil {
		return nil, fmt.Errorf("mcp_tool hook %s/%s: %w", t.Server, t.Tool, err)
	}
	if isError {
		return nil, fmt.Errorf("mcp_tool hook %s/%s: tool error: %s", t.Server, t.Tool, truncate(text))
	}
	return parseCommandOutput(ev.Type, []byte(text))
}

var pathRef = regexp.MustCompile(`\$\{([^}]*)\}`)

// substitute returns v with "${path}" in its strings replaced by values of
// doc.
func substitute(v, doc any) any {
	switch v := v.(type) {
	case string:
		return pathRef.ReplaceAllStringFunc(v, func(ref string) string {
			return lookup(doc, strings.TrimSpace(ref[2:len(ref)-1]))
		})
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			out[k] = substitute(x, doc)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = substitute(x, doc)
		}
		return out
	}
	return v
}

// lookup returns the value at a dotted path in doc as text.
func lookup(doc any, path string) string {
	for key := range strings.SplitSeq(path, ".") {
		m, ok := doc.(map[string]any)
		if !ok {
			return ""
		}
		if doc, ok = m[key]; !ok {
			return ""
		}
	}
	switch v := doc.(type) {
	case nil:
		return ""
	case string:
		return v
	}
	out, _ := json.Marshal(doc)
	return string(out)
}
