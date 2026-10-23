package process_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nostosx/agentx/internal/process"
)

func TestTreeKillsDescendantsAfterExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell syntax")
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	cmd := process.ShellCommand(t.Context(), `sleep 30 > /dev/null 2>&1 & echo $! > "$AGENTX_TEST_PID_FILE"`)
	cmd.Env = append(os.Environ(), "AGENTX_TEST_PID_FILE="+pidFile)
	tree := process.NewTree(cmd)
	if err := tree.Start(); err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	// The orphaned sleep is reaped by init; until then signal 0 succeeds.
	for syscallAlive(t, pid) {
		if time.Now().After(deadline) {
			t.Fatalf("descendant %d survived Close", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := tree.Kill(); !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("Kill of an empty tree = %v, want os.ErrProcessDone", err)
	}
}

func syscallAlive(t *testing.T, pid int) bool {
	t.Helper()
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() == 1 && strings.TrimSpace(string(out)) == "" {
			return false
		}
		t.Fatalf("ps %d: %v", pid, err)
	}
	return !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

func TestTreeCancelKillsTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell syntax")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cmd := process.ShellCommand(ctx, "sleep 30")
	tree := process.NewTree(cmd)
	if err := tree.Start(); err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	cancel()
	err := cmd.Wait()
	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok {
		t.Fatalf("Wait = %v, want an exit error", err)
	}
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signal() != syscall.SIGKILL {
		t.Errorf("signal = %v, want SIGKILL", ws.Signal())
	}
}

func TestBashCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell syntax")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not on PATH")
	}
	out, err := process.BashCommand(t.Context(), `printf %s "$BASH_VERSION"`).Output()
	if err != nil || len(out) == 0 {
		t.Fatalf("BashCommand did not run bash: %q, %v", out, err)
	}
	out, err = process.ShellCommand(t.Context(), `echo "a 'b' \"c\""`).Output()
	if err != nil || strings.TrimSpace(string(out)) != `a 'b' "c"` {
		t.Fatalf("ShellCommand quoting: %q, %v", out, err)
	}
}
