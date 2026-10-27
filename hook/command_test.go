package hook

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nostosx/agentx/model"
)

func TestCommandHandle(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell snippets; parser and payload tests run on all platforms")
	}
	for _, tt := range []struct {
		name, command       string
		event               EventType
		decision            Decision
		context, wantReason string
		wantErr             bool
	}{
		{"JSON", `printf '%s' '{"decision":"approve","reason":"ok"}'`, PreToolUse, Allow, "", "ok", false},
		{"plain prompt", `printf 'welcome'`, SessionStart, Continue, "welcome", "", false},
		{"plain tool ignored", `printf 'ignored'`, PreToolUse, Continue, "", "", false},
		{"deny", `printf 'blocked' >&2; exit 2`, PreToolUse, Deny, "", "blocked", false},
		{"deny default", `exit 2`, PreToolUse, Deny, "", "blocked by hook", false},
		{"error", `printf 'failed' >&2; exit 1`, PreToolUse, Continue, "", "", true},
		{"malformed", `printf '{bad'`, PreToolUse, Continue, "", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := &Command{Command: tt.command}
			res, err := c.Handle(t.Context(), &Event{Type: tt.event})
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected command error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if res == nil {
				if tt.decision != Continue || tt.context != "" || tt.wantReason != "" {
					t.Fatal("missing result")
				}
				return
			}
			if res.Decision != tt.decision || res.AdditionalContext != tt.context || res.Reason != tt.wantReason {
				t.Fatalf("result=%+v", res)
			}
		})
	}
	c := &Command{Command: `printf '%s|%s|%s' "$AGENTX_HOOK_EVENT" "$AGENTX_SESSION_ID" "$CUSTOM_HOOK_TEST"`, Env: []string{"CUSTOM_HOOK_TEST=value"}, Dir: t.TempDir()}
	res, err := c.Handle(t.Context(), &Event{Type: SessionStart, SessionID: "session"})
	if err != nil || res.AdditionalContext != "SessionStart|session|value" {
		t.Fatalf("environment=%+v,%v", res, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.Handle(ctx, &Event{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled command=%v", err)
	}
}

func commandPayloadOf(t *testing.T, ev *Event) map[string]any {
	t.Helper()
	raw, err := json.Marshal(commandInput(ev))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestCommandInput(t *testing.T) {
	t.Parallel()
	ev := &Event{Type: PostToolUse, SessionID: "session", RunID: "run", Agent: "agent", Turn: 2, Prompt: "prompt", Message: "message", Reason: "reason", StopHookActive: true, ToolCall: &model.ToolCall{ID: "call", Name: "Edit", Arguments: json.RawMessage(`{"x":1}`)}, ToolResult: &model.ToolResult{Content: "failure", IsError: true}, Data: map[string]any{"extra": true}}
	got := commandPayloadOf(t, ev)
	if got["tool_name"] != "Edit" || got["tool_use_id"] != "call" || got["hook_event_name"] != "PostToolUse" || got["stop_hook_active"] != true || got["tool_input"].(map[string]any)["x"] != float64(1) || got["tool_response"].(map[string]any)["is_error"] != true {
		t.Fatalf("payload=%v", got)
	}
	// Claude Code's common fields are always present.
	if got["session_id"] != "session" || got["transcript_path"] != "" || got["permission_mode"] != "default" {
		t.Fatalf("common fields=%v", got)
	}
	if _, ok := got["cwd"]; !ok {
		t.Fatalf("payload has no cwd: %v", got)
	}
	if got := commandPayloadOf(t, &Event{Type: PreToolUse}); got["session_id"] != "" || got["stop_hook_active"] != nil || got["last_assistant_message"] != nil {
		t.Fatalf("empty payload=%v", got)
	}
}

func TestCommandInputEventFields(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		ev     *Event
		want   map[string]any
		absent []string
	}{
		{"SessionStart", &Event{Type: SessionStart, Reason: "startup", Prompt: "hi"},
			map[string]any{"source": "startup", "prompt": "hi"}, []string{"stop_hook_active"}},
		{"PostToolUseFailure", &Event{Type: PostToolUseFailure, ToolCall: &model.ToolCall{Name: "bash"}, ToolResult: &model.ToolResult{Content: "Exit code 1", IsError: true}},
			map[string]any{"tool_name": "bash", "error": "Exit code 1", "is_interrupt": false}, []string{"tool_response"}},
		{"Notification", &Event{Type: Notification, Reason: "approval", Message: "Approval required for deploy", Title: "Approval required"},
			map[string]any{"notification_type": "permission_prompt", "message": "Approval required for deploy", "title": "Approval required"}, nil},
		{"PreCompact", &Event{Type: PreCompact, Reason: "auto"},
			map[string]any{"trigger": "auto", "custom_instructions": nil}, []string{"compact_summary"}},
		{"PostCompact", &Event{Type: PostCompact, Reason: "auto", Message: "summary"},
			map[string]any{"trigger": "auto", "compact_summary": "summary"}, []string{"custom_instructions"}},
		{"Stop", &Event{Type: Stop, Message: "done"},
			map[string]any{"stop_hook_active": false, "last_assistant_message": "done"}, []string{"agent_id"}},
		{"SubagentStart", &Event{Type: SubagentStart, Agent: "writer", RunID: "sub-run", Reason: "writer", Prompt: "draft it"},
			map[string]any{"agent_id": "sub-run", "agent_type": "writer", "prompt": "draft it"}, []string{"agent_transcript_path", "last_assistant_message"}},
		{"SubagentStop", &Event{Type: SubagentStop, Agent: "writer", RunID: "sub-run", Reason: "writer", Message: "draft", StopHookActive: true},
			map[string]any{"stop_hook_active": true, "last_assistant_message": "draft", "agent_id": "sub-run", "agent_type": "writer", "agent_transcript_path": ""}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := commandPayloadOf(t, tt.ev)
			for k, v := range tt.want {
				if gv, ok := got[k]; !ok || gv != v {
					t.Errorf("%s = %#v (present %v), want %#v", k, gv, ok, v)
				}
			}
			for _, k := range tt.absent {
				if _, ok := got[k]; ok {
					t.Errorf("unexpected %s in %v", k, got)
				}
			}
		})
	}
}

func TestCommandTimeoutDefaults(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		deadline := func(ctx context.Context, timeout time.Duration, event EventType) time.Duration {
			ctx, cancel := withTimeout(ctx, timeout, event)
			defer cancel()
			d, ok := ctx.Deadline()
			if !ok {
				return -1
			}
			return time.Until(d)
		}
		ctx := t.Context()
		for _, tt := range []struct {
			ctx     context.Context
			timeout time.Duration
			event   EventType
			want    time.Duration
		}{
			{ctx, 0, PreToolUse, 600 * time.Second},
			{ctx, 0, UserPromptSubmit, 30 * time.Second},
			{ctx, 0, SessionEnd, 1500 * time.Millisecond},
			{context.WithValue(ctx, sessionEndKey{}, 5*time.Second), 0, SessionEnd, 5 * time.Second},
			{ctx, time.Second, UserPromptSubmit, time.Second},
			{ctx, -1, PreToolUse, -1},
		} {
			if got := deadline(tt.ctx, tt.timeout, tt.event); got != tt.want {
				t.Errorf("timeout(%v, %s) = %v, want %v", tt.timeout, tt.event, got, tt.want)
			}
		}
	})
}

func TestParseCommandOutput(t *testing.T) {
	t.Parallel()
	res, err := parseCommandOutput(PreToolUse, []byte(`{"decision":"allow","reason":"top","additionalContext":"one","continue":false,"stopReason":"stop","systemMessage":"careful","suppressOutput":true,"hookSpecificOutput":{"permissionDecision":"block","permissionDecisionReason":"specific","additionalContext":"two","updatedInput":{"x":2},"updatedToolOutput":"replacement"}}`))
	if err != nil || res.Decision != Deny || res.Reason != "specific" || res.AdditionalContext != "one\ntwo" || !res.Halt || res.HaltReason != "stop" || res.SystemMessage != "careful" || string(res.UpdatedInput) != `{"x":2}` || res.UpdatedOutput == nil || *res.UpdatedOutput != "replacement" {
		t.Fatalf("parsed=%+v,%v", res, err)
	}
	res, err = parseCommandOutput(PreToolUse, []byte(`{"hookSpecificOutput":{"updatedInput":null}}`))
	if err != nil || res.UpdatedInput != nil {
		t.Fatalf("null update=%+v,%v", res, err)
	}
	if res, err := parseCommandOutput(Stop, []byte(" \n ")); res != nil || err != nil {
		t.Fatalf("empty=%+v,%v", res, err)
	}
	if _, err := parseCommandOutput(Stop, []byte("{broken")); err == nil || !strings.Contains(err.Error(), "parse hook output") {
		t.Fatalf("malformed=%v", err)
	}
	for _, tt := range []struct {
		name string
		want Decision
	}{{"ALLOW", Allow}, {"approve", Allow}, {"ask", Ask}, {"defer", Defer}, {"deny", Deny}, {"BLOCK", Deny}, {"unknown", Continue}} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := parseDecision(tt.name); got != tt.want {
				t.Fatalf("parseDecision=%q", got)
			}
		})
	}
}

func TestParseCommandOutputUpdatedToolOutput(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, specific string
		want           *string
	}{
		{"string", `"updatedToolOutput":"new"`, new("new")},
		{"content object", `"updatedToolOutput":{"content":"new","is_error":false}`, new("new")},
		{"mismatched shape ignored", `"updatedToolOutput":{"stdout":"new"}`, nil},
		{"null ignored", `"updatedToolOutput":null`, nil},
		{"mcp", `"updatedMCPToolOutput":"mcp"`, new("mcp")},
		{"removed agentx name ignored", `"updatedOutput":"old"`, nil},
		{"precedence", `"updatedMCPToolOutput":"mcp","updatedToolOutput":"new"`, new("new")},
		{"mismatch keeps mcp", `"updatedMCPToolOutput":"mcp","updatedToolOutput":[1]`, new("mcp")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res, err := parseCommandOutput(PostToolUse, []byte(`{"hookSpecificOutput":{"hookEventName":"PostToolUse",`+tt.specific+`}}`))
			if err != nil {
				t.Fatal(err)
			}
			if (res.UpdatedOutput == nil) != (tt.want == nil) || tt.want != nil && *res.UpdatedOutput != *tt.want {
				t.Fatalf("UpdatedOutput = %v, want %v", res.UpdatedOutput, tt.want)
			}
		})
	}
}

func TestParseCommandOutputEventRules(t *testing.T) {
	t.Parallel()
	// A deferred call drops updatedInput and additionalContext.
	res, err := parseCommandOutput(PreToolUse, []byte(`{"additionalContext":"top","hookSpecificOutput":{"permissionDecision":"defer","permissionDecisionReason":"later","additionalContext":"ctx","updatedInput":{"x":1}}}`))
	if err != nil || res.Decision != Defer || res.Reason != "later" || res.UpdatedInput != nil || res.AdditionalContext != "" {
		t.Fatalf("defer=%+v,%v", res, err)
	}
	// Notification, PreCompact and PostCompact discard continue and
	// systemMessage.
	for _, ev := range []EventType{Notification, PreCompact, PostCompact} {
		res, err := parseCommandOutput(ev, []byte(`{"continue":false,"stopReason":"stop","systemMessage":"note","decision":"block"}`))
		if err != nil || res.Halt || res.HaltReason != "" || res.SystemMessage != "" || res.Decision != Deny {
			t.Errorf("%s: %+v,%v", ev, res, err)
		}
	}
	if res, err := parseCommandOutput(Stop, []byte(`{"continue":false,"systemMessage":"note"}`)); err != nil || !res.Halt || res.SystemMessage != "note" {
		t.Fatalf("stop=%+v,%v", res, err)
	}
}

func TestKillLeftoversUnixOnly(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		goos string
		want bool
	}{{"linux", true}, {"darwin", true}, {"windows", false}} {
		called := false
		cmd := &exec.Cmd{Process: &os.Process{Pid: 1}, Cancel: func() error { called = true; return nil }}
		killLeftovers(cmd, tt.goos)
		if called != tt.want {
			t.Errorf("%s: Cancel called = %v, want %v", tt.goos, called, tt.want)
		}
	}
	// A command that never started has nothing to kill.
	killLeftovers(&exec.Cmd{Cancel: func() error { t.Error("Cancel called without a process"); return nil }}, "linux")
}
