//go:build !windows

package process

import (
	"errors"
	"os"
	"sync"
	"testing"
)

// countGroupSignals makes signalGroup count its calls until the test ends.
// Tests using it must not run in parallel.
func countGroupSignals(t *testing.T) func() int {
	t.Helper()
	var mu sync.Mutex
	n := 0
	orig := signalGroup
	signalGroup = func(pgid int) error {
		mu.Lock()
		n++
		mu.Unlock()
		return orig(pgid)
	}
	t.Cleanup(func() { signalGroup = orig })
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

// TestTreeSignalsOnceAfterWait checks that the process group is signalled
// at most once after the command was waited for: its ID may later belong
// to an unrelated group.
func TestTreeSignalsOnceAfterWait(t *testing.T) {
	for _, tc := range []struct {
		name string
		wait func(*Tree) error
	}{
		{"Tree.Wait", (*Tree).Wait},
		{"cmd.Wait then Kill", func(tr *Tree) error {
			err := tr.cmd.Wait()
			_ = tr.Kill()
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			signals := countGroupSignals(t)
			tree := NewTree(ShellCommand(t.Context(), "exit 0"))
			if err := tree.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				_ = tree.Close()
				_ = tree.cmd.Wait()
			}()
			if err := tc.wait(tree); err != nil {
				t.Fatal(err)
			}
			if n := signals(); n != 1 {
				t.Fatalf("%d signals after the wait, want 1", n)
			}
			for range 2 {
				if err := tree.Kill(); !errors.Is(err, os.ErrProcessDone) {
					t.Errorf("Kill = %v, want os.ErrProcessDone", err)
				}
				if err := tree.Close(); !errors.Is(err, os.ErrProcessDone) {
					t.Errorf("Close = %v, want os.ErrProcessDone", err)
				}
			}
			if n := signals(); n != 1 {
				t.Errorf("%d signals in total, want 1", n)
			}
		})
	}
}

// TestTreeKillBeforeWaitSignals checks that Kill signals the running
// command, and that Close afterwards signals nothing more once the command
// was waited for.
func TestTreeKillBeforeWaitSignals(t *testing.T) {
	signals := countGroupSignals(t)
	tree := NewTree(ShellCommand(t.Context(), "sleep 30"))
	if err := tree.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = tree.Close()
		_ = tree.cmd.Wait()
	}()
	if err := tree.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := tree.Wait(); err == nil {
		t.Fatal("killed command succeeded")
	}
	if err := tree.Close(); !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("Close = %v, want os.ErrProcessDone", err)
	}
	if n := signals(); n != 2 {
		t.Errorf("%d signals, want 2: Kill and the kill after Wait", n)
	}
}
