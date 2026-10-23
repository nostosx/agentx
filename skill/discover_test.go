package skill_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nostosx/agentx/skill"
)

func writeSkill(t *testing.T, folder, dir, name, description string) string {
	t.Helper()
	p := filepath.Join(folder, dir)
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatal(err)
	}
	front := "---\n"
	if name != "" {
		front += "name: " + name + "\n"
	}
	if err := os.WriteFile(filepath.Join(p, skill.FileName), []byte(front+"description: "+description+"\n---\nBody of "+description), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func descriptions(d *skill.Discovery) map[string]string {
	out := map[string]string{}
	for _, s := range d.Skills {
		out[s.Metadata().Name] = s.Metadata().Description
	}
	return out
}

func TestDiscover(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	cwd := filepath.Join(repo, "packages", "app")
	home := filepath.Join(base, "home")
	managed := filepath.Join(base, "managed")
	extra := filepath.Join(base, "extra")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Above the repository root: not searched.
	writeSkill(t, filepath.Join(base, ".claude", "skills"), "outside", "", "outside the root")

	writeSkill(t, filepath.Join(managed, ".claude", "skills"), "policy", "", "managed policy")
	writeSkill(t, filepath.Join(home, ".claude", "skills"), "deploy", "", "user deploy")
	writeSkill(t, filepath.Join(home, ".agents", "skills"), "deploy", "", "user agents deploy")
	writeSkill(t, filepath.Join(home, ".agents", "skills"), "notes", "", "user notes")
	writeSkill(t, filepath.Join(repo, ".claude", "skills"), "deploy", "", "project deploy")
	writeSkill(t, filepath.Join(repo, ".claude", "skills"), "policy", "", "project policy")
	writeSkill(t, filepath.Join(repo, ".claude", "skills"), "lint", "", "root lint")
	writeSkill(t, filepath.Join(cwd, ".agents", "skills"), "lint", "", "package lint")
	writeSkill(t, filepath.Join(cwd, ".claude", "skills"), "build", "", "package build")
	writeSkill(t, filepath.Join(cwd, ".claude", "skills"), "synced", "", "synced from claude.ai")
	writeSkill(t, filepath.Join(cwd, ".claude", "skills", "Synced"), "inner", "", "synced inner")
	writeSkill(t, filepath.Join(extra, ".claude", "skills"), "extra", "", "additional")
	writeSkill(t, filepath.Join(extra, ".claude", "skills"), "build", "", "additional build")
	// Nested below the working directory.
	writeSkill(t, filepath.Join(cwd, "web", ".claude", "skills"), "build", "", "web build")
	writeSkill(t, filepath.Join(cwd, "web", ".claude", "skills"), "serve", "", "web serve")
	writeSkill(t, filepath.Join(cwd, "node_modules", "x", ".claude", "skills"), "dep", "", "dependency")
	writeSkill(t, filepath.Join(cwd, ".hidden", ".claude", "skills"), "hidden", "", "hidden")
	// The same skill through a symbolic link loads once.
	shared := writeSkill(t, filepath.Join(base, "shared"), "shared", "", "shared")
	if err := os.MkdirAll(filepath.Join(repo, ".agents", "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	linked := os.Symlink(shared, filepath.Join(repo, ".claude", "skills", "shared")) == nil &&
		os.Symlink(shared, filepath.Join(repo, ".agents", "skills", "shared")) == nil

	d, err := skill.Discover(skill.DiscoverConfig{Dir: cwd})
	if err != nil {
		t.Fatal(err)
	}
	got := descriptions(d)
	want := map[string]string{"deploy": "project deploy", "policy": "project policy", "lint": "package lint", "build": "package build"}
	if linked {
		want["shared"] = "shared"
	}
	if !maps(got, want) {
		t.Errorf("default Discover() = %v, want %v", got, want)
	}
	if src := d.Sources["lint"]; src.Scope != skill.ScopeProject || src.Folder != filepath.Join(cwd, ".agents", "skills") {
		t.Errorf("lint source = %+v", src)
	}
	if !slices.ContainsFunc(d.Conflicts, func(c skill.Conflict) bool {
		return c.Name == "lint" && c.Shadowed == filepath.Join(repo, ".claude", "skills", "lint")
	}) || slices.ContainsFunc(d.Conflicts, func(c skill.Conflict) bool { return c.Name == "shared" }) {
		t.Errorf("conflicts = %+v", d.Conflicts)
	}

	d, err = skill.Discover(skill.DiscoverConfig{Dir: cwd, Home: home, Managed: managed, AdditionalDirs: []string{extra}, Nested: true})
	if err != nil {
		t.Fatal(err)
	}
	got = descriptions(d)
	want = map[string]string{
		"policy":    "managed policy",
		"deploy":    "user deploy",
		"notes":     "user notes",
		"lint":      "package lint",
		"build":     "package build",
		"extra":     "additional",
		"web:build": "web build (Applies to files in web.)",
		"serve":     "web serve",
	}
	if linked {
		want["shared"] = "shared"
	}
	if !maps(got, want) {
		t.Errorf("full Discover() = %v, want %v", got, want)
	}
	for name, scope := range map[string]skill.Scope{"policy": skill.ScopeManaged, "deploy": skill.ScopeUser, "extra": skill.ScopeAdditional, "serve": skill.ScopeNested} {
		if d.Sources[name].Scope != scope {
			t.Errorf("%s scope = %v, want %v", name, d.Sources[name].Scope, scope)
		}
	}
	r := d.Registry()
	serve, _ := r.Get("serve")
	if sc, ok := serve.(skill.Scoped); !ok || sc.ScopeDir() != filepath.Join(cwd, "web") {
		t.Fatalf("nested skill scope = %v", serve)
	}
	if skill.Applies(serve, cwd, []string{"main.go"}) || !skill.Applies(serve, cwd, []string{"web/index.ts"}) || !skill.Applies(serve, "", []string{filepath.Join(cwd, "web", "a")}) {
		t.Error("nested skill scope not applied")
	}
	if body, err := serve.Content(t.Context()); err != nil || body != "Body of web serve" {
		t.Errorf("nested Content() = %q, %v", body, err)
	}

	d, err = skill.Discover(skill.DiscoverConfig{Dir: cwd, Home: home, ProjectFirst: true, Root: cwd, Folders: []string{".claude/skills"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := descriptions(d); !maps(got, map[string]string{"build": "package build", "deploy": "user deploy"}) {
		t.Errorf("project-first Discover() = %v", got)
	}
}

func TestDiscoverErrorsAndStrict(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	folder := filepath.Join(dir, ".claude", "skills")
	writeSkill(t, folder, "good", "", "fine")
	writeSkill(t, folder, "Upper", "", "Claude Code accepts this")
	if err := os.MkdirAll(filepath.Join(folder, "broken"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "broken", skill.FileName), []byte("no frontmatter"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := skill.Discover(skill.DiscoverConfig{Dir: dir})
	if err == nil || len(d.Skills) != 2 {
		t.Errorf("Discover() = %v, %v", descriptions(d), err)
	}
	d, err = skill.Discover(skill.DiscoverConfig{Dir: dir, Strict: true})
	if err == nil || len(d.Skills) != 1 || d.Skills[0].Metadata().Name != "good" {
		t.Errorf("strict Discover() = %v, %v", descriptions(d), err)
	}
	if d, err := skill.Discover(skill.DiscoverConfig{Dir: filepath.Join(dir, "missing")}); err != nil || len(d.Skills) != 0 {
		t.Errorf("missing directory = %v, %v", d, err)
	}
}

func maps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
