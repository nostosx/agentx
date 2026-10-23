//go:build !windows

package process

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// shellCommand returns a command running script with "sh -c", or with
// "bash -c" when preferBash is set and bash is on PATH.
func shellCommand(ctx context.Context, script string, preferBash bool) *exec.Cmd {
	shell := "sh"
	if preferBash {
		if p, err := exec.LookPath("bash"); err == nil {
			shell = p
		}
	}
	return exec.CommandContext(ctx, shell, "-c", script)
}

// killProcessTree starts cmd in its own process group and makes context
// cancellation kill the whole group, so children such as a sleep started by
// the shell do not outlive the command.
func killProcessTree(cmd *exec.Cmd) {
	prepareTree(cmd)
	cmd.Cancel = func() error { return killGroup(cmd) }
}

// prepareTree makes cmd start in a new process group whose ID is its PID.
func prepareTree(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// signalGroup sends SIGKILL to the process group pgid. Tests replace it to
// count the signals sent.
var signalGroup = func(pgid int) error { return syscall.Kill(-pgid, syscall.SIGKILL) }

// killGroup kills the process group of cmd. It also reaches members that
// outlived the group leader.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return errors.New("exec: not started")
	}
	err := signalGroup(cmd.Process.Pid)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

// treeJob holds the platform state of a Tree; Unix needs none.
type treeJob struct{}

func (t *Tree) start() error { return t.cmd.Start() }

// kill kills the process group. While the leader was not waited for, its
// ID still names the group. Right after it was waited for, the group's
// remaining members still hold the ID; Tree.Kill makes this the last
// signal.
func (t *Tree) kill(bool) error { return killGroup(t.cmd) }

// release has nothing to free on Unix; it resets the empty job state.
func (t *Tree) release() { t.job = treeJob{} }
