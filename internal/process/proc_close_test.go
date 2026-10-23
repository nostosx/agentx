package process_test

import (
	"errors"
	"os"
	"runtime"
	"testing"

	"github.com/nostosx/agentx/internal/process"
)

func TestTreeCloseIsIdempotent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell syntax")
	}
	t.Parallel()
	cmd := process.ShellCommand(t.Context(), "exit 0")
	tree := process.NewTree(cmd)
	if err := tree.Start(); err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	// Nothing is left to kill, and releasing twice is harmless.
	for range 2 {
		if err := tree.Close(); !errors.Is(err, os.ErrProcessDone) {
			t.Fatalf("Close = %v, want os.ErrProcessDone", err)
		}
	}
	if err := tree.Kill(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Kill after Close = %v, want os.ErrProcessDone", err)
	}
}

func TestTreeCloseBeforeStart(t *testing.T) {
	t.Parallel()
	tree := process.NewTree(process.ShellCommand(t.Context(), "exit 0"))
	if err := tree.Close(); err == nil {
		t.Fatal("Close of an unstarted tree succeeded")
	}
}
