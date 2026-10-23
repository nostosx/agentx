package diff

import (
	"math/rand/v2"
	"strings"
	"testing"
)

// check verifies that edits turn a into b, visiting every line once.
func check(t *testing.T, a, b []string, edits []Edit) {
	t.Helper()
	i, j := 0, 0
	for _, e := range edits {
		switch e.Op {
		case Equal:
			if e.A != i || e.B != j || i >= len(a) || j >= len(b) || a[i] != b[j] {
				t.Fatalf("bad equal %+v at %d,%d", e, i, j)
			}
			i++
			j++
		case Delete:
			if e.A != i || e.B != j || i >= len(a) {
				t.Fatalf("bad delete %+v at %d,%d", e, i, j)
			}
			i++
		case Insert:
			if e.A != i || e.B != j || j >= len(b) {
				t.Fatalf("bad insert %+v at %d,%d", e, i, j)
			}
			j++
		default:
			t.Fatalf("unknown edit operation: %+v", e)
		}
	}
	if i != len(a) || j != len(b) {
		t.Fatalf("script ends at %d,%d, want %d,%d", i, j, len(a), len(b))
	}
}

func changes(edits []Edit) int {
	n := 0
	for _, e := range edits {
		if e.Op != Equal {
			n++
		}
	}
	return n
}

// lcs returns the length of a longest common subsequence.
func lcs(a, b []string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for i := range a {
		for j := range b {
			if a[i] == b[j] {
				cur[j+1] = prev[j] + 1
			} else {
				cur[j+1] = max(cur[j], prev[j+1])
			}
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func randomLines(r *rand.Rand, n, alphabet int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = string(rune('a' + r.IntN(alphabet)))
	}
	return out
}

func TestMyersIsMinimal(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1, 2))
	for range 3000 {
		a := randomLines(r, r.IntN(14), 1+r.IntN(4))
		b := randomLines(r, r.IntN(14), 1+r.IntN(4))
		ia, ib := make([]int, len(a)), make([]int, len(b))
		for i, l := range a {
			ia[i] = int(l[0])
		}
		for i, l := range b {
			ib[i] = int(l[0])
		}
		d := &differ{a: ia, b: ib}
		d.myers(0, len(a), 0, len(b))
		check(t, a, b, d.edits)
		if got, want := changes(d.edits), len(a)+len(b)-2*lcs(a, b); got != want {
			t.Fatalf("%q -> %q: %d changes, want %d", a, b, got, want)
		}
	}
}

func TestLinesProducesValidScripts(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(3, 4))
	for range 3000 {
		a := randomLines(r, r.IntN(40), 1+r.IntN(20))
		b := randomLines(r, r.IntN(40), 1+r.IntN(20))
		check(t, a, b, Lines(a, b))
	}
	// Edits of a longer text keep the unchanged lines.
	a := randomLines(r, 500, 26)
	b := append([]string(nil), a...)
	b[100] = "changed"
	b = append(b[:300], b[310:]...)
	edits := Lines(a, b)
	check(t, a, b, edits)
	if got := changes(edits); got != 12 {
		t.Errorf("changes = %d, want 12", got)
	}
}

func TestPatienceKeepsDeclarationsApart(t *testing.T) {
	t.Parallel()
	// Myers would match the braces and blank lines of the removed function
	// with those of the added one; patience diff anchors on the unique
	// signatures and removes one function whole.
	old := "func a() {\n\treturn 1\n}\n\nfunc b() {\n\treturn 2\n}\n"
	new := "func b() {\n\treturn 2\n}\n"
	got := Unified("a/x.go", "b/x.go", old, new, 3)
	want := "--- a/x.go\n+++ b/x.go\n@@ -1,7 +1,3 @@\n-func a() {\n-\treturn 1\n-}\n-\n func b() {\n \treturn 2\n }\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestUnified(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, old, new string
		context        int
		want           string
	}{
		{"equal", "a\n", "a\n", 3, ""},
		// The next three match the diffs of Codex's apply-patch tests,
		// which use one line of context.
		{"two changes", "foo\nbar\nbaz\nqux\n", "foo\nBAR\nbaz\nQUX\n", 1,
			"@@ -1,4 +1,4 @@\n foo\n-bar\n+BAR\n baz\n-qux\n+QUX\n"},
		{"last line", "foo\nbar\nbaz\n", "foo\nbar\nBAZ\n", 1,
			"@@ -2,2 +2,2 @@\n bar\n-baz\n+BAZ\n"},
		{"append", "foo\nbar\nbaz\n", "foo\nbar\nbaz\nquux\n", 1,
			"@@ -3 +3,2 @@\n baz\n+quux\n"},
		{"new file", "", "a\nb\n", 3, "@@ -0,0 +1,2 @@\n+a\n+b\n"},
		{"emptied", "a\n", "", 3, "@@ -1 +0,0 @@\n-a\n"},
		{"no newline", "a\nb", "a\nb\n", 3,
			"@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+b\n"},
		{"insert at start", "b\nc\n", "a\nb\nc\n", 0, "@@ -0,0 +1 @@\n+a\n"},
		{"crlf", "a\r\nb\r\n", "a\r\nc\r\n", 3, "@@ -1,2 +1,2 @@\n a\n-b\n+c\n"},
		{"separate hunks", "1\n2\n3\n4\n5\n6\n7\n8\n9\n", "1\nX\n3\n4\n5\n6\n7\nY\n9\n", 1,
			"@@ -1,3 +1,3 @@\n 1\n-2\n+X\n 3\n@@ -7,3 +7,3 @@\n 7\n-8\n+Y\n 9\n"},
		{"merged hunks", "1\n2\n3\n4\n5\n", "X\n2\n3\n4\nY\n", 2,
			"@@ -1,5 +1,5 @@\n-1\n+X\n 2\n 3\n 4\n-5\n+Y\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Unified("a/f", "b/f", tc.old, tc.new, tc.context)
			if tc.want != "" {
				tc.want = "--- a/f\n+++ b/f\n" + tc.want
			}
			if got != tc.want {
				t.Errorf("got\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

func TestLargeRegionsAreBounded(t *testing.T) {
	t.Parallel()
	// No line is unique, so nothing anchors the comparison, and the
	// region is too large for Myers' algorithm.
	var a, b []string
	for i := range 20000 {
		a = append(a, []string{"x\n", "y\n"}[i%2])
		b = append(b, []string{"y\n", "z\n", "x\n"}[i%3])
	}
	edits := Lines(a, b)
	check(t, a, b, edits)
	if got, want := changes(edits), len(a)+len(b); got != want {
		t.Errorf("large unanchored region: %d changes, want replacement with %d", got, want)
	}
}

func TestSplitLines(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"": "", "a": "a", "a\n": "a\n", "a\nb": "a\n|b", "\n\n": "\n|\n"} {
		if got := strings.Join(SplitLines(in), "|"); got != want {
			t.Errorf("SplitLines(%q) = %q, want %q", in, got, want)
		}
	}
}
