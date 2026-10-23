package builtin

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/tool"
)

func patch(t *testing.T, tl tool.Tool, body string) *tool.Result {
	t.Helper()
	return call(t, tl, map[string]any{"input": wrapPatch(body)})
}

// snapshot returns every file and directory under dir with its content.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			out[rel+"/"] = ""
			return nil
		}
		b, err := os.ReadFile(p)
		out[rel] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func readAll(t *testing.T, cfg Config, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if res := call(t, ReadFile(cfg), map[string]any{"path": p}); res.IsError {
			t.Fatalf("read %s: %s", p, res.Content)
		}
	}
}

func TestApplyPatchInfo(t *testing.T) {
	t.Parallel()
	tl := ApplyPatch(Config{Dir: t.TempDir()})
	spec := tool.Spec(tl)
	if spec.Grammar == nil || spec.Grammar.Syntax != "lark" || !strings.HasPrefix(spec.Grammar.Definition, "start: begin_patch hunk+ end_patch") {
		t.Fatalf("grammar = %+v", spec.Grammar)
	}
	var params struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal(spec.Parameters, &params); err != nil {
		t.Fatal(err)
	}
	if _, ok := params.Properties[model.FreeformInput]; !ok || !slices.Equal(params.Required, []string{model.FreeformInput}) {
		t.Fatalf("parameters = %s", spec.Parameters)
	}
	if !tl.Info().RequiresApproval || ApplyPatch(Config{NoApproval: true}).Info().RequiresApproval {
		t.Fatal("approval")
	}
}

func TestToolsEditStyle(t *testing.T) {
	t.Parallel()
	names := func(style EditStyle) []string {
		var out []string
		for _, tl := range Tools(Config{Dir: t.TempDir(), Edit: style}) {
			out = append(out, tl.Info().Name)
		}
		return out
	}
	for style, want := range map[EditStyle][2]bool{EditAll: {true, true}, EditReplace: {true, false}, EditPatch: {false, true}} {
		got := names(style)
		if slices.Contains(got, "edit_file") != want[0] || slices.Contains(got, "apply_patch") != want[1] || !slices.Contains(got, "write_file") {
			t.Errorf("style %d: %v", style, got)
		}
	}
}

func TestApplyPatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"src/app.py":   "import os\n\ndef greet():\n    print(\"Hi\")\n\ndef main():\n    greet()\n",
		"obsolete.txt": "old\n",
		"keep.txt":     "a\nb\nc\n",
	})
	if runtime.GOOS != "windows" {
		if err := os.Chmod(filepath.Join(dir, "src/app.py"), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{Dir: dir, Files: &FileTracker{}}
	readAll(t, cfg, "src/app.py", "obsolete.txt", "keep.txt")
	ap := ApplyPatch(cfg)
	res := patch(t, ap, `*** Add File: docs/hello.txt
+Hello world
*** Update File: src/app.py
*** Move to: src/main.py
@@ def greet():
-    print("Hi")
+    print("Hello, world!")
*** Delete File: obsolete.txt
*** Update File: keep.txt
@@
 a
-b
+B`)
	if res.IsError {
		t.Fatalf("patch: %s", res.Content)
	}
	wantSummary := "Success. Updated the following files:\nA docs/hello.txt\nM src/main.py\nM keep.txt\nD obsolete.txt\n"
	if !strings.HasPrefix(res.Content, wantSummary) {
		t.Fatalf("summary:\n%s", res.Content)
	}
	for _, want := range []string{"--- a/src/app.py\n+++ b/src/main.py\n", "-    print(\"Hi\")\n+    print(\"Hello, world!\")", "--- a/keep.txt\n+++ b/keep.txt\n@@ -1,3 +1,3 @@\n a\n-b\n+B\n c"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("result lacks %q:\n%s", want, res.Content)
		}
	}
	if strings.Contains(res.Content, "+++ b/docs/hello.txt") || strings.Contains(res.Content, "obsolete") && strings.Contains(res.Content, "--- a/obsolete.txt") {
		t.Errorf("diffs of new or deleted files:\n%s", res.Content)
	}
	got := snapshot(t, dir)
	want := map[string]string{
		"docs/": "", "docs/hello.txt": "Hello world\n",
		"src/": "", "src/main.py": "import os\n\ndef greet():\n    print(\"Hello, world!\")\n\ndef main():\n    greet()\n",
		"keep.txt": "a\nB\nc\n",
	}
	if !mapsEqual(got, want) {
		t.Fatalf("files = %q", got)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(filepath.Join(dir, "src/main.py")); st.Mode().Perm() != 0o750 {
			t.Errorf("moved file mode = %v", st.Mode().Perm())
		}
	}
	// The written files can be changed again without reading them.
	if res := patch(t, ap, "*** Update File: src/main.py\n@@\n-import os\n+import sys\n*** Delete File: docs/hello.txt"); res.IsError {
		t.Fatalf("second patch: %s", res.Content)
	}
	// A deleted file is forgotten: a new file of the same name is new.
	cfg.Files.mu.Lock()
	_, tracked := cfg.Files.files[filepath.Join(dir, "docs", "hello.txt")]
	cfg.Files.mu.Unlock()
	if tracked {
		t.Error("deleted file still tracked")
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func TestApplyPatchFailuresChangeNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body, err string
		read            []string
	}{
		{"unread update", "*** Add File: new/x.txt\n+x\n*** Update File: a.txt\n@@\n-one\n+ONE", "a.txt has not been read", nil},
		{"unread delete", "*** Add File: new/x.txt\n+x\n*** Delete File: a.txt", "a.txt has not been read", nil},
		{"unread overwrite", "*** Add File: a.txt\n+x", "a.txt has not been read", nil},
		{"unread move destination", "*** Update File: a.txt\n*** Move to: b.txt\n@@\n-one\n+ONE", "b.txt has not been read", []string{"a.txt"}},
		{"lines not found", "*** Add File: new/x.txt\n+x\n*** Update File: a.txt\n@@\n-zero\n+ONE", "failed to find expected lines in a.txt", []string{"a.txt"}},
		{"later hunk fails", "*** Update File: a.txt\n@@\n-one\n+ONE\n*** Delete File: missing.txt", "failed to delete file missing.txt", []string{"a.txt"}},
		{"update missing", "*** Update File: missing.txt\n@@\n-a\n+b", "failed to read file to update missing.txt", nil},
		{"outside", "*** Add File: ../escape.txt\n+x", "escape", nil},
		{"absolute outside", "*** Add File: {outside}\n+x", "outside the working directory", nil},
		{"directory", "*** Delete File: sub", "sub is a directory", nil},
		{"binary", "*** Update File: bin.dat\n@@\n-a\n+b", "not valid UTF-8", []string{"bin.dat"}},
		{"no hunks", "", "No files were modified.", nil},
		{"syntax", "*** Update File: a.txt", "Update file hunk for path 'a.txt' is empty", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeFiles(t, dir, map[string]string{"a.txt": "one\n", "b.txt": "bee\n", "sub/c.txt": "c\n", "bin.dat": "\xff\xfe\n"})
			cfg := Config{Dir: dir, Files: &FileTracker{}}
			readAll(t, cfg, tc.read...)
			before := snapshot(t, dir)
			body := strings.ReplaceAll(tc.body, "{outside}", filepath.Join(filepath.Dir(dir), "agentx-escape.txt"))
			res := call(t, ApplyPatch(cfg), map[string]any{"input": strings.Replace(wrapPatch(body), "\n\n", "\n", 1)})
			if !res.IsError || !strings.Contains(res.Content, tc.err) {
				t.Fatalf("got %q, want error %q", res.Content, tc.err)
			}
			if after := snapshot(t, dir); !mapsEqual(before, after) {
				t.Fatalf("files changed:\nbefore %q\nafter  %q", before, after)
			}
		})
	}
}

func TestApplyPatchSameFileTwice(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a.txt": "one\n"})
	cfg := Config{Dir: dir, Files: &FileTracker{}}
	readAll(t, cfg, "a.txt")
	res := patch(t, ApplyPatch(cfg), `*** Add File: new.txt
+first
*** Update File: new.txt
@@
-first
+second
*** Delete File: a.txt
*** Add File: a.txt
+replaced
*** Update File: a.txt
*** Move to: a.txt
@@
-replaced
+moved onto itself`)
	if res.IsError {
		t.Fatal(res.Content)
	}
	if got := snapshot(t, dir); !mapsEqual(got, map[string]string{"new.txt": "second\n", "a.txt": "moved onto itself\n"}) {
		t.Fatalf("files = %q", got)
	}
}

func TestApplyPatchKeepsLineEndingsAndHeredoc(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"w.txt": "a\r\nb\r\nc\r\n"})
	res := call(t, ApplyPatch(Config{Dir: dir}), map[string]any{
		"input": "<<'EOF'\n*** Begin Patch\n*** Update File: w.txt\n@@\n a\n-b\n+B\n+B2\n*** End Patch\nEOF\n",
	})
	if res.IsError {
		t.Fatal(res.Content)
	}
	if got := snapshot(t, dir)["w.txt"]; got != "a\r\nB\r\nB2\r\nc\r\n" {
		t.Fatalf("content %q", got)
	}
}

// TestApplyPatchRollsBack fails each step of the commit in turn and checks
// that the files are restored. It sets commitHook, so it does not run in
// parallel.
func TestApplyPatchRollsBack(t *testing.T) {
	origHook := commitHook
	t.Cleanup(func() { commitHook = origHook })
	body := "*** Add File: new/deep/x.txt\n+x\n*** Update File: a.txt\n@@\n-one\n+ONE\n*** Delete File: b.txt\n*** Update File: c.txt\n*** Move to: d.txt\n@@\n-see\n+SEE"
	for fail := range 5 {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{"a.txt": "one\n", "b.txt": "bee\n", "c.txt": "see\n"})
		if runtime.GOOS != "windows" {
			if err := os.Chmod(filepath.Join(dir, "a.txt"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		before := snapshot(t, dir)
		steps := 0
		commitHook = func(rel string) error {
			steps++
			if steps == fail+1 {
				return errors.New("injected failure")
			}
			return nil
		}
		res := patch(t, ApplyPatch(Config{Dir: dir}), body)
		commitHook = nil
		if !res.IsError || !strings.Contains(res.Content, "injected failure") || !strings.Contains(res.Content, "no file was changed") {
			t.Fatalf("step %d: %q", fail, res.Content)
		}
		if after := snapshot(t, dir); !mapsEqual(before, after) {
			t.Fatalf("step %d: files changed:\nbefore %q\nafter  %q", fail, before, after)
		}
		if runtime.GOOS != "windows" {
			if st, _ := os.Stat(filepath.Join(dir, "a.txt")); st.Mode().Perm() != 0o600 {
				t.Errorf("step %d: restored mode %v", fail, st.Mode().Perm())
			}
		}
	}
	// Five steps: three writes and two removals.
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a.txt": "one\n", "b.txt": "bee\n", "c.txt": "see\n"})
	steps := 0
	commitHook = func(string) error { steps++; return nil }
	res := patch(t, ApplyPatch(Config{Dir: dir}), body)
	commitHook = nil
	if res.IsError || steps != 5 {
		t.Fatalf("steps = %d: %s", steps, res.Content)
	}
	if got := snapshot(t, dir); !mapsEqual(got, map[string]string{"new/": "", "new/deep/": "", "new/deep/x.txt": "x\n", "a.txt": "ONE\n", "d.txt": "SEE\n"}) {
		t.Fatalf("files = %q", got)
	}
}

func TestApplyPatchStagingFailure(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX permissions")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a.txt": "one\n", "locked/b.txt": "bee\n"})
	locked := filepath.Join(dir, "locked")
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	before := snapshot(t, dir)
	res := patch(t, ApplyPatch(Config{Dir: dir}), "*** Update File: a.txt\n@@\n-one\n+ONE\n*** Add File: new/x.txt\n+x\n*** Update File: locked/b.txt\n@@\n-bee\n+BEE")
	if !res.IsError {
		t.Fatalf("patch into a read-only directory: %s", res.Content)
	}
	if after := snapshot(t, dir); !mapsEqual(before, after) {
		t.Fatalf("files changed:\nbefore %q\nafter  %q", before, after)
	}
}

func TestApplyPatchThroughSymlink(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"target.txt": "one\n", "other.txt": "x\n"})
	if err := os.Symlink("target.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	res := patch(t, ApplyPatch(Config{Dir: dir}), "*** Update File: link.txt\n@@\n-one\n+ONE")
	if res.IsError {
		t.Fatal(res.Content)
	}
	if st, err := os.Lstat(filepath.Join(dir, "link.txt")); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "target.txt")); string(b) != "ONE\n" {
		t.Fatalf("target = %q", b)
	}
}

// TestCommitRestoresLinks sets commitHook, so it does not run in parallel.
func TestCommitRestoresLinks(t *testing.T) {
	origHook := commitHook
	t.Cleanup(func() { commitHook = origHook })
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"target.txt": "one\n", "e.txt": "e\n"})
	for _, link := range []string{"link.txt", "dlink.txt"} {
		if err := os.Symlink("target.txt", filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var changes []fileChange
	for _, c := range []struct {
		rel    string
		data   []byte
		remove bool
	}{{"link.txt", []byte("changed\n"), false}, {"dlink.txt", nil, true}, {"e.txt", nil, true}} {
		old, err := root.ReadFile(c.rel)
		if err != nil {
			t.Fatal(err)
		}
		fc, err := statChange(root, c.rel, c.data, old)
		if err != nil {
			t.Fatal(err)
		}
		fc.remove = c.remove
		changes = append(changes, fc)
	}
	commitHook = func(rel string) error {
		if rel == "e.txt" {
			return errors.New("injected failure")
		}
		return nil
	}
	err = commit(root, changes)
	commitHook = nil
	if err == nil {
		t.Fatal("commit succeeded")
	}
	for _, link := range []string{"link.txt", "dlink.txt"} {
		if target, err := os.Readlink(filepath.Join(dir, link)); err != nil || target != "target.txt" {
			t.Errorf("%s: %q, %v", link, target, err)
		}
	}
	if got := snapshot(t, dir); got["target.txt"] != "one\n" || got["e.txt"] != "e\n" {
		t.Fatalf("files = %q", got)
	}
}

func TestApplyPatchOutputIsBounded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var old, body strings.Builder
	for i := range 3000 {
		old.WriteString("line " + strings.Repeat("x", i%7) + "\n")
	}
	writeFiles(t, dir, map[string]string{"big.txt": old.String()})
	body.WriteString("*** Add File: big.txt\n")
	for range 3000 {
		body.WriteString("+changed line\n")
	}
	res := call(t, ApplyPatch(Config{Dir: dir, MaxOutput: 5000}), map[string]any{"input": wrapPatch(strings.TrimSuffix(body.String(), "\n"))})
	if res.IsError || len(res.Content) > 5200 || !strings.Contains(res.Content, "truncated") {
		t.Fatalf("%d bytes: %.300s", len(res.Content), res.Content)
	}
}
