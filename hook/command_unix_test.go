//go:build !windows

package hook

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCommandBackgroundChildDoesNotDropDecision(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, command, reason string }{
		{"JSON deny", `(sleep 3) & echo '{"decision":"deny","reason":"no"}'`, "no"},
		{"exit 2", `(sleep 3) & echo 'stderr reason' >&2; exit 2`, "stderr reason"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res, err := (&Command{Command: tt.command}).Handle(t.Context(), &Event{Type: PreToolUse})
			if err != nil || res == nil || res.Decision != Deny || res.Reason != tt.reason {
				t.Fatalf("result=%+v, %v", res, err)
			}
		})
	}
}

func TestCommandKillsBackgroundChildren(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "pid")
	script := `sh -c 'echo $$ > "$PID_FILE"; exec sleep 30' & while [ ! -s "$PID_FILE" ]; do sleep 0.01; done; echo ok`
	res, err := (&Command{Command: script, Env: []string{"PID_FILE=" + pidFile}}).Handle(t.Context(), &Event{Type: SessionStart})
	if err != nil || res == nil || res.AdditionalContext != "ok" {
		t.Fatalf("result=%+v, %v", res, err)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("background child survived the hook")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCommandExitStatusesWithJSON(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, command string
		decision      Decision
		reason, extra string
		wantErr       bool
	}{
		// Claude Code reads a JSON object whatever the exit status.
		{"exit 1 with JSON", `echo '{"decision":"block","reason":"json"}'; exit 1`, Deny, "json", "", false},
		{"exit 1 plain", `echo plain; exit 1`, Continue, "", "", true},
		// JSON fields apply on exit 2 but cannot override the deny.
		{"exit 2 with JSON", `echo '{"hookSpecificOutput":{"permissionDecision":"allow","additionalContext":"ctx"}}'; echo nope >&2; exit 2`, Deny, "nope", "ctx", false},
		{"exit 2 JSON reason", `echo '{"reason":"json reason"}'; exit 2`, Deny, "json reason", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res, err := (&Command{Command: tt.command}).Handle(t.Context(), &Event{Type: PreToolUse})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", res)
				}
				return
			}
			if err != nil || res == nil || res.Decision != tt.decision || res.Reason != tt.reason || res.AdditionalContext != tt.extra {
				t.Fatalf("result=%+v, %v", res, err)
			}
		})
	}
}

func TestCommandProjectDirAndExecForm(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stdin := filepath.Join(dir, "stdin.json")
	// Shell form: CLAUDE_PROJECT_DIR is set and the payload's cwd is Dir.
	c := &Command{Command: `cat > "$OUT"; printf '%s' "$CLAUDE_PROJECT_DIR"`, Dir: dir, Env: []string{"OUT=" + stdin}}
	res, err := c.Handle(t.Context(), &Event{Type: SessionStart, Reason: "startup"})
	if err != nil || res == nil || res.AdditionalContext != dir {
		t.Fatalf("shell form=%+v, %v", res, err)
	}
	raw, err := os.ReadFile(stdin)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"cwd":"`+dir+`"`) || !strings.Contains(string(raw), `"source":"startup"`) {
		t.Fatalf("stdin=%s", raw)
	}
	// Exec form substitutes the placeholder without a shell.
	script := filepath.Join(dir, "hook.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s|%s' \"$1\" \"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c = &Command{Command: "${CLAUDE_PROJECT_DIR}/hook.sh", Args: []string{"${CLAUDE_PROJECT_DIR}", "it's $HOME"}, Dir: dir}
	res, err = c.Handle(t.Context(), &Event{Type: UserPromptSubmit})
	if err != nil || res == nil || res.AdditionalContext != dir+"|it's $HOME" {
		t.Fatalf("exec form=%+v, %v", res, err)
	}
	// An empty Args still selects the exec form.
	c = &Command{Command: script, Args: []string{}}
	if res, err := c.Handle(t.Context(), &Event{Type: UserPromptSubmit}); err != nil || res == nil || res.AdditionalContext != "|" {
		t.Fatalf("empty args=%+v, %v", res, err)
	}
}

func TestCommandRewakeShowsStdout(t *testing.T) {
	t.Parallel()
	c := &Command{Command: `echo "build failed"; exit 2`}
	ctx := t.Context()
	res, err := (&Async{Hook: c, Rewake: true}).Handle(ctx, &Event{Type: Stop})
	if err != nil || res.Decision != Deny || res.Reason != "build failed" {
		t.Fatalf("rewake = %+v, %v", res, err)
	}
	// Synchronous hooks keep the generic reason.
	res, err = c.Handle(t.Context(), &Event{Type: Stop})
	if err != nil || res.Reason != "blocked by hook" {
		t.Fatalf("sync = %+v, %v", res, err)
	}
}
