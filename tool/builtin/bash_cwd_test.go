package builtin

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nostosx/agentx/internal/process"
)

func TestBashKeepsWorkingDirectory(t *testing.T) {
	t.Parallel()
	skipWindows(t)
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"sub/deeper/f.txt": "x"})
	shells := &Shells{}
	t.Cleanup(func() { shells.Close() })
	cfg := Config{Dir: dir, Shells: shells}
	b := Bash(cfg)
	pwd := func() string {
		t.Helper()
		res := call(t, b, map[string]any{"command": "pwd"})
		if res.IsError {
			t.Fatalf("pwd: %q", res.Content)
		}
		return strings.TrimSpace(res.Content)
	}
	if got := pwd(); got != dir {
		t.Fatalf("initial pwd = %q", got)
	}
	// A cd carries over, also after a failing command that exits.
	res := call(t, b, map[string]any{"command": "cd sub && exit 3"})
	if !res.IsError || !strings.Contains(res.Content, "[exit code 3]") {
		t.Fatalf("exit: %q", res.Content)
	}
	if got := pwd(); got != filepath.Join(dir, "sub") {
		t.Fatalf("pwd after cd = %q", got)
	}
	call(t, b, map[string]any{"command": "cd deeper"})
	if res := call(t, b, map[string]any{"command": "cat f.txt"}); res.Content != "x" {
		t.Fatalf("relative path after cd: %q", res.Content)
	}
	// Background commands start there but do not move it.
	res = call(t, b, map[string]any{"command": "pwd; cd /", "run_in_background": true})
	if out := waitShell(t, cfg, shellID(t, res.Content), 5*time.Second); !strings.Contains(out, filepath.Join(dir, "sub", "deeper")) {
		t.Fatalf("background pwd: %q", out)
	}
	if got := pwd(); got != filepath.Join(dir, "sub", "deeper") {
		t.Fatalf("pwd after background cd = %q", got)
	}
	// A cd out of the working directory resets it.
	res = call(t, b, map[string]any{"command": "cd / && echo moved"})
	if !strings.Contains(res.Content, "moved") || !strings.Contains(res.Content, "[Shell cwd was reset to "+dir+"]") {
		t.Fatalf("cd out: %q", res.Content)
	}
	if got := pwd(); got != dir {
		t.Fatalf("pwd after reset = %q", got)
	}
	// So does a link that leads out of it.
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "out")); err != nil {
		t.Fatal(err)
	}
	if res := call(t, b, map[string]any{"command": "cd out"}); !strings.Contains(res.Content, "Shell cwd was reset") {
		t.Fatalf("cd through a link: %q", res.Content)
	}
	// A directory that disappeared resets it before the next command.
	call(t, b, map[string]any{"command": "mkdir gone && cd gone"})
	if err := os.Remove(filepath.Join(dir, "gone")); err != nil {
		t.Fatal(err)
	}
	res = call(t, b, map[string]any{"command": "pwd"})
	if !strings.HasPrefix(res.Content, dir+"\n") || !strings.Contains(res.Content, "Shell cwd was reset") {
		t.Fatalf("vanished directory: %q", res.Content)
	}
	// Another bash tool, such as a sub-agent's, starts in the working
	// directory.
	call(t, b, map[string]any{"command": "cd sub"})
	if res := call(t, Bash(Config{Dir: dir}), map[string]any{"command": "pwd"}); strings.TrimSpace(res.Content) != dir {
		t.Fatalf("other tool pwd = %q", res.Content)
	}
}

func TestBashResetDir(t *testing.T) {
	t.Parallel()
	skipWindows(t)
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"sub/f": ""})
	b := Bash(Config{Dir: dir, ResetDir: true})
	call(t, b, map[string]any{"command": "cd sub"})
	if res := call(t, b, map[string]any{"command": "pwd"}); strings.TrimSpace(res.Content) != dir {
		t.Fatalf("pwd = %q", res.Content)
	}
	if !strings.Contains(b.Info().Description, "cd do not persist") {
		t.Fatalf("description: %s", b.Info().Description)
	}
}

func TestBashCwdTrapKeepsShellSemantics(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("no exit trap on Windows")
	}
	dir := t.TempDir()
	b := Bash(Config{Dir: dir})
	// Line numbers, the exit status and a trap of the command's own work
	// as before; a replaced trap only loses the directory.
	const script = "echo $LINENO\necho $LINENO; false"
	// Bash versions differ in LINENO for -c scripts; compare with the same
	// shell without the cwd trap instead of assuming a particular version.
	plain := process.BashCommand(t.Context(), script)
	plain.Dir = dir
	output, err := plain.CombinedOutput()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("plain shell: output %q, err %v", output, err)
	}
	res := call(t, b, map[string]any{"command": script})
	if want := string(output) + "\n[exit code 1]"; !res.IsError || res.Content != want {
		t.Fatalf("result: %q, want %q with an error", res.Content, want)
	}
	writeFiles(t, dir, map[string]string{"sub/f": ""})
	res = call(t, b, map[string]any{"command": "trap 'echo own trap' EXIT; cd sub"})
	if !strings.Contains(res.Content, "own trap") || strings.Contains(res.Content, "reset") {
		t.Fatalf("own trap: %q", res.Content)
	}
	if res := call(t, b, map[string]any{"command": "pwd"}); strings.TrimSpace(res.Content) != dir {
		t.Fatalf("pwd after a replaced trap = %q", res.Content)
	}
	// Output on descriptor 3 does not move the directory outside.
	res = call(t, b, map[string]any{"command": "echo / >&3"})
	if !strings.Contains(res.Content, "Shell cwd was reset") {
		t.Fatalf("fd 3: %q", res.Content)
	}
}
