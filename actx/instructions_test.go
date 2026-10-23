package actx_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nostosx/agentx/actx"
)

// writeFiles creates files under dir; names use slashes.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
}

func loadSections(t *testing.T, cfg actx.InstructionConfig) []actx.Section {
	t.Helper()
	got, err := actx.InstructionFiles(cfg).Sections(t.Context(), &actx.Input{})
	if err != nil {
		t.Fatalf("Sections() error = %v", err)
	}
	return got
}

// labels returns the section names.
func labels(sections []actx.Section) []string {
	var out []string
	for _, s := range sections {
		out = append(out, s.Name)
	}
	return out
}

func checkLabels(t *testing.T, sections []actx.Section, want ...string) {
	t.Helper()
	if got := labels(sections); !reflect.DeepEqual(got, want) {
		t.Fatalf("loaded %q, want %q", got, want)
	}
}

func TestInstructionFilesHierarchy(t *testing.T) {
	t.Parallel()
	outer := t.TempDir()
	root := filepath.Join(outer, "repo")
	writeFiles(t, outer, map[string]string{
		"AGENTS.md":             "outside the project",
		"repo/.git/HEAD":        "ref: refs/heads/main",
		"repo/AGENTS.md":        "root rules\n",
		"repo/a/CLAUDE.md":      "a rules",
		"repo/a/b/AGENTS.md":    "b rules",
		"repo/a/b/c/README.md":  "not an instruction file",
		"repo/a/sibling/AGENTS": "unrelated",
	})
	dir := filepath.Join(root, "a", "b", "c")
	got := loadSections(t, actx.InstructionConfig{Dir: dir, Priority: 7})
	checkLabels(t, got, "AGENTS.md", "a/CLAUDE.md", "a/b/AGENTS.md")
	want := actx.Section{Name: "a/CLAUDE.md", Content: "Contents of a/CLAUDE.md (project instructions):\n\na rules", Placement: actx.PlaceSystem, Priority: 7}
	if got[1] != want {
		t.Errorf("section = %+v, want %+v", got[1], want)
	}

	m := actx.NewManager(actx.Config{Providers: []actx.Provider{actx.InstructionFiles(actx.InstructionConfig{Dir: dir})}})
	out, err := m.Prepare(t.Context(), "base", &actx.Input{})
	if err != nil {
		t.Fatal(err)
	}
	wantSystem := "base\n\n" +
		"Contents of AGENTS.md (project instructions):\n\nroot rules\n\n" +
		"Contents of a/CLAUDE.md (project instructions):\n\na rules\n\n" +
		"Contents of a/b/AGENTS.md (project instructions):\n\nb rules"
	if out.System != wantSystem || out.Reminder != "" {
		t.Errorf("Prepare() = %+v, want system %q", out, wantSystem)
	}

	// A .git file, as in a git worktree, marks the root too.
	wt := filepath.Join(root, "a", "wt")
	writeFiles(t, wt, map[string]string{".git": "gitdir: elsewhere", "AGENTS.md": "worktree", "x/AGENTS.md": "nested"})
	checkLabels(t, loadSections(t, actx.InstructionConfig{Dir: filepath.Join(wt, "x")}), "AGENTS.md", "x/AGENTS.md")
}

func TestInstructionFilesPrecedence(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		".git/HEAD":                 "",
		"AGENTS.override.md":        "override",
		"AGENTS.md":                 "shadowed",
		"CLAUDE.md":                 "shadowed",
		"a/AGENTS.md":               "a agents",
		"a/CLAUDE.md":               "shadowed",
		"a/CLAUDE.local.md":         "a local",
		"a/b/CLAUDE.md":             "b claude",
		"a/b/c/AGENTS.override.md":  " \n",
		"a/b/c/AGENTS.md":           "shadowed by an empty override",
		"a/b/c/d/.claude/CLAUDE.md": "dot claude",
	})
	if err := os.Mkdir(filepath.Join(root, "a", "b", "c", "d", "AGENTS.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "a", "b", "c", "d")
	got := loadSections(t, actx.InstructionConfig{Dir: dir})
	checkLabels(t, got, "AGENTS.override.md", "a/AGENTS.md", "a/b/CLAUDE.md")
	if !strings.HasSuffix(got[0].Content, "\n\noverride") {
		t.Errorf("content = %q", got[0].Content)
	}

	got = loadSections(t, actx.InstructionConfig{
		Dir:        dir,
		Names:      []string{"CLAUDE.md", ".claude/CLAUDE.md", "AGENTS.md"},
		LocalNames: []string{"CLAUDE.local.md"},
	})
	checkLabels(t, got, "CLAUDE.md", "a/CLAUDE.md", "a/CLAUDE.local.md", "a/b/CLAUDE.md", "a/b/c/AGENTS.md", "a/b/c/d/.claude/CLAUDE.md")
	if want := "Contents of a/CLAUDE.local.md (local instructions):\n\na local"; got[2].Content != want {
		t.Errorf("local content = %q, want %q", got[2].Content, want)
	}

	for _, name := range []string{"", "../AGENTS.md", "/etc/AGENTS.md", "a/../../x"} {
		_, err := actx.InstructionFiles(actx.InstructionConfig{Dir: dir, Names: []string{name}}).Sections(t.Context(), &actx.Input{})
		if err == nil {
			t.Errorf("name %q accepted", name)
		}
		_, err = actx.InstructionFiles(actx.InstructionConfig{Dir: dir, LocalNames: []string{name}}).Sections(t.Context(), &actx.Input{})
		if err == nil {
			t.Errorf("local name %q accepted", name)
		}
	}
}

func TestInstructionFilesRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		".git/HEAD":         "",
		"AGENTS.md":         "root",
		"sub/AGENTS.md":     "sub",
		"other/AGENTS.md":   "other",
		"other/x/AGENTS.md": "x",
		"other/.marker":     "",
	})
	sub := filepath.Join(root, "sub")
	// An empty marker list disables parent traversal.
	checkLabels(t, loadSections(t, actx.InstructionConfig{Dir: sub, RootMarkers: []string{}}), "AGENTS.md")
	// Without a marker in any ancestor, only Dir is searched.
	got := loadSections(t, actx.InstructionConfig{Dir: sub, RootMarkers: []string{"no-such-agentx-marker"}})
	checkLabels(t, got, "AGENTS.md")
	if !strings.HasSuffix(got[0].Content, "\n\nsub") {
		t.Errorf("content = %q", got[0].Content)
	}
	// The nearest marker wins.
	checkLabels(t, loadSections(t, actx.InstructionConfig{Dir: filepath.Join(root, "other", "x"), RootMarkers: []string{".marker", ".git"}}), "AGENTS.md", "x/AGENTS.md")

	// Relative Dir.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, sub)
	if err != nil {
		t.Skip("no relative path to the temporary directory")
	}
	checkLabels(t, loadSections(t, actx.InstructionConfig{Dir: rel}), "AGENTS.md", "sub/AGENTS.md")
}

func TestInstructionFilesEmptyAndErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{".git/HEAD": "", "README.md": "x", "AGENTS.md": "\n\t \n"})
	got, err := actx.InstructionFiles(actx.InstructionConfig{Dir: dir}).Sections(t.Context(), &actx.Input{})
	if err != nil || got != nil {
		t.Fatalf("Sections() = %v, %v; want none", got, err)
	}
	if _, err := actx.InstructionFiles(actx.InstructionConfig{Dir: filepath.Join(dir, "missing")}).Sections(t.Context(), &actx.Input{}); err == nil {
		t.Error("missing Dir accepted")
	}
	if _, err := actx.InstructionFiles(actx.InstructionConfig{Dir: filepath.Join(dir, "README.md")}).Sections(t.Context(), &actx.Input{}); err == nil {
		t.Error("file as Dir accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := actx.InstructionFiles(actx.InstructionConfig{Dir: dir}).Sections(ctx, &actx.Input{}); err == nil {
		t.Error("canceled context ignored")
	}
}

func TestInstructionFilesReadFreshEachTime(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{".git/HEAD": "", "AGENTS.md": "first"})
	p := actx.InstructionFiles(actx.InstructionConfig{Dir: dir})
	for _, want := range []string{"first", "second"} {
		writeFiles(t, dir, map[string]string{"AGENTS.md": want})
		got, err := p.Sections(t.Context(), &actx.Input{})
		if err != nil || len(got) != 1 || !strings.HasSuffix(got[0].Content, want) {
			t.Fatalf("Sections() = %+v, %v; want %q", got, err, want)
		}
	}
}

func TestInstructionFilesMaxBytes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		".git/HEAD":     "",
		"AGENTS.md":     "0123456789",
		"a/AGENTS.md":   "abcdefghij",
		"a/b/AGENTS.md": "omitted",
	})
	dir := filepath.Join(root, "a", "b")
	got := loadSections(t, actx.InstructionConfig{Dir: dir, MaxBytes: 15})
	checkLabels(t, got, "AGENTS.md", "a/AGENTS.md")
	if want := "Contents of a/AGENTS.md (project instructions):\n\nabcde\n\n[Instruction files truncated: they exceed the 15-byte limit.]"; got[1].Content != want {
		t.Errorf("truncated content = %q, want %q", got[1].Content, want)
	}

	// Exactly at the limit: the next file is truncated to nothing.
	got = loadSections(t, actx.InstructionConfig{Dir: dir, MaxBytes: 10})
	checkLabels(t, got, "AGENTS.md")
	if !strings.HasSuffix(got[0].Content, "0123456789\n\n[Instruction files truncated: they exceed the 10-byte limit.]") {
		t.Errorf("content = %q", got[0].Content)
	}

	// Truncation keeps whole UTF-8 characters.
	utf := t.TempDir()
	writeFiles(t, utf, map[string]string{"AGENTS.md": "日本語"})
	got = loadSections(t, actx.InstructionConfig{Dir: utf, MaxBytes: 5})
	if want := "Contents of AGENTS.md (project instructions):\n\n日\n\n[Instruction files truncated: they exceed the 5-byte limit.]"; len(got) != 1 || got[0].Content != want {
		t.Errorf("sections = %+v, want %q", got, want)
	}
	got = loadSections(t, actx.InstructionConfig{Dir: utf, MaxBytes: 2})
	if want := "[Instruction files truncated: they exceed the 2-byte limit.]"; len(got) != 1 || got[0].Content != want {
		t.Errorf("sections = %+v, want only the note", got)
	}

	// The default is 32 KiB; a negative limit disables it.
	big := t.TempDir()
	writeFiles(t, big, map[string]string{"AGENTS.md": strings.Repeat("z", 40<<10)})
	got = loadSections(t, actx.InstructionConfig{Dir: big})
	if n := strings.Count(got[0].Content, "z"); n != actx.DefaultInstructionMaxBytes || actx.DefaultInstructionMaxBytes != 32<<10 {
		t.Errorf("default limit kept %d bytes", n)
	}
	got = loadSections(t, actx.InstructionConfig{Dir: big, MaxBytes: -1})
	if n := strings.Count(got[0].Content, "z"); n != 40<<10 || strings.Contains(got[0].Content, "truncated") {
		t.Errorf("unlimited kept %d bytes", n)
	}
}

func TestInstructionFilesImports(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		".git/HEAD": "",
		"AGENTS.md": "See @docs/style.md, @missing.md and @docs/. (@docs/nested/deep.md)\n" +
			"Also (see @paren.md).\n" +
			"```\n@docs/fenced.md\n```\n" +
			"Inline `@docs/span.md` text, mail@docs/email.md, @\"docs/quoted.md\".\n" +
			"- @Design\\ Docs/api.md\n" +
			"- @" + filepath.ToSlash(filepath.Join(root, "abs.md")) + "\n",
		"docs/style.md":       "style @nested/deep.md",
		"docs/nested/deep.md": "deep",
		"docs/fenced.md":      "fenced",
		"docs/span.md":        "span",
		"docs/email.md":       "email",
		"docs/quoted.md":      "quoted",
		"Design Docs/api.md":  "api",
		"abs.md":              "absolute",
		"paren.md":            "paren",
		"sub/AGENTS.md":       "sub @../docs/style.md @../shared.md",
		"shared.md":           "shared",
	})
	got := loadSections(t, actx.InstructionConfig{Dir: filepath.Join(root, "sub")})
	checkLabels(t, got, "AGENTS.md", "docs/style.md", "docs/nested/deep.md", "paren.md", "Design Docs/api.md", "abs.md", "sub/AGENTS.md", "shared.md")
	if want := "Contents of docs/nested/deep.md (imported by docs/style.md):\n\ndeep"; got[2].Content != want {
		t.Errorf("import content = %q, want %q", got[2].Content, want)
	}
	if want := "Contents of shared.md (imported by sub/AGENTS.md):\n\nshared"; got[7].Content != want {
		t.Errorf("import content = %q, want %q", got[7].Content, want)
	}
}

func TestInstructionFilesImportDepthAndCycles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"AGENTS.md": "@1.md",
		"1.md":      "@2.md",
		"2.md":      "@3.md",
		"3.md":      "@4.md",
		"4.md":      "@5.md",
		"5.md":      "five",
	})
	checkLabels(t, loadSections(t, actx.InstructionConfig{Dir: root}), "AGENTS.md", "1.md", "2.md", "3.md", "4.md")
	checkLabels(t, loadSections(t, actx.InstructionConfig{Dir: root, ImportDepth: 1}), "AGENTS.md", "1.md")
	checkLabels(t, loadSections(t, actx.InstructionConfig{Dir: root, ImportDepth: -1}), "AGENTS.md")
	checkLabels(t, loadSections(t, actx.InstructionConfig{Dir: root, ImportDepth: 9}), "AGENTS.md", "1.md", "2.md", "3.md", "4.md", "5.md")

	cyc := t.TempDir()
	writeFiles(t, cyc, map[string]string{
		"AGENTS.md": "@a.md @a.md @./AGENTS.md",
		"a.md":      "@b.md @AGENTS.md",
		"b.md":      "@a.md @c.md",
		"c.md":      "@b.md",
	})
	checkLabels(t, loadSections(t, actx.InstructionConfig{Dir: cyc, ImportDepth: 100}), "AGENTS.md", "a.md", "b.md", "c.md")

	// A file imported by a parent is not loaded again for its own directory.
	nested := t.TempDir()
	writeFiles(t, nested, map[string]string{".git/HEAD": "", "AGENTS.md": "@sub/AGENTS.md", "sub/AGENTS.md": "sub", "sub/CLAUDE.md": "not a fallback"})
	checkLabels(t, loadSections(t, actx.InstructionConfig{Dir: filepath.Join(nested, "sub")}), "AGENTS.md", "sub/AGENTS.md")
}

func TestInstructionFilesConfinement(t *testing.T) {
	t.Parallel()
	outer := t.TempDir()
	root := filepath.Join(outer, "repo")
	ext := filepath.Join(outer, "ext")
	secret := filepath.Join(outer, "secret.md")
	writeFiles(t, outer, map[string]string{
		"secret.md":          "SECRET",
		"ext/shared.md":      "shared",
		"repo/.git/HEAD":     "",
		"repo/AGENTS.md":     "@../secret.md @" + filepath.ToSlash(secret) + " @docs/../../secret.md @link.md @linkdir/secret.md @inner.md @absolute.md @" + filepath.ToSlash(filepath.Join(ext, "shared.md")),
		"repo/docs/x.md":     "inner",
		"repo/sub/CLAUDE.md": "fallback",
	})
	symlink(t, secret, filepath.Join(root, "link.md"))
	symlink(t, outer, filepath.Join(root, "linkdir"))
	symlink(t, filepath.Join("docs", "x.md"), filepath.Join(root, "inner.md"))
	// Absolute links are refused even inside the root.
	symlink(t, filepath.Join(root, "docs", "x.md"), filepath.Join(root, "absolute.md"))
	// An instruction file that links outside the root is not loaded; the
	// next name is used instead.
	symlink(t, secret, filepath.Join(root, "sub", "AGENTS.md"))

	got := loadSections(t, actx.InstructionConfig{Dir: filepath.Join(root, "sub")})
	checkLabels(t, got, "AGENTS.md", "inner.md", "sub/CLAUDE.md")
	for _, s := range got {
		if strings.Contains(s.Content, "SECRET") {
			t.Errorf("read outside the project: %+v", s)
		}
	}

	// ImportDirs allows imports from another directory, but not escapes
	// from it.
	got = loadSections(t, actx.InstructionConfig{Dir: root, ImportDirs: []string{ext}})
	checkLabels(t, got, "AGENTS.md", "inner.md", filepath.ToSlash(filepath.Join(ext, "shared.md")))

	// Without a root, Dir confines the files.
	sub := filepath.Join(root, "sub")
	writeFiles(t, sub, map[string]string{"CLAUDE.md": "@../docs/x.md"})
	checkLabels(t, loadSections(t, actx.InstructionConfig{Dir: sub, RootMarkers: []string{}}), "CLAUDE.md")
}

func TestInstructionFilesUserFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	root := t.TempDir()
	writeFiles(t, home, map[string]string{
		".codex/AGENTS.md": "user @rules.md @~/.codex/more.md @../outside.md @~/private.md",
		".codex/rules.md":  "user rules",
		".codex/more.md":   "more",
		".codex/dir.md/x":  "a directory named like a file",
		"outside.md":       "OUTSIDE",
		"private.md":       "PRIVATE",
		"shared/s.md":      "shared",
	})
	writeFiles(t, root, map[string]string{
		".git/HEAD": "",
		"AGENTS.md": "project @~/.codex/rules.md @~/shared/s.md @~/private.md",
	})
	cfg := actx.InstructionConfig{
		Dir:       root,
		UserFiles: []string{"~/.codex/AGENTS.md", "~/missing.md", "~/.codex/dir.md", "~/.codex/rules.md"},
	}
	got := loadSections(t, cfg)
	checkLabels(t, got, "~/.codex/AGENTS.md", "~/.codex/rules.md", "~/.codex/more.md", "AGENTS.md")
	if want := "Contents of ~/.codex/AGENTS.md (user instructions):\n\nuser @rules.md @~/.codex/more.md @../outside.md @~/private.md"; got[0].Content != want {
		t.Errorf("user content = %q, want %q", got[0].Content, want)
	}
	if want := "Contents of ~/.codex/rules.md (imported by ~/.codex/AGENTS.md):\n\nuser rules"; got[1].Content != want {
		t.Errorf("import content = %q, want %q", got[1].Content, want)
	}

	cfg.ImportDirs = []string{"~/shared"}
	checkLabels(t, loadSections(t, cfg), "~/.codex/AGENTS.md", "~/.codex/rules.md", "~/.codex/more.md", "AGENTS.md", "~/shared/s.md")

	// An absolute user file is labeled with its path.
	abs := filepath.Join(home, "private.md")
	got = loadSections(t, actx.InstructionConfig{Dir: root, UserFiles: []string{abs}, ImportDepth: -1})
	checkLabels(t, got, filepath.ToSlash(abs), "AGENTS.md")
}
