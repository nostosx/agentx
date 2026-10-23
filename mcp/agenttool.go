package mcp

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/tool"
)

// AgentToolOptions configures AgentTool. The zero value gives the
// defaults.
type AgentToolOptions struct {
	// Name and Description default to the agent's.
	Name        string
	Description string
	// Approve decides the tool calls the agent needs approval for. It
	// defaults to ElicitApproval, which asks the MCP client's user.
	Approve func(ctx context.Context, req *loop.ApprovalRequest) (loop.Approval, error)
	// MaxApprovalRounds bounds how often a call resumes the agent after
	// approvals. Defaults to 16.
	MaxApprovalRounds int
}

type agentArgs struct {
	Prompt string `json:"prompt" jsonschema:"A complete, self-contained description of the task."`
}

// AgentTool exposes an agent as a tool to serve with a Server: each call
// runs the agent on the prompt without history and returns its final
// answer. When the agent needs approval for a tool call, the call asks
// Approve and resumes the agent with the decisions, which the agent must
// support (agent.StateResumer or agent.Resumer, as LLMAgent does).
//
// agent.AsTool serves an agent too, but outside a run it cannot obtain
// approvals.
func AgentTool(a agent.Agent, opts *AgentToolOptions) tool.Tool {
	var o AgentToolOptions
	if opts != nil {
		o = *opts
	}
	approve := o.Approve
	if approve == nil {
		approve = ElicitApproval
	}
	rounds := cmp.Or(o.MaxApprovalRounds, 16)
	desc := cmp.Or(o.Description, a.Description(), "Delegate a task to the "+a.Name()+" agent.")
	return tool.Func(cmp.Or(o.Name, a.Name()), desc, func(ctx context.Context, in agentArgs) (*tool.Result, error) {
		if strings.TrimSpace(in.Prompt) == "" {
			return tool.Errorf("prompt is empty"), nil
		}
		res, err := loop.Collect(a.Run(ctx, &agent.Input{Message: model.UserText(in.Prompt)}), nil)
		for round := 0; err == nil && res.Status == loop.StatusInterrupted && res.Interrupt != nil; round++ {
			if round >= rounds {
				return tool.Errorf("agent %s asked for approval too often", a.Name()), nil
			}
			approvals := map[string]loop.Approval{}
			for _, req := range res.Interrupt.Requests {
				ap, aerr := approve(ctx, req)
				if aerr != nil {
					return nil, aerr
				}
				approvals[req.Call.ID] = ap
			}
			res, err = resumeAgent(ctx, a, res.Interrupt, approvals)
		}
		if err != nil {
			return nil, err
		}
		switch res.Status {
		case loop.StatusCompleted:
			return tool.Text(res.Output), nil
		default:
			out := fmt.Sprintf("agent %s ended with status %s", a.Name(), res.Status)
			if res.Reason != "" {
				out += ": " + res.Reason
			}
			if res.Output != "" {
				out += "\n\nPartial answer:\n" + res.Output
			}
			return tool.Errorf("%s", out), nil
		}
	})
}

func resumeAgent(ctx context.Context, a agent.Agent, intr *loop.Interrupt, approvals map[string]loop.Approval) (*loop.Result, error) {
	if sr, ok := a.(agent.StateResumer); ok && intr.State != nil {
		return loop.Collect(sr.ResumeState(ctx, intr.State, approvals), nil)
	}
	if r, ok := a.(agent.Resumer); ok {
		return loop.Collect(r.Resume(ctx, intr.RunID, approvals), nil)
	}
	return nil, fmt.Errorf("mcp: agent %s cannot resume runs", a.Name())
}

// approvalSchema is the form ElicitApproval shows.
const approvalSchema = `{"type":"object","properties":{` +
	`"approve":{"type":"boolean","title":"Approve","description":"Allow this tool call"},` +
	`"reason":{"type":"string","title":"Reason","description":"Optional reason, shown to the agent when denied"}},` +
	`"required":["approve"]}`

// ElicitApproval asks the MCP client's user to approve a tool call with a
// form elicitation. A user who declines or cancels denies the call, as
// does a client without the elicitation capability.
func ElicitApproval(ctx context.Context, req *loop.ApprovalRequest) (loop.Approval, error) {
	msg := fmt.Sprintf("Allow the tool call %s with arguments %s?", req.Call.Name, req.Call.Arguments)
	if req.Agent != "" {
		msg = fmt.Sprintf("Agent %s asks: %s", req.Agent, msg)
	}
	if req.Reason != "" {
		msg += " (" + req.Reason + ")"
	}
	req2 := &ElicitRequest{Mode: ElicitationForm, Message: msg, RequestedSchema: []byte(approvalSchema)}
	if !clientCan(ctx, "elicitation/create", req2) {
		return loop.Approval{Reason: "the MCP client cannot ask the user for approval"}, nil
	}
	res, err := Elicit(ctx, req2)
	if err != nil {
		return loop.Approval{}, err
	}
	switch res.Action {
	case ElicitDecline:
		return loop.Approval{Reason: "the user declined the tool call"}, nil
	case ElicitCancel:
		return loop.Approval{Reason: "the user cancelled the approval"}, nil
	}
	ok, _ := res.Content["approve"].(bool)
	reason, _ := res.Content["reason"].(string)
	if !ok && reason == "" {
		reason = "the user denied the tool call"
	}
	return loop.Approval{Approved: ok, Reason: reason}, nil
}
