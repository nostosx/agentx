package builtin

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nostosx/agentx/internal/process"

	"github.com/nostosx/agentx/tool"
)

// Limits of background commands. Like Claude Code in unattended sessions,
// a background command may run for 30 minutes unless the model asks for a
// longer timeout, up to two hours.
const (
	defaultMaxRunning        = 8
	defaultShellOutput       = 1 << 20
	defaultBackgroundTimeout = 30 * time.Minute
	maxBackgroundTimeout     = 2 * time.Hour
	// maxFinishedShells bounds the finished commands kept for bash_output.
	maxFinishedShells = 32
	// shellStopWait bounds how long kill_shell and Close wait for a killed
	// command to be reaped.
	shellStopWait = 5 * time.Second
)

// Shells holds the commands bash runs in the background, which bash_output
// reads and kill_shell stops. The zero value is ready to use. Each command
// runs in its own process tree, like a foreground command, with its output
// kept in a bounded buffer. Close stops them all; call it when the agent
// is done, as background commands otherwise run until they exit or time
// out. A Shells is safe for concurrent use.
type Shells struct {
	// MaxRunning caps the background commands running at once; the
	// default is 8.
	MaxRunning int
	// MaxOutput caps the bytes of output kept per command; the oldest
	// output is dropped beyond it. The default is 1 MiB.
	MaxOutput int

	mu     sync.Mutex
	shells map[string]*shell
	next   int
	closed bool
}

// shell is a background command.
type shell struct {
	id, command, dir string
	started          time.Time
	tree             *process.Tree
	// ctx bounds the command's lifetime; cancel releases it.
	ctx    context.Context
	cancel context.CancelFunc
	// done is closed once the command was waited for and its output
	// drained.
	done chan struct{}
	// sandbox reports refused hosts, if the command ran in a sandbox that
	// tracks them.
	sandbox interface{ DeniedHosts(time.Time) []string }

	mu sync.Mutex
	// out holds the output from offset start; total counts all of it and
	// read is the offset bash_output has reported up to.
	out          []byte
	start, total int64
	read         int64
	max          int
	ended        time.Time
	err          error
	killed       bool
	timeout      time.Duration
	held         bool
}

// Write appends output, dropping the oldest beyond the limit.
func (s *shell) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out = append(s.out, p...)
	s.total += int64(len(p))
	// Compact only once the buffer is twice the limit, so each byte is
	// copied a bounded number of times.
	if len(s.out) > 2*s.max {
		drop := len(s.out) - s.max
		s.out = slices.Delete(s.out, 0, drop)
		s.start += int64(drop)
	}
	return len(p), nil
}

// status describes the state of the command; s.mu must be held.
func (s *shell) status() string {
	select {
	case <-s.done:
	default:
		return fmt.Sprintf("running for %s", time.Since(s.started).Round(time.Second))
	}
	ran := s.ended.Sub(s.started).Round(time.Millisecond)
	switch {
	case s.killed:
		return fmt.Sprintf("killed after %s", ran)
	case s.timeout > 0 && errors.Is(s.err, context.DeadlineExceeded):
		return fmt.Sprintf("timed out after %s and was killed", s.timeout)
	case s.err == nil:
		return fmt.Sprintf("exited with code 0 after %s", ran)
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](s.err); ok {
		if exitErr.ExitCode() < 0 {
			return fmt.Sprintf("terminated by a signal after %s", ran)
		}
		return fmt.Sprintf("exited with code %d after %s", exitErr.ExitCode(), ran)
	}
	return fmt.Sprintf("failed after %s: %v", ran, s.err)
}

// running reports whether the command has not finished.
func (s *shell) running() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// start runs the command built by build in the background. build receives
// the context that bounds the command's lifetime.
func (r *Shells) start(command, dir string, timeout time.Duration, sb Sandbox,
	build func(ctx context.Context) (*exec.Cmd, error)) (*shell, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errors.New("background commands were shut down")
	}
	running := 0
	for _, s := range r.shells {
		if s.running() {
			running++
		}
	}
	if limit := cmp.Or(r.MaxRunning, defaultMaxRunning); running >= limit {
		return nil, fmt.Errorf("%d background commands are already running, the limit; stop one with kill_shell first", limit)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	cmd, err := build(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	rd, w, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = w, w
	r.next++
	s := &shell{
		id: "bash_" + strconv.Itoa(r.next), command: command, dir: dir,
		started: time.Now(), ctx: ctx, cancel: cancel, done: make(chan struct{}),
		max: cmp.Or(r.MaxOutput, defaultShellOutput), timeout: timeout,
	}
	if d, ok := sb.(interface{ DeniedHosts(time.Time) []string }); ok {
		s.sandbox = d
	}
	s.tree = process.NewTree(cmd)
	err = s.tree.Start()
	w.Close()
	if err != nil {
		rd.Close()
		cancel()
		return nil, err
	}
	if r.shells == nil {
		r.shells = map[string]*shell{}
	}
	r.shells[s.id] = s
	r.prune()
	go s.wait(rd)
	return s, nil
}

// wait waits for the command like runTree and records how it ended.
func (s *shell) wait(rd *os.File) {
	defer close(s.done)
	defer s.cancel()
	defer s.tree.Close()
	sink := &stoppableWriter{w: s}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(sink, rd)
	}()
	err := s.tree.Wait()
	held := false
	select {
	case <-drained:
	case <-time.After(outputDrainDelay):
		held = true
		sink.stop()
	}
	rd.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ended, s.held = time.Now(), held
	s.err = err
	if !s.killed && errors.Is(s.ctx.Err(), context.DeadlineExceeded) {
		s.err = context.DeadlineExceeded
	}
}

// prune drops the oldest finished commands beyond maxFinishedShells; r.mu
// must be held.
func (r *Shells) prune() {
	var finished []*shell
	for _, s := range r.shells {
		if !s.running() {
			finished = append(finished, s)
		}
	}
	if len(finished) <= maxFinishedShells {
		return
	}
	slices.SortFunc(finished, func(a, b *shell) int { return a.started.Compare(b.started) })
	for _, s := range finished[:len(finished)-maxFinishedShells] {
		delete(r.shells, s.id)
	}
}

// get returns the command with the given ID.
func (r *Shells) get(id string) (*shell, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.shells[strings.TrimSpace(id)]; ok {
		return s, nil
	}
	ids := slices.Sorted(maps.Keys(r.shells))
	if len(ids) == 0 {
		return nil, fmt.Errorf("no background command has ID %q; there are none", id)
	}
	return nil, fmt.Errorf("no background command has ID %q; known IDs: %s", id, strings.Join(ids, ", "))
}

// kill kills the command's process tree and waits for it to be reaped.
func (s *shell) kill() {
	s.mu.Lock()
	if s.running() {
		s.killed = true
	}
	s.mu.Unlock()
	_ = s.tree.Kill()
	s.cancel()
	select {
	case <-s.done:
	case <-time.After(shellStopWait):
	}
}

// Close kills the background commands and refuses new ones.
func (r *Shells) Close() error {
	r.mu.Lock()
	r.closed = true
	shells := slices.Collect(maps.Values(r.shells))
	r.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range shells {
		if s.running() {
			wg.Go(s.kill)
		}
	}
	wg.Wait()
	return nil
}

// Running returns the IDs of the commands that are still running, sorted.
func (r *Shells) Running() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for id, s := range r.shells {
		if s.running() {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

type bashOutputArgs struct {
	ShellID string `json:"shell_id" jsonschema:"The ID of the background command, as returned by bash."`
	Filter  string `json:"filter,omitempty" jsonschema:"Optional regular expression; only new output lines matching it are returned, and the other new lines are discarded."`
}

// BashOutput returns the bash_output tool, which reads the output a
// background command of Config.Shells wrote since the previous read,
// optionally only the lines matching a regular expression, and reports
// whether the command is running or how it ended. It needs no approval and
// may run concurrently with other calls, but it is not read-only: a read
// consumes the output it returns.
func BashOutput(cfg Config) tool.Tool {
	e := newEnv(cfg)
	return tool.Func("bash_output",
		"Read the new output of a background command started by bash with run_in_background: what it wrote since the last read, "+
			"and whether it is still running or how it ended. filter keeps only the new lines matching a regular expression.",
		func(_ context.Context, in bashOutputArgs) (*tool.Result, error) {
			if cfg.Shells == nil {
				return tool.Errorf("background commands are not enabled"), nil
			}
			var re *regexp.Regexp
			if in.Filter != "" {
				var err error
				if re, err = regexp.Compile(in.Filter); err != nil {
					return tool.Errorf("invalid filter: %v", err), nil
				}
			}
			s, err := cfg.Shells.get(in.ShellID)
			if err != nil {
				return tool.Errorf("%v", err), nil
			}
			return tool.Text(s.report(re, e.MaxOutput)), nil
		}, tool.Concurrent())
}

// report returns the output since the last report and the status.
func (s *shell) report(re *regexp.Regexp, maxOutput int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.status()
	var b strings.Builder
	fmt.Fprintf(&b, "Shell %s: %s\nCommand: %s\n", s.id, status, s.command)
	from := max(s.read, s.start)
	if dropped := from - s.read; dropped > 0 {
		fmt.Fprintf(&b, "[%d bytes of earlier output were dropped]\n", dropped)
	}
	text := strings.ToValidUTF8(string(s.out[from-s.start:]), "")
	s.read = s.total
	if re != nil {
		var kept []string
		for line := range strings.Lines(text) {
			if re.MatchString(strings.TrimRight(line, "\r\n")) {
				kept = append(kept, line)
			}
		}
		text = strings.Join(kept, "")
	}
	if strings.TrimSpace(text) == "" {
		b.WriteString("(no new output)")
	} else {
		b.WriteString("Output:\n")
		b.WriteString(tool.Truncate(strings.TrimRight(text, "\n"), maxOutput))
	}
	if s.held {
		b.WriteString("\n[a process kept the output open after the command ended; later output was discarded]")
	}
	if s.sandbox != nil {
		if hosts := s.sandbox.DeniedHosts(s.started); len(hosts) > 0 {
			fmt.Fprintf(&b, "\n[the sandbox refused connections to %s]", strings.Join(hosts, ", "))
		}
	}
	return b.String()
}

type killShellArgs struct {
	ShellID string `json:"shell_id" jsonschema:"The ID of the background command to stop."`
}

// KillShell returns the kill_shell tool, which kills a background command
// of Config.Shells together with every process it started. It needs no
// approval: it can only stop commands the agent started.
func KillShell(cfg Config) tool.Tool {
	return tool.Func("kill_shell",
		"Stop a background command started by bash with run_in_background, together with every process it started.",
		func(_ context.Context, in killShellArgs) (*tool.Result, error) {
			if cfg.Shells == nil {
				return tool.Errorf("background commands are not enabled"), nil
			}
			s, err := cfg.Shells.get(in.ShellID)
			if err != nil {
				return tool.Errorf("%v", err), nil
			}
			if !s.running() {
				s.mu.Lock()
				defer s.mu.Unlock()
				return tool.Text(fmt.Sprintf("Shell %s had already finished: %s.", s.id, s.status())), nil
			}
			s.kill()
			s.mu.Lock()
			defer s.mu.Unlock()
			return tool.Text(fmt.Sprintf("Killed shell %s (%s).", s.id, s.status())), nil
		})
}
