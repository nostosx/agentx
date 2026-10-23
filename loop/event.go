package loop

import "github.com/nostosx/agentx/model"

// EventType identifies a runtime event.
type EventType string

const (
	// EventRunStart is the first event of a run or a resumed run.
	EventRunStart EventType = "run_start"
	// EventTurnStart precedes each model call.
	EventTurnStart EventType = "turn_start"
	// EventDelta carries a streaming delta when streaming is enabled.
	EventDelta EventType = "delta"
	// EventMessage carries a complete assistant message.
	EventMessage EventType = "message"
	// EventToolCall is emitted before a tool call runs.
	EventToolCall EventType = "tool_call"
	// EventToolResult carries the result of a tool call.
	EventToolResult EventType = "tool_result"
	// EventCompacted reports that the conversation was compacted.
	EventCompacted EventType = "compacted"
	// EventOutputRetry reports that the final answer did not match
	// Config.Output and was sent back to the model; see OutputRetry.
	EventOutputRetry EventType = "output_retry"
	// EventHandoff reports that the run was handed off to another agent,
	// whose events follow; see Handoff. Its Agent is the agent handing off.
	EventHandoff EventType = "handoff"
	// EventInterrupted reports that the run paused for approval.
	EventInterrupted EventType = "interrupted"
	// EventRunEnd is the last event and carries the Result.
	EventRunEnd EventType = "run_end"
)

// Event is emitted by a run.
type Event struct {
	Type      EventType `json:"type"`
	RunID     string    `json:"run_id,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
	Agent     string    `json:"agent,omitempty"`
	Turn      int       `json:"turn,omitempty"`

	Delta      *model.StreamEvent `json:"delta,omitempty"`
	Message    *model.Message     `json:"message,omitempty"`
	ToolCall   *model.ToolCall    `json:"tool_call,omitempty"`
	ToolResult *model.ToolResult  `json:"tool_result,omitempty"`
	Compaction *Compaction        `json:"compaction,omitempty"`
	// OutputRetry describes the rejected answer for EventOutputRetry.
	OutputRetry *OutputRetry `json:"output_retry,omitempty"`
	Interrupt   *Interrupt   `json:"interrupt,omitempty"`
	// Handoff describes the transfer for EventHandoff.
	Handoff *Handoff `json:"handoff,omitempty"`
	// Usage is the usage of the model call for EventMessage.
	Usage  model.Usage `json:"usage,omitzero"`
	Result *Result     `json:"result,omitempty"`

	// Depth is zero for the events of the run itself. Events of nested runs,
	// such as a sub-agent's, that a tool call forwards with ForwardEvent
	// have the depth of the nested run below this one: 1 for a sub-agent, 2
	// for a sub-agent of a sub-agent. Their RunID, Agent and Turn are those
	// of the nested run, and an EventRunEnd among them ends the nested run,
	// not this one.
	Depth int `json:"depth,omitempty"`
	// ParentCallID is set on forwarded events to the ID of the tool call
	// that started the nested run that emitted them, in the run directly
	// above it.
	ParentCallID string `json:"parent_call_id,omitempty"`
}

// Compaction describes a compaction of the conversation for
// EventCompacted.
type Compaction struct {
	// Messages is the conversation after compaction.
	Messages []*model.Message `json:"messages"`
	// Before and After are the estimated input sizes in tokens.
	Before  int    `json:"before"`
	After   int    `json:"after"`
	Summary string `json:"summary,omitempty"`
}

// Text returns the text of a delta or message event.
func (e *Event) Text() string {
	switch {
	case e.Delta != nil && e.Delta.Delta != nil && e.Delta.Delta.Type == model.PartText:
		return e.Delta.Delta.Text
	case e.Type == EventMessage && e.Message != nil:
		return e.Message.Text()
	}
	return ""
}
