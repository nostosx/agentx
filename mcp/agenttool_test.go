package mcp

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/tool"
)

// newAgentServer serves an agent whose model calls a tool that requires
// approval, then answers.
func newAgentServer(t *testing.T) (*Server, *atomic.Int32) {
	t.Helper()
	var ran atomic.Int32
	remove := tool.Func("remove", "Remove a file.", func(_ context.Context, in echoArgs) (string, error) {
		ran.Add(1)
		return "removed " + in.Text, nil
	}, tool.RequireApproval())
	m := modeltest.New(
		modeltest.CallTools(modeltest.Call{ID: "call-1", Name: "remove", Args: map[string]any{"text": "a.txt"}}),
		modeltest.Reply("cleaned up"),
	)
	a := agent.MustNew("janitor", agent.WithModel(m), agent.WithTools(remove), agent.WithDescription("Cleans up files."))
	s := NewServer(Implementation{Name: "agents"}, &ServerOptions{Tools: []tool.Tool{AgentTool(a, nil)}})
	t.Cleanup(func() { _ = s.Close() })
	return s, &ran
}

func TestAgentToolApprovedThroughElicitation(t *testing.T) {
	for _, g := range generations {
		for _, transport := range []string{"stdio", "http"} {
			t.Run(transport+"/"+g.name, func(t *testing.T) {
				t.Parallel()
				s, ran := newAgentServer(t)
				var asked []string
				approve := WithElicitation(func(_ context.Context, req *ElicitRequest) (*ElicitResult, error) {
					asked = append(asked, req.Message)
					return &ElicitResult{Action: ElicitAccept, Content: map[string]any{"approve": true}}, nil
				})
				c := connectVia(t, s, transport, append(slices.Clone(g.opts), approve)...)
				tools, err := c.ListTools(t.Context())
				if err != nil || len(tools) != 1 || tools[0].Name != "janitor" || tools[0].Description != "Cleans up files." ||
					!strings.Contains(string(tools[0].InputSchema), "prompt") {
					t.Fatalf("tools = %+v, %v", tools, err)
				}
				out, isErr := callText(t, c, "janitor", `{"prompt":"clean up"}`)
				if isErr || out != "cleaned up" {
					t.Fatalf("janitor = %q, %v", out, isErr)
				}
				if ran.Load() != 1 || len(asked) != 1 || !strings.Contains(asked[0], `remove with arguments {"text":"a.txt"}`) {
					t.Errorf("ran %d, asked %q", ran.Load(), asked)
				}
			})
		}
	}
}

func TestAgentToolDeniedWithoutElicitation(t *testing.T) {
	t.Parallel()
	s, ran := newAgentServer(t)
	c := connectStdio(t, s)
	out, isErr := callText(t, c, "janitor", `{"prompt":"clean up"}`)
	if isErr || out != "cleaned up" || ran.Load() != 0 {
		t.Errorf("janitor = %q, %v, ran %d", out, isErr, ran.Load())
	}
	if out, isErr := callText(t, c, "janitor", `{"prompt":"  "}`); !isErr || !strings.Contains(out, "empty") {
		t.Errorf("empty prompt = %q", out)
	}
}

func TestAgentToolCustomApproval(t *testing.T) {
	t.Parallel()
	var ran atomic.Int32
	remove := tool.Func("remove", "", func(context.Context, echoArgs) (string, error) {
		ran.Add(1)
		return "removed", nil
	}, tool.RequireApproval())
	m := modeltest.New(
		modeltest.CallTools(modeltest.Call{ID: "c1", Name: "remove", Args: map[string]any{"text": "x"}}),
		modeltest.Reply("done"),
	)
	a := agent.MustNew("worker", agent.WithModel(m), agent.WithTools(remove))
	var seen []string
	at := AgentTool(a, &AgentToolOptions{Name: "delegate", Description: "Delegate.",
		Approve: func(_ context.Context, req *loop.ApprovalRequest) (loop.Approval, error) {
			seen = append(seen, req.Call.ID)
			return loop.Approval{Approved: true}, nil
		}})
	res, err := at.Call(t.Context(), json.RawMessage(`{"prompt":"go"}`))
	if err != nil || res.Content != "done" || ran.Load() != 1 || !slices.Equal(seen, []string{"c1"}) {
		t.Errorf("Call = %+v, %v; ran %d, approvals %v", res, err, ran.Load(), seen)
	}
	if info := at.Info(); info.Name != "delegate" || info.Description != "Delegate." {
		t.Errorf("info = %+v", info)
	}
}
