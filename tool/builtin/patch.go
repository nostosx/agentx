package builtin

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// This file parses and applies the patch format of OpenAI's Codex
// (codex-rs/apply-patch), which GPT models are trained to write. The parser
// follows Codex's lenient parser rather than only its Lark grammar
// (applyPatchGrammar): markers may carry surrounding whitespace, a bare
// empty line in an update is an empty context line, and a patch wrapped in
// a heredoc (<<EOF ... EOF) is unwrapped. The optional Environment ID line
// of Codex's multi-environment mode is not supported.

const (
	beginPatchMarker   = "*** Begin Patch"
	endPatchMarker     = "*** End Patch"
	addFileMarker      = "*** Add File: "
	deleteFileMarker   = "*** Delete File: "
	updateFileMarker   = "*** Update File: "
	moveToMarker       = "*** Move to: "
	eofMarker          = "*** End of File"
	contextMarker      = "@@ "
	emptyContextMarker = "@@"
)

// applyPatchGrammar is Codex's Lark grammar for the patch format
// (codex-rs/core/assets/tools/apply_patch.lark), which constrains the input
// of the freeform apply_patch tool.
const applyPatchGrammar = `start: begin_patch hunk+ end_patch
begin_patch: "*** Begin Patch" LF
end_patch: "*** End Patch" LF?

hunk: add_hunk | delete_hunk | update_hunk
add_hunk: "*** Add File: " filename LF add_line+
delete_hunk: "*** Delete File: " filename LF
update_hunk: "*** Update File: " filename LF change_move? change?

filename: /(.+)/
add_line: "+" /(.*)/ LF -> line

change_move: "*** Move to: " filename LF
change: (change_context | change_line)+ eof_line?
change_context: ("@@" | "@@ " /(.+)/) LF
change_line: ("+" | "-" | " ") /(.*)/ LF
eof_line: "*** End of File" LF

%import common.LF
`

type hunkKind int8

const (
	addHunk hunkKind = iota
	deleteHunk
	updateHunk
)

// patchHunk is one file operation of a patch.
type patchHunk struct {
	kind hunkKind
	path string
	// movePath is the new path of an updated file, or "".
	movePath string
	// contents is the content of an added file.
	contents string
	// chunks are the changes of an updated file, in file order.
	chunks []patchChunk
}

// patchChunk is one change of an updated file.
type patchChunk struct {
	// context is a line, usually a declaration, that occurs before the
	// change and narrows down where it is, if hasContext is set.
	context    string
	hasContext bool
	// old are the lines to replace and new their replacement; context
	// lines occur in both.
	old, new []string
	// contextLines pairs the indexes in old and new of the lines given as
	// context, as opposed to removed and added lines that happen to be
	// equal.
	contextLines [][2]int
	// eof requires old to match at the end of the file.
	eof bool
}

func (c *patchChunk) pushContext(line string) {
	c.contextLines = append(c.contextLines, [2]int{len(c.old), len(c.new)})
	c.old = append(c.old, line)
	c.new = append(c.new, line)
}

func (c *patchChunk) empty() bool { return len(c.old) == 0 && len(c.new) == 0 }

func patchError(msg string) error { return fmt.Errorf("invalid patch: %s", msg) }

func hunkError(line int, format string, args ...any) error {
	return fmt.Errorf("invalid patch hunk on line %d: %s", line, fmt.Sprintf(format, args...))
}

const (
	errFirstLine = "The first line of the patch must be '*** Begin Patch'"
	errLastLine  = "The last line of the patch must be '*** End Patch'"
)

// parsePatch parses a patch into its hunks. It checks the syntax only.
func parsePatch(text string) ([]patchHunk, error) {
	lines, err := patchBoundaries(splitPatchLines(strings.TrimSpace(text)))
	if err != nil {
		return nil, err
	}
	p := &patchParser{}
	for i, line := range lines {
		p.line = i + 1
		if i < len(lines)-1 {
			// Codex splits the lines twice, removing a carriage return
			// before each newline both times.
			if err := p.process(strings.TrimSuffix(line, "\r")); err != nil {
				return nil, err
			}
			continue
		}
		// The last line, which ends the patch, is compared without the
		// surrounding whitespace.
		if trimmed := strings.TrimSpace(line); trimmed == endPatchMarker {
			if err := p.ensureUpdateNotEmpty(trimmed); err != nil {
				return nil, err
			}
			p.mode = modeEnded
		} else if err := p.process(line); err != nil {
			return nil, err
		}
	}
	if p.mode != modeEnded {
		return nil, patchError(errLastLine)
	}
	return p.hunks, nil
}

// splitPatchLines splits text at newlines, removing a carriage return before
// a newline, like Rust's str::lines.
func splitPatchLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	for i := range lines[:len(lines)-1] {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	return lines
}

// patchBoundaries checks the first and last lines of a patch, unwrapping a
// patch inside a heredoc, as models write it for a shell.
func patchBoundaries(lines []string) ([]string, error) {
	err := checkBoundaries(lines)
	if err == nil {
		return lines, nil
	}
	if len(lines) >= 4 {
		first, last := lines[0], lines[len(lines)-1]
		if (first == "<<EOF" || first == "<<'EOF'" || first == `<<"EOF"`) && strings.HasSuffix(last, "EOF") {
			inner := lines[1 : len(lines)-1]
			return inner, checkBoundaries(inner)
		}
	}
	return nil, err
}

func checkBoundaries(lines []string) error {
	if len(lines) == 0 {
		return patchError(errLastLine)
	}
	first, last := strings.TrimSpace(lines[0]), strings.TrimSpace(lines[len(lines)-1])
	switch {
	case first != beginPatchMarker:
		return patchError(errFirstLine)
	case last != endPatchMarker:
		return patchError(errLastLine)
	}
	return nil
}

type parseMode int8

const (
	modeNotStarted parseMode = iota
	modeStarted
	modeAdd
	modeDelete
	modeUpdate
	modeEnded
)

// patchParser is a port of Codex's StreamingPatchParser, applied to whole
// lines.
type patchParser struct {
	mode  parseMode
	hunks []patchHunk
	// line is the number of the current line and hunkLine that of the
	// current update header.
	line, hunkLine int
}

func (p *patchParser) last() *patchHunk {
	if len(p.hunks) == 0 {
		return nil
	}
	return &p.hunks[len(p.hunks)-1]
}

// ensureUpdateNotEmpty fails if the update hunk that line ends has no
// changes or ends with an empty one.
func (p *patchParser) ensureUpdateNotEmpty(line string) error {
	h := p.last()
	if h == nil || h.kind != updateHunk {
		return nil
	}
	if len(h.chunks) == 0 && p.mode == modeUpdate {
		return hunkError(p.hunkLine, "Update file hunk for path '%s' is empty", h.path)
	}
	if len(h.chunks) > 0 && h.chunks[len(h.chunks)-1].empty() {
		if line == endPatchMarker {
			return hunkError(p.line, "Update hunk does not contain any lines")
		}
		return unexpectedLine(p.line, line)
	}
	return nil
}

func unexpectedLine(n int, line string) error {
	return hunkError(n, "Unexpected line found in update hunk: '%s'. Every line should start with ' ' (context line), '+' (added line), or '-' (removed line)", line)
}

func invalidHeader(n int, line string) error {
	return hunkError(n, "'%s' is not a valid hunk header. Valid hunk headers: '*** Add File: {path}', '*** Delete File: {path}', '*** Update File: {path}'", line)
}

// header handles a hunk header or the end of the patch, reporting whether
// line is one.
func (p *patchParser) header(line string) (bool, error) {
	var h patchHunk
	switch {
	case line == endPatchMarker:
		if err := p.ensureUpdateNotEmpty(line); err != nil {
			return false, err
		}
		p.mode = modeEnded
		return true, nil
	case strings.HasPrefix(line, addFileMarker):
		h = patchHunk{kind: addHunk, path: line[len(addFileMarker):]}
	case strings.HasPrefix(line, deleteFileMarker):
		h = patchHunk{kind: deleteHunk, path: line[len(deleteFileMarker):]}
	case strings.HasPrefix(line, updateFileMarker):
		h = patchHunk{kind: updateHunk, path: line[len(updateFileMarker):]}
	default:
		return false, nil
	}
	if err := p.ensureUpdateNotEmpty(line); err != nil {
		return false, err
	}
	p.hunks = append(p.hunks, h)
	switch h.kind {
	case addHunk:
		p.mode = modeAdd
	case deleteHunk:
		p.mode = modeDelete
	case updateHunk:
		p.mode, p.hunkLine = modeUpdate, p.line
	}
	return true, nil
}

func (p *patchParser) process(line string) error {
	trimmed := strings.TrimSpace(line)
	switch p.mode {
	case modeNotStarted:
		if trimmed != beginPatchMarker {
			return patchError(errFirstLine)
		}
		p.mode = modeStarted
		return nil
	case modeStarted, modeDelete:
		if ok, err := p.header(trimmed); ok || err != nil {
			return err
		}
		return invalidHeader(p.line, trimmed)
	case modeAdd:
		if ok, err := p.header(trimmed); ok || err != nil {
			return err
		}
		if rest, ok := strings.CutPrefix(line, "+"); ok {
			p.last().contents += rest + "\n"
			return nil
		}
		return invalidHeader(p.line, trimmed)
	case modeUpdate:
		return p.update(line)
	}
	if trimmed != "" {
		return patchError(errLastLine)
	}
	return nil
}

// update handles a line of an update hunk.
func (p *patchParser) update(line string) error {
	u := strings.TrimRightFunc(line, unicode.IsSpace)
	if ok, err := p.header(u); ok || err != nil {
		return err
	}
	h := p.last()
	var chunk *patchChunk
	if n := len(h.chunks); n > 0 {
		chunk = &h.chunks[n-1]
	}
	isContext := u == emptyContextMarker || strings.HasPrefix(u, contextMarker)
	if chunk != nil && chunk.eof {
		// Only empty lines or a new change may follow the end of the file.
		if u == "" {
			return nil
		}
		if !isContext {
			return hunkError(p.line, "Expected update hunk to start with a @@ context marker, got: '%s'", line)
		}
	}
	if chunk == nil && h.movePath == "" {
		if dest, ok := strings.CutPrefix(u, moveToMarker); ok {
			h.movePath = dest
			return nil
		}
	}
	if isContext && chunk != nil && chunk.empty() {
		return unexpectedLine(p.line, line)
	}
	if u == emptyContextMarker {
		h.chunks = append(h.chunks, patchChunk{})
		return nil
	}
	if ctx, ok := strings.CutPrefix(u, contextMarker); ok {
		h.chunks = append(h.chunks, patchChunk{context: ctx, hasContext: true})
		return nil
	}
	if u == eofMarker {
		if chunk != nil && chunk.empty() {
			return hunkError(p.line, "Update hunk does not contain any lines")
		}
		if chunk != nil {
			chunk.eof = true
		}
		return nil
	}
	// A change line starts the first change if no @@ did.
	current := func() *patchChunk {
		if len(h.chunks) == 0 {
			h.chunks = append(h.chunks, patchChunk{})
		}
		return &h.chunks[len(h.chunks)-1]
	}
	if line == "" {
		current().pushContext("")
		return nil
	}
	switch line[0] {
	case ' ':
		current().pushContext(line[1:])
		return nil
	case '+':
		c := current()
		c.new = append(c.new, line[1:])
		return nil
	case '-':
		c := current()
		c.old = append(c.old, line[1:])
		return nil
	}
	if chunk != nil && !chunk.empty() {
		return hunkError(p.line, "Expected update hunk to start with a @@ context marker, got: '%s'", line)
	}
	return unexpectedLine(p.line, line)
}

// seekSequence returns the index of the first occurrence of pattern in
// lines at or after start, or -1. Like Codex it tries ever looser
// comparisons: exact, ignoring trailing whitespace, ignoring surrounding
// whitespace, and finally also treating typographic dashes, quotes and
// spaces as their ASCII forms. With eof, the search starts where pattern
// would end the file.
func seekSequence(lines, pattern []string, start int, eof bool) int {
	if len(pattern) == 0 {
		return start
	}
	if len(pattern) > len(lines) {
		return -1
	}
	from := start
	if eof {
		from = max(len(lines)-len(pattern), start)
	}
	for _, norm := range []func(string) string{
		nil,
		func(s string) string { return strings.TrimRightFunc(s, unicode.IsSpace) },
		strings.TrimSpace,
		normalizePunctuation,
	} {
		for i := from; i <= len(lines)-len(pattern); i++ {
			if matchAt(lines[i:i+len(pattern)], pattern, norm) {
				return i
			}
		}
	}
	return -1
}

func matchAt(lines, pattern []string, norm func(string) string) bool {
	for i, p := range pattern {
		if norm == nil {
			if lines[i] != p {
				return false
			}
		} else if norm(lines[i]) != norm(p) {
			return false
		}
	}
	return true
}

// normalizePunctuation trims s and replaces typographic dashes, quotes and
// spaces with their ASCII forms.
func normalizePunctuation(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\u2010', '\u2011', '\u2012', '\u2013', '\u2014', '\u2015', '\u2212':
			return '-'
		case '\u2018', '\u2019', '\u201A', '\u201B':
			return '\''
		case '\u201C', '\u201D', '\u201E', '\u201F':
			return '"'
		case '\u00A0', '\u2002', '\u2003', '\u2004', '\u2005', '\u2006', '\u2007',
			'\u2008', '\u2009', '\u200A', '\u202F', '\u205F', '\u3000':
			return ' '
		}
		return r
	}, strings.TrimSpace(s))
}

// replacement replaces n lines at start with lines.
type replacement struct {
	start, n int
	lines    []string
}

// computeReplacements locates the chunks of an update in lines, the lines
// of the file without their endings, in order.
func computeReplacements(lines []string, path string, chunks []patchChunk) ([]replacement, error) {
	var out []replacement
	index := 0
	for _, c := range chunks {
		if c.hasContext {
			i := seekSequence(lines, []string{c.context}, index, false)
			if i < 0 {
				return nil, fmt.Errorf("failed to find context '%s' in %s", c.context, path)
			}
			index = i + 1
		}
		// Like Codex, a change of added lines only is appended to the
		// file.
		if len(c.old) == 0 {
			out = append(out, replacement{start: len(lines), lines: c.new})
			continue
		}
		pattern, newLines := c.old, c.new
		found := seekSequence(lines, pattern, index, c.eof)
		if found < 0 && pattern[len(pattern)-1] == "" {
			// A trailing empty line often stands for the newline that ends
			// the file, which is not a line of its own.
			pattern = pattern[:len(pattern)-1]
			if len(newLines) > 0 && newLines[len(newLines)-1] == "" {
				newLines = newLines[:len(newLines)-1]
			}
			found = seekSequence(lines, pattern, index, c.eof)
		}
		if found < 0 {
			return nil, fmt.Errorf("failed to find expected lines in %s:\n%s", path, strings.Join(c.old, "\n"))
		}
		// Context lines keep the file's text, which may differ from the
		// patch in whitespace, and their line endings.
		oldStart, newStart := 0, 0
		for _, cl := range c.contextLines {
			if cl[0] >= len(pattern) || cl[1] >= len(newLines) {
				break
			}
			if oldStart != cl[0] || newStart != cl[1] {
				out = append(out, replacement{start: found + oldStart, n: cl[0] - oldStart, lines: newLines[newStart:cl[1]]})
			}
			oldStart, newStart = cl[0]+1, cl[1]+1
		}
		if oldStart != len(pattern) || newStart != len(newLines) {
			out = append(out, replacement{start: found + oldStart, n: len(pattern) - oldStart, lines: newLines[newStart:]})
		}
		index = found + len(pattern)
	}
	slices.SortStableFunc(out, func(a, b replacement) int { return a.start - b.start })
	return out, nil
}

// sourceLine is a line of a file and its ending: "\n", "\r\n", "\r" or ""
// for a last line without one.
type sourceLine struct {
	text, ending string
}

// sourceFile is a file split into lines that keep their endings.
type sourceFile struct {
	lines []sourceLine
	// ending is the first line ending of the file, used for new lines.
	ending string
}

func parseSource(s string) *sourceFile {
	f := &sourceFile{}
	start := 0
	for i := 0; i < len(s); i++ {
		var ending string
		switch {
		case s[i] == '\r' && i+1 < len(s) && s[i+1] == '\n':
			ending = "\r\n"
		case s[i] == '\r':
			ending = "\r"
		case s[i] == '\n':
			ending = "\n"
		default:
			continue
		}
		if f.ending == "" {
			f.ending = ending
		}
		f.lines = append(f.lines, sourceLine{text: s[start:i], ending: ending})
		i += len(ending) - 1
		start = i + 1
	}
	if start < len(s) {
		f.lines = append(f.lines, sourceLine{text: s[start:]})
	}
	if f.ending == "" {
		f.ending = "\n"
	}
	return f
}

func (f *sourceFile) texts() []string {
	out := make([]string, len(f.lines))
	for i, l := range f.lines {
		out[i] = l.text
	}
	return out
}

// apply applies replacements sorted by start. Unchanged lines keep their
// endings and new lines get the file's; like Codex, every line ends with a
// newline afterwards.
func (f *sourceFile) apply(reps []replacement, path string) error {
	var out []sourceLine
	next := 0
	for _, r := range reps {
		if r.start < next {
			return fmt.Errorf("changes to %s overlap", path)
		}
		out = append(out, f.lines[next:r.start]...)
		for _, l := range r.lines {
			out = append(out, sourceLine{text: l, ending: f.ending})
		}
		next = r.start + r.n
	}
	out = append(out, f.lines[next:]...)
	for i := range out {
		if out[i].ending == "" {
			out[i].ending = f.ending
		}
	}
	f.lines = out
	return nil
}

func (f *sourceFile) String() string {
	var b strings.Builder
	for _, l := range f.lines {
		b.WriteString(l.text)
		b.WriteString(l.ending)
	}
	return b.String()
}

// applyChunks returns the content of a file after an update.
func applyChunks(content, path string, chunks []patchChunk) (string, error) {
	f := parseSource(content)
	reps, err := computeReplacements(f.texts(), path, chunks)
	if err != nil {
		return "", err
	}
	if err := f.apply(reps, path); err != nil {
		return "", err
	}
	return f.String(), nil
}
