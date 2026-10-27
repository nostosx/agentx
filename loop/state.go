package loop

import (
	"encoding/json"
	"time"

	"github.com/nostosx/agentx/model"
)

// Status is the state of a run.
type Status string

const (
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	// StatusInterrupted means the run is waiting for tool approvals and can
	// be resumed.
	StatusInterrupted Status = "interrupted"
	StatusFailed      Status = "failed"
	// StatusMaxTurns means the turn limit was reached.
	StatusMaxTurns Status = "max_turns"
	// StatusStopped means middleware, for example a hook, stopped the run.
	StatusStopped Status = "stopped"
	// StatusBlocked means middleware, for example a UserPromptSubmit hook,
	// rejected the input.
	StatusBlocked Status = "blocked"
	// StatusIncomplete means the final answer is incomplete: it was cut off
	// by the output token limit or withheld by a content filter. Reason
	// says which. Result.Output holds what was returned unless an AnswerHolder
	// withholds it.
	StatusIncomplete Status = "incomplete"
	// StatusLimitExceeded means the run reached one of its Limits or its
	// Budget; Reason names it. Like StatusMaxTurns it is final: continue
	// the conversation with a new run, which starts with its own limits.
	StatusLimitExceeded Status = "limit_exceeded"
)

// Done reports whether the status is final.
func (s Status) Done() bool { return s != StatusRunning && s != StatusInterrupted && s != "" }

// Resumable reports whether a run in this status can be resumed: an
// interrupted run waiting for approvals, a failed run, or a run still
// marked running because its process crashed or its consumer stopped.
// Checkpoints of runs that end in any other status are deleted.
func (s Status) Resumable() bool {
	return s == StatusInterrupted || s == StatusRunning || s == StatusFailed
}

// StateVersion is the version of the State format written by this package.
// Resume accepts only states of this version.
const StateVersion = 1

// State is the resumable state of a run. It is what checkpoints store.
type State struct {
	// Version is the format version; see StateVersion.
	Version   int    `json:"version,omitempty"`
	RunID     string `json:"run_id"`
	SessionID string `json:"session_id,omitempty"`
	Agent     string `json:"agent,omitempty"`
	Turn      int    `json:"turn"`
	// System is the system prompt assembled at the start of the run.
	System string `json:"system,omitempty"`
	// Messages is the whole conversation, including prior history.
	Messages []*model.Message `json:"messages"`
	// Start is the index in Messages where this run's input begins.
	Start  int         `json:"start"`
	Usage  model.Usage `json:"usage"`
	Status Status      `json:"status"`
	Reason string      `json:"reason,omitempty"`
	// FinishReason is the finish reason of the last model response. A run
	// resumed after the model's final answer uses it to handle the answer.
	FinishReason model.FinishReason `json:"finish_reason,omitempty"`
	// Pending holds the planned tool calls of the current turn while they
	// are being approved or executed.
	Pending []*PendingCall `json:"pending,omitempty"`
	// Values holds middleware state; see Run.SetValue.
	Values    map[string]string `json:"values,omitempty"`
	UpdatedAt time.Time         `json:"updated_at"`
	// MaxTurns and Limits are the run's overrides of the configuration,
	// from its Request, kept so that a resumed run applies them too.
	MaxTurns int     `json:"max_turns,omitempty"`
	Limits   *Limits `json:"limits,omitempty"`
	// OutputRetries counts the answers sent back to the model because they
	// did not match Config.Output.
	OutputRetries int `json:"output_retries,omitempty"`
}

// PendingCall is a tool call whose pre-execution checks have run.
type PendingCall struct {
	Call *model.ToolCall `json:"call"`
	// Denied is set when middleware or the approver rejected the call.
	Denied bool   `json:"denied,omitempty"`
	Reason string `json:"reason,omitempty"`
	// NeedsApproval is set while approval is outstanding.
	NeedsApproval bool `json:"needs_approval,omitempty"`
	ReadOnly      bool `json:"read_only,omitempty"`
	// Concurrent is set for calls of tools marked tool.Concurrent.
	Concurrent bool `json:"concurrent,omitempty"`
	// Values holds middleware state; see ToolPlan.SetValue.
	Values map[string]string `json:"values,omitempty"`
	// ModelArguments holds the arguments the model sent when middleware or
	// an approval changed them; Call holds the arguments that run.
	ModelArguments json.RawMessage `json:"model_arguments,omitempty"`
	// Started is set once the call began executing, and Result once it
	// finished. A resumed run reuses results and does not repeat a call
	// with side effects that may have run.
	Started bool              `json:"started,omitempty"`
	Result  *model.ToolResult `json:"result,omitempty"`
	// Nested is the interrupt the call raised for approvals of a nested
	// run, such as a sub-agent; see InterruptCall. NestedApprovals holds
	// the decisions once they are made.
	Nested          *Interrupt          `json:"nested,omitempty"`
	NestedApprovals map[string]Approval `json:"nested_approvals,omitempty"`
	// Approval is the decision of the approval the call needed, if any,
	// without its UpdatedInput; see ToolPlan.Approval.
	Approval *Approval `json:"approval,omitempty"`
	// Resuming is set while the call is repeated to resume its nested run.
	// If the run stops then, the nested run may have executed approved
	// calls, so a call with side effects is not repeated, like any cut-off
	// call with side effects. A read-only call is repeated with its nested
	// run, like any cut-off read-only call.
	Resuming bool `json:"resuming,omitempty"`
}

// Clone returns a deep copy of s.
func (s *State) Clone() *State {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	var c State
	if err := json.Unmarshal(b, &c); err != nil {
		panic(err)
	}
	return &c
}

// NewMessages returns the messages added by this run.
func (s *State) NewMessages() []*model.Message {
	if s.Start > len(s.Messages) {
		return nil
	}
	return s.Messages[s.Start:]
}
