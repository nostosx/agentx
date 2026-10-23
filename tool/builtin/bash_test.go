package builtin

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestBashBackgroundChildDoesNotFailCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell syntax")
	}
	dir := t.TempDir()
	start := time.Now()
	res := call(t, Bash(Config{Dir: dir}), map[string]any{
		"command": "echo hello; sleep 30 & echo $! > pid",
	})
	if res.IsError || !strings.Contains(res.Content, "hello") {
		t.Fatalf("got %q", res.Content)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %s; the background child held the call", d)
	}
	b, err := os.ReadFile(filepath.Join(dir, "pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(b))
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(t, pid) {
		if time.Now().After(deadline) {
			t.Fatalf("background child %s survived the command", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestBashPrefersBash(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell syntax")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not on PATH")
	}
	res := call(t, Bash(Config{Dir: t.TempDir()}), map[string]any{"command": `echo "v=$BASH_VERSION"; [[ 1 == 1 ]] && echo bashism`})
	if res.IsError || strings.Contains(res.Content, "v=\n") || !strings.Contains(res.Content, "bashism") {
		t.Fatalf("not run by bash: %q", res.Content)
	}
}
