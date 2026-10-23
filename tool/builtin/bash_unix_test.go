//go:build !windows

package builtin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// heldOutputChildEnv makes TestBashDiscardsOutputOfEscapedProcess act as a
// process that leaves the command's process group and keeps its output
// open.
const heldOutputChildEnv = "AGENTX_TEST_HELD_OUTPUT_CHILD"

func TestBashDiscardsOutputOfEscapedProcess(t *testing.T) {
	if pidFile := os.Getenv(heldOutputChildEnv); pidFile != "" {
		holdOutput(pidFile)
		return
	}
	t.Parallel()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	t.Cleanup(func() {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res := call(t, Bash(Config{Dir: dir, Env: []string{"TEST_BIN=" + bin, heldOutputChildEnv + "=" + pidFile}}), map[string]any{
		"command": `"$TEST_BIN" -test.run='^TestBashDiscardsOutputOfEscapedProcess$' & while [ ! -s pid ]; do sleep 0.01; done; echo shell done`,
	})
	if elapsed := time.Since(start); elapsed < outputDrainDelay || elapsed > outputDrainDelay+5*time.Second {
		t.Errorf("returned after %s", elapsed)
	}
	if res.IsError || !strings.Contains(res.Content, "early output") || !strings.Contains(res.Content, "shell done") ||
		!strings.Contains(res.Content, "[a background process kept the output open; later output was discarded]") {
		t.Fatalf("result = %q", res.Content)
	}
	// The escaped process survives the tree's kill, which is why its
	// output had to be cut off.
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		t.Fatalf("escaped process %d is gone; the held path was not exercised", pid)
	}
}

// holdOutput leaves the process group, reports its PID and keeps standard
// output open until it is killed.
func holdOutput(pidFile string) {
	if _, err := syscall.Setsid(); err != nil {
		fmt.Println("setsid:", err)
		os.Exit(1)
	}
	fmt.Println("early output")
	tmp := pidFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		os.Exit(1)
	}
	if err := os.Rename(tmp, pidFile); err != nil {
		os.Exit(1)
	}
	time.Sleep(30 * time.Second)
	fmt.Println("late output")
	os.Exit(0)
}
