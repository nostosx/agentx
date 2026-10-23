package permission

import (
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// This file prepares shell commands for Bash rules the way Claude Code
// does; see https://code.claude.com/docs/en/permissions#bash. A command is
// split into simple commands, which rules match one at a time; process
// wrappers such as timeout and leading environment assignments are
// stripped; and the files a command reads or writes through redirections
// and well-known file commands are checked against Read and Edit rules.

// subcommand is a simple command prepared for rule matching.
type subcommand struct {
	cmd *simpleCmd
	// forms are the texts deny and ask rules match: the command as
	// written and after each stripped assignment and wrapper.
	forms []string
	// text is the text allow rules match: with wrappers stripped, and
	// leading assignments stripped if they all set known-safe variables.
	text string
	// args are the words of the command with wrappers stripped, starting
	// with the program.
	args []word
	// safeEnv is false if an assignment of another variable precedes the
	// command.
	safeEnv bool
	// cwd is the directory the command runs in, or "" if a cd earlier in
	// the command line makes it unknown.
	cwd string
	// inline is set for the commands of a script run with sh -c and the
	// like. Deny and ask rules and the critical-path check see them;
	// allow rules match the command that runs the script.
	inline bool
}

// program returns the program of the command, or "".
func (s *subcommand) program() string {
	if len(s.args) == 0 || s.args[0].expand || s.args[0].subst {
		return ""
	}
	return s.args[0].lit
}

// bashCall is an analyzed shell command.
type bashCall struct {
	raw  string
	bad  bool
	subs []*subcommand
}

// safeEnv lists the variables an allow rule matches past when a command
// assigns them first, such as NODE_ENV=test npm test. Deny and ask rules
// match past any assignment.
var safeEnv = map[string]bool{
	"LANG": true, "LANGUAGE": true, "LC_ALL": true, "NO_COLOR": true, "FORCE_COLOR": true,
	"CLICOLOR": true, "TERM": true, "TZ": true, "NODE_ENV": true, "CI": true,
	"RUST_BACKTRACE": true, "PYTHONUNBUFFERED": true, "PYTHONDONTWRITEBYTECODE": true,
	"GOOS": true, "GOARCH": true, "CGO_ENABLED": true,
}

func isSafeEnv(name string) bool { return safeEnv[name] || strings.HasPrefix(name, "LC_") }

// analyzeBash splits command into subcommands. dir is the working
// directory, home the home directory.
func analyzeBash(command, dir, home string) *bashCall {
	bc := &bashCall{raw: strings.TrimSpace(command)}
	bc.analyze(command, dir, home, false, 0)
	return bc
}

// maxInlineDepth bounds how deeply sh -c scripts are followed.
const maxInlineDepth = 3

func (bc *bashCall) analyze(command, dir, home string, inline bool, depth int) {
	sc := lex(command)
	bc.bad = bc.bad || sc.bad
	cwd := dir
	for _, c := range sc.cmds {
		s := prepare(c)
		s.cwd, s.inline = cwd, inline
		bc.subs = append(bc.subs, s)
		switch prog := s.program(); {
		case prog == "cd":
			// Relative paths after a cd are not followed.
			cwd = ""
		case isShell(prog) && depth < maxInlineDepth:
			if script, ok := inlineScript(s.args); ok {
				bc.analyze(script, cwd, home, true, depth+1)
			}
		}
	}
}

// prepare computes the forms of a simple command.
func prepare(c *simpleCmd) *subcommand {
	s := &subcommand{cmd: c, safeEnv: true}
	for _, a := range c.assigns {
		if !isSafeEnv(assignName(a)) {
			s.safeEnv = false
		}
	}
	words := c.words
	if len(c.assigns) > 0 {
		s.forms = append(s.forms, joinRaw(append(slices.Clone(c.assigns), words...)))
	}
	if len(words) > 0 {
		s.forms = append(s.forms, joinRaw(words))
	}
	i := 0
	for i < len(words) {
		n := wrapperLen(words[i:])
		if n == 0 || i+n >= len(words) {
			break
		}
		i += n
		s.forms = append(s.forms, joinRaw(words[i:]))
	}
	s.args = words[i:]
	if s.safeEnv {
		s.text = joinRaw(s.args)
	} else {
		s.text = joinRaw(append(slices.Clone(c.assigns), words...))
	}
	if len(s.forms) == 0 {
		s.forms = []string{s.text}
	}
	return s
}

func joinRaw(ws []word) string {
	raws := make([]string, len(ws))
	for i, w := range ws {
		raws[i] = w.raw
	}
	return strings.Join(raws, " ")
}

// wrapperLen returns the number of words of a process wrapper at the start
// of ws, including its options, or 0 if ws does not start with one. The
// wrappers are those Claude Code strips: timeout, time, nice, nohup,
// stdbuf, command, builtin, noglob and xargs without flags.
func wrapperLen(ws []word) int {
	if ws[0].quoted || ws[0].expand {
		return 0
	}
	arg := func(i int) string {
		if i < len(ws) {
			return ws[i].lit
		}
		return ""
	}
	switch ws[0].lit {
	case "nohup", "builtin", "noglob":
		return 1
	case "time":
		if arg(1) == "-p" {
			return 2
		}
		return 1
	case "command":
		switch {
		case arg(1) == "-p":
			return 2
		case strings.HasPrefix(arg(1), "-"):
			// command -v and -V look a command up instead of running it.
			return 0
		}
		return 1
	case "xargs":
		if strings.HasPrefix(arg(1), "-") {
			return 0
		}
		return 1
	case "nice":
		switch a := arg(1); {
		case a == "-n":
			return 3
		case strings.HasPrefix(a, "--adjustment="), strings.HasPrefix(a, "-n") && isDigits(a[2:]),
			strings.HasPrefix(a, "-") && isDigits(strings.TrimPrefix(a[1:], "-")):
			return 2
		case strings.HasPrefix(a, "-"):
			return 0
		}
		return 1
	case "timeout":
		i := 1
		for strings.HasPrefix(arg(i), "-") {
			switch a := arg(i); {
			case a == "-s" || a == "-k" || a == "--signal" || a == "--kill-after":
				i += 2
			case a == "--preserve-status" || a == "--foreground" || a == "-v" || a == "--verbose",
				strings.HasPrefix(a, "--signal="), strings.HasPrefix(a, "--kill-after="),
				len(a) > 2 && (a[1] == 's' || a[1] == 'k'):
				i++
			default:
				return 0
			}
		}
		if arg(i) == "" {
			return 0
		}
		return i + 1
	case "stdbuf":
		i := 1
		for strings.HasPrefix(arg(i), "-") {
			switch a := arg(i); {
			case a == "-i" || a == "-o" || a == "-e":
				i += 2
			case len(a) > 2 && strings.IndexByte("ioe", a[1]) >= 0,
				strings.HasPrefix(a, "--input="), strings.HasPrefix(a, "--output="), strings.HasPrefix(a, "--error="):
				i++
			default:
				return 0
			}
		}
		return i
	}
	return 0
}

func isShell(prog string) bool {
	switch filepath.Base(prog) {
	case "sh", "bash", "zsh", "dash", "ksh":
		return true
	}
	return false
}

// inlineScript returns the script of a shell run with -c.
func inlineScript(args []word) (string, bool) {
	for i := 1; i < len(args); i++ {
		a := args[i].lit
		if !strings.HasPrefix(a, "-") || strings.HasPrefix(a, "--") {
			return "", false
		}
		if strings.Contains(a, "c") && i+1 < len(args) {
			return args[i+1].lit, true
		}
	}
	return "", false
}

// readOnlyPrograms are the commands Claude Code runs without approval in
// every mode; see https://code.claude.com/docs/en/permissions#read-only-commands.
// The value reports whether their flags are all read-only, which permits
// unquoted globs.
var readOnlyPrograms = map[string]bool{
	"ls": true, "cat": true, "echo": true, "pwd": true, "head": true, "tail": true,
	"grep": true, "wc": true, "which": true, "diff": true, "stat": true, "du": true,
	"find": false, "git": false, "cd": true,
}

// findActions are the find options that run commands, delete or write
// files, or read the paths to search from a file.
var findActions = map[string]bool{
	"-exec": true, "-execdir": true, "-ok": true, "-okdir": true, "-delete": true,
	"-fprint": true, "-fprint0": true, "-fprintf": true, "-fls": true, "-files0-from": true,
}

// execWrappers run their arguments as a command; a rule with a wildcard
// cannot approve them.
var execWrappers = map[string]bool{"watch": true, "setsid": true, "ionice": true, "flock": true}

// readOnlyGit lists the git subcommands that only read, with their
// options that write files or run programs.
var readOnlyGit = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true, "rev-parse": true, "ls-files": true,
	"blame": true, "describe": true, "shortlog": true, "branch": true, "remote": true,
}

// writesRedirect reports whether the command redirects output to a file.
func (s *subcommand) writesRedirect() bool {
	for _, r := range s.cmd.redirs {
		if r.output() && r.file() {
			return true
		}
	}
	return false
}

// readOnly reports whether the command only reads, as Claude Code's
// built-in set of read-only commands does, apart from its redirections.
// inWorkDir reports whether a path is inside the working directories.
func (s *subcommand) readOnly(inWorkDir func(string) bool) bool {
	if !s.safeEnv {
		return false
	}
	if len(s.args) == 0 {
		// Safe assignments and input redirections only.
		return true
	}
	prog := s.program()
	globsOK, ok := readOnlyPrograms[prog]
	if !ok {
		return false
	}
	args := s.args[1:]
	if !globsOK {
		for _, a := range args {
			if a.glob {
				return false
			}
		}
	}
	switch prog {
	case "find":
		for _, a := range args {
			if findActions[a.lit] {
				return false
			}
		}
	case "cd":
		if len(args) != 1 || args[0].expand || args[0].subst || args[0].glob || s.cwd == "" {
			return false
		}
		return inWorkDir(joinPath(s.cwd, args[0].lit))
	case "git":
		return readOnlyGitArgs(args)
	}
	// Like the file tools, read-only commands read only inside the working
	// directories without approval: cat ~/.ssh/id_rsa needs it.
	for _, w := range readPaths(prog, args) {
		if w.expand || w.subst || s.cwd == "" && !filepath.IsAbs(w.lit) {
			return false
		}
		if !inWorkDir(joinPath(s.cwd, w.lit)) {
			return false
		}
	}
	return true
}

// readPaths returns the operands of a read-only command that name files
// or directories it reads: all of them, except the pattern of grep and
// for commands that read none.
func readPaths(prog string, args []word) []word {
	ops := operands(args)
	switch prog {
	case "echo", "pwd", "which", "cd", "git":
		return nil
	case "grep":
		if !hasPatternOption(args) && len(ops) > 0 {
			ops = ops[1:]
		}
	}
	return ops
}

// hasPatternOption reports whether grep's pattern is given by an option,
// so its first operand is a file.
func hasPatternOption(args []word) bool {
	for _, a := range args {
		l := a.lit
		if l == "--" {
			return false
		}
		if l == "-e" || l == "-f" || strings.HasPrefix(l, "--regexp") || strings.HasPrefix(l, "--file") ||
			strings.HasPrefix(l, "-") && !strings.HasPrefix(l, "--") && strings.ContainsAny(l[1:], "ef") && !strings.ContainsAny(l[1:], "0123456789") {
			return true
		}
	}
	return false
}

func readOnlyGitArgs(args []word) bool {
	if len(args) == 0 || args[0].expand || !readOnlyGit[args[0].lit] {
		// Options before the subcommand, such as -c or -C, can run
		// programs or change the repository.
		return false
	}
	sub := args[0].lit
	rest := args[1:]
	for _, a := range rest {
		l := a.lit
		if a.expand || a.subst || strings.HasPrefix(l, "--output") || l == "--ext-diff" || strings.HasPrefix(l, "--exec") || l == "--no-index" {
			return false
		}
	}
	switch sub {
	case "branch":
		for _, a := range rest {
			switch a.lit {
			case "--list", "-l", "-a", "--all", "-r", "--remotes", "-v", "-vv", "--verbose", "--show-current":
			default:
				return false
			}
		}
	case "remote":
		return len(rest) == 0 || len(rest) == 1 && (rest[0].lit == "-v" || rest[0].lit == "--verbose")
	}
	return true
}

// editPrograms are the filesystem commands acceptEdits mode approves for
// paths inside the working directories; see
// https://code.claude.com/docs/en/permission-modes#auto-approve-file-edits-with-acceptedits-mode.
// Claude Code also approves sed, whose scripts can write files (w) and run
// commands (e), which this package does not analyze, so sed is left out.
var editPrograms = map[string]bool{"mkdir": true, "touch": true, "rm": true, "rmdir": true, "mv": true, "cp": true}

// fileAccess is a file a command reads or writes.
type fileAccess struct {
	w     word
	write bool
	// redirect is set for the target of a redirection, tee for a file
	// written by tee.
	redirect, tee bool
}

// files returns the files the command reads and writes through
// redirections and well-known file commands.
func (s *subcommand) files() []fileAccess {
	var out []fileAccess
	for _, r := range s.cmd.redirs {
		if r.file() {
			out = append(out, fileAccess{w: r.target, write: r.output(), redirect: true})
		}
	}
	prog := s.program()
	if prog == "" {
		return out
	}
	operands := operands(s.args[1:])
	switch prog {
	case "cat", "head", "tail", "less", "more":
		for _, w := range operands {
			out = append(out, fileAccess{w: w})
		}
	case "tee":
		for _, w := range operands {
			out = append(out, fileAccess{w: w, write: true, tee: true})
		}
	case "cp":
		for i, w := range operands {
			out = append(out, fileAccess{w: w, write: i == len(operands)-1})
		}
	case "mkdir", "touch", "rm", "rmdir", "mv":
		for _, w := range operands {
			out = append(out, fileAccess{w: w, write: true})
		}
	case "sed":
		inPlace := false
		for _, a := range s.args[1:] {
			if a.lit == "-i" || strings.HasPrefix(a.lit, "-i") || strings.HasPrefix(a.lit, "--in-place") {
				inPlace = true
			}
		}
		files := operands
		if !hasScriptOption(s.args[1:]) && len(files) > 0 {
			files = files[1:]
		}
		for _, w := range files {
			out = append(out, fileAccess{w: w, write: inPlace})
		}
	}
	return out
}

// operands returns the arguments that are not options. After "--" every
// argument is an operand.
func operands(args []word) []word {
	var out []word
	opts := true
	for _, a := range args {
		switch {
		case opts && a.lit == "--":
			opts = false
		case opts && strings.HasPrefix(a.lit, "-") && a.lit != "-":
		default:
			out = append(out, a)
		}
	}
	return out
}

func hasScriptOption(args []word) bool {
	for _, a := range args {
		if a.lit == "-e" || a.lit == "-f" || strings.HasPrefix(a.lit, "--expression") || strings.HasPrefix(a.lit, "--file") {
			return true
		}
	}
	return false
}

// literal reports whether w names a path without expansions or globs.
func literal(w word) bool { return !w.expand && !w.subst && !w.glob && w.lit != "" }

// joinPath resolves a path given in a command against dir.
func joinPath(dir, p string) string {
	// On Windows, a rooted path without a volume is relative to the drive
	// root, never to the working directory (filepath.IsAbs returns false).
	if filepath.VolumeName(p) == "" && strings.HasPrefix(filepath.ToSlash(p), "/") {
		p = filepath.VolumeName(dir) + p
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(dir, p)
}

// topLevelNames are the common top-level directory names a removal is
// critical for when they follow a variable that may be empty, as in
// rm -rf "$PREFIX/usr".
var topLevelNames = map[string]bool{
	"bin": true, "boot": true, "dev": true, "etc": true, "home": true, "lib": true, "lib64": true,
	"mnt": true, "opt": true, "proc": true, "root": true, "run": true, "sbin": true, "srv": true,
	"sys": true, "tmp": true, "usr": true, "var": true, "Users": true, "Applications": true,
	"Library": true, "System": true, "Volumes": true, "private": true,
}

// criticalTarget reports whether an rm or rmdir operand targets a critical
// path; see https://code.claude.com/docs/en/permission-modes#critical-paths.
// critical reports whether an absolute path is critical, and globCritical
// whether a glob directly under it is.
func criticalTarget(w word, recursive bool, cwd, home string, critical, globCritical func(string) bool) bool {
	l := w.lit
	switch {
	case w.onlySubst:
		return recursive
	case l != "" && strings.Trim(l, `\`) == "":
		return true
	}
	// A trailing command substitution is checked as if it expanded empty.
	if i := strings.Index(l, "$("); i > 0 && w.subst && strings.HasSuffix(l, ")") {
		l = l[:i]
	} else if i := strings.IndexByte(l, '`'); i > 0 && w.subst && strings.HasSuffix(l, "`") {
		l = l[:i]
	}
	switch {
	case l == "~" || strings.HasPrefix(l, "~/"):
		l = home + l[1:]
	case strings.HasPrefix(l, "${HOME}"):
		l = home + l[len("${HOME}"):]
	case strings.HasPrefix(l, "$HOME") && (len(l) == 5 || l[5] == '/'):
		l = home + l[len("$HOME"):]
	case strings.HasPrefix(l, "$"):
		v := varToken(l)
		if strings.Contains(v, ":?") {
			// "${DIR:?}" stops the shell when the variable is empty.
			return false
		}
		rest := l[len(v):]
		if !strings.HasPrefix(rest, "/") {
			return false
		}
		clean := path.Clean(rest)
		first, _, _ := strings.Cut(strings.TrimPrefix(clean, "/"), "/")
		return rest == "/" || rest == "/*" || clean == "/"+first && topLevelNames[first]
	}
	if strings.Contains(l, "$") {
		return false
	}
	if strings.HasSuffix(l, "/*/*") || strings.HasSuffix(l, "/*/") {
		return true
	}
	if cwd == "" && !filepath.IsAbs(l) && !strings.HasPrefix(l, "~") {
		// After a cd, a relative glob may reach anywhere.
		return strings.Contains(l, "*")
	}
	base := cwd
	if base == "" {
		base = "/"
	}
	if g, ok := strings.CutSuffix(l, "*"); ok && (g == "" || strings.HasSuffix(g, "/")) {
		return globCritical(joinPath(base, strings.TrimSuffix(g, "/")))
	}
	return critical(joinPath(base, strings.TrimRight(l, "/")))
}

// varToken returns the variable reference at the start of s, such as
// "$DIR" or "${DIR:-x}".
func varToken(s string) string {
	if strings.HasPrefix(s, "${") {
		if i := strings.IndexByte(s, '}'); i > 0 {
			return s[:i+1]
		}
		return s
	}
	i := 1
	for i < len(s) && isNameByte(s[i], i == 1) {
		i++
	}
	if i == 1 && i < len(s) {
		i++
	}
	return s[:i]
}

// recursiveRemoval reports whether rm arguments include -r, -R or
// --recursive.
func recursiveRemoval(args []word) bool {
	for _, a := range args {
		l := a.lit
		if l == "--recursive" || strings.HasPrefix(l, "-") && !strings.HasPrefix(l, "--") && strings.ContainsAny(l, "rR") {
			return true
		}
	}
	return false
}
