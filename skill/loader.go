package skill

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const utf8BOM = "\xef\xbb\xbf"

// Parse splits a SKILL.md document into metadata and body. It understands
// the subset of YAML used by skill frontmatter: nested block mappings and
// sequences, flow mappings and sequences ({a: b}, [a, b]), plain scalars,
// single- and double-quoted scalars (which may span lines and contain
// "#"), and folded or literal block scalars with chomping and indentation
// indicators. As in YAML, a "#" that starts a line or follows white space
// outside quotes begins a comment, so "description: Use C# # note" reads
// as "Use C#".
//
// Fields that take a list, such as allowed-tools, accept a YAML list or a
// string separated by commas and, except for paths, white space. Boolean
// fields accept true, yes, on and 1, or false, no, off and 0, in any letter
// case. Unrecognized keys are kept in Extra.
func Parse(doc string) (Metadata, string, error) {
	doc = strings.TrimPrefix(doc, utf8BOM)
	doc = strings.ReplaceAll(doc, "\r\n", "\n")
	first, rest, _ := strings.Cut(doc, "\n")
	if !isDelimiter(first) {
		return Metadata{}, "", errors.New("skill: missing frontmatter")
	}
	var front []string
	body, closed := "", false
	for rest != "" {
		var line string
		line, rest, _ = strings.Cut(rest, "\n")
		if isDelimiter(line) {
			body, closed = rest, true
			break
		}
		front = append(front, line)
	}
	if !closed {
		return Metadata{}, "", errors.New("skill: unterminated frontmatter")
	}
	fields, err := parseFrontmatter(front)
	if err != nil {
		return Metadata{}, "", err
	}
	return metadataOf(fields), strings.TrimSpace(body), nil
}

// metadataOf converts parsed frontmatter to Metadata.
func metadataOf(fields map[string]any) Metadata {
	meta := Metadata{Extra: map[string]string{}}
	boolean := func(k string, v any) bool {
		b, ok := asBool(v)
		if !ok {
			meta.Extra[k] = asString(v)
		}
		return b
	}
	for k, v := range fields {
		switch k {
		case "name":
			meta.Name = asString(v)
		case "description":
			meta.Description = asString(v)
		case "when_to_use":
			meta.WhenToUse = asString(v)
		case "argument-hint":
			meta.ArgumentHint = asString(v)
		case "arguments":
			meta.Arguments = asList(v, true)
		case "disable-model-invocation":
			meta.DisableModelInvocation = boolean(k, v)
		case "user-invocable":
			if b, ok := asBool(v); ok {
				meta.UserInvocable = &b
			} else {
				meta.Extra[k] = asString(v)
			}
		case "allowed-tools", "allowed_tools":
			meta.AllowedTools = asList(v, true)
		case "disallowed-tools":
			meta.DisallowedTools = asList(v, true)
		case "model":
			meta.Model = asString(v)
		case "effort":
			meta.Effort = asString(v)
		case "context":
			meta.Context = asString(v)
		case "agent":
			meta.Agent = asString(v)
		case "background":
			if b, ok := asBool(v); ok {
				meta.Background = &b
			} else {
				meta.Extra[k] = asString(v)
			}
		case "hooks":
			if m, ok := toJSON(v).(map[string]any); ok {
				meta.Hooks = m
			} else if asString(v) != "" {
				meta.Extra[k] = asString(v)
			}
		case "paths":
			meta.Paths = asList(v, false)
		case "shell":
			meta.Shell = asString(v)
		case "license":
			meta.License = asString(v)
		case "compatibility":
			meta.Compatibility = asString(v)
		case "metadata":
			// As in Claude Code, a value that is not a mapping is dropped.
			if m, ok := toStrings(v).(map[string]any); ok {
				meta.Metadata = m
			}
		default:
			meta.Extra[k] = asString(v)
		}
	}
	if len(meta.Extra) == 0 {
		meta.Extra = nil
	}
	return meta
}

// isDelimiter reports whether line is a frontmatter delimiter, allowing
// trailing whitespace.
func isDelimiter(line string) bool { return strings.TrimRight(line, " \t") == "---" }

// blockHeader is the header of a YAML block scalar, such as "|-" or ">2".
type blockHeader struct {
	folded bool
	keep   bool // "+" chomping keeps trailing newlines.
	indent int  // explicit indentation, or 0 to detect it
}

func parseBlockHeader(raw string) (blockHeader, bool) {
	if raw == "" || (raw[0] != '|' && raw[0] != '>') {
		return blockHeader{}, false
	}
	h := blockHeader{folded: raw[0] == '>'}
	var chomp, indent bool
	for _, c := range raw[1:] {
		switch {
		case (c == '+' || c == '-') && !chomp:
			chomp, h.keep = true, c == '+'
		case c >= '1' && c <= '9' && !indent:
			indent, h.indent = true, int(c-'0')
		default:
			return blockHeader{}, false
		}
	}
	return h, true
}

func blockScalar(block []string, h blockHeader) string {
	indent := h.indent
	if indent == 0 {
		indent = -1
		for _, b := range block {
			if strings.TrimSpace(b) == "" {
				continue
			}
			n := len(b) - len(strings.TrimLeft(b, " \t"))
			if indent < 0 || n < indent {
				indent = n
			}
		}
	}
	lines := make([]string, 0, len(block))
	for _, b := range block {
		if len(b) >= indent && indent > 0 && strings.TrimSpace(b[:indent]) == "" {
			b = b[indent:]
		} else {
			b = strings.TrimSpace(b)
		}
		lines = append(lines, b)
	}
	text := strings.TrimRight(strings.Join(lines, "\n"), "\n")
	if h.folded {
		var b strings.Builder
		for para := range strings.SplitSeq(text, "\n\n") {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(strings.Join(strings.Fields(para), " "))
		}
		text = b.String()
	}
	if h.keep && text != "" {
		// Keep the final line break and any trailing blank lines.
		trailing := 0
		for _, l := range slices.Backward(lines) {
			if l != "" {
				break
			}
			trailing++
		}
		text += strings.Repeat("\n", trailing+1)
	}
	return text
}

// quotedScalar reads the YAML quoted scalar that starts first and may
// continue over the indented lines in block. Line breaks fold the YAML way:
// a single break becomes a space and each blank line a line break; in
// double quotes, a backslash before the break joins the lines. A comment
// may follow the closing quote. ok is false when the scalar is never
// closed or other text follows it.
func quotedScalar(first string, block []string) (string, bool) {
	q := first[0]
	segs := make([]string, 0, len(block)+1)
	segs = append(segs, first[1:])
	for _, b := range block {
		segs = append(segs, strings.TrimSpace(b))
	}
	var sb strings.Builder
	for n := 0; n < len(segs); n++ {
		s := segs[n]
		if n > 0 {
			s = strings.TrimLeft(s, " \t")
		}
		joined := false // the line ends with an escaped line break
		for i := 0; i < len(s); i++ {
			c := s[i]
			switch {
			case c == q && q == '\'' && i+1 < len(s) && s[i+1] == '\'':
				sb.WriteByte('\'')
				i++
			case c == q:
				if after := strings.TrimSpace(s[i+1:]); after != "" && !strings.HasPrefix(after, "#") {
					// More follows the scalar, as in a list of quoted and
					// plain entries.
					return "", false
				}
				if q == '"' {
					return decodeEscapes(sb.String()), true
				}
				return sb.String(), true
			case c == '\\' && q == '"':
				if i+1 == len(s) {
					joined = true
					continue
				}
				// Keep the escape for decodeEscapes, which runs once the
				// scalar is complete.
				sb.WriteByte(c)
				sb.WriteByte(s[i+1])
				i++
			default:
				sb.WriteByte(c)
			}
		}
		if n+1 == len(segs) {
			break
		}
		if !joined {
			// Trailing white space before a break is not content.
			trimmed := strings.TrimRight(sb.String(), " \t")
			sb.Reset()
			sb.WriteString(trimmed)
		}
		blank := 0
		for n+1 < len(segs) && segs[n+1] == "" {
			blank++
			n++
		}
		switch {
		case blank > 0:
			sb.WriteString(strings.Repeat("\n", blank))
		case !joined:
			sb.WriteByte(' ')
		}
	}
	return "", false
}

// escapes maps YAML double-quoted escape characters to their values.
var escapes = map[byte]string{
	'0': "\x00", 'a': "\a", 'b': "\b", 't': "\t", '\t': "\t", 'n': "\n",
	'v': "\v", 'f': "\f", 'r': "\r", 'e': "\x1b", ' ': " ", '"': `"`,
	'/': "/", '\\': `\`, 'N': "\u0085", '_': "\u00a0", 'L': "\u2028",
	'P': "\u2029",
}

// decodeEscapes decodes the escape sequences of a YAML double-quoted
// scalar. Unknown escapes are kept as written.
func decodeEscapes(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		c := s[i+1]
		if e, ok := escapes[c]; ok {
			b.WriteString(e)
			i++
			continue
		}
		width := 0
		switch c {
		case 'x':
			width = 2
		case 'u':
			width = 4
		case 'U':
			width = 8
		}
		if width > 0 && i+2+width <= len(s) {
			if r, err := strconv.ParseUint(s[i+2:i+2+width], 16, 32); err == nil && utf8.ValidRune(rune(r)) {
				b.WriteRune(rune(r))
				i += 1 + width
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// stripComment removes a YAML comment, a "#" at the start or after
// whitespace, outside quotes.
func stripComment(s string) string {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			switch {
			case c == '\'' && quote == '\'' && i+1 < len(s) && s[i+1] == '\'':
				i++ // '' is an escaped quote
			case c == quote:
				quote = 0
			case c == '\\' && quote == '"':
				i++
			}
		case c == '"' || c == '\'':
			if i == 0 || s[i-1] == ' ' || s[i-1] == '\t' || s[i-1] == '[' || s[i-1] == ',' || s[i-1] == '-' {
				quote = c
			}
		case c == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t'):
			return strings.TrimRight(s[:i], " \t")
		}
	}
	return s
}

// splitList splits s at commas, and also at whitespace when spaces is
// set, outside parentheses, brackets and quotes, so entries such as
// "Bash(git add:*)" stay whole.
func splitList(s string, spaces bool) []string {
	var out []string
	depth, start := 0, 0
	var quote rune
	add := func(end int) {
		if item := unquote(strings.TrimSpace(s[start:end])); item != "" {
			out = append(out, item)
		}
	}
	for i, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			if strings.TrimSpace(s[start:i]) == "" {
				quote = r
			}
		case r == '(' || r == '[' || r == '{':
			depth++
		case (r == ')' || r == ']' || r == '}') && depth > 0:
			depth--
		case depth == 0 && (r == ',' || (spaces && unicode.IsSpace(r))):
			add(i)
			start = i + len(string(r))
		}
	}
	add(len(s))
	return out
}

func flowSeq(raw string) []string {
	return splitList(strings.TrimSuffix(strings.TrimPrefix(raw, "["), "]"), false)
}

func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			inner := s[1 : len(s)-1]
			if s[0] == '\'' {
				return strings.ReplaceAll(inner, "''", "'")
			}
			return decodeEscapes(inner)
		}
	}
	return s
}

// LoadOption configures LoadFS and LoadDir.
type LoadOption func(*loadOptions)

type loadOptions struct{ strict bool }

// Strict validates skills with Metadata.ValidateStrict, the Agent Skills
// specification, instead of Claude Code's rules, and requires each name to
// match its skill's directory, so skills loaded this way also load in
// other Agent Skills clients.
func Strict() LoadOption { return func(o *loadOptions) { o.strict = true } }

// LoadFS loads every skill under root in fsys. A skill is any directory
// containing SKILL.md; nested skills are found at any depth, including
// through symbolic links to directories. base is the directory on disk that
// fsys represents and is reported as the skills' Dir prefix. Leave it empty
// for file systems that are not backed by a directory, such as embedded
// files; their skills then have an empty Dir.
//
// A skill without a name is named after its directory, and one without a
// description is described by the first non-empty line of its body, as in
// Claude Code. Skills that fail to load are reported in the joined error;
// the others are returned.
func LoadFS(fsys fs.FS, root, base string, opts ...LoadOption) ([]Skill, error) {
	var o loadOptions
	for _, opt := range opts {
		opt(&o)
	}
	if root == "" {
		root = "."
	}
	info, err := fs.Stat(fsys, root)
	if err != nil {
		return nil, err
	}
	var skills []Skill
	var errs []error
	found := func(p string) {
		s, err := loadFile(fsys, p, base, o)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
			return
		}
		skills = append(skills, s)
	}
	if !info.IsDir() {
		if path.Base(root) == FileName {
			found(root)
		}
	} else if err := walkSkills(fsys, root, []fs.FileInfo{info}, found); err != nil {
		return nil, err
	}
	slices.SortFunc(skills, func(a, b Skill) int { return strings.Compare(a.Metadata().Name, b.Metadata().Name) })
	return skills, errors.Join(errs...)
}

// maxSkillDepth bounds directory nesting, a backstop for symbolic link
// cycles that os.SameFile cannot detect on the file system.
const maxSkillDepth = 32

// walkSkills calls found for every SKILL.md under dir in lexical order. It
// follows symbolic links to directories; ancestors holds the directories on
// the current path so a link back to one of them is not followed again.
func walkSkills(fsys fs.FS, dir string, ancestors []fs.FileInfo, found func(p string)) error {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := path.Join(dir, e.Name())
		isDir := e.IsDir()
		var info fs.FileInfo
		if e.Type()&fs.ModeSymlink != 0 {
			// fs.Stat follows the link; a broken link is skipped.
			if info, err = fs.Stat(fsys, p); err != nil {
				continue
			}
			isDir = info.IsDir()
		}
		if !isDir {
			if e.Name() == FileName {
				found(p)
			}
			continue
		}
		if info == nil {
			if info, err = e.Info(); err != nil {
				return err
			}
		}
		if len(ancestors) >= maxSkillDepth || slices.ContainsFunc(ancestors, func(a fs.FileInfo) bool { return os.SameFile(a, info) }) {
			continue
		}
		if err := walkSkills(fsys, p, append(ancestors, info), found); err != nil {
			return err
		}
	}
	return nil
}

// LoadDir loads skills from a directory on disk with LoadFS. A missing
// directory yields no skills and no error.
func LoadDir(dir string, opts ...LoadOption) ([]Skill, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return LoadFS(os.DirFS(abs), ".", abs, opts...)
}

func loadFile(fsys fs.FS, p, base string, o loadOptions) (Skill, error) {
	b, err := fs.ReadFile(fsys, p)
	if err != nil {
		return nil, err
	}
	meta, body, err := Parse(string(b))
	if err != nil {
		return nil, err
	}
	dir := path.Dir(p)
	dirName := path.Base(dir)
	if dir == "." && base != "" {
		dirName = filepath.Base(base)
	}
	if meta.Name == "" {
		meta.Name = dirName
	}
	if o.strict {
		if err := meta.ValidateStrict(); err != nil {
			return nil, err
		}
		if meta.Name != dirName {
			return nil, fmt.Errorf("skill %q: name must match its directory %q", meta.Name, dirName)
		}
	} else {
		if strings.TrimSpace(meta.Description) == "" {
			meta.Description = firstLine(body)
		}
		if err := meta.Validate(); err != nil {
			return nil, err
		}
	}
	// Without a base, fsys is not a directory on disk and the path inside
	// it would mislead the model, which cannot read it.
	full := ""
	if base != "" {
		full = filepath.Join(base, filepath.FromSlash(dir))
	}
	return &fsSkill{meta: meta, fsys: fsys, path: p, dir: full}, nil
}
