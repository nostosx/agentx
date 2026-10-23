package fsmatch

import (
	"bufio"
	"io/fs"
	"path"
	"strings"
)

// Rule is one pattern of a .gitignore file.
type Rule struct {
	segs []string // pattern split on "/", matched like glob segments
	// Negate re-includes a path ("!pattern").
	Negate  bool
	dirOnly bool // "pattern/" matches only directories
	// anchored patterns contain a slash and match paths relative to the
	// .gitignore's directory; others match a file or directory name at any
	// depth.
	anchored bool
}

// ParseIgnore parses .gitignore content. It supports comments, negation,
// directory-only and anchored patterns, "**" and escaped "#", "!" and
// trailing spaces.
func ParseIgnore(content string) []Rule {
	var rules []Rule
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		// Trailing spaces are ignored unless escaped with a backslash.
		for strings.HasSuffix(line, " ") && !strings.HasSuffix(line, `\ `) {
			line = line[:len(line)-1]
		}
		if line == "" || line[0] == '#' {
			continue
		}
		var r Rule
		switch {
		case line[0] == '!':
			r.Negate = true
			line = line[1:]
		case strings.HasPrefix(line, `\!`), strings.HasPrefix(line, `\#`):
			line = line[1:]
		}
		if strings.HasSuffix(line, "/") {
			r.dirOnly = true
			line = strings.TrimRight(line, "/")
		}
		if line == "" {
			continue
		}
		r.anchored = strings.Contains(line, "/")
		line = strings.TrimPrefix(line, "/")
		// gitignore negates character classes with "!", path.Match with "^".
		line = strings.ReplaceAll(line, "[!", "[^")
		r.segs = strings.Split(line, "/")
		if len(r.segs) > 1 && r.segs[len(r.segs)-1] == "**" {
			// A trailing "/**" matches everything inside a directory but
			// not the directory itself, which a later "!dir/file" could
			// otherwise not re-include.
			r.segs = append(r.segs, "*")
		}
		rules = append(rules, r)
	}
	return rules
}

// Match reports whether the rule matches rel, a slash-separated path
// relative to the directory of the rule's .gitignore file.
func (r *Rule) Match(rel string, isDir bool) bool {
	if r.dirOnly && !isDir {
		return false
	}
	if !r.anchored {
		ok, _ := path.Match(r.segs[0], path.Base(rel))
		return ok
	}
	return MatchSegments(r.segs, strings.Split(rel, "/"))
}

// OuterRules are the rules of an ignore file above the root of a tree,
// which apply to paths relative to that file's directory.
type OuterRules struct {
	// Prefix is the slash-separated path of the tree root relative to the
	// ignore file's directory, or "" for the same directory.
	Prefix string
	Rules  []Rule
}

// Ignorer evaluates the .gitignore files of a file tree. Files are loaded
// lazily and cached per directory. It is not safe for concurrent use.
type Ignorer struct {
	fsys  fs.FS
	rules map[string][]Rule
	// outer holds the rules from outside the tree, lowest precedence first.
	outer []OuterRules
}

// NewIgnorer returns an Ignorer for the tree fsys. outer are rules from
// outside the tree, such as a repository's info/exclude file, lowest
// precedence first.
func NewIgnorer(fsys fs.FS, outer ...OuterRules) *Ignorer {
	return &Ignorer{fsys: fsys, rules: map[string][]Rule{}, outer: outer}
}

func (ig *Ignorer) load(dir string) []Rule {
	if rules, ok := ig.rules[dir]; ok {
		return rules
	}
	var rules []Rule
	if b, err := fs.ReadFile(ig.fsys, path.Join(dir, ".gitignore")); err == nil {
		rules = ParseIgnore(string(b))
	}
	ig.rules[dir] = rules
	return rules
}

// Ignored reports whether name, a slash-separated path relative to the
// tree root, is excluded by the ignore files of its ancestors. As in git,
// the last matching pattern wins, deeper files override shallower ones
// and outer rules have the lowest precedence.
//
// Like git, it does not check whether a parent directory is excluded;
// callers walking the tree skip excluded directories instead.
func (ig *Ignorer) Ignored(name string, isDir bool) bool {
	ignored := false
	for _, o := range ig.outer {
		rel := name
		if o.Prefix != "" {
			rel = o.Prefix + "/" + name
		}
		for _, r := range o.Rules {
			if r.Match(rel, isDir) {
				ignored = !r.Negate
			}
		}
	}
	dir := "."
	rest := name
	for {
		for _, r := range ig.load(dir) {
			if r.Match(rest, isDir) {
				ignored = !r.Negate
			}
		}
		seg, after, ok := strings.Cut(rest, "/")
		if !ok {
			return ignored
		}
		dir = path.Join(dir, seg)
		rest = after
	}
}
