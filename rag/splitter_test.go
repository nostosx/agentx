package rag

import (
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/nostosx/agentx/actx"
)

func runes(s string) int { return utf8.RuneCountInString(s) }

func contents(chunks []*Chunk) []string {
	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = c.Content
	}
	return out
}

// checkChunks verifies the invariants every splitter guarantees: chunks
// are trimmed substrings at their offsets, offsets increase, IDs are
// stable and unique, and every non-space character is covered, except on
// the lines MarkdownSplitter drops.
func checkChunks(t *testing.T, doc *Document, chunks []*Chunk, size int, length LengthFunc) {
	t.Helper()
	covered := make([]bool, len(doc.Content))
	ids := map[string]bool{}
	last := -1
	for i, c := range chunks {
		if c.Index != i {
			t.Errorf("chunk %d has index %d", i, c.Index)
		}
		if c.Offset <= last {
			t.Errorf("chunk %d offset %d not after %d", i, c.Offset, last)
		}
		last = c.Offset
		if got := doc.Content[c.Offset : c.Offset+len(c.Content)]; got != c.Content {
			t.Fatalf("chunk %d is not the substring at %d: %q vs %q", i, c.Offset, c.Content, got)
		}
		if c.Content != strings.TrimSpace(c.Content) || c.Content == "" {
			t.Errorf("chunk %d not trimmed: %q", i, c.Content)
		}
		if n := length(c.Content); n > size && runes(c.Content) > 1 {
			t.Errorf("chunk %d has length %d > %d: %q", i, n, size, c.Content)
		}
		if c.ID != ChunkID(doc.source(), doc.id(), c.Offset) || ids[c.ID] {
			t.Errorf("chunk %d has unexpected or duplicate ID %s", i, c.ID)
		}
		ids[c.ID] = true
		if c.DocumentID != doc.id() || c.Source != doc.source() {
			t.Errorf("chunk %d document = %q, source = %q", i, c.DocumentID, c.Source)
		}
		for j := c.Offset; j < c.Offset+len(c.Content); j++ {
			covered[j] = true
		}
	}
	for i, r := range doc.Content {
		if !unicode.IsSpace(r) && !covered[i] && !strings.HasPrefix(doc.Content[strings.LastIndexByte(doc.Content[:i], '\n')+1:], "# Guide\n") {
			t.Fatalf("byte %d (%q) is not in any chunk", i, r)
		}
	}
}

func TestRecursiveSplitterInvariants(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	words := []string{"alpha", "beta", "gamma", "delta", "检索", "增强生成", "x", "supercalifragilistic", "a.", "b?"}
	seps := []string{" ", " ", " ", "\n", "\n\n", ". ", "。", ""}
	for iter := range 300 {
		var b strings.Builder
		for range rng.IntN(400) {
			b.WriteString(words[rng.IntN(len(words))])
			b.WriteString(seps[rng.IntN(len(seps))])
		}
		size := 1 + rng.IntN(60)
		overlap := rng.IntN(size+1) - 1
		if overlap >= size {
			overlap = size - 1
		}
		if overlap == 0 {
			overlap = -1
		}
		lengths := []LengthFunc{nil, runes}
		length := lengths[iter%2]
		s := &RecursiveSplitter{Size: size, Overlap: overlap, Length: length}
		doc := &Document{ID: "doc", Source: "src", Content: b.String()}
		chunks, err := s.Split(t.Context(), doc)
		if err != nil {
			t.Fatal(err)
		}
		if length == nil {
			length = actx.EstimateTokens
		}
		checkChunks(t, doc, chunks, size, length)
		again, err := s.Split(t.Context(), doc)
		if err != nil || !reflect.DeepEqual(again, chunks) {
			t.Fatalf("splitting is not deterministic: %v", err)
		}
	}
}

func TestRecursiveSplitterParagraphsAndOverlap(t *testing.T) {
	text := "First sentence here. Second sentence here. Third sentence here.\n\nNext paragraph is short."
	s := &RecursiveSplitter{Size: 45, Overlap: 22, Length: runes}
	doc := &Document{ID: "d", Content: text}
	chunks, err := s.Split(t.Context(), doc)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range chunks {
		got = append(got, c.Content)
	}
	want := []string{
		"First sentence here. Second sentence here.",
		"Second sentence here. Third sentence here.",
		"Next paragraph is short.",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("chunks = %q, want %q", got, want)
	}
	checkChunks(t, doc, chunks, 45, runes)
	if chunks[0].Source != "d" || chunks[0].DocumentID != "d" {
		t.Fatalf("source defaults: %+v", chunks[0])
	}

	// Without overlap the sentences are packed greedily.
	chunks, _ = (&RecursiveSplitter{Size: 45, Overlap: -1, Length: runes}).Split(t.Context(), doc)
	got = got[:0]
	for _, c := range chunks {
		got = append(got, c.Content)
	}
	want = []string{"First sentence here. Second sentence here.", "Third sentence here.", "Next paragraph is short."}
	if !slices.Equal(got, want) {
		t.Fatalf("no overlap: %q", got)
	}
}

func TestRecursiveSplitterDefaultsAndErrors(t *testing.T) {
	doc := &Document{ID: "d", Content: strings.Repeat("word ", 1000)}
	chunks, err := (&RecursiveSplitter{}).Split(t.Context(), doc)
	if err != nil {
		t.Fatal(err)
	}
	checkChunks(t, doc, chunks, DefaultChunkSize, actx.EstimateTokens)
	if len(chunks) < 3 {
		t.Fatalf("got %d chunks", len(chunks))
	}
	// Consecutive chunks overlap by the default overlap.
	if chunks[1].Offset >= chunks[0].Offset+len(chunks[0].Content) {
		t.Fatal("default overlap not applied")
	}
	if _, err := (&RecursiveSplitter{Size: 10, Overlap: 10}).Split(t.Context(), doc); err == nil {
		t.Fatal("overlap equal to size accepted")
	}
	if _, err := (&RecursiveSplitter{Separators: []string{"("}, Regexp: true}).Split(t.Context(), doc); err == nil {
		t.Fatal("invalid regexp accepted")
	}
	if chunks, err := (&RecursiveSplitter{}).Split(t.Context(), &Document{ID: "e", Content: " \n "}); err != nil || len(chunks) != 0 {
		t.Fatalf("blank document = %v, %v", chunks, err)
	}
}

func TestRecursiveSplitterCJKAndLongWords(t *testing.T) {
	text := strings.Repeat("检索增强生成把外部知识引入模型", 20)
	doc := &Document{ID: "cjk", Content: text}
	chunks, err := (&RecursiveSplitter{Size: 30, Overlap: 5}).Split(t.Context(), doc)
	if err != nil {
		t.Fatal(err)
	}
	checkChunks(t, doc, chunks, 30, actx.EstimateTokens)
	if len(chunks) < 10 {
		t.Fatalf("got %d chunks", len(chunks))
	}
	long := &Document{ID: "w", Content: strings.Repeat("x", 500)}
	chunks, _ = (&RecursiveSplitter{Size: 20, Overlap: -1, Length: runes}).Split(t.Context(), long)
	if len(chunks) != 25 {
		t.Fatalf("long word: %d chunks", len(chunks))
	}
}

func TestCutPoint(t *testing.T) {
	cases := map[string]int{"\n\n": 2, ". ": 2, "\nfunc ": 1, "\n\n\tx": 3, "。": 3}
	for m, want := range cases {
		if got := cutPoint(m); got != want {
			t.Errorf("cutPoint(%q) = %d, want %d", m, got, want)
		}
	}
}

func TestChunkIDStable(t *testing.T) {
	a, b := ChunkID("s", "d", 1), ChunkID("s", "d", 1)
	if a != b || !strings.HasPrefix(a, "c_") || len(a) != 18 {
		t.Fatalf("ChunkID = %q, %q", a, b)
	}
	for _, other := range []string{ChunkID("s", "d", 2), ChunkID("s2", "d", 1), ChunkID("s", "d2", 1), ChunkID("sd", "", 1)} {
		if other == a {
			t.Fatal("distinct chunks share an ID")
		}
	}
}

const goSource = `package demo

import "fmt"

// Hello greets someone.
// It prints a line.
func Hello(name string) {
	fmt.Println("hello", name)

	fmt.Println("bye")
}

// World is a type.
type World struct {
	Name string
}

func (w World) Spin() int {
	return 42
}
`

func TestCodeSplitterGo(t *testing.T) {
	doc := &Document{ID: "demo.go", Content: goSource, Metadata: map[string]any{MetaLanguage: "go"}}
	chunks, err := (&CodeSplitter{Size: 30, Overlap: -1, Length: runes}).Split(t.Context(), doc)
	if err != nil {
		t.Fatal(err)
	}
	// Small limits split inside functions, but larger ones keep each
	// declaration with its doc comment.
	checkChunks(t, doc, chunks, 30, runes)
	chunks, _ = (&CodeSplitter{Language: "go", Size: 32, Overlap: -1}).Split(t.Context(), doc)
	checkChunks(t, doc, chunks, 32, actx.EstimateTokens)
	var starts []string
	for _, c := range chunks {
		starts = append(starts, strings.SplitN(c.Content, "\n", 2)[0])
	}
	want := []string{`package demo`, `// Hello greets someone.`, `// World is a type.`}
	if !slices.Equal(starts, want) {
		t.Fatalf("chunk starts = %q, want %q", starts, want)
	}
	if !strings.Contains(chunks[2].Content, "func (w World) Spin()") {
		t.Fatalf("last chunk = %q", chunks[2].Content)
	}
}

func TestCodeSplitterPythonAndJS(t *testing.T) {
	py := "class A:\n    x = 1\n\n    @property\n    def b(self):\n        return 1\n\n    async def c(self):\n        return 2\n"
	doc := &Document{ID: "a.py", Content: py}
	chunks, err := (&CodeSplitter{Language: "py", Size: 50, Overlap: -1, Length: runes}).Split(t.Context(), doc)
	if err != nil {
		t.Fatal(err)
	}
	checkChunks(t, doc, chunks, 50, runes)
	var got []string
	for _, c := range chunks {
		got = append(got, c.Content)
	}
	want := []string{"class A:\n    x = 1", "@property\n    def b(self):\n        return 1", "async def c(self):\n        return 2"}
	if !slices.Equal(got, want) {
		t.Fatalf("python chunks = %q", got)
	}

	js := "import x from 'x';\nexport function a() {\n  return 1;\n}\nconst b = () => 2;\nclass C {}\n"
	doc = &Document{ID: "a.ts", Content: js}
	chunks, _ = (&CodeSplitter{Language: "typescript", Size: 40, Overlap: -1, Length: runes}).Split(t.Context(), doc)
	checkChunks(t, doc, chunks, 40, runes)
	if len(chunks) < 2 || !strings.HasPrefix(chunks[1].Content, "export function a()") && !strings.HasPrefix(chunks[1].Content, "const b") {
		t.Fatalf("js chunks = %q", contents(chunks))
	}
	for _, lang := range []string{"go", "golang", ".py", "JS", "tsx"} {
		if LanguageSeparators(lang) == nil {
			t.Errorf("no separators for %q", lang)
		}
	}
	if LanguageSeparators("cobol") != nil {
		t.Error("separators for unknown language")
	}
	// Unknown languages fall back to the default separators.
	chunks, err = (&CodeSplitter{Language: "cobol", Size: 5, Overlap: -1}).Split(t.Context(), &Document{ID: "x", Content: "MOVE A TO B.\n\nDISPLAY B."})
	if err != nil || len(chunks) != 2 {
		t.Fatalf("fallback: %v, %v", chunks, err)
	}
}

const markdownDoc = `Intro text before any header.

# Guide

## Install

Run the installer.

` + "```sh\n# not a header\nmake install\n```" + `

### Linux

Use the package manager.

## Usage ##

Call the API.

#NotAHeader stays in Usage.
`

func TestMarkdownSplitter(t *testing.T) {
	doc := &Document{ID: "guide.md", Content: markdownDoc, Metadata: map[string]any{MetaFormat: "markdown"}}
	chunks, err := (&MarkdownSplitter{HeaderContext: true}).Split(t.Context(), doc)
	if err != nil {
		t.Fatal(err)
	}
	checkChunks(t, doc, chunks, DefaultChunkSize, actx.EstimateTokens)
	type want struct {
		headers []string
		prefix  string
	}
	wants := []want{
		{nil, "Intro text"},
		{[]string{"Guide", "Install"}, "## Install"},
		{[]string{"Guide", "Install", "Linux"}, "### Linux"},
		{[]string{"Guide", "Usage"}, "## Usage ##"},
	}
	if len(chunks) != len(wants) {
		t.Fatalf("got %d chunks: %q", len(chunks), contents(chunks))
	}
	for i, w := range wants {
		c := chunks[i]
		hs, _ := c.Metadata[MetaHeaders].([]string)
		if !slices.Equal(hs, w.headers) || !strings.HasPrefix(c.Content, w.prefix) {
			t.Errorf("chunk %d: headers %q content %q", i, hs, c.Content)
		}
		if c.Metadata[MetaFormat] != "markdown" {
			t.Errorf("chunk %d lost document metadata", i)
		}
		if wantCtx := strings.Join(w.headers, " > "); c.Context != wantCtx {
			t.Errorf("chunk %d context %q, want %q", i, c.Context, wantCtx)
		}
	}
	if !strings.Contains(chunks[1].Content, "# not a header") || !strings.Contains(chunks[3].Content, "#NotAHeader") {
		t.Fatal("fenced or invalid headers started sections")
	}
	if got := chunks[2].Text(); got != "Guide > Install > Linux\n\n### Linux\n\nUse the package manager." {
		t.Fatalf("Text = %q", got)
	}

	// MaxLevel keeps deeper headers inside their parent section, and a
	// large section is split further, every piece keeping the path.
	chunks, _ = (&MarkdownSplitter{MaxLevel: 2, Size: 8, Overlap: -1}).Split(t.Context(), doc)
	checkChunks(t, doc, chunks, 8, actx.EstimateTokens)
	for _, c := range chunks {
		hs, _ := c.Metadata[MetaHeaders].([]string)
		if strings.Contains(c.Content, "Linux") && !slices.Equal(hs, []string{"Guide", "Install"}) {
			t.Errorf("chunk %q has headers %q", c.Content, hs)
		}
		if c.Context != "" {
			t.Error("context set without HeaderContext")
		}
	}
}

func TestAtxHeader(t *testing.T) {
	cases := []struct {
		line  string
		level int
		text  string
		ok    bool
	}{
		{"# A", 1, "A", true},
		{"###### F #", 6, "F", true},
		{"####### no", 0, "", false},
		{"#hash", 0, "", false},
		{"## C#", 2, "C#", true},
		{"#", 1, "", true},
		{"## ##", 2, "", true},
	}
	for _, c := range cases {
		level, text, ok := atxHeader(c.line)
		if level != c.level || text != c.text || ok != c.ok {
			t.Errorf("atxHeader(%q) = %d, %q, %v", c.line, level, text, ok)
		}
	}
}

func TestAutoSplitter(t *testing.T) {
	s := &AutoSplitter{Size: 20, Overlap: -1}
	md, _ := s.Split(t.Context(), &Document{ID: "a.md", Content: "# T\n\nbody", Metadata: map[string]any{MetaFormat: "markdown"}})
	if len(md) != 1 || md[0].Context != "T" {
		t.Fatalf("markdown: %+v", md)
	}
	code, _ := s.Split(t.Context(), &Document{ID: "a.go", Content: goSource, Metadata: map[string]any{MetaLanguage: "go"}})
	if len(code) == 0 || !strings.HasPrefix(code[0].Content, "package demo") {
		t.Fatalf("code: %+v", code)
	}
	text, _ := s.Split(t.Context(), &Document{ID: "a.txt", Content: "# not markdown here"})
	if len(text) != 1 || text[0].Context != "" {
		t.Fatalf("text: %+v", text)
	}
}
