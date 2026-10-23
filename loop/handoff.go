package loop

import "encoding/json"

// HandoffFromKey is the run value that names the agent that handed the run
// off to the agent running it, in a run continued by agent.WithHandoffs.
// Middleware such as tracing reads it.
const HandoffFromKey = "agentx.handoff.from"

// Handoff describes the transfer of a run from one agent to another, for
// EventHandoff. See agent.WithHandoffs.
type Handoff struct {
	// From and To name the agents.
	From string `json:"from"`
	To   string `json:"to"`
	// Tool and CallID identify the tool call that requested the handoff.
	Tool   string `json:"tool,omitempty"`
	CallID string `json:"call_id,omitempty"`
	// Input holds the arguments of the call, for handoffs with a typed
	// input.
	Input json.RawMessage `json:"input,omitempty"`
}
