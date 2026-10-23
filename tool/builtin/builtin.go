// Package builtin provides general-purpose tools modeled on those of coding
// agents such as Claude Code and Codex: file reading, writing and editing,
// patches, glob and grep search, shell commands, web fetching and a todo
// list.
//
// File tools are confined to Config.Dir with os.Root, so paths cannot escape
// it through ".." or symbolic links. The bash tool is not confined unless
// Config.Sandbox is set: commands start in Config.Dir but can access
// anything the process can. write_file, edit_file, apply_patch and bash
// therefore require approval unless Config.NoApproval is set. With a
// sandbox, which NewSandbox builds from the operating system's Seatbelt or
// bubblewrap, bash commands run confined to the writable roots and allowed
// hosts without approval, and only commands that leave the sandbox need
// it.
//
// The file-changing tools stage regular files in the same directory and
// replace them with os.Rename; per-file atomicity depends on the platform.
// Symbolic links are written through in place. Multi-file changes attempt
// rollback on failure but do not isolate changes from concurrent readers
// or writers. The tools return a unified diff of the change. With
// Config.History they first save the state of the files they change, so
// tracked files can be restored to a checkpoint; see History.
//
// bash can run commands in the background; bash_output reads their output
// and kill_shell stops them. Close the tools when the agent is done.
//
// web_fetch is read-only and does not require approval. By default it
// refuses loopback, private, link-local and shared addresses, including
// after redirects and, for direct connections, DNS changes; set
// Config.AllowPrivateNetwork to lift the restriction. Through a proxy from
// the environment, destinations are checked by name before each request
// only, since the proxy resolves them again. A Config.HTTPClient replaces
// these checks entirely.
package builtin

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nostosx/agentx/tool"
)

// Config configures the built-in tools.
type Config struct {
	// Dir is the working directory. File tools are confined to it. The
	// default is the current directory.
	Dir string
	// HTTPClient is used by web_fetch instead of the default client, which
	// refuses non-public addresses. The caller is then responsible for
	// restricting the addresses it can reach.
	HTTPClient *http.Client
	// AllowPrivateNetwork lets the default web_fetch client reach loopback,
	// private, link-local and shared (CGNAT, 100.64.0.0/10) addresses.
	AllowPrivateNetwork bool
	// MaxOutput caps the bytes of a tool result. The default is 30000.
	MaxOutput int
	// CommandTimeout is the default bash timeout. The default is two
	// minutes; the model may request up to ten.
	CommandTimeout time.Duration
	// Env is added to the environment of bash commands.
	Env []string
	// Todos is the list updated by todo_write. If nil, Tools creates one.
	Todos *TodoList
	// Files tracks the files read_file has read. When set, write_file,
	// edit_file and apply_patch only change or delete an existing file
	// after it was read and while it is unchanged since. If nil, Tools
	// creates one; tools constructed individually without it do not check.
	Files *FileTracker
	// NoApproval lets write_file, edit_file, apply_patch and bash run
	// without approval.
	NoApproval bool
	// CheckRedirect, if set, is called before web_fetch follows a
	// redirect, after the client's own checks, with the arguments of
	// http.Client.CheckRedirect; an error stops the fetch and is reported
	// to the model. permission.Policy.CheckRedirect applies WebFetch
	// permission rules to redirects this way.
	CheckRedirect func(req *http.Request, via []*http.Request) error
	// Edit selects the editing tools Tools returns besides write_file.
	Edit EditStyle
	// History, if set, saves the state of files before write_file,
	// edit_file and apply_patch change them, so the files of a session can
	// be restored to a checkpoint with agent.Runner.Rewind. If a save
	// fails, the tool changes nothing. Changes made by bash are not
	// tracked. A FileHistory forgets the files it restores in Files.
	History History
	// Sandbox, if set, runs every bash command, foreground and
	// background, inside it; see NewSandbox. Sandboxed commands need no
	// approval unless AskSandboxed is set; commands that run outside it
	// always do. See Bash.
	Sandbox Sandbox
	// SandboxFallback decides what bash does when Sandbox is set but not
	// available: SandboxRequired, the default, fails the command, and
	// SandboxBestEffort runs it unsandboxed after approval.
	SandboxFallback SandboxFallback
	// AskSandboxed keeps approval for sandboxed bash commands unless
	// NoApproval is set, like Claude Code's regular-permissions sandbox
	// mode.
	AskSandboxed bool
	// NoSandboxEscape removes bash's dangerously_disable_sandbox argument,
	// like Claude Code's allowUnsandboxedCommands set to false, so
	// commands always run in the sandbox.
	NoSandboxEscape bool
	// Shells holds the commands bash runs in the background, which
	// bash_output reads and kill_shell stops. If nil, Tools creates one;
	// bash constructed individually without it offers no background
	// commands. Close it, or call Close with the tools, when the agent is
	// done.
	Shells *Shells
	// ResetDir starts every bash command in Dir. By default a cd carries
	// over to later commands while it stays inside Dir.
	ResetDir bool
}

// EditStyle selects the file-editing tools of Tools.
type EditStyle int

const (
	// EditAll returns edit_file and apply_patch, so each model can use the
	// format it was trained on: Claude models exact-text replacement, GPT
	// models Codex patches.
	EditAll EditStyle = iota
	// EditReplace returns only edit_file.
	EditReplace
	// EditPatch returns only apply_patch.
	EditPatch
)

const (
	defaultMaxOutput      = 30000
	defaultCommandTimeout = 2 * time.Minute
	maxCommandTimeout     = 10 * time.Minute
)

// Tools returns all built-in tools: read_file, write_file, edit_file,
// apply_patch, glob, grep, bash, bash_output, kill_shell, web_fetch and
// todo_write. Config.Edit can leave out edit_file or apply_patch. Call
// Close with the tools when the agent is done, to stop the commands bash
// left running in the background.
func Tools(cfg Config) []tool.Tool {
	if cfg.Todos == nil {
		cfg.Todos = &TodoList{}
	}
	if cfg.Files == nil {
		cfg.Files = &FileTracker{}
	}
	if cfg.Shells == nil {
		cfg.Shells = &Shells{}
	}
	tools := []tool.Tool{ReadFile(cfg), WriteFile(cfg)}
	if cfg.Edit != EditPatch {
		tools = append(tools, EditFile(cfg))
	}
	if cfg.Edit != EditReplace {
		tools = append(tools, ApplyPatch(cfg))
	}
	return append(tools,
		Glob(cfg),
		Grep(cfg),
		Bash(cfg),
		BashOutput(cfg),
		KillShell(cfg),
		WebFetch(cfg),
		TodoWrite(cfg.Todos),
	)
}

// Close closes the tools that hold resources, such as bash, which stops
// its background commands. It returns the first error.
func Close(tools []tool.Tool) error {
	var first error
	for _, t := range tools {
		if c, ok := t.(io.Closer); ok {
			if err := c.Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

// ReadOnlyTools returns the tools without side effects: read_file, glob,
// grep and web_fetch. They suit research sub-agents. Pass the parent's
// Config.Files to let the parent edit files a sub-agent has read.
func ReadOnlyTools(cfg Config) []tool.Tool {
	return []tool.Tool{ReadFile(cfg), Glob(cfg), Grep(cfg), WebFetch(cfg)}
}

// env is a Config with defaults applied and the working directory resolved.
type env struct {
	Config
	// dir is the absolute working directory and real is dir with symbolic
	// links resolved; absolute paths may be given relative to either.
	dir, real string
}

func newEnv(cfg Config) *env {
	e := &env{Config: cfg}
	e.MaxOutput = cmp.Or(e.MaxOutput, defaultMaxOutput)
	e.CommandTimeout = cmp.Or(e.CommandTimeout, defaultCommandTimeout)
	e.dir = cmp.Or(cfg.Dir, ".")
	if abs, err := filepath.Abs(e.dir); err == nil {
		e.dir = abs
	}
	e.real = e.dir
	if real, err := filepath.EvalSymlinks(e.dir); err == nil {
		e.real = real
	}
	if h, ok := cfg.History.(interface{ track(*FileTracker) }); ok {
		h.track(cfg.Files)
	}
	return e
}

// saveHistory saves the state of files to Config.History before a tool
// changes them. It returns an error result if the save failed.
func (e *env) saveHistory(ctx context.Context, name string, edits []FileEdit) *tool.Result {
	if e.History == nil || len(edits) == 0 {
		return nil
	}
	if err := e.History.Save(ctx, name, e.dir, edits); err != nil {
		return tool.Errorf("saving the file history failed, so no file was changed: %v", err)
	}
	return nil
}

// approval returns the options for tools with side effects.
func (e *env) approval() []tool.Option {
	if e.NoApproval {
		return nil
	}
	return []tool.Option{tool.RequireApproval()}
}

// rel converts a path given by the model to one relative to the working
// directory. os.Root rejects relative paths that escape it.
func (e *env) rel(p string) (string, error) {
	if p == "" {
		return ".", nil
	}
	if !filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	p = filepath.Clean(p)
	for _, base := range []string{e.dir, e.real} {
		r, err := filepath.Rel(base, p)
		if err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			return r, nil
		}
	}
	return "", fmt.Errorf("path %s is outside the working directory %s", p, e.dir)
}

// abs returns the absolute path of rel, the key used by Config.Files.
func (e *env) abs(rel string) string { return filepath.Join(e.dir, rel) }

// slashRel is like rel but returns a slash-separated path for fs.FS.
func (e *env) slashRel(p string) (string, error) {
	r, err := e.rel(p)
	return filepath.ToSlash(r), err
}

// limitWriter keeps about max bytes of what is written to it: the first two
// thirds and the last third, like tool.Truncate. The end of command output
// usually holds the error, so it must survive truncation. Unlike
// tool.Truncate it works on a stream without holding all of it, and it
// drops a UTF-8 sequence split at a cut instead of moving the cut.
type limitWriter struct {
	max  int
	head []byte
	// tail is a ring buffer of the most recent bytes after head filled up;
	// pos is the next write position once it is full.
	tail  []byte
	pos   int
	total int
}

func (w *limitWriter) Write(p []byte) (int, error) {
	written := len(p)
	w.total += written
	headMax := w.max * 2 / 3
	if n := min(headMax-len(w.head), len(p)); n > 0 {
		w.head = append(w.head, p[:n]...)
		p = p[n:]
	}
	tailMax := w.max - headMax
	if tailMax <= 0 {
		return written, nil
	}
	if len(p) >= tailMax {
		w.tail = append(w.tail[:0], p[len(p)-tailMax:]...)
		w.pos = 0
		return written, nil
	}
	for _, b := range p {
		if len(w.tail) < tailMax {
			w.tail = append(w.tail, b)
			continue
		}
		w.tail[w.pos] = b
		w.pos = (w.pos + 1) % tailMax
	}
	return written, nil
}

func (w *limitWriter) String() string {
	tail := append(slices.Clone(w.tail[w.pos:]), w.tail[:w.pos]...)
	dropped := w.total - len(w.head) - len(tail)
	if dropped <= 0 {
		return strings.ToValidUTF8(string(w.head)+string(tail), "")
	}
	return fmt.Sprintf("%s\n... [%d bytes truncated] ...\n%s",
		strings.ToValidUTF8(string(w.head), ""), dropped, strings.ToValidUTF8(string(tail), ""))
}
