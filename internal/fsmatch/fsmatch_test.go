package fsmatch

import (
	"testing"
	"testing/fstest"
)

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"**", "a/b/c", true},
		{"**/c", "c", true},
		{"a/**/c", "a/c", true},
		{"a/**/c", "a/x/y/c", true},
		{"a/**/c", "a/x/y/d", false},
		{"*.go", "a/b.go", false},
		{"{a,b}/*.go", "b/x.go", true},
		{"**/*.{md,txt}", "docs/x/y.txt", true},
	}
	for _, c := range cases {
		g, err := CompileGlob(c.pattern)
		if err != nil {
			t.Fatal(err)
		}
		if got := g.Match(c.name); got != c.want {
			t.Errorf("Match(%q, %q) = %v", c.pattern, c.name, got)
		}
	}
	if _, err := CompileGlob("a/[x"); err == nil {
		t.Error("CompileGlob accepted an invalid pattern")
	}
}

func TestParseIgnore(t *testing.T) {
	rules := ParseIgnore("a/**/b\n[!x]y\ntrail\\ \n  \n")
	cases := []struct {
		name  string
		isDir bool
		want  bool
	}{
		{"a/b", false, true},
		{"a/x/y/b", false, true},
		{"c/a/b", false, false},
		{"zy", false, true},
		{"xy", false, false},
		{"trail ", false, true},
	}
	for _, c := range cases {
		got := false
		for _, r := range rules {
			if r.Match(c.name, c.isDir) {
				got = !r.Negate
			}
		}
		if got != c.want {
			t.Errorf("%q: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIgnorer(t *testing.T) {
	fsys := fstest.MapFS{
		".gitignore":     {Data: []byte("*.log\nbuild/\n")},
		"sub/.gitignore": {Data: []byte("!keep.log\nlocal.txt\n")},
	}
	outer := OuterRules{Prefix: "repo", Rules: ParseIgnore("/repo/secret.txt\n")}
	ig := NewIgnorer(fsys, outer)
	cases := []struct {
		name  string
		isDir bool
		want  bool
	}{
		{"a.log", false, true},
		{"sub/keep.log", false, false},
		{"sub/other.log", false, true},
		{"sub/local.txt", false, true},
		{"local.txt", false, false},
		{"build", true, true},
		{"build", false, false},
		{"secret.txt", false, true},
		{"sub/secret.txt", false, false},
	}
	for _, c := range cases {
		if got := ig.Ignored(c.name, c.isDir); got != c.want {
			t.Errorf("Ignored(%q, %v) = %v, want %v", c.name, c.isDir, got, c.want)
		}
	}
}
