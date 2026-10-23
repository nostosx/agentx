package builtin

import (
	"reflect"
	"strings"
	"testing"
)

func wrapPatch(body string) string { return "*** Begin Patch\n" + body + "\n*** End Patch" }

func TestParsePatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		patch string
		want  []patchHunk
		err   string
	}{
		{name: "not a patch", patch: "bad", err: "The first line of the patch must be '*** Begin Patch'"},
		{name: "no end", patch: "*** Begin Patch\nbad", err: "The last line of the patch must be '*** End Patch'"},
		{name: "empty input", patch: "  \n", err: "The last line of the patch must be '*** End Patch'"},
		{name: "begin only", patch: "*** Begin Patch", err: "The last line"},
		{name: "empty patch", patch: "*** Begin Patch\n*** End Patch", want: nil},
		{
			name:  "whitespace around markers",
			patch: "*** Begin Patch \n*** Add File: foo\n+hi\n *** End Patch",
			want:  []patchHunk{{kind: addHunk, path: "foo", contents: "hi\n"}},
		},
		{
			name:  "empty update",
			patch: wrapPatch("*** Update File: test.py"),
			err:   "invalid patch hunk on line 2: Update file hunk for path 'test.py' is empty",
		},
		{
			name: "all operations",
			patch: wrapPatch("*** Add File: path/add.py\n+abc\n+def\n*** Delete File: path/delete.py\n" +
				"*** Update File: path/update.py\n*** Move to: path/update2.py\n@@ def f():\n-    pass\n+    return 123"),
			want: []patchHunk{
				{kind: addHunk, path: "path/add.py", contents: "abc\ndef\n"},
				{kind: deleteHunk, path: "path/delete.py"},
				{kind: updateHunk, path: "path/update.py", movePath: "path/update2.py", chunks: []patchChunk{
					{context: "def f():", hasContext: true, old: []string{"    pass"}, new: []string{"    return 123"}},
				}},
			},
		},
		{
			name:  "update followed by add",
			patch: wrapPatch("*** Update File: file.py\n@@\n+line\n*** Add File: other.py\n+content"),
			want: []patchHunk{
				{kind: updateHunk, path: "file.py", chunks: []patchChunk{{new: []string{"line"}}}},
				{kind: addHunk, path: "other.py", contents: "content\n"},
			},
		},
		{
			name:  "first change without @@",
			patch: wrapPatch("*** Update File: file2.py\n import foo\n+bar"),
			want: []patchHunk{{kind: updateHunk, path: "file2.py", chunks: []patchChunk{
				{old: []string{"import foo"}, new: []string{"import foo", "bar"}, contextLines: [][2]int{{0, 0}}},
			}}},
		},
		{
			name:  "heredoc",
			patch: "<<'EOF'\n" + wrapPatch("*** Update File: file2.py\n import foo\n+bar") + "\nEOF\n",
			want: []patchHunk{{kind: updateHunk, path: "file2.py", chunks: []patchChunk{
				{old: []string{"import foo"}, new: []string{"import foo", "bar"}, contextLines: [][2]int{{0, 0}}},
			}}},
		},
		{
			name:  "mismatched heredoc quotes",
			patch: "<<\"EOF'\n" + wrapPatch("*** Delete File: a") + "\nEOF\n",
			err:   "The first line of the patch must be '*** Begin Patch'",
		},
		{
			name:  "heredoc without end patch",
			patch: "<<EOF\n*** Begin Patch\n*** Update File: file2.py\nEOF\n",
			err:   "The last line of the patch must be '*** End Patch'",
		},
		{
			name:  "end of file then empty lines",
			patch: "*** Begin Patch\n*** Update File: file.txt\n@@\n+quux\n*** End of File\n\n*** End Patch",
			want: []patchHunk{{kind: updateHunk, path: "file.txt", chunks: []patchChunk{
				{new: []string{"quux"}, eof: true},
			}}},
		},
		{
			name:  "indented markers are context",
			patch: wrapPatch("*** Update File: a.txt\n@@\n-old a\n+new a\n *** Update File: b.txt\n@@\n-old b\n+new b"),
			want: []patchHunk{{kind: updateHunk, path: "a.txt", chunks: []patchChunk{
				{old: []string{"old a", "*** Update File: b.txt"}, new: []string{"new a", "*** Update File: b.txt"}, contextLines: [][2]int{{1, 1}}},
				{old: []string{"old b"}, new: []string{"new b"}},
			}}},
		},
		{
			name:  "bare empty lines are context",
			patch: wrapPatch("*** Update File: file.txt\n@@\n context before\n\n context after"),
			want: []patchHunk{{kind: updateHunk, path: "file.txt", chunks: []patchChunk{
				{old: []string{"context before", "", "context after"}, new: []string{"context before", "", "context after"}, contextLines: [][2]int{{0, 0}, {1, 1}, {2, 2}}},
			}}},
		},
		{
			name:  "crlf",
			patch: "*** Begin Patch\r\n*** Update File: file.txt\r\n@@\r\n-old\r\r\n+new\r\n*** End Patch\r\n",
			want: []patchHunk{{kind: updateHunk, path: "file.txt", chunks: []patchChunk{
				{old: []string{"old"}, new: []string{"new"}},
			}}},
		},
		{
			name:  "two @@ in a row",
			patch: wrapPatch("*** Update File: f\n@@ class A\n@@ def b\n-x\n+y"),
			err:   "line 4: Unexpected line found in update hunk: '@@ def b'",
		},
		{
			name:  "empty change at end",
			patch: wrapPatch("*** Update File: f\n@@\n-x\n@@"),
			err:   "Update hunk does not contain any lines",
		},
		{
			name:  "bad line in update",
			patch: wrapPatch("*** Update File: f\n@@\n-x\nbad"),
			err:   "line 5: Expected update hunk to start with a @@ context marker, got: 'bad'",
		},
		{
			name:  "bad line after header",
			patch: wrapPatch("*** Update File: f\nbad"),
			err:   "line 3: Unexpected line found in update hunk: 'bad'",
		},
		{
			name:  "change after end of file",
			patch: wrapPatch("*** Update File: f\n@@\n-x\n*** End of File\n-y"),
			err:   "Expected update hunk to start with a @@ context marker, got: '-y'",
		},
		{
			name:  "invalid header",
			patch: wrapPatch("*** Frobnicate File: f"),
			err:   "line 2: '*** Frobnicate File: f' is not a valid hunk header",
		},
		{
			name:  "add line without plus",
			patch: wrapPatch("*** Add File: f\n+a\nb"),
			err:   "line 4: 'b' is not a valid hunk header",
		},
		{
			name:  "content after end patch",
			patch: "*** Begin Patch\n*** Delete File: a\n*** End Patch\n*** Delete File: b\n*** End Patch",
			err:   "The last line of the patch must be '*** End Patch'",
		},
		{
			name:  "move after a change",
			patch: wrapPatch("*** Update File: f\n@@\n-x\n*** Move to: g"),
			err:   "Expected update hunk to start with a @@ context marker",
		},
		{
			name:  "empty add file",
			patch: wrapPatch("*** Add File: empty"),
			want:  []patchHunk{{kind: addHunk, path: "empty"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parsePatch(tc.patch)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestSeekSequence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		lines, pattern []string
		start          int
		eof            bool
		want           int
	}{
		{"exact", []string{"foo", "bar", "baz"}, []string{"bar", "baz"}, 0, false, 1},
		{"trailing whitespace", []string{"foo   ", "bar\t\t"}, []string{"foo", "bar"}, 0, false, 0},
		{"surrounding whitespace", []string{"    foo   ", "   bar\t"}, []string{"foo", "bar"}, 0, false, 0},
		{"punctuation", []string{"a \u2013 b\u00a0\u201cq\u201d \u2018s\u2019"}, []string{`a - b "q" 's'`}, 0, false, 0},
		{"too long", []string{"one"}, []string{"too", "many"}, 0, false, -1},
		{"empty pattern", []string{"a"}, nil, 1, false, 1},
		{"from start", []string{"x", "y", "x"}, []string{"x"}, 1, false, 2},
		{"eof", []string{"x", "y", "x", "y"}, []string{"x", "y"}, 0, true, 2},
		{"eof does not fall back", []string{"x", "y", "z"}, []string{"x"}, 0, true, -1},
		{"exact beats a looser earlier match", []string{"  a", "a"}, []string{"a"}, 0, false, 1},
		{"missing", []string{"a"}, []string{"b"}, 0, false, -1},
	} {
		if got := seekSequence(tc.lines, tc.pattern, tc.start, tc.eof); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

// applyUpdate applies the update hunk of patch body to content.
func applyUpdate(t *testing.T, content, body string) (string, error) {
	t.Helper()
	hunks, err := parsePatch(wrapPatch("*** Update File: f\n" + body))
	if err != nil {
		t.Fatal(err)
	}
	return applyChunks(content, "f", hunks[0].chunks)
}

func TestApplyChunks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, content, body, want, err string
	}{
		{name: "two changes", content: "foo\nbar\nbaz\nqux\n", body: "@@\n foo\n-bar\n+BAR\n@@\n baz\n-qux\n+QUX", want: "foo\nBAR\nbaz\nQUX\n"},
		{name: "interleaved with eof", content: "a\nb\nc\nd\ne\nf\n",
			body: "@@\n a\n-b\n+B\n@@\n c\n d\n-e\n+E\n@@\n f\n+g\n*** End of File", want: "a\nB\nc\nd\nE\nf\ng\n"},
		// Like Codex, a change of added lines only goes to the end.
		{name: "pure addition then removal", content: "line1\nline2\nline3\n",
			body: "@@\n+after-context\n+second-line\n@@\n line1\n-line2\n-line3\n+line2-replacement",
			want: "line1\nline2-replacement\nafter-context\nsecond-line\n"},
		{name: "unicode dash", content: "import asyncio  # local import \u2013 avoids top\u2011level dep\n",
			body: "@@\n-import asyncio  # local import - avoids top-level dep\n+import asyncio  # HELLO", want: "import asyncio  # HELLO\n"},
		{name: "context header", content: "def a():\n    pass\ndef b():\n    pass\n",
			body: "@@ def b():\n-    pass\n+    return 1", want: "def a():\n    pass\ndef b():\n    return 1\n"},
		{name: "context not found", content: "x\n", body: "@@ def z():\n-x\n+y", err: "failed to find context 'def z():' in f"},
		{name: "lines not found", content: "x\n", body: "@@\n-y\n+z", err: "failed to find expected lines in f:\ny"},
		{name: "changes in order", content: "a\nb\n", body: "@@\n-b\n+B\n@@\n-a\n+A", err: "failed to find expected lines"},
		{name: "context keeps file whitespace", content: "  keep  \nold\n", body: "@@\n keep\n-old\n+new", want: "  keep  \nnew\n"},
		{name: "crlf kept", content: "a\r\nb\r\n", body: "@@\n a\n-b\n+c\n+d", want: "a\r\nc\r\nd\r\n"},
		{name: "mixed endings kept", content: "a\nb\r\nc\n", body: "@@\n a\n b\n-c\n+C", want: "a\nb\r\nC\n"},
		{name: "adds final newline", content: "a\nb", body: "@@\n-a\n+A", want: "A\nb\n"},
		{name: "trailing empty line stands for the end", content: "a\nb\n", body: "@@\n a\n-b\n+B\n ", want: "a\nB\n"},
		{name: "empty file", content: "", body: "@@\n+first", want: "first\n"},
		{name: "duplicates use the first after the context", content: "f()\nx\ng()\nx\n", body: "@@ g()\n-x\n+y", want: "f()\nx\ng()\ny\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := applyUpdate(t, tc.content, tc.body)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSourceRejectsOverlap(t *testing.T) {
	t.Parallel()
	// computeReplacements never overlaps changes, since each is searched
	// for after the previous one; apply still refuses to.
	f := parseSource("a\nb\n")
	reps := []replacement{{start: 0, n: 2, lines: []string{"x"}}, {start: 1, n: 1, lines: []string{"y"}}}
	if err := f.apply(reps, "f"); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("overlap: %v", err)
	}
}

func TestParseSource(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"", "a", "a\n", "a\r\nb\rc\n", "\n\n", "x\r"} {
		f := parseSource(s)
		if got := f.String(); got != s {
			t.Errorf("round trip of %q: %q", s, got)
		}
	}
	if f := parseSource("a\r\nb\n"); f.ending != "\r\n" || !reflect.DeepEqual(f.texts(), []string{"a", "b"}) {
		t.Errorf("parse: %+v", f)
	}
}
