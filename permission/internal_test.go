package permission

import (
	"runtime"
	"slices"
	"testing"
)

func TestLex(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		in   string
		want []string // the first form of each command
		bad  bool
	}{
		{"a && b || c; d | e |& f & g\nh", []string{"a", "b", "c", "d", "e", "f", "g", "h"}, false},
		{`echo "a && b" 'c | d' e\ f`, []string{`echo "a && b" 'c | d' e\ f`}, false},
		{"echo $(a; b) `c`", []string{"a", "b", "c", "echo $(a; b) `c`"}, false},
		{"echo \"$(a \"x\")\"", []string{`a "x"`, "echo \"$(a \"x\")\""}, false},
		{"(a && b) | { c; }", []string{"a", "b", "c"}, false},
		{"diff <(a) >(b)", []string{"a", "b", "diff <(a) >(b)"}, false},
		{"a 2>&1 >out <in &>all", []string{"a"}, false},
		{"echo $((1 + 2)) ${x:-y}", []string{"echo $((1 + 2)) ${x:-y}"}, false},
		{"echo $'a\\'b'", []string{"echo $'a\\'b'"}, false},
		{"a \\\n b", []string{"a b"}, false},
		{"a &&\n b", []string{"a", "b"}, false},
		{"cat <<-X\n\tbody; rm\n\tX\nb", []string{"cat", "b"}, false},
		{"while true; do a; done", []string{"true", "a"}, false},
		{"! a", []string{"a"}, false},
		{"A=1 B=2", []string{"A=1 B=2"}, false},
		{"a &&", []string{"a"}, true},
		{"| a", []string{"a"}, true},
		{"a; && b", []string{"a", "b"}, true},
		{"echo 'x", nil, true},
		{"echo \"x", nil, true},
		{"echo $(a", []string{"a", "echo $(a"}, true},
		{"echo `a", nil, true},
		{"f() { a; }", nil, true},
		{"cat <<X\nno end", []string{"cat"}, true},
		// Expansions expand the words inside them.
		{"echo ${x:-$(a)}", []string{"a", "echo ${x:-$(a)}"}, false},
		{`echo "${x:-$(a)}"`, []string{"a", `echo "${x:-$(a)}"`}, false},
		{"echo ${x/y/`a`}", []string{"a", "echo ${x/y/`a`}"}, false},
		{"echo ${x:-${y:-$(a)}}", []string{"a", "echo ${x:-${y:-$(a)}}"}, false},
		{"echo $(( $(a) + 1 ))", []string{"a", "echo $(( $(a) + 1 ))"}, false},
		{"echo ${x:-'$(a)'}", []string{"echo ${x:-'$(a)'}"}, false},
		{"echo ${x:-<(a)}", nil, true},
		// The body of a here-document with an unquoted delimiter is
		// expanded; a quoted delimiter or an escape prevents it.
		{"cat <<EOF\n$(a)\nEOF", []string{"cat", "a"}, false},
		{"cat <<EOF\nx ${y:-`a`}\nEOF\nb", []string{"cat", "a", "b"}, false},
		{"cat <<'EOF'\n$(a)\nEOF", []string{"cat"}, false},
		{"cat <<\\EOF\n$(a)\nEOF", []string{"cat"}, false},
		{"cat <<EOF\n\\$(a)\nEOF", []string{"cat"}, false},
		{"cat <<EOF\n$(a\nEOF", nil, true},
		{"case x in y) a;; esac", nil, true},
	} {
		sc := lex(tt.in)
		var got []string
		for _, c := range sc.cmds {
			got = append(got, prepare(c).forms[0])
		}
		if sc.bad != tt.bad || (tt.want != nil || !tt.bad) && !slices.Equal(got, tt.want) {
			t.Errorf("lex(%q) = %q, bad=%v; want %q, bad=%v", tt.in, got, sc.bad, tt.want, tt.bad)
		}
	}
}

func TestWordFlags(t *testing.T) {
	t.Parallel()
	cmds := lex(`x *.go '*.md' "$HOME" ~/a "$(pwd)"`).cmds
	w := cmds[len(cmds)-1].words
	if !w[1].glob || w[2].glob || !w[3].expand || !w[4].expand || !w[5].onlySubst || w[3].onlySubst {
		t.Fatalf("words = %+v", w)
	}
	if w[2].lit != "*.md" || w[3].lit != "$HOME" {
		t.Fatalf("literals = %q %q", w[2].lit, w[3].lit)
	}
}

func TestPrepareWrappers(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ in, text string }{
		{"timeout -s KILL 5 make", "make"},
		{"timeout --kill-after=1 5s make", "make"},
		{"timeout 5", "timeout 5"},
		{"timeout -x 5 make", "timeout -x 5 make"},
		{"nice -10 make", "make"},
		{"nice --adjustment=3 make", "make"},
		{"stdbuf -o L -eL make", "make"},
		{"time -p make", "make"},
		{"command -p make", "make"},
		{"LANG=C LC_ALL=C make", "make"},
		{"X=1 make", "X=1 make"},
	} {
		s := prepare(lex(tt.in).cmds[0])
		if s.text != tt.text {
			t.Errorf("%q: text %q, want %q", tt.in, s.text, tt.text)
		}
	}
}

func TestSettingsBase(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX paths")
	}
	for _, tt := range []struct{ path, base, project string }{
		{"/home/u/.claude/settings.json", "/home/u/.claude", "/home/u"},
		{"/p/.claude/settings.json", "", "/p"},
		{"/p/.claude/settings.local.json", "", "/p"},
		{"/etc/claude/managed.json", "/etc/claude", "/etc/claude"},
	} {
		base, project := settingsBase(tt.path, "/home/u")
		if base != tt.base || project != tt.project {
			t.Errorf("%s: %q %q, want %q %q", tt.path, base, project, tt.base, tt.project)
		}
	}
}

func TestDomainPattern(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"", "a..b", "http://x", "a:80"} {
		if _, err := parseDomain(s); err == nil {
			t.Errorf("parseDomain(%q): no error", s)
		}
	}
}

func TestCaseFolding(t *testing.T) {
	t.Parallel()
	dir := []string{"/Work/Repo"}
	compile := func(spec string, fold bool) *pathPattern {
		ps, err := parsePathSpec(spec, listDeny)
		if err != nil {
			t.Fatal(err)
		}
		ps.fold = fold
		return ps.compile(dir, dir, dir)
	}
	for _, tt := range []struct {
		spec, path string
		fold, want bool
	}{
		{"./.env", "/Work/Repo/.ENV", true, true},
		{"./.env", "/work/repo/.env", true, true},
		{"./.env", "/Work/Repo/.ENV", false, false},
		{"secrets/**", "/Work/Repo/a/Secrets/key", true, true},
		{"*.PEM", "/Work/Repo/id.pem", true, true},
		{"./[A-Z]*.md", "/Work/Repo/readme.md", true, true},
	} {
		if got := compile(tt.spec, tt.fold).match(tt.path, false); got != tt.want {
			t.Errorf("%s (fold %v) matches %s = %v, want %v", tt.spec, tt.fold, tt.path, got, tt.want)
		}
	}
	if dir[0] != "/Work/Repo" {
		t.Errorf("folding changed the shared anchors: %q", dir)
	}
	for _, rel := range []string{".GIT/hooks/pre-commit", "/.Claude/settings.json", "sub/.BASHRC"} {
		if !protectedPath(rel, true) {
			t.Errorf("%s is not protected when folding", rel)
		}
		if protectedPath(rel, false) {
			t.Errorf("%s is protected without folding", rel)
		}
	}
	if protectedPath(".Claude/worktrees/x/a.go", true) {
		t.Error("worktree protected when folding")
	}
}
