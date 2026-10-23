package hook

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseSettings(t *testing.T) {
	t.Parallel()
	m, warnings, err := ParseSettings([]byte(`{
		"hooks": {
			"PreToolUse": [
				{"matcher": "Edit|Write", "hooks": [
					{"type": "command", "command": "./lint.sh", "timeout": 30, "statusMessage": "Linting"},
					{"type": "command", "command": "node", "args": ["${CLAUDE_PROJECT_DIR}/check.js"], "onFailure": "block"},
					{"type": "http", "url": "https://example.com/hook"},
					{"type": "prompt", "prompt": "Is this safe?"},
					{"type": "command", "command": "./if.sh", "if": "Bash(git *)"},
					{"type": "command", "command": "./async.sh", "async": true},
					{"type": "command", "command": "./ps.ps1", "shell": "powershell"}
				]},
				{"matcher": "[", "hooks": [{"type": "command", "command": "./bad.sh"}]}
			],
			"SubagentStart": [{"matcher": "writer", "hooks": [{"type": "command", "command": "./brief.sh"}]}],
			"Stop": [{"matcher": "ignored", "hooks": [{"type": "command", "command": "./stop.sh", "onFailure": "block"}]}],
			"PermissionRequest": [{"hooks": [{"type": "command", "command": "./perm.sh"}]}]
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	pre := m.entries[PreToolUse]
	if len(pre) != 4 {
		t.Fatalf("PreToolUse hooks = %d", len(pre))
	}
	lint, ok := pre[0].hook.(*Command)
	if !ok || lint.Command != "./lint.sh" || lint.Args != nil || lint.Timeout != 30*time.Second || !pre[0].matcher.match("Write") {
		t.Fatalf("lint hook = %#v", pre[0])
	}
	blocking, ok := pre[1].hook.(blockOnFailure)
	if !ok {
		t.Fatalf("onFailure block hook = %#v", pre[1].hook)
	}
	if check := blocking.Hook.(*Command); check.Command != "node" || !slices.Equal(check.Args, []string{"${CLAUDE_PROJECT_DIR}/check.js"}) || check.Timeout != 0 {
		t.Fatalf("exec hook = %#v", check)
	}
	if h, ok := pre[2].hook.(*HTTP); !ok || h.URL != "https://example.com/hook" {
		t.Fatalf("http hook = %#v", pre[2].hook)
	}
	if a, ok := pre[3].hook.(*Async); !ok || a.Rewake || a.Hook.(*Command).Timeout >= 0 {
		t.Fatalf("async hook = %#v", pre[3].hook)
	}
	if start := m.entries[SubagentStart]; len(start) != 1 || !start[0].matcher.match("writer") || start[0].matcher.match("reviewer") {
		t.Fatalf("SubagentStart hooks = %#v", start)
	}
	// Stop has no matcher support, so its matcher is ignored, and
	// onFailure has no effect on it.
	stop := m.entries[Stop]
	if len(stop) != 1 || stop[0].matcher != nil {
		t.Fatalf("Stop hooks = %#v", stop)
	}
	if _, ok := stop[0].hook.(*Command); !ok {
		t.Fatalf("Stop hook wrapped: %#v", stop[0].hook)
	}
	want := []string{
		`hooks.PermissionRequest: event not dispatched by agentx`,
		`hooks.PreToolUse[0].hooks[3]: prompt hooks need SettingsOptions.Model`,
		`hooks.PreToolUse[0].hooks[4]: the "if" field is not supported`,
		`hooks.PreToolUse[0].hooks[6]: shell "powershell" is not supported`,
		`hooks.PreToolUse[1]: invalid matcher "["`,
	}
	if len(warnings) != len(want) {
		t.Fatalf("warnings = %q", warnings)
	}
	for i, w := range want {
		if !strings.HasPrefix(warnings[i], w) || !strings.HasSuffix(warnings[i], "skipped") {
			t.Errorf("warning %d = %q, want prefix %q", i, warnings[i], w)
		}
	}
	if _, _, err := ParseSettings([]byte(`{"hooks": [`)); err == nil {
		t.Fatal("malformed settings accepted")
	}
	if m, _, err := ParseSettings([]byte(`{"disableAllHooks": true, "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "x"}]}]}}`)); err != nil || m.Has(Stop) {
		t.Fatalf("disableAllHooks = %v, %v", m.Has(Stop), err)
	}
}

func TestLoadSettingsMergesFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	shared := `{"matcher": "Bash", "hooks": [{"type": "command", "command": "./guard.sh"}]}`
	user := write("user.json", `{"disableAllHooks": true, "hooks": {"PreToolUse": [`+shared+`]}}`)
	project := write("project.json", `{"disableAllHooks": false, "hooks": {"PreToolUse": [`+shared+`, {"matcher": "Bash", "hooks": [{"type": "command", "command": "./guard.sh", "args": []}]}], "PostCompact": [{"hooks": [{"type": "agent", "prompt": "x"}]}]}}`)
	m, warnings, err := LoadSettings(user, filepath.Join(dir, "missing.json"), project)
	if err != nil {
		t.Fatal(err)
	}
	// The identical handler runs once; the exec form differs.
	if got := len(m.entries[PreToolUse]); got != 2 {
		t.Fatalf("PreToolUse hooks = %d", got)
	}
	if len(warnings) != 1 || !strings.HasPrefix(warnings[0], project+`: hooks.PostCompact[0].hooks[0]: PostCompact does not support agent hooks`) {
		t.Fatalf("warnings = %q", warnings)
	}
	// The later file decides disableAllHooks.
	if m, _, err := LoadSettings(project, user); err != nil || m.Has(PreToolUse) {
		t.Fatalf("disabled = %v, %v", m.Has(PreToolUse), err)
	}
	if _, _, err := LoadSettings(write("bad.json", "{")); err == nil || !strings.Contains(err.Error(), "bad.json") {
		t.Fatalf("malformed file = %v", err)
	}
	if _, _, err := LoadSettings(dir); err == nil {
		t.Fatal("directory accepted as settings file")
	}
}

func TestBlockOnFailure(t *testing.T) {
	t.Parallel()
	failing := blockOnFailure{Func(func(context.Context, *Event) (*Result, error) { return nil, errors.New("missing script") })}
	res, err := failing.Handle(t.Context(), &Event{Type: PreToolUse})
	if err != nil || res.Decision != Deny || !strings.Contains(res.Reason, `onFailure is "block": missing script`) {
		t.Fatalf("result = %+v, %v", res, err)
	}
	ok := blockOnFailure{Func(func(context.Context, *Event) (*Result, error) { return &Result{Decision: Allow}, nil })}
	if res, err := ok.Handle(t.Context(), &Event{Type: PreToolUse}); err != nil || res.Decision != Allow {
		t.Fatalf("result = %+v, %v", res, err)
	}
}
