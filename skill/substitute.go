package skill

import (
	"slices"
	"strconv"
	"strings"
)

// Vars are the values substituted into skill content; see Substitute.
type Vars struct {
	// Arguments is the argument string, as a user would type it after the
	// skill's name.
	Arguments string
	// SessionID replaces ${CLAUDE_SESSION_ID}.
	SessionID string
	// ProjectDir replaces ${CLAUDE_PROJECT_DIR}.
	ProjectDir string
	// Effort replaces ${CLAUDE_EFFORT}.
	Effort string
}

// Substitute expands the placeholders of skill content as Claude Code does
// (https://code.claude.com/docs/en/skills#available-string-substitutions):
//
//   - $ARGUMENTS is the whole argument string.
//   - $ARGUMENTS[N] and $N are the argument at 0-based index N; arguments
//     are split shell-style, so quotes group words. Without an argument at
//     that index the placeholder stays as written.
//   - $name is the argument at the position of name in meta.Arguments, or
//     empty.
//   - ${CLAUDE_SESSION_ID}, ${CLAUDE_EFFORT}, ${CLAUDE_SKILL_DIR} (dir)
//     and ${CLAUDE_PROJECT_DIR} are replaced when their value is known,
//     also inside inserted arguments, which are not expanded otherwise.
//
// A single backslash before an argument placeholder, as in \$1.00, makes
// it literal. When arguments were given but no placeholder received them,
// "ARGUMENTS: <arguments>" is appended.
func Substitute(content string, meta Metadata, dir string, v Vars) string {
	e := newExpander(meta, v)
	out := e.claude(e.expand(content), dir)
	return e.finish(out)
}

// expander substitutes the argument placeholders of one invocation.
type expander struct {
	vars     Vars
	args     []string
	names    []string // declared names, longest first
	index    map[string]int
	received bool
}

func newExpander(meta Metadata, v Vars) *expander {
	e := &expander{vars: v, args: splitArgs(v.Arguments), index: map[string]int{}}
	for i, n := range meta.Arguments {
		if _, dup := e.index[n]; !dup && n != "" {
			e.index[n] = i
			e.names = append(e.names, n)
		}
	}
	slices.SortStableFunc(e.names, func(a, b string) int { return len(b) - len(a) })
	return e
}

// finish appends the arguments when no placeholder received them.
func (e *expander) finish(s string) string {
	if e.received || strings.TrimSpace(e.vars.Arguments) == "" {
		return s
	}
	return strings.TrimRight(s, "\n") + "\n\nARGUMENTS: " + e.vars.Arguments
}

// expand replaces the argument placeholders of s.
func (e *expander) expand(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		switch s[i] {
		case '\\':
			j := i
			for j < len(s) && s[j] == '\\' {
				j++
			}
			if j-i == 1 && j < len(s) {
				received := e.received
				tok, _, _ := e.placeholder(s[j:])
				e.received = received
				if tok != "" {
					// \$1 is a literal $1.
					b.WriteString(tok)
					i = j + len(tok)
					continue
				}
			}
			b.WriteString(s[i:j])
			i = j
		case '$':
			tok, val, ok := e.placeholder(s[i:])
			if tok != "" && ok {
				b.WriteString(val)
				i += len(tok)
				continue
			}
			b.WriteByte('$')
			i++
		default:
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String()
}

// scan records whether s has placeholders that receive the arguments,
// without expanding them.
func (e *expander) scan(s string) {
	for i := strings.IndexByte(s, '$'); i >= 0; {
		e.placeholder(s[i:])
		next := strings.IndexByte(s[i+1:], '$')
		if next < 0 {
			break
		}
		i += next + 1
	}
}

// placeholder parses the argument placeholder at the start of s, which
// starts with "$". It returns the placeholder's text, or "" if there is
// none, its value, and whether it expands. It records placeholders that
// receive an argument.
func (e *expander) placeholder(s string) (tok, val string, ok bool) {
	if len(s) < 2 || s[0] != '$' {
		return "", "", false
	}
	indexed := func(tok, digits string) (string, string, bool) {
		n, err := strconv.Atoi(digits)
		if err != nil || n >= len(e.args) {
			return tok, "", false
		}
		e.received = true
		return tok, e.args[n], true
	}
	if rest, found := strings.CutPrefix(s[1:], "ARGUMENTS"); found {
		if strings.HasPrefix(rest, "[") {
			if end := strings.IndexByte(rest, ']'); end > 1 && allDigits(rest[1:end]) {
				return indexed(s[:len("$ARGUMENTS")+end+1], rest[1:end])
			}
		}
		if rest == "" || !identByte(rest[0]) {
			e.received = true
			return "$ARGUMENTS", e.vars.Arguments, true
		}
	}
	if n := digitsPrefix(s[1:]); n > 0 {
		return indexed(s[:1+n], s[1:1+n])
	}
	for _, name := range e.names {
		if rest, found := strings.CutPrefix(s[1:], name); found && (rest == "" || !identByte(rest[0])) {
			e.received = true
			if i := e.index[name]; i < len(e.args) {
				return "$" + name, e.args[i], true
			}
			return "$" + name, "", true
		}
	}
	return "", "", false
}

// claude replaces the ${CLAUDE_*} variables whose values are known.
func (e *expander) claude(s, dir string) string {
	if !strings.Contains(s, "${CLAUDE_") {
		return s
	}
	var pairs []string
	for _, kv := range [][2]string{
		{"${CLAUDE_SESSION_ID}", e.vars.SessionID},
		{"${CLAUDE_EFFORT}", e.vars.Effort},
		{"${CLAUDE_SKILL_DIR}", dir},
		{"${CLAUDE_PROJECT_DIR}", e.vars.ProjectDir},
	} {
		if kv[1] != "" {
			pairs = append(pairs, kv[0], kv[1])
		}
	}
	return strings.NewReplacer(pairs...).Replace(s)
}

func identByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func allDigits(s string) bool { return s != "" && digitsPrefix(s) == len(s) }

func digitsPrefix(s string) int {
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	return n
}

// splitArgs splits an argument string shell-style: white space separates
// arguments, single quotes keep text literally, and in double quotes and
// unquoted text a backslash escapes the next character (in double quotes
// only ", \, $ and `). An unclosed quote extends to the end.
func splitArgs(s string) []string {
	var out []string
	var b strings.Builder
	inArg := false
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				b.WriteByte(c)
			}
		case quote == '"':
			switch {
			case c == '"':
				quote = 0
			case c == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`", s[i+1]) >= 0:
				i++
				b.WriteByte(s[i])
			default:
				b.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote, inArg = c, true
		case c == '\\' && i+1 < len(s):
			i++
			b.WriteByte(s[i])
			inArg = true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if inArg {
				out = append(out, b.String())
				b.Reset()
				inArg = false
			}
		default:
			b.WriteByte(c)
			inArg = true
		}
	}
	if inArg {
		out = append(out, b.String())
	}
	return out
}
