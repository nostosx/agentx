package skill

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/nostosx/kitx/utf8text"

	"github.com/nostosx/agentx/internal/process"
)

// Defaults of command injection, as in Claude Code, whose injected
// commands share the Bash tool's 2-minute timeout.
const (
	DefaultShellTimeout = 2 * time.Minute
	defaultShellOutput  = 30000
)

// segment is a part of skill content: text, or an injected command that
// its output replaces.
type segment struct {
	text    string // the text, or the placeholder as written
	command string // the command of a placeholder
	isCmd   bool
}

// inlineCommand matches !`command` at the start of a line or after white
// space.
var inlineCommand = regexp.MustCompile("(?:^|[ \t])(!`([^`\n]*)`)")

// segments splits content into text and the command placeholders of
// Claude Code's dynamic context injection
// (https://code.claude.com/docs/en/skills#inject-dynamic-context):
// !`command` at the start of a line or after white space, and fenced
// blocks opened with ```!.
func segments(content string) []segment {
	var out []segment
	text := func(s string) {
		if s == "" {
			return
		}
		if n := len(out); n > 0 && !out[n-1].isCmd {
			out[n-1].text += s
			return
		}
		out = append(out, segment{text: s})
	}
	lines := strings.SplitAfter(content, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimRight(line, " \t\r\n") == "```!" {
			end := -1
			for j := i + 1; j < len(lines); j++ {
				if strings.TrimRight(lines[j], " \t\r\n") == "```" {
					end = j
					break
				}
			}
			if end > 0 {
				block := strings.Join(lines[i:end+1], "")
				nl := strings.HasSuffix(block, "\n")
				out = append(out, segment{
					text:    strings.TrimSuffix(block, "\n"),
					command: strings.TrimSuffix(strings.Join(lines[i+1:end], ""), "\n"),
					isCmd:   true,
				})
				if nl {
					text("\n")
				}
				i = end
				continue
			}
		}
		last := 0
		for _, m := range inlineCommand.FindAllStringSubmatchIndex(line, -1) {
			text(line[last:m[2]])
			out = append(out, segment{text: line[m[2]:m[3]], command: line[m[4]:m[5]], isCmd: true})
			last = m[3]
		}
		text(line[last:])
	}
	return out
}

// shellRunner runs the injected commands of skills.
type shellRunner struct {
	dir     string
	timeout time.Duration
	limit   int
}

// benignExit1 lists the commands for which, as in Claude Code's Bash tool
// (https://code.claude.com/docs/en/tools-reference), exit status 1 is a
// result rather than a failure: no match or a difference.
var benignExit1 = []string{"grep", "rg", "egrep", "fgrep", "find", "diff", "test", "[", "git diff", "git grep"}

// run runs one injected command of a skill and returns its output. The
// command text gets the ${CLAUDE_*} variables; the arguments reach it
// through the environment (ARGUMENTS, ARGUMENTS_0, ... and declared names)
// and, with sh or bash, as the positional parameters $0, $1, ..., so
// argument values are never parsed as shell code.
func (r *shellRunner) run(ctx context.Context, meta Metadata, skillDir, command string, e *expander) (string, error) {
	script := e.claude(command, skillDir)
	env := os.Environ()
	add := func(k, v string) { env = append(env, k+"="+v) }
	add("ARGUMENTS", e.vars.Arguments)
	for i, a := range e.args {
		add("ARGUMENTS_"+strconv.Itoa(i), a)
	}
	for name, i := range e.index {
		if validEnvName(name) && i < len(e.args) {
			add(name, e.args[i])
		}
	}
	for k, v := range map[string]string{
		"CLAUDE_SESSION_ID":  e.vars.SessionID,
		"CLAUDE_SKILL_DIR":   skillDir,
		"CLAUDE_PROJECT_DIR": e.vars.ProjectDir,
		"CLAUDE_EFFORT":      e.vars.Effort,
	} {
		if v != "" {
			add(k, v)
		}
	}

	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	var cmd *exec.Cmd
	if strings.EqualFold(meta.Shell, "powershell") {
		for _, name := range []string{"pwsh", "powershell"} {
			if p, err := exec.LookPath(name); err == nil {
				cmd = process.ExecCommand(ctx, p, "-NoProfile", "-NonInteractive", "-Command", script)
				break
			}
		}
	}
	if cmd == nil {
		if goruntime.GOOS != "windows" {
			script = positional(script)
		}
		cmd = process.BashCommand(ctx, script)
		if goruntime.GOOS != "windows" {
			// sh -c script a b: $0 is a and $1 is b, the 0-based indexes
			// of Claude Code's $N.
			cmd.Args = append(cmd.Args, e.args...)
		}
	}
	cmd.Dir, cmd.Env = r.dir, env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	// A background process that keeps the output open does not hold the
	// skill up for long.
	cmd.WaitDelay = time.Second
	tree := process.NewTree(cmd)
	err := tree.Start()
	if err == nil {
		err = tree.Wait()
	}
	_ = tree.Close()
	text := strings.TrimRight(out.String(), "\n")
	text = utf8text.Truncate(text, r.limit, "\n[output truncated]")
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil
	}
	if err == nil {
		return text, nil
	}
	if ctx.Err() != nil {
		return "", fmt.Errorf("timed out after %s\n[stderr]\n%s", r.timeout, text)
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		if exitErr.ExitCode() == 1 && benign(command) {
			return text, nil
		}
		return "", fmt.Errorf("exit status %d\n[stderr]\n%s", exitErr.ExitCode(), text)
	}
	return "", err
}

// benign reports whether exit status 1 of command is a result: its last
// command, after any separator or pipe, is one of benignExit1.
func benign(command string) bool {
	lines := strings.Split(strings.TrimSpace(command), "\n")
	last := lines[len(lines)-1]
	if i := strings.LastIndexAny(last, "|;&"); i >= 0 {
		// The exit status is that of the last command.
		last = last[i+1:]
	}
	fields := strings.Fields(last)
	for len(fields) > 0 && strings.Contains(fields[0], "=") {
		fields = fields[1:] // environment assignments
	}
	if len(fields) == 0 {
		return false
	}
	for _, c := range benignExit1 {
		words := strings.Fields(c)
		if len(fields) >= len(words) && strings.Join(fields[:len(words)], " ") == c {
			return true
		}
	}
	return false
}

// indexedArg matches $ARGUMENTS[N] and $N with two or more digits.
var indexedArg = regexp.MustCompile(`\$ARGUMENTS\[([0-9]+)\]|\$([0-9]{2,})`)

// positional rewrites $ARGUMENTS[N] and $NN to ${N} and ${NN}, the
// positional parameters of sh, unless escaped with a backslash.
func positional(script string) string {
	var b strings.Builder
	last := 0
	for _, m := range indexedArg.FindAllStringSubmatchIndex(script, -1) {
		if m[0] > 0 && script[m[0]-1] == '\\' {
			continue
		}
		b.WriteString(script[last:m[0]])
		b.WriteString("${")
		if m[2] >= 0 {
			b.WriteString(script[m[2]:m[3]])
		} else {
			b.WriteString(script[m[4]:m[5]])
		}
		b.WriteString("}")
		last = m[1]
	}
	b.WriteString(script[last:])
	return b.String()
}

func validEnvName(s string) bool {
	if s == "" || s[0] >= '0' && s[0] <= '9' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !identByte(s[i]) {
			return false
		}
	}
	return true
}

// controlChar reports whether s holds a control character other than tab
// and newline, which Claude Code refuses in injected commands.
func controlChar(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) && r != '\t' && r != '\n' })
}
