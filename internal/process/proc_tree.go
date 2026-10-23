package process

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

// Tree runs a command so that it can be killed together with every process
// it starts. On Unix the command runs in a new process group. On Windows it
// is placed in a Job Object before it runs; if the job cannot be set up,
// Kill falls back to taskkill /T, which misses descendants whose parent
// already exited and cannot reach any descendant once the command was
// waited for.
//
// A process group is addressed by the ID of the command's process, which
// the system may reuse once the command was waited for and the group is
// empty. Tree therefore kills the group at most once after the command was
// waited for; later calls of Kill and Close signal nothing.
type Tree struct {
	cmd *exec.Cmd
	mu  sync.Mutex
	// over is set once the tree was killed after the command was waited
	// for, or Close released it. Nothing is signalled afterwards.
	over bool
	job  treeJob
}

// NewTree prepares cmd, which must not have been started. If cmd was
// created with exec.CommandContext, cancelling the context kills the tree.
// The command must be started with Tree.Start, not cmd.Start or cmd.Run.
func NewTree(cmd *exec.Cmd) *Tree {
	t := &Tree{cmd: cmd}
	prepareTree(cmd)
	if cmd.Cancel != nil {
		cmd.Cancel = t.Kill
	}
	return t
}

// Start starts the command. As with exec.Cmd.Start, the caller must wait
// for the command once Start succeeds, preferably with Tree.Wait.
func (t *Tree) Start() error { return t.start() }

// Wait waits for the command like cmd.Wait and then kills the processes it
// left behind.
func (t *Tree) Wait() error {
	err := t.cmd.Wait()
	_ = t.Kill()
	return err
}

// Kill kills the command and its descendants. It may be called at any time
// after Start. Once the command was waited for, the first call kills the
// descendants that outlived it; a caller that waits with cmd.Wait instead
// of Tree.Wait should call Kill right after it returns. Later calls, and
// calls after Close, signal nothing and return os.ErrProcessDone. Kill also
// returns os.ErrProcessDone when it finds nothing left to kill.
func (t *Tree) Kill() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cmd.Process == nil {
		return errors.New("exec: not started")
	}
	if t.over {
		return os.ErrProcessDone
	}
	// os.Process reports a waited process as done without signalling it.
	waited := errors.Is(t.cmd.Process.Signal(syscall.Signal(0)), os.ErrProcessDone)
	err := t.kill(waited)
	if waited {
		t.over = true
	}
	return err
}

// Close kills what is left of the tree and releases its resources. Like
// Kill, it signals nothing once the tree was killed after the command was
// waited for.
func (t *Tree) Close() error {
	err := t.Kill()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.over = true
	t.release()
	return err
}
