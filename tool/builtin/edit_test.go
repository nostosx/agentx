package builtin

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestEditFileReturnsDiff(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a.go": "package a\n\nfunc A() int {\n\treturn 1\n}\n"})
	res := call(t, EditFile(Config{Dir: dir}), map[string]any{"path": "a.go", "old_string": "return 1", "new_string": "return 2"})
	want := "Edited a.go (1 replacement)\n--- a/a.go\n+++ b/a.go\n@@ -1,5 +1,5 @@\n package a\n \n func A() int {\n-\treturn 1\n+\treturn 2\n }"
	if res.IsError || res.Content != want {
		t.Fatalf("got\n%s\nwant\n%s", res.Content, want)
	}
}

func TestEditFileMultipleEdits(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const original = "alpha\nbeta\ngamma\nbeta\n"
	writeFiles(t, dir, map[string]string{"f.txt": original})
	cfg := Config{Dir: dir, Files: &FileTracker{}}
	readAll(t, cfg, "f.txt")
	edit := EditFile(cfg)
	content := func() string {
		b, err := os.ReadFile(filepath.Join(dir, "f.txt"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	for _, tc := range []struct {
		name string
		args map[string]any
		err  string
	}{
		{"both forms", map[string]any{"path": "f.txt", "old_string": "alpha", "new_string": "A", "edits": []any{map[string]any{"old_string": "gamma", "new_string": "G"}}}, "not both"},
		{"later edit missing", map[string]any{"path": "f.txt", "edits": []any{
			map[string]any{"old_string": "alpha", "new_string": "A"},
			map[string]any{"old_string": "alpha", "new_string": "again"},
		}}, "edit 2: old_string was not found in f.txt after the previous edits"},
		{"ambiguous", map[string]any{"path": "f.txt", "edits": []any{map[string]any{"old_string": "beta", "new_string": "B"}}}, "edit 1: old_string occurs 2 times"},
		{"empty", map[string]any{"path": "f.txt", "edits": []any{map[string]any{"old_string": "alpha", "new_string": "A"}, map[string]any{"old_string": "", "new_string": "x"}}}, "edit 2: old_string is empty"},
		{"identical", map[string]any{"path": "f.txt", "edits": []any{map[string]any{"old_string": "x", "new_string": "x"}}}, "edit 1: old_string and new_string are identical"},
		{"neither", map[string]any{"path": "f.txt"}, "old_string is empty"},
	} {
		res := call(t, edit, tc.args)
		if !res.IsError || !strings.Contains(res.Content, tc.err) {
			t.Fatalf("%s: got %q, want %q", tc.name, res.Content, tc.err)
		}
		if got := content(); got != original {
			t.Fatalf("%s: file changed to %q", tc.name, got)
		}
	}

	res := call(t, edit, map[string]any{"path": "f.txt", "edits": []any{
		map[string]any{"old_string": "alpha", "new_string": "ALPHA"},
		map[string]any{"old_string": "beta", "new_string": "B", "replace_all": true},
		// Each edit sees the result of the previous ones.
		map[string]any{"old_string": "ALPHA\nB", "new_string": "first"},
	}})
	if res.IsError || !strings.HasPrefix(res.Content, "Edited f.txt (3 edits, 4 replacements)\n--- a/f.txt") {
		t.Fatalf("edits: %q", res.Content)
	}
	if got := content(); got != "first\ngamma\nB\n" {
		t.Fatalf("content %q", got)
	}
	// The tracker knows the new content.
	if res := call(t, edit, map[string]any{"path": "f.txt", "old_string": "gamma", "new_string": "G"}); res.IsError {
		t.Fatalf("edit after edits: %q", res.Content)
	}
}

func TestWriteFileReturnsDiffAndReplacesAtomically(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := Config{Dir: dir, Files: &FileTracker{}}
	write := WriteFile(cfg)
	if res := call(t, write, map[string]any{"path": "x.txt", "content": "one\ntwo\n"}); res.Content != "Created x.txt (8 bytes)" {
		t.Fatalf("create: %q", res.Content)
	}
	p := filepath.Join(dir, "x.txt")
	if runtime.GOOS != "windows" {
		if err := os.Chmod(p, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	readAll(t, cfg, "x.txt")
	res := call(t, write, map[string]any{"path": "x.txt", "content": "one\n2\n"})
	want := "Updated x.txt (6 bytes)\n--- a/x.txt\n+++ b/x.txt\n@@ -1,2 +1,2 @@\n one\n-two\n+2"
	if res.IsError || res.Content != want {
		t.Fatalf("got %q, want %q", res.Content, want)
	}
	if res := call(t, write, map[string]any{"path": "x.txt", "content": "one\n2\n"}); !strings.HasSuffix(res.Content, "(no changes)") {
		t.Fatalf("same content: %q", res.Content)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o640 {
			t.Errorf("mode = %v", st.Mode().Perm())
		}
	}
	// No temporary files are left behind.
	if got := snapshot(t, dir); len(got) != 1 || got["x.txt"] != "one\n2\n" {
		t.Fatalf("files = %q", got)
	}
	if res := call(t, write, map[string]any{"path": "sub", "content": "x"}); res.IsError {
		t.Fatalf("create sub: %q", res.Content)
	}
	if err := os.Remove(filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if res := call(t, write, map[string]any{"path": "sub", "content": "x"}); !res.IsError || !strings.Contains(res.Content, "directory") {
		t.Fatalf("write over a directory: %q", res.Content)
	}
}

func TestWriteFileKeepsSymlink(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"target.txt": "old\n"})
	if err := os.Symlink("target.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if res := call(t, WriteFile(Config{Dir: dir}), map[string]any{"path": "link.txt", "content": "new\n"}); res.IsError {
		t.Fatal(res.Content)
	}
	if target, err := os.Readlink(filepath.Join(dir, "link.txt")); err != nil || target != "target.txt" {
		t.Fatalf("link = %q, %v", target, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "target.txt")); string(b) != "new\n" {
		t.Fatalf("target = %q", b)
	}
}

func TestFileDiffIsBounded(t *testing.T) {
	t.Parallel()
	e := newEnv(Config{Dir: t.TempDir()})
	var old, updated strings.Builder
	for i := range 5000 {
		old.WriteString("old line\n")
		updated.WriteString("new line " + strings.Repeat("y", i%5) + "\n")
	}
	d := e.fileDiff("f", "f", []byte(old.String()), []byte(updated.String()))
	if len(d) > maxDiffOutput+100 || !strings.Contains(d, "truncated") || !strings.HasPrefix(d, "--- a/f\n+++ b/f\n") {
		t.Fatalf("%d bytes: %.200s", len(d), d)
	}
	big := make([]byte, maxDiffInput+1)
	if d := e.fileDiff("f", "f", big, nil); !strings.Contains(d, "too large") {
		t.Fatalf("large file: %q", d)
	}
	if d := e.fileDiff("f", "f", []byte("\xff\n"), []byte("\xfe\n")); !strings.Contains(d, "�") {
		t.Fatalf("invalid UTF-8: %q", d)
	}
}
