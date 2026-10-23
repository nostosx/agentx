package permission

import (
	"strings"
)

// This file holds a small lexer for shell commands. It is not a shell
// parser: it splits a command line into the simple commands that Bash
// permission rules match, the way Claude Code does, and records what the
// rules and checks need about their words and redirections. Constructs it
// does not understand make the command unparseable, which keeps allow
// rules from approving it, while deny and ask rules still see every
// command it found and the whole command text.

// maxCommandLen bounds the commands the lexer analyzes. As in Claude Code,
// longer commands always need approval.
const maxCommandLen = 10000

// word is a shell word.
type word struct {
	// raw is the word as written, lit its value with quotes and escapes
	// removed. Expansions are kept as written in lit.
	raw, lit string
	// quoted is set if any part of the word was quoted or escaped.
	quoted bool
	// glob is set for an unquoted *, ? or [.
	glob bool
	// expand is set for a parameter, arithmetic or command expansion
	// outside single quotes, and for a leading ~.
	expand bool
	// subst is set if the word holds a command or process substitution,
	// onlySubst if it is nothing else.
	subst, onlySubst bool
}

// redirect is a redirection of a simple command.
type redirect struct {
	// op is the operator without a file descriptor number, such as ">",
	// ">>", "<", "<<", "<<<", ">&" or "&>".
	op     string
	target word
}

// output reports whether r writes its target.
func (r redirect) output() bool { return strings.Contains(r.op, ">") }

// file reports whether r names a file: here-documents, here-strings,
// descriptor duplications such as 2>&1 and /dev/null do not.
func (r redirect) file() bool {
	switch r.op {
	case "<<", "<<-", "<<<":
		return false
	case ">&", "<&":
		if isDigits(r.target.lit) || r.target.lit == "-" {
			return false
		}
	}
	return r.target.lit != "/dev/null"
}

// simpleCmd is a simple command: assignments, words and redirections.
type simpleCmd struct {
	assigns []word
	words   []word
	redirs  []redirect
	// nested is set for commands inside a substitution, a subshell's
	// parentheses are not nesting.
	nested bool
}

// script is the result of lexing a command line.
type script struct {
	cmds []*simpleCmd
	// bad is set when the command could not be split reliably, for
	// example for an unterminated quote or a trailing &&.
	bad bool
}

type lexer struct {
	s   string
	pos int
	out *script
	// heredocs are the delimiters of here-documents whose bodies start
	// after the current line.
	heredocs []heredoc
}

type heredoc struct {
	delim string
	tabs  bool
	// expand is set when the delimiter is unquoted, so the body is
	// expanded.
	expand bool
}

// lex splits a command line into simple commands.
func lex(s string) *script {
	out := &script{}
	if len(s) > maxCommandLen {
		out.bad = true
		return out
	}
	l := &lexer{s: s, out: out}
	l.list(0)
	if l.pos < len(l.s) {
		out.bad = true
	}
	if len(l.heredocs) > 0 {
		// The body never ended.
		out.bad = true
	}
	return out
}

func (l *lexer) peek() byte {
	if l.pos < len(l.s) {
		return l.s[l.pos]
	}
	return 0
}

func (l *lexer) at(prefix string) bool { return strings.HasPrefix(l.s[l.pos:], prefix) }

// blank skips spaces, tabs, line continuations and comments, but not
// newlines.
func (l *lexer) blank() {
	for l.pos < len(l.s) {
		switch c := l.s[l.pos]; {
		case c == ' ' || c == '\t' || c == '\r':
			l.pos++
		case c == '\\' && l.at("\\\n"):
			l.pos += 2
		case c == '#':
			for l.pos < len(l.s) && l.s[l.pos] != '\n' {
				l.pos++
			}
		default:
			return
		}
	}
}

// newline consumes a newline and the bodies of pending here-documents.
func (l *lexer) newline() {
	l.pos++
	for len(l.heredocs) > 0 {
		h := l.heredocs[0]
		found := false
		var body strings.Builder
		for l.pos < len(l.s) {
			end := strings.IndexByte(l.s[l.pos:], '\n')
			line := l.s[l.pos:]
			if end >= 0 {
				line = line[:end]
			}
			if h.tabs {
				line = strings.TrimLeft(line, "\t")
			}
			if end < 0 {
				l.pos = len(l.s)
			} else {
				l.pos += end + 1
			}
			if strings.TrimSuffix(line, "\r") == h.delim {
				found = true
				break
			}
			body.WriteString(line)
			body.WriteByte('\n')
		}
		if h.expand {
			l.heredocBody(body.String())
		}
		if !found {
			return
		}
		l.heredocs = l.heredocs[1:]
	}
}

// list parses commands separated by operators until end, a closing
// parenthesis when stop is ')' or a backquote when stop is '`'.
func (l *lexer) list(stop byte) {
	// pendingOp is set after && and the like, which need a command after
	// them.
	pendingOp, empty := false, true
	for {
		l.blank()
		if l.pos >= len(l.s) {
			if pendingOp {
				l.out.bad = true
			}
			return
		}
		c := l.s[l.pos]
		switch {
		case stop != 0 && c == stop:
			if pendingOp {
				l.out.bad = true
			}
			return
		case c == '\n':
			l.newline()
			if !pendingOp {
				empty = true
			}
			continue
		case l.at("&&"), l.at("||"):
			if empty || pendingOp {
				l.out.bad = true
			}
			l.pos += 2
			pendingOp = true
			continue
		case l.at(";;"):
			l.pos += 2
			pendingOp, empty = false, true
			continue
		case c == ';':
			if pendingOp {
				l.out.bad = true
			}
			l.pos++
			empty = true
			continue
		case l.at("|&"), c == '|':
			if pendingOp || empty {
				l.out.bad = true
			}
			l.pos++
			if c == '|' && l.peek() == '&' {
				l.pos++
			}
			pendingOp = true
			continue
		case c == '&' && !l.at("&>"):
			if pendingOp {
				l.out.bad = true
			}
			l.pos++
			empty = true
			continue
		case c == ')':
			// A closing parenthesis without an opening one, as in case
			// patterns.
			l.out.bad = true
			l.pos++
			continue
		}
		before := l.pos
		l.command()
		if l.pos == before {
			// Nothing was consumed; avoid looping forever.
			l.out.bad = true
			l.pos++
		}
		pendingOp, empty = false, false
	}
}

// keywords are reserved words that start or end a compound command. They
// are dropped, so the commands inside are matched on their own.
var keywords = map[string]bool{
	"{": true, "}": true, "!": true, "if": true, "then": true, "else": true, "elif": true,
	"do": true, "while": true, "until": true, "fi": true, "done": true, "esac": true,
}

// command parses a simple command or a subshell.
func (l *lexer) command() {
	cmd := &simpleCmd{}
	for {
		l.blank()
		if l.pos >= len(l.s) {
			break
		}
		c := l.s[l.pos]
		if c == '(' && len(cmd.words) == 0 && len(cmd.assigns) == 0 {
			l.pos++
			l.list(')')
			if l.peek() != ')' {
				l.out.bad = true
				return
			}
			l.pos++
			continue
		}
		if c == '\n' || c == ';' || c == '|' || c == ')' || (c == '&' && !l.at("&>")) {
			break
		}
		if c == '(' {
			// A function definition or another construct this lexer does
			// not follow.
			l.out.bad = true
			l.pos++
			continue
		}
		if r, ok := l.redirection(""); ok {
			cmd.redirs = append(cmd.redirs, r)
			continue
		}
		w := l.word()
		if w.raw == "" {
			l.out.bad = true
			return
		}
		// A number right before < or > is a file descriptor.
		if isDigits(w.raw) && (l.peek() == '<' || l.peek() == '>') {
			if r, ok := l.redirection(w.raw); ok {
				cmd.redirs = append(cmd.redirs, r)
				continue
			}
		}
		switch {
		case len(cmd.words) == 0 && isAssignment(w):
			cmd.assigns = append(cmd.assigns, w)
		case len(cmd.words) == 0 && len(cmd.assigns) == 0 && !w.quoted && keywords[w.raw]:
			// Dropped; see keywords.
		default:
			cmd.words = append(cmd.words, w)
		}
	}
	if len(cmd.words) > 0 || len(cmd.assigns) > 0 || len(cmd.redirs) > 0 {
		l.out.cmds = append(l.out.cmds, cmd)
	}
}

// redirection parses a redirection operator and its target at the
// current position. fd is the descriptor number already consumed.
func (l *lexer) redirection(fd string) (redirect, bool) {
	ops := []string{"&>>", "&>", "<<<", "<<-", "<<", "<>", ">>", ">|", ">&", "<&", ">", "<"}
	for _, op := range ops {
		if !l.at(op) {
			continue
		}
		if fd != "" && op[0] == '&' {
			return redirect{}, false
		}
		// <( and >( start a process substitution, which is a word.
		if (op == "<" || op == ">") && l.pos+1 < len(l.s) && l.s[l.pos+1] == '(' {
			return redirect{}, false
		}
		l.pos += len(op)
		l.blank()
		t := l.word()
		if t.raw == "" {
			l.out.bad = true
		}
		name := op
		switch op {
		case ">|":
			name = ">"
		case "<>":
			name = ">"
		}
		if op == "<<" || op == "<<-" {
			l.heredocs = append(l.heredocs, heredoc{delim: t.lit, tabs: op == "<<-", expand: !t.quoted})
		}
		return redirect{op: name, target: t}, true
	}
	return redirect{}, false
}

// word parses a word.
func (l *lexer) word() word {
	var w word
	var lit strings.Builder
	start := l.pos
	substParts := 0
	for l.pos < len(l.s) {
		c := l.s[l.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == ';' || c == '&' || c == '|' || c == ')':
			goto done
		case c == '`':
			from := l.pos
			l.pos++
			l.backquote()
			lit.WriteString(l.s[from:l.pos])
			w.expand, w.subst = true, true
			substParts++
		case c == '(':
			goto done
		case (c == '<' || c == '>') && l.pos+1 < len(l.s) && l.s[l.pos+1] == '(':
			if l.pos != start {
				goto done
			}
			l.pos += 2
			l.substitution()
			w.expand, w.subst = true, true
			substParts++
			lit.WriteString(l.s[start:l.pos])
		case c == '<' || c == '>':
			goto done
		case c == '\\':
			if l.pos+1 >= len(l.s) {
				l.pos++
				w.quoted = true
				continue
			}
			if l.s[l.pos+1] == '\n' {
				l.pos += 2
				continue
			}
			lit.WriteByte(l.s[l.pos+1])
			l.pos += 2
			w.quoted = true
		case c == '\'':
			end := strings.IndexByte(l.s[l.pos+1:], '\'')
			if end < 0 {
				l.out.bad = true
				l.pos = len(l.s)
				goto done
			}
			lit.WriteString(l.s[l.pos+1 : l.pos+1+end])
			l.pos += end + 2
			w.quoted = true
		case c == '"':
			l.pos++
			if !l.double(&w, &lit, &substParts) {
				goto done
			}
			w.quoted = true
		case c == '$':
			l.dollar(&w, &lit, &substParts)
		default:
			if c == '*' || c == '?' || c == '[' {
				w.glob = true
			}
			if c == '~' && l.pos == start {
				w.expand = true
			}
			lit.WriteByte(c)
			l.pos++
		}
	}
done:
	w.raw = l.s[start:l.pos]
	w.lit = lit.String()
	w.onlySubst = substParts == 1 && onlySubstitution(w.raw)
	return w
}

// onlySubstitution reports whether raw consists of a single command
// substitution, possibly in double quotes.
func onlySubstitution(raw string) bool {
	raw = strings.TrimSuffix(strings.TrimPrefix(raw, `"`), `"`)
	switch {
	case strings.HasPrefix(raw, "$(") && strings.HasSuffix(raw, ")"):
		return !strings.HasPrefix(raw, "$((")
	case strings.HasPrefix(raw, "`") && strings.HasSuffix(raw, "`") && len(raw) > 1:
		return true
	}
	return false
}

// double parses the rest of a double-quoted string. It reports false if
// the string is unterminated.
func (l *lexer) double(w *word, lit *strings.Builder, substParts *int) bool {
	for l.pos < len(l.s) {
		c := l.s[l.pos]
		switch c {
		case '"':
			l.pos++
			return true
		case '\\':
			if l.pos+1 < len(l.s) {
				n := l.s[l.pos+1]
				switch n {
				case '$', '`', '"', '\\':
					lit.WriteByte(n)
					l.pos += 2
					continue
				case '\n':
					l.pos += 2
					continue
				}
			}
			lit.WriteByte(c)
			l.pos++
		case '$':
			l.dollar(w, lit, substParts)
		case '`':
			start := l.pos
			l.pos++
			l.backquote()
			lit.WriteString(l.s[start:l.pos])
			w.expand, w.subst = true, true
			*substParts++
		default:
			lit.WriteByte(c)
			l.pos++
		}
	}
	l.out.bad = true
	return false
}

// dollar parses an expansion starting with $.
func (l *lexer) dollar(w *word, lit *strings.Builder, substParts *int) {
	start := l.pos
	switch {
	case l.at("$(("):
		l.pos += 3
		w.subst = l.expansion('(', ')', 2) || w.subst
		w.expand = true
	case l.at("$("):
		l.pos += 2
		l.substitution()
		w.expand, w.subst = true, true
		*substParts++
	case l.at("${"):
		l.pos += 2
		w.subst = l.expansion('{', '}', 1) || w.subst
		w.expand = true
	case l.at("$'"):
		// ANSI-C quoting.
		l.pos += 2
		var b strings.Builder
		for l.pos < len(l.s) && l.s[l.pos] != '\'' {
			if l.s[l.pos] == '\\' && l.pos+1 < len(l.s) {
				b.WriteByte(l.s[l.pos+1])
				l.pos += 2
				continue
			}
			b.WriteByte(l.s[l.pos])
			l.pos++
		}
		if l.pos >= len(l.s) {
			l.out.bad = true
			return
		}
		l.pos++
		lit.WriteString(b.String())
		w.quoted = true
		return
	default:
		l.pos++
		if l.pos < len(l.s) && strings.IndexByte("@*#?$!-0123456789", l.s[l.pos]) >= 0 {
			l.pos++
			w.expand = true
		} else {
			n := 0
			for l.pos < len(l.s) && isNameByte(l.s[l.pos], n == 0) {
				l.pos++
				n++
			}
			w.expand = w.expand || n > 0
		}
	}
	lit.WriteString(l.s[start:l.pos])
}

// substitution parses the commands of a $(...) or <(...) substitution up
// to and including the closing parenthesis.
func (l *lexer) substitution() {
	n := len(l.out.cmds)
	l.list(')')
	for _, c := range l.out.cmds[n:] {
		c.nested = true
	}
	if l.peek() != ')' {
		l.out.bad = true
		return
	}
	l.pos++
}

// backquote parses the commands of a `...` substitution after the opening
// backquote, up to and including the closing one.
func (l *lexer) backquote() {
	end := l.pos
	for end < len(l.s) && l.s[end] != '`' {
		if l.s[end] == '\\' {
			end++
		}
		end++
	}
	if end >= len(l.s) {
		l.out.bad = true
		l.pos = len(l.s)
		return
	}
	inner := strings.NewReplacer("\\`", "`", "\\\\", "\\", "\\$", "$").Replace(l.s[l.pos:end])
	sub := lex(inner)
	for _, c := range sub.cmds {
		c.nested = true
	}
	l.out.cmds = append(l.out.cmds, sub.cmds...)
	l.out.bad = l.out.bad || sub.bad
	l.pos = end + 1
}

// expansion parses the rest of a parameter expansion ${...} or an
// arithmetic expansion $((...)) up to after depth closing characters.
// Their words are expanded too, so the command substitutions inside, such
// as the $(rm x) of ${v:-$(rm x)}, are parsed like any other; process
// substitutions there make the command unparseable. It reports whether it
// found a substitution.
func (l *lexer) expansion(open, close byte, depth int) (subst bool) {
	for l.pos < len(l.s) && depth > 0 {
		switch c := l.s[l.pos]; {
		case c == open:
			depth++
		case c == close:
			depth--
		case c == '\\':
			l.pos++
		case c == '\'' && open == '{':
			// Single quotes quote only in parameter expansions.
			if end := strings.IndexByte(l.s[l.pos+1:], '\''); end >= 0 {
				l.pos += end + 1
			}
		case l.at("$(("):
			l.pos += 3
			subst = l.expansion('(', ')', 2) || subst
			continue
		case l.at("$("):
			l.pos += 2
			l.substitution()
			subst = true
			continue
		case l.at("${"):
			l.pos += 2
			subst = l.expansion('{', '}', 1) || subst
			continue
		case c == '`':
			l.pos++
			l.backquote()
			subst = true
			continue
		case l.at("<("), l.at(">("):
			l.out.bad = true
		}
		l.pos++
	}
	if depth > 0 {
		l.out.bad = true
	}
	return subst
}

// heredocBody parses the expansions in the body of a here-document whose
// delimiter is unquoted, which the shell expands like a double-quoted
// string: their command substitutions are commands of the script.
func (l *lexer) heredocBody(body string) {
	sub := &lexer{s: body, out: l.out}
	var w word
	var lit strings.Builder
	n := 0
	for sub.pos < len(sub.s) {
		switch sub.s[sub.pos] {
		case '\\':
			sub.pos += 2
		case '$':
			sub.dollar(&w, &lit, &n)
		case '`':
			sub.pos++
			sub.backquote()
		default:
			sub.pos++
		}
	}
	if len(sub.heredocs) > 0 {
		l.out.bad = true
	}
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isNameByte(c byte, first bool) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || !first && c >= '0' && c <= '9'
}

// isAssignment reports whether w is a variable assignment such as A=1.
func isAssignment(w word) bool {
	name, _, ok := strings.Cut(w.raw, "=")
	name = strings.TrimSuffix(name, "+")
	if !ok || name == "" {
		return false
	}
	for i := range len(name) {
		if !isNameByte(name[i], i == 0) {
			return false
		}
	}
	return true
}

// assignName returns the variable an assignment sets.
func assignName(w word) string {
	name, _, _ := strings.Cut(w.raw, "=")
	return strings.TrimSuffix(name, "+")
}
