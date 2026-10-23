package hook_test

import (
	"context"
	"testing"

	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/model"
)

func TestMatcherFollowsClaudeCodeRules(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name            string
		event           hook.EventType
		matcher, target string
		want            bool
	}{
		{"exact", hook.PreToolUse, "bash", "bash", true},
		{"exact is whole", hook.PreToolUse, "bash", "bash2", false},
		{"pipe list", hook.PreToolUse, "Edit|Write", "Write", true},
		{"comma list", hook.PreToolUse, "Edit, Write", "Write", true},
		{"blank", hook.PreToolUse, "  ", "anything", true},
		{"hyphen name", hook.SubagentStop, "code-reviewer", "code-reviewer", true},
		{"hyphen name exact", hook.SubagentStop, "code-reviewer", "code-reviewer-2", false},
		{"regex unanchored", hook.PreToolUse, "Edit.*", "NotebookEdit", true},
		{"regex anchored", hook.PreToolUse, "^Edit$", "NotebookEdit", false},
		{"regex prefix", hook.PreToolUse, "^Notebook", "NotebookEdit", true},
		{"mcp exact", hook.PreToolUse, "mcp__memory", "mcp__memory__create", false},
		{"mcp regex", hook.PreToolUse, "mcp__memory__.*", "mcp__memory__create", true},
		{"alias edit", hook.PreToolUse, "Edit|Write", "edit_file", true},
		{"alias write", hook.PostToolUse, "Edit|Write", "write_file", true},
		{"alias multi edit", hook.PreToolUse, "MultiEdit", "edit_file", true},
		{"alias edit patch", hook.PreToolUse, "Edit", "apply_patch", true},
		{"alias write patch", hook.PreToolUse, "^Write$", "apply_patch", true},
		{"alias patch not multi edit", hook.PreToolUse, "MultiEdit", "apply_patch", false},
		{"alias regex", hook.PreToolUse, "^Edit$", "edit_file", true},
		{"alias failure", hook.PostToolUseFailure, "Bash", "bash", true},
		{"alias read", hook.PreToolUse, "Read", "read_file", true},
		{"alias glob grep", hook.PreToolUse, "Glob|Grep", "grep", true},
		{"alias web fetch", hook.PreToolUse, "WebFetch", "web_fetch", true},
		{"alias todo", hook.PreToolUse, "TodoWrite", "todo_write", true},
		{"alias is one way", hook.PreToolUse, "Edit", "Edit2", false},
		{"alias not for others", hook.PreToolUse, "Edit", "read_file", false},
		{"notification type", hook.Notification, "permission_prompt", "approval", true},
		{"notification reason", hook.Notification, "approval", "approval", true},
		{"notification other", hook.Notification, "idle_prompt", "approval", false},
		{"session source", hook.SessionStart, "startup", "startup", true},
		{"session resume", hook.SessionStart, "resume", "startup", false},
		{"compact trigger", hook.PostCompact, "auto", "auto", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := hook.NewManager()
			called := false
			on(t, m, tt.event, tt.matcher, func(context.Context, *hook.Event) (*hook.Result, error) { called = true; return nil, nil })
			ev := &hook.Event{Type: tt.event, Reason: tt.target, ToolCall: &model.ToolCall{Name: tt.target}}
			if _, err := m.Dispatch(t.Context(), ev); err != nil || called != tt.want {
				t.Fatalf("matcher %q on %q: called=%v, err=%v", tt.matcher, tt.target, called, err)
			}
		})
	}
}
