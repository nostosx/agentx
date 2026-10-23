package hook

import "github.com/nostosx/agentx/model"

// EventType identifies a lifecycle point at which hooks run.
type EventType string

const (
	// SessionStart runs when a session starts, with Reason as Claude
	// Code's source: "startup" for the first run of a new session,
	// "resume" when an agent.Runner first runs or resumes an existing
	// session, "clear" and "fork" for the first run of a session made by
	// Runner.Clear or Runner.Fork, and "compact" after automatic
	// compaction. AdditionalContext is added to the conversation: to the
	// run's input, or after the compaction summary. It cannot block the
	// session.
	SessionStart EventType = "SessionStart"
	// SessionEnd runs when agent.Runner.EndSession ends a session, with
	// Reason as Claude Code's reason, such as "clear", "logout",
	// "prompt_input_exit" or "other". It cannot block, and its results are
	// ignored. Its hooks share a time budget; see SessionEndTimeout.
	SessionEnd EventType = "SessionEnd"
	// UserPromptSubmit runs before user input enters the conversation. Deny
	// blocks the run; AdditionalContext is attached to the input.
	UserPromptSubmit EventType = "UserPromptSubmit"
	// PreModel runs before each model call; hooks may modify Event.Request.
	PreModel EventType = "PreModel"
	// PostModel runs after each model call with Event.Response.
	PostModel EventType = "PostModel"
	// PreToolUse runs before a tool call. Deny rejects the call, Ask and
	// Defer request approval, Allow skips approval and UpdatedInput
	// replaces the arguments.
	PreToolUse EventType = "PreToolUse"
	// PostToolUse runs after a tool call that succeeded. UpdatedOutput
	// replaces the result, and AdditionalContext and the Reason of a Deny
	// are appended to it.
	PostToolUse EventType = "PostToolUse"
	// PostToolUseFailure runs instead of PostToolUse after a tool call
	// that ran and returned an error result. AdditionalContext and the
	// Reason of a Deny are appended to the result. Calls denied before
	// they ran fire neither event.
	PostToolUseFailure EventType = "PostToolUseFailure"
	// Stop runs when the model finishes without tool calls. Deny with a
	// Reason, or AdditionalContext, makes the agent continue with them as
	// new input, up to Manager.MaxStopBlocks times in a row (8 by
	// default); a turn with tool calls resets the count.
	Stop EventType = "Stop"
	// SubagentStop runs when a sub-agent invoked as a tool (agent.AsTool)
	// finishes. Like Stop, Deny with a Reason, or AdditionalContext, keeps
	// the sub-agent working with them as new input, under the same cap.
	SubagentStop EventType = "SubagentStop"
	// PreCompact runs before the conversation is compacted. Deny skips
	// automatic compaction.
	PreCompact EventType = "PreCompact"
	// PostCompact runs after the conversation was compacted, with the
	// summary in Message. Its result is ignored.
	PostCompact EventType = "PostCompact"
	// Notification carries informational messages such as approval requests.
	Notification EventType = "Notification"
	// SubagentStart runs when a sub-agent invoked as a tool (agent.AsTool)
	// starts a task, before its run. Agent and Reason are the sub-agent's
	// name, which the matcher matches, RunID is the ID given to its run and
	// Prompt is the task. It cannot block the sub-agent; AdditionalContext
	// is added in front of the task.
	SubagentStart EventType = "SubagentStart"
)

// Event is the input to a hook.
type Event struct {
	Type      EventType `json:"hook_event_name"`
	SessionID string    `json:"session_id,omitempty"`
	RunID     string    `json:"run_id,omitempty"`
	Agent     string    `json:"agent,omitempty"`
	Turn      int       `json:"turn,omitempty"`
	// PermissionMode is the run's permission mode, such as "default" or
	// "plan"; see Manager.SetPermissionMode.
	PermissionMode string `json:"permission_mode,omitempty"`

	// Prompt is the user input for UserPromptSubmit and SessionStart.
	Prompt string `json:"prompt,omitempty"`
	// Request is the pending model request for PreModel. Hooks may modify it.
	Request *model.Request `json:"-"`
	// Response is the model response for PostModel.
	Response *model.Response `json:"-"`
	// ToolCall is set for tool events.
	ToolCall *model.ToolCall `json:"-"`
	// ToolResult is set for PostToolUse and PostToolUseFailure.
	ToolResult *model.ToolResult `json:"-"`
	// Messages is the conversation for Stop, SubagentStop, PreCompact and
	// PostCompact.
	Messages []*model.Message `json:"-"`
	// Message is the text of a Notification, the final text for Stop and
	// SubagentStop, or the summary for PostCompact.
	Message string `json:"message,omitempty"`
	// Title is the title of a Notification.
	Title string `json:"title,omitempty"`
	// Reason explains why the event fired: the source for SessionStart
	// ("startup", "resume", "clear", "fork" or "compact"), the reason for
	// SessionEnd, "auto" for PreCompact and PostCompact, which fire only for
	// automatic compaction, the sub-agent's name for SubagentStart and
	// SubagentStop, and "approval" for a Notification that asks for tool
	// approval.
	Reason string `json:"reason,omitempty"`
	// StopHookActive is true when the agent is already continuing because a
	// Stop or SubagentStop hook blocked a previous stop in the same run or
	// sub-agent call, even after later tool calls. Hooks use it to avoid
	// loops.
	StopHookActive bool `json:"stop_hook_active,omitempty"`
	// Data carries additional values.
	Data map[string]any `json:"data,omitempty"`
}

// ToolName returns the tool name for tool events.
func (e *Event) ToolName() string {
	if e.ToolCall == nil {
		return ""
	}
	return e.ToolCall.Name
}
