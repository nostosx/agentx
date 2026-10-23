package rag

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nostosx/agentx/actx"
)

// Splitter cuts documents into chunks.
type Splitter interface {
	Split(ctx context.Context, doc *Document) ([]*Chunk, error)
}

// LengthFunc measures text, usually in tokens.
type LengthFunc func(string) int

// Default chunk sizes in tokens.
const (
	DefaultChunkSize    = 512
	DefaultChunkOverlap = 64
)

// DefaultSeparators are the separators of RecursiveSplitter: paragraphs,
// lines, sentences, words and finally characters.
var DefaultSeparators = []string{"\n\n", "\n", ". ", "。", " ", ""}

// sizing resolves the size settings shared by the splitters.
type sizing struct {
	size, overlap int
	length        LengthFunc
}

func newSizing(size, overlap int, length LengthFunc) (sizing, error) {
	if size <= 0 {
		size = DefaultChunkSize
	}
	switch {
	case overlap == 0:
		overlap = min(DefaultChunkOverlap, size/4)
	case overlap < 0:
		overlap = 0
	}
	if overlap >= size {
		return sizing{}, fmt.Errorf("rag: chunk overlap %d must be smaller than the size %d", overlap, size)
	}
	if length == nil {
		length = actx.EstimateTokens
	}
	return sizing{size: size, overlap: overlap, length: length}, nil
}

// RecursiveSplitter splits text at the first separator that occurs in it,
// merges adjacent pieces into chunks of at most Size and splits pieces that
// are still too large with the next separators, as LangChain's
// RecursiveCharacterTextSplitter does. Consecutive chunks cut from the same
// run of pieces share up to Overlap of text.
//
// Text is cut after a separator, so it stays with the preceding piece,
// except that a separator that starts with white space followed by other
// text, such as "\nfunc ", is cut after the white space so the rest begins
// the next piece. The empty separator splits between characters. Chunks are
// trimmed of surrounding white space and are exact substrings of the
// document, so Chunk.Offset locates them.
type RecursiveSplitter struct {
	// Size is the maximum chunk length measured by Length. Defaults to
	// DefaultChunkSize. A single character longer than Size is kept whole.
	Size int
	// Overlap is the length shared by consecutive chunks. Zero selects
	// DefaultChunkOverlap (at most a quarter of Size); a negative value
	// disables overlap.
	Overlap int
	// Length measures text. Defaults to the token estimate of the context
	// package (four ASCII bytes or one other character per token).
	Length LengthFunc
	// Separators are tried in order. Defaults to DefaultSeparators.
	Separators []string
	// Regexp interprets the separators as regular expressions.
	Regexp bool
}

// Split implements Splitter.
func (s *RecursiveSplitter) Split(_ context.Context, doc *Document) ([]*Chunk, error) {
	spans, err := s.spans(doc.Content)
	if err != nil {
		return nil, err
	}
	return makeChunks(doc, spans), nil
}

func (s *RecursiveSplitter) spans(text string) ([]span, error) {
	sz, err := newSizing(s.Size, s.Overlap, s.Length)
	if err != nil {
		return nil, err
	}
	seps := s.Separators
	if len(seps) == 0 {
		seps = DefaultSeparators
	}
	matchers := make([]*regexp.Regexp, len(seps))
	for i, sep := range seps {
		if !s.Regexp {
			sep = regexp.QuoteMeta(sep)
		}
		re, err := regexp.Compile(sep)
		if err != nil {
			return nil, fmt.Errorf("rag: invalid separator %q: %w", seps[i], err)
		}
		matchers[i] = re
	}
	r := &recursive{sizing: sz, text: text}
	r.split(0, len(text), matchers)
	return r.out, nil
}

// span is a chunk of text before it becomes a Chunk.
type span struct {
	off  int
	text string
	meta map[string]any
	ctx  string
}

// piece is a byte range of the text being split.
type piece struct{ start, end int }

type recursive struct {
	sizing
	text string
	out  []span
}

func (r *recursive) fits(start, end int) bool { return r.length(r.text[start:end]) <= r.size }

// split splits text[start:end] with the first of seps that occurs in it.
func (r *recursive) split(start, end int, seps []*regexp.Regexp) {
	sub := r.text[start:end]
	var sep *regexp.Regexp
	var rest []*regexp.Regexp
	for i, re := range seps {
		if re.String() == "" {
			sep, rest = re, nil
			break
		}
		if re.MatchString(sub) {
			sep, rest = re, seps[i+1:]
			break
		}
	}
	pieces := r.cut(start, end, sep)
	var run []piece
	flush := func() {
		r.merge(run)
		run = nil
	}
	for _, p := range pieces {
		if r.fits(p.start, p.end) {
			run = append(run, p)
			continue
		}
		flush()
		if len(rest) == 0 {
			r.emit(p.start, p.end)
		} else {
			r.split(p.start, p.end, rest)
		}
	}
	flush()
}

// cut divides text[start:end] at the matches of sep into contiguous
// pieces. A nil sep yields the whole range and an empty one single
// characters.
func (r *recursive) cut(start, end int, sep *regexp.Regexp) []piece {
	if sep == nil {
		return []piece{{start, end}}
	}
	var out []piece
	if sep.String() == "" {
		for i := start; i < end; {
			_, n := utf8.DecodeRuneInString(r.text[i:end])
			out = append(out, piece{i, i + n})
			i += n
		}
		return out
	}
	last := start
	for _, m := range sep.FindAllStringIndex(r.text[start:end], -1) {
		at := start + cutPoint(r.text[start+m[0]:start+m[1]]) + m[0]
		if at > last {
			out = append(out, piece{last, at})
			last = at
		}
	}
	if last < end {
		out = append(out, piece{last, end})
	}
	return out
}

// cutPoint returns where to cut within a separator match: after leading
// white space when other text follows it, otherwise after the match.
func cutPoint(m string) int {
	trimmed := strings.TrimLeftFunc(m, unicode.IsSpace)
	if trimmed != "" && len(trimmed) < len(m) {
		return len(m) - len(trimmed)
	}
	return len(m)
}

// merge joins a run of contiguous pieces, each of which fits, into chunks
// that fit, overlapping consecutive chunks by up to r.overlap.
func (r *recursive) merge(run []piece) {
	n := len(run)
	for i := 0; i < n; {
		// The largest j such that run[i..j] fits; run[i] alone always does.
		j := i + sort.Search(n-i, func(k int) bool { return !r.fits(run[i].start, run[i+k].end) }) - 1
		r.emit(run[i].start, run[j].end)
		if j == n-1 {
			return
		}
		next := j + 1
		if r.overlap > 0 {
			// The first piece after i from which the tail up to j fits in
			// the overlap...
			k := i + 1 + sort.Search(j-i, func(x int) bool {
				return r.length(r.text[run[i+1+x].start:run[j].end]) <= r.overlap
			})
			// ...moved on until the next chunk can include run[j+1].
			next = k + sort.Search(j+1-k, func(x int) bool { return r.fits(run[k+x].start, run[j+1].end) })
		}
		i = next
	}
}

func (r *recursive) emit(start, end int) {
	s := r.text[start:end]
	trimmed := strings.TrimLeftFunc(s, unicode.IsSpace)
	off := start + len(s) - len(trimmed)
	trimmed = strings.TrimRightFunc(trimmed, unicode.IsSpace)
	if trimmed == "" {
		return
	}
	r.out = append(r.out, span{off: off, text: trimmed})
}

// makeChunks turns spans of doc into chunks with stable IDs.
func makeChunks(doc *Document, spans []span) []*Chunk {
	source, id := doc.source(), doc.id()
	chunks := make([]*Chunk, 0, len(spans))
	for i, s := range spans {
		meta := maps.Clone(doc.Metadata)
		if len(s.meta) > 0 {
			if meta == nil {
				meta = map[string]any{}
			}
			maps.Copy(meta, s.meta)
		}
		chunks = append(chunks, &Chunk{
			ID:         ChunkID(source, id, s.off),
			DocumentID: id,
			Source:     source,
			Index:      i,
			Offset:     s.off,
			Content:    s.text,
			Context:    s.ctx,
			Metadata:   meta,
		})
	}
	return chunks
}

// CodeSplitter splits source code at declaration boundaries before
// falling back to blank lines, lines and words. See LanguageSeparators.
type CodeSplitter struct {
	// Language selects the separators. When empty, the document's
	// MetaLanguage metadata is used; unknown languages use the separators
	// of RecursiveSplitter.
	Language string
	// Size, Overlap and Length are as in RecursiveSplitter.
	Size, Overlap int
	Length        LengthFunc
}

// Split implements Splitter.
func (s *CodeSplitter) Split(ctx context.Context, doc *Document) ([]*Chunk, error) {
	lang := s.Language
	if lang == "" {
		lang, _ = doc.Metadata[MetaLanguage].(string)
	}
	r := &RecursiveSplitter{Size: s.Size, Overlap: s.Overlap, Length: s.Length}
	if seps := LanguageSeparators(lang); seps != nil {
		r.Separators, r.Regexp = seps, true
	}
	return r.Split(ctx, doc)
}

// Code separators, as regular expressions. A blank line followed by text
// at the start of a line separates top-level declarations together with
// their doc comments in Go, Python and JavaScript formatted as usual.
const (
	sepTopLevel = `\n\n+\S`
	sepBlank    = `\n\n`
	sepLine     = `\n`
	sepWord     = ` `
)

// LanguageSeparators returns the regular expression separators that
// CodeSplitter uses for a language: "go", "python" or "javascript" (also
// "typescript"), or common aliases and extensions such as "py" and "ts".
// It returns nil for other languages.
func LanguageSeparators(lang string) []string {
	switch strings.ToLower(strings.TrimPrefix(lang, ".")) {
	case "go", "golang":
		return []string{sepTopLevel, sepBlank, sepLine, sepWord, ""}
	case "python", "py":
		return []string{sepTopLevel, `\n[ \t]+(?:@[^\n]*\n[ \t]+)*(?:(?:async[ \t]+)?def|class)[ \t]`, sepBlank, sepLine, sepWord, ""}
	case "javascript", "js", "jsx", "mjs", "cjs", "typescript", "ts", "tsx":
		return []string{sepTopLevel, `\n[ \t]*(?:export[ \t]|(?:async[ \t]+)?function\b|class[ \t]|const[ \t]|let[ \t])`, sepBlank, sepLine, sepWord, ""}
	}
	return nil
}

// MarkdownSplitter splits Markdown into sections at ATX headers ("#" to
// "######"), ignoring headers inside fenced code blocks, and splits each
// section that is too large with a RecursiveSplitter. Each chunk records
// the header path above it in MetaHeaders. Sections that hold nothing but
// their header are dropped, since their children carry the header path.
// Setext headers (underlined with "=" or "-") are not recognized.
type MarkdownSplitter struct {
	// Size, Overlap and Length are as in RecursiveSplitter.
	Size, Overlap int
	Length        LengthFunc
	// MaxLevel is the deepest header level that starts a section. Defaults
	// to 6.
	MaxLevel int
	// HeaderContext sets Chunk.Context to the header path, joined with
	// " > ", so it is embedded and searched with each chunk.
	HeaderContext bool
}

// Split implements Splitter.
func (s *MarkdownSplitter) Split(_ context.Context, doc *Document) ([]*Chunk, error) {
	maxLevel := s.MaxLevel
	if maxLevel <= 0 || maxLevel > 6 {
		maxLevel = 6
	}
	inner := &RecursiveSplitter{Size: s.Size, Overlap: s.Overlap, Length: s.Length}
	var spans []span
	for _, sec := range markdownSections(doc.Content, maxLevel) {
		body := doc.Content[sec.start:sec.end]
		if sec.bodyStart >= sec.end || strings.TrimSpace(doc.Content[sec.bodyStart:sec.end]) == "" {
			if len(sec.headers) > 0 {
				continue
			}
		}
		sub, err := inner.spans(body)
		if err != nil {
			return nil, err
		}
		for _, sp := range sub {
			sp.off += sec.start
			if len(sec.headers) > 0 {
				sp.meta = map[string]any{MetaHeaders: slices.Clone(sec.headers)}
				if s.HeaderContext {
					sp.ctx = strings.Join(sec.headers, " > ")
				}
			}
			spans = append(spans, sp)
		}
	}
	return makeChunks(doc, spans), nil
}

type mdSection struct {
	start, bodyStart, end int
	headers               []string
}

type mdHeader struct {
	level int
	text  string
}

// markdownSections returns the sections of text. A section starts at a
// header of at most maxLevel and its body after the header line.
func markdownSections(text string, maxLevel int) []mdSection {
	var secs []mdSection
	var stack []mdHeader
	cur := mdSection{}
	fence := ""
	for off := 0; off < len(text); {
		end := strings.IndexByte(text[off:], '\n')
		next := len(text)
		if end >= 0 {
			next = off + end + 1
		}
		line := strings.TrimRight(text[off:next], "\r\n")
		indent := len(line) - len(strings.TrimLeft(line, " "))
		trimmed := strings.TrimLeft(line, " ")
		switch {
		case indent > 3:
		case fence != "":
			if strings.HasPrefix(trimmed, fence) && strings.Trim(trimmed, fence[:1]) == "" {
				fence = ""
			}
		case strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~"):
			n := len(trimmed) - len(strings.TrimLeft(trimmed, trimmed[:1]))
			fence = strings.Repeat(trimmed[:1], n)
		default:
			if level, title, ok := atxHeader(trimmed); ok && level <= maxLevel {
				if off > cur.start {
					cur.end = off
					secs = append(secs, cur)
				}
				for len(stack) > 0 && stack[len(stack)-1].level >= level {
					stack = stack[:len(stack)-1]
				}
				stack = append(stack, mdHeader{level, title})
				headers := make([]string, len(stack))
				for i, h := range stack {
					headers[i] = h.text
				}
				cur = mdSection{start: off, bodyStart: next, headers: headers}
			}
		}
		off = next
	}
	if len(text) > cur.start {
		cur.end = len(text)
		secs = append(secs, cur)
	}
	return secs
}

// atxHeader parses an ATX header line without its indentation.
func atxHeader(line string) (int, string, bool) {
	level := len(line) - len(strings.TrimLeft(line, "#"))
	if level == 0 || level > 6 {
		return 0, "", false
	}
	rest := line[level:]
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return 0, "", false
	}
	rest = strings.TrimSpace(rest)
	// An optional closing sequence of "#" must follow a space.
	if t := strings.TrimRight(rest, "#"); t != rest && (t == "" || strings.HasSuffix(t, " ") || strings.HasSuffix(t, "\t")) {
		rest = strings.TrimSpace(t)
	}
	return level, rest, true
}

// AutoSplitter chooses a splitter for each document from its metadata:
// MarkdownSplitter with HeaderContext for MetaFormat "markdown",
// CodeSplitter for documents with a MetaLanguage and RecursiveSplitter
// otherwise.
type AutoSplitter struct {
	// Size, Overlap and Length are as in RecursiveSplitter.
	Size, Overlap int
	Length        LengthFunc
}

// Split implements Splitter.
func (s *AutoSplitter) Split(ctx context.Context, doc *Document) ([]*Chunk, error) {
	if f, _ := doc.Metadata[MetaFormat].(string); f == "markdown" {
		return (&MarkdownSplitter{Size: s.Size, Overlap: s.Overlap, Length: s.Length, HeaderContext: true}).Split(ctx, doc)
	}
	if lang, _ := doc.Metadata[MetaLanguage].(string); lang != "" {
		return (&CodeSplitter{Language: lang, Size: s.Size, Overlap: s.Overlap, Length: s.Length}).Split(ctx, doc)
	}
	return (&RecursiveSplitter{Size: s.Size, Overlap: s.Overlap, Length: s.Length}).Split(ctx, doc)
}
