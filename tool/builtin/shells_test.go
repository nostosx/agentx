package builtin

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nostosx/agentx/tool"
)

var shellIDPattern = regexp.MustCompile(`bash_\d+`)

// shellID returns the ID in the result of a background bash call.
func shellID(t *testing.T, content string) string {
	t.Helper()
	id := shellIDPattern.FindString(content)
	if id == "" {
		t.Fatalf("no shell ID in %q", content)
	}
	return id
}

// waitShell reads the output of a background command until it finished
// and returns all of it.
func waitShell(t *testing.T, cfg Config, id string, timeout time.Duration) string {
	t.Helper()
	out := BashOutput(cfg)
	var all strings.Builder
	deadline := time.Now().Add(timeout)
	for {
		res := call(t, out, map[string]any{"shell_id": id})
		if res.IsError {
			t.Fatalf("bash_output: %s", res.Content)
		}
		all.WriteString(res.Content + "\n")
		if !strings.Contains(res.Content, ": running for") {
			return all.String()
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s still running after %s:\n%s", id, timeout, all.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func skipWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell syntax")
	}
}

func TestBackgroundCommandOutput(t *testing.T) {
	t.Parallel()
	skipWindows(t)
	shells := &Shells{}
	t.Cleanup(func() { shells.Close() })
	cfg := Config{Dir: t.TempDir(), Shells: shells}
	res := call(t, Bash(cfg), map[string]any{
		"command":           "echo one; echo skip; while [ ! -f go ]; do sleep 0.01; done; echo two; echo skip2; exit 3",
		"run_in_background": true,
	})
	if res.IsError {
		t.Fatalf("start: %q", res.Content)
	}
	id := shellID(t, res.Content)
	out := BashOutput(cfg)
	deadline := time.Now().Add(5 * time.Second)
	var first string
	// The command line, which every read repeats, contains "one" too.
	for !strings.Contains(first, "Output:\none") {
		if time.Now().After(deadline) {
			t.Fatalf("no output: %q", first)
		}
		time.Sleep(10 * time.Millisecond)
		first += call(t, out, map[string]any{"shell_id": id, "filter": "^(one|two)$"}).Content
	}
	if _, output, _ := strings.Cut(first, "Output:"); strings.Contains(output, "skip") || !strings.Contains(first, "running for") {
		t.Fatalf("first read: %q", first)
	}
	// A read consumes the output, including the lines the filter dropped.
	if res := call(t, out, map[string]any{"shell_id": id}); !strings.Contains(res.Content, "(no new output)") {
		t.Fatalf("second read: %q", res.Content)
	}
	if err := os.WriteFile(filepath.Join(cfg.Dir, "go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	rest := waitShell(t, cfg, id, 5*time.Second)
	if !strings.Contains(rest, "\ntwo") || !strings.Contains(rest, "\nskip2") || strings.Contains(rest, "\none") ||
		!strings.Contains(rest, "exited with code 3") {
		t.Fatalf("rest: %q", rest)
	}
	// kill_shell on a finished command reports how it ended.
	if res := call(t, KillShell(cfg), map[string]any{"shell_id": id}); res.IsError || !strings.Contains(res.Content, "already finished: exited with code 3") {
		t.Fatalf("kill finished: %q", res.Content)
	}
	if res := call(t, out, map[string]any{"shell_id": id, "filter": "("}); !res.IsError {
		t.Fatalf("bad filter: %q", res.Content)
	}
	if res := call(t, out, map[string]any{"shell_id": "bash_99"}); !res.IsError || !strings.Contains(res.Content, id) {
		t.Fatalf("unknown ID: %q", res.Content)
	}
}

func TestKillShellKillsTree(t *testing.T) {
	t.Parallel()
	skipWindows(t)
	shells := &Shells{}
	t.Cleanup(func() { shells.Close() })
	dir := t.TempDir()
	cfg := Config{Dir: dir, Shells: shells}
	res := call(t, Bash(cfg), map[string]any{
		"command":           "sleep 60 & echo $! > child; echo started; wait",
		"run_in_background": true,
	})
	id := shellID(t, res.Content)
	pid := waitFile(t, filepath.Join(dir, "child"))
	if got := shells.Running(); len(got) != 1 || got[0] != id {
		t.Fatalf("Running = %v", got)
	}
	res = call(t, KillShell(cfg), map[string]any{"shell_id": id})
	if res.IsError || !strings.Contains(res.Content, "Killed shell "+id) || !strings.Contains(res.Content, "killed after") {
		t.Fatalf("kill: %q", res.Content)
	}
	waitGone(t, pid)
	if got := shells.Running(); len(got) != 0 {
		t.Fatalf("Running after kill = %v", got)
	}
	if res := call(t, BashOutput(cfg), map[string]any{"shell_id": id}); !strings.Contains(res.Content, "started") || !strings.Contains(res.Content, "killed after") {
		t.Fatalf("output after kill: %q", res.Content)
	}
}

func TestShellsLimitAndClose(t *testing.T) {
	t.Parallel()
	skipWindows(t)
	shells := &Shells{MaxRunning: 2}
	t.Cleanup(func() { shells.Close() })
	dir := t.TempDir()
	cfg := Config{Dir: dir, Shells: shells}
	bash := Bash(cfg)
	for i := range 2 {
		res := call(t, bash, map[string]any{"command": "sleep 60", "run_in_background": true})
		if res.IsError {
			t.Fatalf("start %d: %q", i, res.Content)
		}
	}
	res := call(t, bash, map[string]any{"command": "sleep 60", "run_in_background": true})
	if !res.IsError || !strings.Contains(res.Content, "limit") {
		t.Fatalf("third start: %q", res.Content)
	}
	if err := Close([]tool.Tool{bash}); err != nil {
		t.Fatal(err)
	}
	if got := shells.Running(); len(got) != 0 {
		t.Fatalf("Running after Close = %v", got)
	}
	res = call(t, bash, map[string]any{"command": "true", "run_in_background": true})
	if !res.IsError || !strings.Contains(res.Content, "shut down") {
		t.Fatalf("start after Close: %q", res.Content)
	}
}

func TestBackgroundTimeout(t *testing.T) {
	t.Parallel()
	skipWindows(t)
	shells := &Shells{}
	t.Cleanup(func() { shells.Close() })
	cfg := Config{Dir: t.TempDir(), Shells: shells}
	res := call(t, Bash(cfg), map[string]any{"command": "echo begun; sleep 60", "run_in_background": true, "timeout_ms": 300})
	out := waitShell(t, cfg, shellID(t, res.Content), 10*time.Second)
	if !strings.Contains(out, "begun") || !strings.Contains(out, "timed out after 300ms") {
		t.Fatalf("output: %q", out)
	}
}

func TestShellOutputBuffer(t *testing.T) {
	t.Parallel()
	s := &shell{max: 10, done: make(chan struct{})}
	s.started = time.Now()
	for range 5 {
		_, _ = s.Write([]byte("0123456789"))
	}
	if len(s.out) > 2*s.max || s.total != 50 {
		t.Fatalf("buffer holds %d bytes, total %d", len(s.out), s.total)
	}
	got := s.report(nil, 1000)
	if !strings.Contains(got, "bytes of earlier output were dropped") || !strings.HasSuffix(got, "0123456789") {
		t.Fatalf("report: %q", got)
	}
	if got := s.report(nil, 1000); !strings.Contains(got, "(no new output)") || strings.Contains(got, "dropped") {
		t.Fatalf("second report: %q", got)
	}
}

func TestBashWithoutShellsHasNoBackground(t *testing.T) {
	t.Parallel()
	b := Bash(Config{Dir: t.TempDir()})
	if _, ok := b.Info().Parameters.Properties["run_in_background"]; ok {
		t.Fatal("run_in_background offered without Shells")
	}
	if _, ok := b.Info().Parameters.Properties["dangerously_disable_sandbox"]; ok {
		t.Fatal("dangerously_disable_sandbox offered without a sandbox")
	}
	res := call(t, b, map[string]any{"command": "echo hi", "run_in_background": true})
	if !res.IsError || !strings.Contains(res.Content, "not enabled") {
		t.Fatalf("background without Shells: %q", res.Content)
	}
	if res := call(t, BashOutput(Config{}), map[string]any{"shell_id": "bash_1"}); !res.IsError {
		t.Fatalf("bash_output without Shells: %q", res.Content)
	}
	if res := call(t, KillShell(Config{}), map[string]any{"shell_id": "bash_1"}); !res.IsError {
		t.Fatalf("kill_shell without Shells: %q", res.Content)
	}
}

// waitFile waits for a file with a process ID and returns it.
func waitFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil && strings.HasSuffix(string(b), "\n") {
			return strings.TrimSpace(string(b))
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was not written", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitGone waits for a process to exit.
func waitGone(t *testing.T, pid string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(t, pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %s survived", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
