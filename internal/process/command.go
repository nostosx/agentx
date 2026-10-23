package process

import (
	"context"
	"os/exec"
)

// ShellCommand returns a command that runs script with "sh -c", or
// "cmd /d /s /c" on Windows, where script is passed to cmd.exe verbatim and
// cmd.exe parses it as typed at its prompt, expanding %VAR% and honoring
// ^, &, |, < and >.
// Cancelling ctx kills the shell and every process it started, not only the
// shell.
func ShellCommand(ctx context.Context, script string) *exec.Cmd {
	cmd := shellCommand(ctx, script, false)
	killProcessTree(cmd)
	return cmd
}

// BashCommand is like ShellCommand but runs script with "bash -c" when bash
// is on PATH, falling back to "sh -c". On Windows it runs cmd.exe like
// ShellCommand.
func BashCommand(ctx context.Context, script string) *exec.Cmd {
	cmd := shellCommand(ctx, script, true)
	killProcessTree(cmd)
	return cmd
}
