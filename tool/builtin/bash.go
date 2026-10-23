package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nostosx/agentx/internal/process"

	"github.com/nostosx/agentx/tool"
)

// outputDrainDelay bounds how long bash waits for the output of a command
// after its process tree was killed, in case a process that left the tree
// still holds the output open.
const outputDrainDelay = time.Second

// cwdTrap makes the shell write its final working directory to descriptor
// 3 when it exits, also through exit, so the next command can start there.
// It is prepended on the command's own line to keep line numbers intact.
const cwdTrap = "trap '{ pwd >&3; } 2>/dev/null' EXIT; "

// cwdReadDelay bounds the wait for the directory the shell reported.
const cwdReadDelay = 100 * time.Millisecond

type bashArgs struct {
	Command   string `json:"command" jsonschema:"The shell command to run."`
	TimeoutMS int    `json:"timeout_ms,omitempty" jsonschema:"Timeout in milliseconds (default 120000, at most 600000). For a background command, how long it may run (default 1800000, at most 7200000)."`
	// RunInBackground is offered when Config.Shells is set.
	RunInBackground bool `json:"run_in_background,omitempty" jsonschema:"Start the command in the background and return its shell ID at once, for servers, watchers and other long-running commands. Read its output with bash_output and stop it with kill_shell."`
	// DangerouslyDisableSandbox is offered when Config.Sandbox is set and
	// Config.NoSandboxEscape is not.
	DangerouslyDisableSandbox bool `json:"dangerously_disable_sandbox,omitempty" jsonschema:"Run the command outside the sandbox. Use it only to retry a command that failed because of sandbox restrictions; it always needs the user's approval."`
}

// bashTool is the bash tool. It decides per call whether a call needs
// approval and remembers the working directory between calls.
type bashTool struct {
	tool.Tool
	e *env

	mu sync.Mutex
	// cwd is where the next command starts.
	cwd string
}

// Bash returns the bash tool. Commands run with "bash -c" when bash is on
// PATH and with "sh -c" otherwise ("cmd /d /s /c" on Windows), starting in
// the working directory.
//
// The command runs in its own process tree: a process group on Unix, a Job
// Object on Windows. When the shell exits or the timeout expires, the whole
// tree is killed, so processes it left in the background do not outlive
// the call. With Config.Shells set, the model can instead start a command
// with run_in_background, which returns a shell ID at once; BashOutput and
// KillShell, given the same Shells, read and stop it.
//
// As in Claude Code, a cd carries over to later calls while it stays
// inside the working directory, symbolic links resolved; a command that
// ends elsewhere, or a directory that disappeared, resets the next command
// to the working directory and the result says "Shell cwd was reset to
// <dir>". The shell reports its final directory through descriptor 3 from
// an EXIT trap, so a command that replaces that trap, closes descriptor 3
// or replaces the shell with exec leaves the directory unchanged. Changes
// made by background commands never carry over. Config.ResetDir starts
// every command in the working directory. On Windows, where cmd.exe has no
// exit trap, every command starts in the working directory.
//
// Without Config.Sandbox, commands are not confined: they can access
// anything the process can, and every call needs approval unless
// Config.NoApproval is set. With a sandbox, commands, including
// background ones, run inside it and need no approval unless
// Config.AskSandboxed is set, like Claude Code's auto-allow sandbox mode.
// A command that fails because of the sandbox can be retried with the
// dangerously_disable_sandbox argument, which runs it outside the sandbox
// and always needs approval; Config.NoSandboxEscape removes that escape.
// Permission rules still apply on top: the argument can be matched as
// "Bash(dangerously_disable_sandbox:true)". When the sandbox is not
// available, Config.SandboxFallback either fails each command or runs it
// unsandboxed after approval. Info().RequiresApproval reports whether any
// call may need approval; bash implements tool.CallApprover for the
// decision of each call.
//
// The tool implements io.Closer: Close stops the background commands of
// Config.Shells.
func Bash(cfg Config) tool.Tool {
	e := newEnv(cfg)
	b := &bashTool{e: e, cwd: e.dir}
	schema := tool.SchemaOf[bashArgs]()
	if cfg.Shells == nil {
		delete(schema.Properties, "run_in_background")
	}
	if cfg.Sandbox == nil || cfg.NoSandboxEscape {
		delete(schema.Properties, "dangerously_disable_sandbox")
	}
	var opts []tool.Option
	if !e.NoApproval || e.Sandbox != nil {
		opts = append(opts, tool.RequireApproval())
	}
	b.Tool = tool.New("bash", b.description(), schema, func(ctx context.Context, args json.RawMessage) (*tool.Result, error) {
		var in bashArgs
		if len(args) > 0 && string(args) != "null" {
			if err := json.Unmarshal(args, &in); err != nil {
				return tool.Errorf("invalid arguments: %v", err), nil
			}
		}
		return b.run(ctx, in)
	}, opts...)
	return b
}

func (b *bashTool) description() string {
	var d strings.Builder
	d.WriteString("Run a shell command in the working directory and return its combined stdout and stderr. ")
	if b.tracksDir() {
		d.WriteString("A cd carries over to later commands while it stays inside the working directory; environment changes do not persist. ")
	} else {
		d.WriteString("Each call starts a new shell in the working directory, so environment changes and cd do not persist. ")
	}
	d.WriteString("Background processes are stopped when the command finishes")
	if b.e.Shells != nil {
		d.WriteString("; to keep a server, watcher or other long-running command going, set run_in_background, then read its output with bash_output and stop it with kill_shell. ")
	} else {
		d.WriteString(". ")
	}
	if b.e.Sandbox != nil {
		d.WriteString("Commands run in a sandbox that limits which files they can write and which hosts they can reach. ")
		if !b.e.NoSandboxEscape {
			d.WriteString("If a command fails because of the sandbox, you may retry it with dangerously_disable_sandbox, which needs the user's approval. ")
		}
	}
	d.WriteString("Prefer read_file, glob and grep over cat, find and grep for inspecting files. Avoid interactive commands.")
	return d.String()
}

// tracksDir reports whether a cd carries over to later commands.
func (b *bashTool) tracksDir() bool { return !b.e.ResetDir && runtime.GOOS != "windows" }

// RequiresApprovalFor reports whether a call needs approval: without a
// sandbox unless Config.NoApproval is set; with one when the command runs
// outside it, or when Config.AskSandboxed is set.
func (b *bashTool) RequiresApprovalFor(args json.RawMessage) bool {
	e := b.e
	if e.Sandbox == nil {
		return !e.NoApproval
	}
	var in bashArgs
	if json.Unmarshal(args, &in) != nil {
		return true
	}
	if in.DangerouslyDisableSandbox && !e.NoSandboxEscape {
		return true
	}
	if e.Sandbox.Available() != nil {
		// A required sandbox fails the call without running anything.
		return e.SandboxFallback == SandboxBestEffort
	}
	return e.AskSandboxed && !e.NoApproval
}

// Close stops the background commands of Config.Shells.
func (b *bashTool) Close() error {
	if b.e.Shells == nil {
		return nil
	}
	return b.e.Shells.Close()
}

// sandbox returns the sandbox the command runs in, nil to run it
// unsandboxed, and notes for the result. It fails when the sandbox is
// required but not available.
func (b *bashTool) sandbox(in bashArgs) (Sandbox, []string, error) {
	e := b.e
	if e.Sandbox == nil {
		return nil, nil, nil
	}
	var notes []string
	if in.DangerouslyDisableSandbox {
		if !e.NoSandboxEscape {
			return nil, nil, nil
		}
		notes = append(notes, "dangerously_disable_sandbox is disabled here; the command ran in the sandbox")
	}
	if err := e.Sandbox.Available(); err != nil {
		if e.SandboxFallback == SandboxBestEffort {
			return nil, append(notes, fmt.Sprintf("the sandbox is not available, so the command ran outside it: %v", err)), nil
		}
		return nil, nil, fmt.Errorf("the command was not run because the sandbox is not available: %w", err)
	}
	return e.Sandbox, notes, nil
}

// dir returns the directory the next command starts in, resetting it
// when it is no longer inside the working directory.
func (b *bashTool) dir() (string, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cwd == b.e.dir || b.inside(b.cwd) {
		return b.cwd, ""
	}
	b.cwd = b.e.dir
	return b.cwd, "Shell cwd was reset to " + b.e.dir
}

// setDir records the directory a command ended in.
func (b *bashTool) setDir(p string) string {
	if p == "" || !filepath.IsAbs(p) {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inside(p) {
		b.cwd = p
		return ""
	}
	b.cwd = b.e.dir
	return "Shell cwd was reset to " + b.e.dir
}

// inside reports whether p is an existing directory inside the working
// directory once symbolic links are resolved.
func (b *bashTool) inside(p string) bool {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return false
	}
	if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
		return false
	}
	return within(b.e.real, real)
}

// command returns the command running script in dir, in sb unless nil.
// cwd, if set, receives the shell's final directory.
func (b *bashTool) command(ctx context.Context, script, dir string, sb Sandbox, cwd *os.File) (*exec.Cmd, error) {
	if cwd != nil {
		script = cwdTrap + script
	}
	cmd := process.BashCommand(ctx, script)
	cmd.Dir = dir
	// The inherited PWD names the parent's directory; later entries win.
	cmd.Env = append(append(os.Environ(), "PWD="+dir), b.e.Env...)
	if cwd != nil {
		cmd.ExtraFiles = []*os.File{cwd}
	}
	if sb != nil {
		if err := sb.Wrap(cmd, b.e.dir); err != nil {
			return nil, fmt.Errorf("the command was not run because it could not be sandboxed: %w", err)
		}
	}
	return cmd, nil
}

func (b *bashTool) run(ctx context.Context, in bashArgs) (*tool.Result, error) {
	e := b.e
	if strings.TrimSpace(in.Command) == "" {
		return tool.Errorf("command is empty"), nil
	}
	sb, notes, err := b.sandbox(in)
	if err != nil {
		return tool.Errorf("%v", err), nil
	}
	dir, reset := b.dir()
	if reset != "" {
		notes = append(notes, reset)
	}
	if in.RunInBackground {
		return b.background(in, dir, sb, notes)
	}
	timeout := e.CommandTimeout
	if in.TimeoutMS > 0 {
		timeout = min(time.Duration(in.TimeoutMS)*time.Millisecond, maxCommandTimeout)
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cwdR, cwdW *os.File
	if b.tracksDir() {
		if cwdR, cwdW, err = os.Pipe(); err != nil {
			return nil, err
		}
		defer cwdR.Close()
	}
	cmd, err := b.command(runCtx, in.Command, dir, sb, cwdW)
	if err != nil {
		if cwdW != nil {
			cwdW.Close()
		}
		return tool.Errorf("%v", err), nil
	}
	start := time.Now()
	out := &limitWriter{max: e.MaxOutput}
	held, err := runTree(cmd, out)
	if cwdW != nil {
		cwdW.Close()
		if note := b.setDir(readCwd(cwdR)); note != "" {
			notes = append(notes, note)
		}
	}
	text := out.String()
	if strings.TrimSpace(text) == "" {
		text = "(no output)"
	}
	if held {
		text += "\n[a background process kept the output open; later output was discarded]"
	}
	if d, ok := sb.(interface{ DeniedHosts(time.Time) []string }); ok {
		if hosts := d.DeniedHosts(start); len(hosts) > 0 {
			notes = append(notes, "the sandbox refused connections to "+strings.Join(hosts, ", "))
		}
	}
	text = withNotes(text, notes)
	switch {
	case err == nil:
		return tool.Text(text), nil
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case runCtx.Err() != nil:
		return tool.Errorf("%s\n[command timed out after %s]", text, timeout), nil
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return tool.Errorf("%s\n[exit code %d]", text, exitErr.ExitCode()), nil
	}
	return nil, fmt.Errorf("run command: %w", err)
}

// background starts the command in Config.Shells.
func (b *bashTool) background(in bashArgs, dir string, sb Sandbox, notes []string) (*tool.Result, error) {
	e := b.e
	if e.Shells == nil {
		return tool.Errorf("background commands are not enabled"), nil
	}
	timeout := defaultBackgroundTimeout
	if in.TimeoutMS > 0 {
		timeout = min(time.Duration(in.TimeoutMS)*time.Millisecond, maxBackgroundTimeout)
	}
	s, err := e.Shells.start(in.Command, dir, timeout, sb, func(ctx context.Context) (*exec.Cmd, error) {
		return b.command(ctx, in.Command, dir, sb, nil)
	})
	if err != nil {
		return tool.Errorf("%v", err), nil
	}
	text := fmt.Sprintf("Started background command %s in %s; it is stopped after %s. "+
		"Read its output with bash_output and stop it with kill_shell. Directory changes it makes do not carry over.",
		s.id, dir, timeout)
	return tool.Text(withNotes(text, notes)), nil
}

// withNotes appends bracketed notes to a result.
func withNotes(text string, notes []string) string {
	for _, n := range notes {
		text += "\n[" + n + "]"
	}
	return text
}

// readCwd returns the first line the shell wrote to r. The shell has
// exited, so the line is already in the pipe; a process it left behind
// may still hold the pipe open, so the read is bounded.
func readCwd(r *os.File) string {
	_ = r.SetReadDeadline(time.Now().Add(cwdReadDelay))
	var buf bytes.Buffer
	p := make([]byte, 4096)
	for buf.Len() < 1<<16 {
		n, err := r.Read(p)
		buf.Write(p[:n])
		if i := bytes.IndexByte(buf.Bytes(), '\n'); i >= 0 {
			return string(buf.Bytes()[:i])
		}
		if err != nil {
			break
		}
	}
	return ""
}

// runTree runs cmd with its stdout and stderr written to out and kills its
// process tree once the shell exits. The output pipe is created here rather
// than by exec, so Wait returns as soon as the shell exits even if a
// background process holds the pipe open; exec would instead wait for
// WaitDelay and fail with exec.ErrWaitDelay. held reports that a process
// outside the tree still held the pipe after outputDrainDelay.
func runTree(cmd *exec.Cmd, out io.Writer) (held bool, err error) {
	r, w, err := os.Pipe()
	if err != nil {
		return false, err
	}
	cmd.Stdout, cmd.Stderr = w, w
	tree := process.NewTree(cmd)
	err = tree.Start()
	w.Close()
	if err != nil {
		r.Close()
		return false, err
	}
	defer tree.Close()
	sink := &stoppableWriter{w: out}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(sink, r)
	}()
	// Wait also kills the background processes of the shell, which would
	// otherwise keep running, and keep the output pipe open.
	err = tree.Wait()
	select {
	case <-drained:
	case <-time.After(outputDrainDelay):
		held = true
		sink.stop()
	}
	// Closing interrupts a pending read of the copy, also on Windows,
	// where Go cancels the pipe's I/O before closing it.
	r.Close()
	return held, err
}

// stoppableWriter forwards writes to w until stop is called and discards
// them afterwards.
type stoppableWriter struct {
	mu      sync.Mutex
	w       io.Writer
	stopped bool
}

func (s *stoppableWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return len(p), nil
	}
	return s.w.Write(p)
}

func (s *stoppableWriter) stop() {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
}
