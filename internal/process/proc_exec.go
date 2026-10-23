package process

import (
	"context"
	"os/exec"
)

// ExecCommand returns a command running name with args directly, without a
// shell. Like ShellCommand, cancelling ctx kills the command together with
// the processes it started.
func ExecCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	killProcessTree(cmd)
	return cmd
}
