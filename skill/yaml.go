package skill

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// plain is an unquoted YAML scalar. Its type, such as a bool or a number,
// depends on where it is used: toJSON resolves it, asString keeps its text.
type plain string

// yamlParser parses the subset of YAML used by skill frontmatter: block
// mappings and sequences nested by indentation, flow mappings and
// sequences ({a: b}, [a, b]), plain scalars, single- and double-quoted
// scalars that may span lines, and literal or folded block scalars. Values
// are plain, string (quoted and block scalars), []any or map[string]any.
// Anchors, aliases, tags and multiple documents are not supported.
type yamlParser struct{ lines []string }

func parseFrontmatter(lines []string) (map[string]any, error) {
	p := &yamlParser{lines: slices.Clone(lines)}
	m, _, err := p.mapping(0, 0)
	return m, err
}

// blank reports whether line i holds no structure: it is empty or a
// comment.
func (p *yamlParser) blank(i int) bool {
	s := strings.TrimSpace(p.lines[i])
	return s == "" || s[0] == '#'
}

func indentOf(line string) int { return len(line) - len(strings.TrimLeft(line, " \t")) }

// mapping parses the block mapping whose keys start at column indent,
// from line i. It returns at the first line indented less.
func (p *yamlParser) mapping(i, indent int) (map[string]any, int, error) {
	out := map[string]any{}
	for i < len(p.lines) {
		if p.blank(i) {
			i++
			continue
		}
		line := p.lines[i]
		n := indentOf(line)
		if n < indent {
			break
		}
		if n > indent {
			// A stray line indented deeper than the keys around it, which
			// no value claimed; YAML rejects it, we skip it.
			i++
			continue
		}
		key, rest, ok := splitKey(line[n:], indent == 0)
		if !ok {
			return nil, i, fmt.Errorf("skill: invalid frontmatter line %q", line)
		}
		v, next, err := p.value(rest, i, indent, false)
		if err != nil {
			return nil, i, err
		}
		out[key] = v
		i = next
	}
	return out, i, nil
}

// sequence parses the block sequence whose "-" indicators are at column
// indent, from line i.
func (p *yamlParser) sequence(i, indent int) ([]any, int, error) {
	out := []any{}
	for i < len(p.lines) {
		if p.blank(i) {
			i++
			continue
		}
		line := p.lines[i]
		n := indentOf(line)
		if n < indent {
			break
		}
		if n > indent {
			i++
			continue
		}
		content := line[n:]
		if content != "-" && !strings.HasPrefix(content, "- ") && !strings.HasPrefix(content, "-\t") {
			break
		}
		item := strings.TrimLeft(content[1:], " \t")
		col := n + len(content) - len(item)
		var (
			v   any
			err error
		)
		switch {
		case strings.TrimSpace(stripComment(item)) == "":
			v, i, err = p.value("", i, n, true)
		case isKeyLine(item):
			// "- key: value" starts a mapping whose keys are at the column
			// of "key"; the line is reparsed with the indicator blanked.
			p.lines[i] = strings.Repeat(" ", col) + item
			v, i, err = p.mapping(i, col)
		case item == "-" || strings.HasPrefix(item, "- "):
			p.lines[i] = strings.Repeat(" ", col) + item
			v, i, err = p.sequence(i, col)
		default:
			v, i, err = p.value(item, i, n, true)
		}
		if err != nil {
			return nil, i, err
		}
		out = append(out, v)
	}
	return out, i, nil
}

// value parses the value that follows a key or, if seqItem is set, a
// sequence indicator on line i, rest, together with the lines below it
// that are indented more than indent.
func (p *yamlParser) value(rest string, i, indent int, seqItem bool) (any, int, error) {
	full := strings.TrimSpace(rest)
	raw := strings.TrimSpace(stripComment(rest))
	// The lines that belong to the value: blank ones and those indented
	// more than indent.
	end := i + 1
	for end < len(p.lines) {
		l := p.lines[end]
		if strings.TrimSpace(l) != "" && indentOf(l) <= indent {
			break
		}
		end++
	}
	block := p.lines[i+1 : end]

	if full != "" && (full[0] == '"' || full[0] == '\'') {
		// A quoted scalar may span lines, and a "#" inside it is text.
		if s, ok := quotedScalar(full, block); ok {
			return s, end, nil
		}
	}
	if h, ok := parseBlockHeader(raw); ok {
		return blockScalar(block, h), end, nil
	}
	if raw != "" && (raw[0] == '[' || raw[0] == '{') {
		// A flow collection continues until its brackets close, however
		// its lines are indented.
		for depth := flowDepth(raw, block); depth > 0 && end < len(p.lines); end++ {
			depth += flowDepth(stripComment(p.lines[end]), nil)
			block = p.lines[i+1 : end+1]
		}
		parts := []string{raw}
		for _, b := range block {
			if s := strings.TrimSpace(stripComment(b)); s != "" {
				parts = append(parts, s)
			}
		}
		text := strings.Join(parts, " ")
		if v, ok := parseFlow(text); ok {
			return v, end, nil
		}
		if raw[0] == '[' {
			items := []any{}
			for _, s := range flowSeq(text) {
				items = append(items, s)
			}
			return items, end, nil
		}
	}
	if raw == "" {
		// A nested collection, or nothing.
		j := i + 1
		for j < len(p.lines) && p.blank(j) {
			j++
		}
		if j < len(p.lines) {
			l := p.lines[j]
			n := indentOf(l)
			item := l[n:]
			isItem := item == "-" || strings.HasPrefix(item, "- ") || strings.HasPrefix(item, "-\t")
			switch {
			case isItem && (n > indent || n == indent && !seqItem):
				// YAML allows a sequence under a key at the key's own
				// indentation.
				return p.sequence(j, n)
			case n > indent && isKeyLine(item):
				return p.mapping(j, n)
			}
		}
	}
	// A plain scalar; its continuation lines fold into one line.
	parts := []string{raw}
	for _, b := range block {
		if s := strings.TrimSpace(stripComment(b)); s != "" {
			parts = append(parts, s)
		}
	}
	text := strings.TrimSpace(strings.Join(parts, " "))
	if u := unquote(text); u != text {
		return u, end, nil
	}
	return plain(text), end, nil
}

// flowDepth returns how many flow brackets first and lines leave open.
func flowDepth(first string, lines []string) int {
	depth := 0
	for _, s := range append([]string{first}, lines...) {
		var quote byte
		prev := byte('[') // a quote starts a scalar only after an indicator
		for i := 0; i < len(s); i++ {
			c := s[i]
			switch {
			case quote != 0:
				if c == quote {
					quote = 0
				} else if c == '\\' && quote == '"' {
					i++
				}
				continue
			case (c == '"' || c == '\'') && strings.IndexByte("[{,:", prev) >= 0:
				quote = c
			case c == '[' || c == '{':
				depth++
			case c == ']' || c == '}':
				depth--
			}
			if c != ' ' && c != '\t' {
				prev = c
			}
		}
	}
	return depth
}

// splitKey splits a mapping line into its key and the rest. The key ends
// at the first ":" followed by white space or the end of the line, or,
// when lenient, at the first ":" if there is no such colon. A key may be
// quoted.
func splitKey(s string, lenient bool) (key, rest string, ok bool) {
	if s != "" && (s[0] == '"' || s[0] == '\'') {
		if end := closingQuote(s); end > 0 {
			after := strings.TrimLeft(s[end+1:], " \t")
			if strings.HasPrefix(after, ":") && (len(after) == 1 || after[1] == ' ' || after[1] == '\t') {
				return unquote(s[:end+1]), after[1:], true
			}
		}
	}
	for i := 0; i < len(s); i++ {
		if s[i] == ':' && (i+1 == len(s) || s[i+1] == ' ' || s[i+1] == '\t') {
			if k := strings.TrimSpace(s[:i]); k != "" {
				return k, s[i+1:], true
			}
			return "", "", false
		}
		if s[i] == '#' && i > 0 && (s[i-1] == ' ' || s[i-1] == '\t') {
			break
		}
	}
	if lenient {
		if k, r, found := strings.Cut(s, ":"); found && strings.TrimSpace(k) != "" {
			return strings.TrimSpace(k), r, true
		}
	}
	return "", "", false
}

// isKeyLine reports whether s, the content of a line, starts a mapping
// entry rather than being a scalar.
func isKeyLine(s string) bool {
	if s == "" || s[0] == '[' || s[0] == '{' || s[0] == '#' {
		return false
	}
	_, _, ok := splitKey(s, false)
	return ok
}

// closingQuote returns the index of the quote that closes the quoted
// scalar at the start of s, or -1.
func closingQuote(s string) int {
	q := s[0]
	for i := 1; i < len(s); i++ {
		switch {
		case q == '\'' && s[i] == '\'' && i+1 < len(s) && s[i+1] == '\'':
			i++
		case q == '"' && s[i] == '\\':
			i++
		case s[i] == q:
			return i
		}
	}
	return -1
}

// parseFlow parses a flow collection or scalar that makes up all of s.
func parseFlow(s string) (any, bool) {
	f := &flowParser{s: s}
	v, ok := f.node(false)
	if !ok {
		return nil, false
	}
	f.space()
	return v, f.i == len(f.s)
}

type flowParser struct {
	s string
	i int
}

func (f *flowParser) space() {
	for f.i < len(f.s) && (f.s[f.i] == ' ' || f.s[f.i] == '\t') {
		f.i++
	}
}

// node parses a flow node. In a key, a plain scalar ends at ": ".
func (f *flowParser) node(key bool) (any, bool) {
	f.space()
	if f.i == len(f.s) {
		return plain(""), true
	}
	switch c := f.s[f.i]; c {
	case '[':
		return f.seq()
	case '{':
		return f.mapping()
	case '"', '\'':
		end := closingQuote(f.s[f.i:])
		if end < 0 {
			return nil, false
		}
		v := unquote(f.s[f.i : f.i+end+1])
		f.i += end + 1
		return v, true
	}
	start, depth := f.i, 0
	for ; f.i < len(f.s); f.i++ {
		c := f.s[f.i]
		switch {
		case c == '(':
			depth++
		case c == ')' && depth > 0:
			depth--
		case depth == 0 && (c == ',' || c == ']' || c == '}'):
			return plain(strings.TrimSpace(f.s[start:f.i])), true
		case depth == 0 && key && c == ':' && (f.i+1 == len(f.s) || strings.ContainsRune(" \t,}", rune(f.s[f.i+1]))):
			return plain(strings.TrimSpace(f.s[start:f.i])), true
		}
	}
	return plain(strings.TrimSpace(f.s[start:])), true
}

func (f *flowParser) seq() (any, bool) {
	f.i++ // [
	out := []any{}
	for {
		f.space()
		if f.i == len(f.s) {
			return nil, false
		}
		switch f.s[f.i] {
		case ']':
			f.i++
			return out, true
		case ',':
			f.i++
			continue
		}
		v, ok := f.node(false)
		if !ok {
			return nil, false
		}
		if s, isPlain := v.(plain); !isPlain || s != "" {
			out = append(out, v)
		}
		f.space()
		if f.i < len(f.s) && f.s[f.i] != ',' && f.s[f.i] != ']' {
			return nil, false
		}
	}
}

func (f *flowParser) mapping() (any, bool) {
	f.i++ // {
	out := map[string]any{}
	for {
		f.space()
		if f.i == len(f.s) {
			return nil, false
		}
		switch f.s[f.i] {
		case '}':
			f.i++
			return out, true
		case ',':
			f.i++
			continue
		}
		k, ok := f.node(true)
		if !ok {
			return nil, false
		}
		key := asString(k)
		f.space()
		var v any = plain("")
		if f.i < len(f.s) && f.s[f.i] == ':' {
			f.i++
			if v, ok = f.node(false); !ok {
				return nil, false
			}
		}
		out[key] = v
		f.space()
		if f.i < len(f.s) && f.s[f.i] != ',' && f.s[f.i] != '}' {
			return nil, false
		}
	}
}

// asString returns a scalar's text. Sequences are joined with ", " and
// mappings encoded as JSON.
func asString(v any) string {
	switch v := v.(type) {
	case nil:
		return ""
	case plain:
		return string(v)
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			parts = append(parts, asString(item))
		}
		return strings.Join(parts, ", ")
	default:
		b, _ := json.Marshal(toJSON(v))
		return string(b)
	}
}

// asList returns a sequence's items as strings, or splits a scalar at
// commas and, when spaces is set, white space, as Claude Code does for
// fields that accept "a space- or comma-separated string, or a YAML list".
func asList(v any, spaces bool) []string {
	items, ok := v.([]any)
	if !ok {
		return splitList(asString(v), spaces)
	}
	var out []string
	for _, item := range items {
		if s := strings.TrimSpace(asString(item)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// asBool reads a boolean as Claude Code does: true, yes, on and 1, or
// false, no, off and 0, in any letter case. ok is false for other values.
func asBool(v any) (value, ok bool) {
	switch strings.ToLower(strings.TrimSpace(asString(v))) {
	case "true", "yes", "on", "1":
		return true, true
	case "false", "no", "off", "0":
		return false, true
	}
	return false, false
}

// toJSON converts a parsed value to JSON types, resolving plain scalars
// as YAML's core schema does: null, booleans, integers and floats.
func toJSON(v any) any {
	switch v := v.(type) {
	case plain:
		return resolvePlain(string(v))
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = toJSON(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = toJSON(item)
		}
		return out
	}
	return v
}

func resolvePlain(s string) any {
	switch s {
	case "", "~", "null", "Null", "NULL":
		return nil
	case "true", "True", "TRUE":
		return true
	case "false", "False", "FALSE":
		return false
	}
	if n, err := strconv.ParseInt(s, 0, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && !math.IsInf(f, 0) && !math.IsNaN(f) && strings.ContainsAny(s, "0123456789") {
		return f
	}
	return s
}

// toStrings converts a parsed value to strings, sequences and mappings,
// keeping the text of every scalar, as the Agent Skills metadata field
// expects.
func toStrings(v any) any {
	switch v := v.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = toStrings(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = toStrings(item)
		}
		return out
	}
	return asString(v)
}
