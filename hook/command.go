package hook

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/nostosx/agentx/internal/process"
)

// Command is a hook that runs a command, compatible with Claude Code
// command hooks.
//
// The event is written to the command's stdin as JSON. It carries Claude
// Code's fields: hook_event_name, session_id, transcript_path (always
// empty), cwd (Dir or the process's working directory) and permission_mode
// (Event.PermissionMode, or "default"); tool_name, tool_use_id and tool_input for tool
// events; tool_response for PostToolUse; error and is_interrupt for
// PostToolUseFailure; prompt; source for SessionStart; trigger for
// PreCompact and PostCompact, with custom_instructions (null) and
// compact_summary; message, title and notification_type for Notification;
// stop_hook_active and last_assistant_message for Stop and SubagentStop;
// agent_id (the sub-agent's run ID) and agent_type for SubagentStart; and
// agent_id, agent_type and agent_transcript_path (empty) for SubagentStop. tool_name and tool_input
// are agentx's, and tool_response is {"content": ..., "is_error": ...}
// rather than the structured output of Claude Code's built-in tools. The
// agentx fields run_id, agent, turn, reason and data (Event.Data) are
// added. The environment has CLAUDE_PROJECT_DIR (Dir or the working
// directory), AGENTX_HOOK_EVENT and AGENTX_SESSION_ID.
//
// Exit status 0 succeeds. If stdout is a JSON object it is parsed as a hook
// output (see below); otherwise non-empty stdout becomes AdditionalContext for
// SessionStart and UserPromptSubmit. Exit status 2 denies the action with
// stderr as the reason; JSON fields on stdout still apply but cannot
// override the deny. Any other status is a hook error, unless stdout is a
// JSON object, which then decides alone. Processes the command leaves
// running in the background do not delay or discard its result for more
// than a second; on Unix they are also killed once it exits. On Windows
// they keep running unless the hook times out or its context is cancelled.
//
// JSON output fields: decision ("allow", "approve", "ask", "defer", "deny"
// or "block"), reason, continue (false halts the run), stopReason,
// systemMessage (Result.SystemMessage), additionalContext and
// hookSpecificOutput with permissionDecision, permissionDecisionReason,
// additionalContext, updatedInput and updatedToolOutput. updatedToolOutput
// must be a string or an object with a string "content" field; other
// values are ignored. updatedMCPToolOutput is accepted in its place. suppressOutput has no effect,
// as in Claude Code. A "defer" decision drops updatedInput and
// additionalContext. Notification, PreCompact and PostCompact ignore
// continue and systemMessage.
type Command struct {
	// Command is run with "sh -c" (or "cmd /d /s /c" on Windows). When Args
	// is non-nil, Command is instead the executable, run directly with Args
	// as its arguments; "${CLAUDE_PROJECT_DIR}" in Command and Args is
	// replaced by the project directory.
	Command string
	// Args, when non-nil (even empty), selects the exec form.
	Args []string
	// Dir is the working directory.
	Dir string
	// Env is added to the process environment.
	Env []string
	// Timeout defaults to 10 minutes, to 30 seconds for UserPromptSubmit
	// and to 1.5 seconds for SessionEnd (see SessionEndTimeout). A negative
	// Timeout disables it.
	Timeout time.Duration
}

func (c *Command) hookTimeout() time.Duration { return c.Timeout }

func (c *Command) hookKey() string {
	key, _ := json.Marshal([]any{"command", c.Command, c.Args, c.Dir, c.Env, c.Timeout})
	return string(key)
}

// Default command hook timeouts, as in Claude Code.
const (
	defaultCommandTimeout = 600 * time.Second
	promptCommandTimeout  = 30 * time.Second
)

// withTimeout applies a hook's timeout, or the default timeout for the
// event if it has none, to ctx. A negative timeout applies none.
func withTimeout(ctx context.Context, timeout time.Duration, event EventType) (context.Context, context.CancelFunc) {
	switch {
	case timeout < 0:
		return context.WithCancel(ctx)
	case timeout == 0:
		timeout = defaultTimeout(ctx, event)
	}
	return context.WithTimeout(ctx, timeout)
}

// Handle runs the command.
func (c *Command) Handle(ctx context.Context, ev *Event) (*Result, error) {
	rewake := rewaking(ctx)
	ctx, cancel := withTimeout(ctx, c.Timeout, ev.Type)
	defer cancel()

	dir := c.Dir
	if dir == "" {
		dir, _ = os.Getwd()
	}
	payload := commandInput(ev)
	payload.Cwd = dir
	input, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var cmd *exec.Cmd
	if c.Args != nil {
		expand := strings.NewReplacer("${CLAUDE_PROJECT_DIR}", dir).Replace
		args := make([]string, len(c.Args))
		for i, a := range c.Args {
			args[i] = expand(a)
		}
		cmd = process.ExecCommand(ctx, expand(c.Command), args...)
	} else {
		cmd = process.ShellCommand(ctx, c.Command)
	}
	cmd.Dir = c.Dir
	cmd.Env = append(os.Environ(), "CLAUDE_PROJECT_DIR="+dir)
	cmd.Env = append(cmd.Env, c.Env...)
	cmd.Env = append(cmd.Env, "AGENTX_HOOK_EVENT="+string(ev.Type), "AGENTX_SESSION_ID="+ev.SessionID)
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Background children may keep the output pipes open; do not wait on
	// them indefinitely once the command itself has exited.
	cmd.WaitDelay = time.Second

	err = cmd.Run()
	killLeftovers(cmd, runtime.GOOS)
	// ErrWaitDelay means the command exited successfully but a background
	// child still held its output open. The command's own output is
	// complete, so it is honored rather than dropped as a hook error, which
	// would let a deny fail open.
	if errors.Is(err, exec.ErrWaitDelay) && ctx.Err() == nil && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		err = nil
	}
	if err == nil {
		return parseCommandOutput(ev.Type, stdout.Bytes())
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && ctx.Err() == nil {
		// A JSON object on stdout is read whatever the exit status.
		var res *Result
		if isJSONObject(stdout.Bytes()) {
			res, _ = parseCommandOutput(ev.Type, stdout.Bytes())
		}
		if exitErr.ExitCode() == 2 {
			if res == nil {
				res = &Result{}
			}
			res.Decision = Deny
			res.Reason = cmp.Or(strings.TrimSpace(stderr.String()), res.Reason)
			if rewake && res.Reason == "" && !isJSONObject(stdout.Bytes()) {
				// An asyncRewake hook shows stdout when stderr is empty.
				res.Reason = strings.TrimSpace(stdout.String())
			}
			res.Reason = cmp.Or(res.Reason, "blocked by hook")
			return res, nil
		}
		if res != nil {
			return res, nil
		}
	}
	switch {
	case ctx.Err() != nil:
		return nil, fmt.Errorf("command %q: %w", c.Command, ctx.Err())
	default:
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return nil, fmt.Errorf("command %q: %w", c.Command, err)
		}
		return nil, fmt.Errorf("command %q: %w: %s", c.Command, err, msg)
	}
}

// killLeftovers kills the background processes an exited shell left
// behind. It is Unix-only: there ShellCommand puts the shell in its own
// process group, and cmd.Cancel kills the group, which outlives the shell.
// On Windows, Cancel runs "taskkill /T" on the shell's PID after Go has
// released the process handle, so a reused PID could be killed, and
// taskkill cannot find orphaned children anyway.
func killLeftovers(cmd *exec.Cmd, goos string) {
	if goos != "windows" && cmd.Process != nil && cmd.Cancel != nil {
		_ = cmd.Cancel()
	}
}

type toolResponse struct {
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}

type commandPayload struct {
	HookEventName  EventType `json:"hook_event_name"`
	SessionID      string    `json:"session_id"`
	TranscriptPath string    `json:"transcript_path"`
	Cwd            string    `json:"cwd"`
	PermissionMode string    `json:"permission_mode"`

	RunID                string          `json:"run_id,omitempty"`
	Agent                string          `json:"agent,omitempty"`
	Turn                 int             `json:"turn,omitempty"`
	Prompt               string          `json:"prompt,omitempty"`
	Source               string          `json:"source,omitempty"`
	ToolName             string          `json:"tool_name,omitempty"`
	ToolUseID            string          `json:"tool_use_id,omitempty"`
	ToolInput            json.RawMessage `json:"tool_input,omitempty"`
	ToolResponse         *toolResponse   `json:"tool_response,omitempty"`
	Error                *string         `json:"error,omitempty"`
	IsInterrupt          *bool           `json:"is_interrupt,omitempty"`
	Message              string          `json:"message,omitempty"`
	Title                string          `json:"title,omitempty"`
	NotificationType     string          `json:"notification_type,omitempty"`
	Trigger              string          `json:"trigger,omitempty"`
	CustomInstructions   json.RawMessage `json:"custom_instructions,omitempty"`
	CompactSummary       *string         `json:"compact_summary,omitempty"`
	Reason               string          `json:"reason,omitempty"`
	StopHookActive       *bool           `json:"stop_hook_active,omitempty"`
	LastAssistantMessage *string         `json:"last_assistant_message,omitempty"`
	AgentID              string          `json:"agent_id,omitempty"`
	AgentType            string          `json:"agent_type,omitempty"`
	AgentTranscriptPath  *string         `json:"agent_transcript_path,omitempty"`
	Data                 map[string]any  `json:"data,omitempty"`
}

func commandInput(ev *Event) commandPayload {
	p := commandPayload{
		HookEventName:  ev.Type,
		SessionID:      ev.SessionID,
		PermissionMode: cmp.Or(ev.PermissionMode, "default"),
		RunID:          ev.RunID,
		Agent:          ev.Agent,
		Turn:           ev.Turn,
		Prompt:         ev.Prompt,
		Message:        ev.Message,
		Title:          ev.Title,
		Reason:         ev.Reason,
		Data:           ev.Data,
	}
	if ev.ToolCall != nil {
		p.ToolName = ev.ToolCall.Name
		p.ToolUseID = ev.ToolCall.ID
		p.ToolInput = ev.ToolCall.Arguments
	}
	if ev.ToolResult != nil {
		if ev.Type == PostToolUseFailure {
			p.Error, p.IsInterrupt = &ev.ToolResult.Content, new(false)
		} else {
			p.ToolResponse = &toolResponse{Content: ev.ToolResult.Content, IsError: ev.ToolResult.IsError}
		}
	}
	if ev.StopHookActive || ev.Type == Stop || ev.Type == SubagentStop {
		p.StopHookActive = new(ev.StopHookActive)
	}
	switch ev.Type {
	case SessionStart:
		p.Source = ev.Reason
	case Notification:
		p.NotificationType = notificationType(ev.Reason)
	case PreCompact:
		p.Trigger, p.CustomInstructions = ev.Reason, json.RawMessage("null")
	case PostCompact:
		p.Trigger, p.CompactSummary = ev.Reason, &ev.Message
	case Stop:
		p.LastAssistantMessage = &ev.Message
	case SubagentStart:
		p.AgentID, p.AgentType = ev.RunID, ev.Agent
	case SubagentStop:
		p.LastAssistantMessage = &ev.Message
		p.AgentID, p.AgentType, p.AgentTranscriptPath = ev.RunID, ev.Agent, new("")
	}
	return p
}

type commandOutput struct {
	Decision           string          `json:"decision"`
	Reason             string          `json:"reason"`
	Continue           *bool           `json:"continue"`
	StopReason         string          `json:"stopReason"`
	SystemMessage      string          `json:"systemMessage"`
	AdditionalContext  string          `json:"additionalContext"`
	HookSpecificOutput *specificOutput `json:"hookSpecificOutput"`
}

type specificOutput struct {
	PermissionDecision       string          `json:"permissionDecision"`
	PermissionDecisionReason string          `json:"permissionDecisionReason"`
	AdditionalContext        string          `json:"additionalContext"`
	UpdatedInput             json.RawMessage `json:"updatedInput"`
	UpdatedToolOutput        json.RawMessage `json:"updatedToolOutput"`
	UpdatedMCPToolOutput     json.RawMessage `json:"updatedMCPToolOutput"`
}

// isJSONObject reports whether stdout looks like a JSON object.
func isJSONObject(stdout []byte) bool {
	out := bytes.TrimSpace(stdout)
	return len(out) > 1 && out[0] == '{' && out[len(out)-1] == '}'
}

func parseCommandOutput(event EventType, stdout []byte) (*Result, error) {
	out := bytes.TrimSpace(stdout)
	if len(out) == 0 {
		return nil, nil
	}
	if out[0] != '{' {
		if event == SessionStart || event == UserPromptSubmit {
			return &Result{AdditionalContext: string(out)}, nil
		}
		return nil, nil
	}
	var co commandOutput
	if err := json.Unmarshal(out, &co); err != nil {
		return nil, fmt.Errorf("parse hook output: %w", err)
	}
	res := &Result{
		Decision:          parseDecision(co.Decision),
		Reason:            co.Reason,
		AdditionalContext: co.AdditionalContext,
		SystemMessage:     co.SystemMessage,
	}
	if co.Continue != nil && !*co.Continue {
		res.Halt = true
		res.HaltReason = co.StopReason
	}
	if so := co.HookSpecificOutput; so != nil {
		if d := parseDecision(so.PermissionDecision); d != Continue {
			res.Decision = d
			res.Reason = so.PermissionDecisionReason
		}
		if so.AdditionalContext != "" {
			if res.AdditionalContext != "" {
				res.AdditionalContext += "\n"
			}
			res.AdditionalContext += so.AdditionalContext
		}
		if len(so.UpdatedInput) > 0 && string(so.UpdatedInput) != "null" {
			res.UpdatedInput = so.UpdatedInput
		}
		if text, ok := toolOutputText(so.UpdatedMCPToolOutput); ok {
			res.UpdatedOutput = &text
		}
		if text, ok := toolOutputText(so.UpdatedToolOutput); ok {
			res.UpdatedOutput = &text
		}
	}
	if res.Decision == Defer {
		// Claude Code ignores both when a call is deferred.
		res.UpdatedInput, res.AdditionalContext = nil, ""
	}
	switch event {
	case Notification, PreCompact, PostCompact:
		res.Halt, res.HaltReason, res.SystemMessage = false, "", ""
	}
	return res, nil
}

// toolOutputText returns the replacement tool result held by an
// updatedToolOutput value: a string, or an object with a string "content"
// field like the tool_response agentx sends.
func toolOutputText(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, true
	}
	var obj struct {
		Content *string `json:"content"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Content != nil {
		return *obj.Content, true
	}
	return "", false
}

func parseDecision(s string) Decision {
	switch strings.ToLower(s) {
	case "allow", "approve":
		return Allow
	case "ask":
		return Ask
	case "defer":
		return Defer
	case "deny", "block":
		return Deny
	}
	return Continue
}
