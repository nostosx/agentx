package builtin

import (
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestGlobSortsByModTime(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"old.go": "", "new.go": "", "mid.go": ""})
	base := time.Unix(1_700_000_000, 0)
	for name, age := range map[string]time.Duration{"old.go": 2 * time.Hour, "mid.go": time.Hour, "new.go": 0} {
		if err := os.Chtimes(filepath.Join(dir, name), base.Add(-age), base.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	res := call(t, Glob(Config{Dir: dir}), map[string]any{"pattern": "*.go"})
	if res.Content != "new.go\nmid.go\nold.go" {
		t.Fatalf("got %q", res.Content)
	}
}

func TestGlobLimit(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{}
	for i := range maxGlobResults + 5 {
		files[filepath.Join("d", strings.Repeat("x", i+1))] = ""
	}
	writeFiles(t, dir, files)
	res := call(t, Glob(Config{Dir: dir}), map[string]any{"pattern": "**"})
	lines := strings.Split(res.Content, "\n")
	if len(lines) != maxGlobResults+1 || !strings.Contains(lines[len(lines)-1], "5 more files") {
		t.Fatalf("got %d lines, last %q", len(lines), lines[len(lines)-1])
	}
}

func TestSearchRespectsGitignore(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		".gitignore":         "# build output\n/dist/\n*.log\n!keep.log\ngen/\n",
		"main.go":            "needle\n",
		"dist/out.go":        "needle\n",
		"app.log":            "needle\n",
		"keep.log":           "needle\n",
		"pkg/gen/x.go":       "needle\n",
		"pkg/dist/y.go":      "needle\n",
		"pkg/.gitignore":     "local.go\n",
		"pkg/local.go":       "needle\n",
		"other/local.go":     "needle\n",
		"docs/notes.md":      "needle\n",
		"docs/sub/notes.md":  "needle\n",
		"docs/.gitignore":    "sub/*.md\n",
		"vendor/lib/lib.go":  "needle\n",
		"vendor/.gitignore":  "*\n!.gitignore\n",
		"vendor/notdir.txt":  "needle\n",
		"escaped/#hash.txt":  "needle\n",
		"escaped/.gitignore": "\\#hash.txt\n",
	})
	cfg := Config{Dir: dir}
	res := call(t, Grep(cfg), map[string]any{"pattern": "needle", "files_only": true})
	got := strings.Split(res.Content, "\n")
	expect := []string{"docs/notes.md", "keep.log", "main.go", "other/local.go", "pkg/dist/y.go"}
	if strings.Join(got, ",") != strings.Join(expect, ",") {
		t.Fatalf("grep files:\ngot  %v\nwant %v", got, expect)
	}
	setModTimes(t, dir, time.Unix(1_700_000_000, 0))
	res = call(t, Glob(cfg), map[string]any{"pattern": "**/*.go"})
	if res.Content != "main.go\nother/local.go\npkg/dist/y.go" {
		t.Fatalf("glob: %q", res.Content)
	}
	res = call(t, Glob(cfg), map[string]any{"pattern": "**/*.go", "include_ignored": true})
	if !strings.Contains(res.Content, "dist/out.go") || !strings.Contains(res.Content, "vendor/lib/lib.go") {
		t.Fatalf("include_ignored: %q", res.Content)
	}
	// A path named explicitly is searched even when it is ignored.
	res = call(t, Grep(cfg), map[string]any{"pattern": "needle", "path": "dist"})
	if res.Content != "dist/out.go:1:needle" {
		t.Fatalf("explicit path: %q", res.Content)
	}
}

func TestGrepEmptyLinesAndContext(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"a.txt": "one\n\nthree\nfour\nfive\nsix\nseven\nhit\nnine\n",
		"b.txt": "hit\nb2\n",
	})
	g := Grep(Config{Dir: dir})
	res := call(t, g, map[string]any{"pattern": "^$"})
	if res.Content != "a.txt:2:" {
		t.Fatalf("empty line: %q", res.Content)
	}
	res = call(t, g, map[string]any{"pattern": "^(three|hit)$", "context": 1})
	want := strings.Join([]string{
		"a.txt-2-",
		"a.txt:3:three",
		"a.txt-4-four",
		"--",
		"a.txt-7-seven",
		"a.txt:8:hit",
		"a.txt-9-nine",
		"b.txt:1:hit",
		"b.txt-2-b2",
	}, "\n")
	if res.Content != want {
		t.Fatalf("context:\ngot  %q\nwant %q", res.Content, want)
	}
	res = call(t, g, map[string]any{"pattern": "^(three|four)$", "context": 1, "path": "a.txt"})
	if res.Content != "a.txt-2-\na.txt:3:three\na.txt:4:four\na.txt-5-five" {
		t.Fatalf("adjacent matches: %q", res.Content)
	}
}

func TestIgnoreTrailingDoubleStarKeepsDirectory(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		".gitignore":       "foo/**\n!foo/keep.txt\n",
		"foo/drop.txt":     "needle\n",
		"foo/deep/x.txt":   "needle\n",
		"foo/keep.txt":     "needle\n",
		"other/keep.txt":   "needle\n",
		"other/foo/x.txt":  "needle\n",
		"other/.gitignore": "",
	})
	res := call(t, Grep(Config{Dir: dir}), map[string]any{"pattern": "needle", "files_only": true})
	if want := "foo/keep.txt\nother/foo/x.txt\nother/keep.txt"; res.Content != want {
		t.Fatalf("got %q, want %q", res.Content, want)
	}
}

// gitCmd runs git with the user's configuration and global excludes
// disabled.
func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := osexec.Command("git", append([]string{"-C", dir}, args...)...)
	home := t.TempDir()
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

func TestIgnoreMatchesGit(t *testing.T) {
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := t.TempDir()
	gitCmd(t, repo, "init", "-q")
	writeFiles(t, repo, map[string]string{
		".gitignore":           "foo/**\n!foo/keep.txt\n*.log\n/sub/anchored.txt\n!sub/deeper/keep.log\nbuild/\n",
		".git/info/exclude":    "secret.txt\n",
		"foo/a.txt":            "x",
		"foo/keep.txt":         "x",
		"foo/deep/b.txt":       "x",
		"top.log":              "x",
		"sub/.gitignore":       "local.txt\n!*.keep\n",
		"sub/x.log":            "x",
		"sub/anchored.txt":     "x",
		"sub/secret.txt":       "x",
		"sub/local.txt":        "x",
		"sub/keep.go":          "x",
		"sub/foo/c.txt":        "x",
		"sub/build/out.go":     "x",
		"sub/deeper/y.log":     "x",
		"sub/deeper/keep.log":  "x",
		"sub/deeper/z.go":      "x",
		"sub/deeper/local.txt": "x",
	})
	for _, dir := range []string{repo, filepath.Join(repo, "sub"), filepath.Join(repo, "sub", "deeper")} {
		want := strings.Fields(gitCmd(t, dir, "ls-files", "--others", "--exclude-standard"))
		res := call(t, Glob(Config{Dir: dir}), map[string]any{"pattern": "**"})
		got := strings.Split(res.Content, "\n")
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s:\nglob %v\ngit  %v", dir, got, want)
		}
	}
}
