package permission

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// pathPattern is a Read or Edit rule's path pattern in gitignore syntax,
// anchored at a directory; see
// https://code.claude.com/docs/en/permissions#read-and-edit.
type pathPattern struct {
	// anchors are the slash-separated absolute directory the pattern is
	// relative to, as given and with symbolic links resolved; "/" for
	// patterns starting with "//".
	anchors []string
	// segs are the pattern's segments, matched like glob segments with
	// "**" matching any number of segments.
	segs []string
	// anchored patterns match paths relative to anchor; others match a
	// file or directory name at any depth below it.
	anchored bool
	dirOnly  bool
	// negate is set for "!pattern", which carves paths out of the rules
	// listed before it.
	negate bool
	// fold makes the match ignore case; see caseInsensitive.
	fold bool
}

// pathSpec is a path specifier before it is anchored: its anchor kind and
// the pattern relative to it.
type pathSpec struct {
	// anchor is "//" (filesystem root), "~" (home), "/" (the rule's
	// source) or "." (the working directory).
	anchor  string
	pattern string
	negate  bool
	// anyDepth makes a single-segment directory pattern such as
	// "secrets/**" match that directory at any depth, as deny and ask
	// rules do.
	anyDepth bool
	// fold makes the pattern ignore case, as deny and ask rules do where
	// the file system ignores it; see caseInsensitive.
	fold bool
}

var errBadPath = errors.New("unusable path pattern")

// caseInsensitive reports whether file names ignore case, as on the default
// file systems of macOS (APFS, HFS+) and Windows (NTFS). There, .GIT/hooks
// and .git/hooks are the same directory, so deny and ask rules and the
// protected paths match regardless of case. Allow rules still match case
// exactly: a path they miss only needs approval.
var caseInsensitive = runtime.GOOS == "darwin" || runtime.GOOS == "windows"

// parsePathSpec splits a path specifier into its anchor and pattern. For
// deny and ask rules, a leading "!" negates the pattern, which is then
// always relative to the working directory.
func parsePathSpec(spec string, list listKind) (pathSpec, error) {
	var ps pathSpec
	ps.fold = list != listAllow && caseInsensitive
	if strings.HasPrefix(spec, "!") {
		if list == listAllow {
			return ps, errors.New(`allow rules cannot negate a path with "!"`)
		}
		ps.negate = true
		ps.pattern = strings.TrimLeft(spec[1:], "/")
		ps.pattern = strings.TrimPrefix(ps.pattern, "./")
		return ps, checkPattern(ps.pattern)
	}
	switch {
	case strings.HasPrefix(spec, "//"):
		ps.anchor, ps.pattern = "//", strings.TrimLeft(spec, "/")
	case spec == "~" || strings.HasPrefix(spec, "~/"):
		ps.anchor, ps.pattern = "~", strings.TrimLeft(strings.TrimPrefix(spec, "~"), "/")
	case strings.HasPrefix(spec, "/"):
		ps.anchor, ps.pattern = "/", strings.TrimLeft(spec, "/")
	case spec == "." || strings.HasPrefix(spec, "./"):
		ps.anchor, ps.pattern = ".", strings.TrimLeft(strings.TrimPrefix(spec, "."), "/")
	default:
		ps.anchor, ps.pattern = "", spec
		if list != listAllow {
			if seg, ok := strings.CutSuffix(spec, "/**"); ok && seg != "" && !strings.Contains(seg, "/") {
				ps.anyDepth = true
			}
		}
	}
	return ps, checkPattern(ps.pattern)
}

// checkPattern reports whether p is usable as a gitignore pattern.
func checkPattern(p string) error {
	for seg := range strings.SplitSeq(strings.TrimSuffix(p, "/"), "/") {
		if _, err := path.Match(seg, ""); err != nil {
			return errBadPath
		}
	}
	return nil
}

// compile anchors ps. base is the directory of the rule's source, dir the
// working directory and home the home directory, each slash-separated and
// absolute, as given and with symbolic links resolved. A pattern that is
// not usable becomes a literal path.
func (ps pathSpec) compile(base, dir, home []string) *pathPattern {
	pp := &pathPattern{negate: ps.negate, fold: ps.fold}
	defer func() {
		if !pp.fold {
			return
		}
		// The anchors are shared with the policy; fold a copy.
		anchors := make([]string, len(pp.anchors))
		for i, a := range pp.anchors {
			anchors[i] = strings.ToLower(a)
		}
		pp.anchors = anchors
		for i, s := range pp.segs {
			pp.segs[i] = strings.ToLower(s)
		}
	}()
	switch ps.anchor {
	case "//":
		pp.anchors = []string{"/"}
	case "~":
		pp.anchors = home
	case "/":
		pp.anchors = base
	default:
		pp.anchors = dir
	}
	p := ps.pattern
	if strings.HasSuffix(p, "/") {
		pp.dirOnly = true
		p = strings.TrimRight(p, "/")
	}
	if p == "" {
		// The anchor's whole tree.
		pp.segs, pp.anchored = []string{"**"}, true
		return pp
	}
	if checkPattern(p) != nil {
		// Deny and ask rules still guard the exact path.
		pp.anchored = true
		for seg := range strings.SplitSeq(path.Clean(p), "/") {
			pp.segs = append(pp.segs, escapeGlob(seg))
		}
		return pp
	}
	// Bare names match at any depth, as in gitignore; patterns starting
	// with an explicit anchor or containing a slash are anchored.
	pp.anchored = ps.anchor != "" || strings.Contains(p, "/")
	pp.segs = strings.Split(path.Clean(p), "/")
	if ps.anyDepth {
		pp.segs = append([]string{"**"}, pp.segs...)
	}
	if len(pp.segs) > 1 && pp.segs[len(pp.segs)-1] == "**" {
		// As in gitignore, a trailing "/**" matches everything inside a
		// directory but not the directory itself.
		pp.segs = append(pp.segs, "*")
	}
	return pp
}

// match reports whether the slash-separated absolute path p matches.
func (pp *pathPattern) match(p string, isDir bool) bool {
	if pp.dirOnly && !isDir {
		return false
	}
	for _, a := range pp.anchors {
		if pp.matchAt(a, p) {
			return true
		}
	}
	return false
}

func (pp *pathPattern) matchAt(anchor, p string) bool {
	if pp.fold {
		p = strings.ToLower(p)
	}
	var rel []string
	switch {
	case p == anchor:
	case anchor == "/":
		rel = strings.Split(strings.TrimPrefix(p, "/"), "/")
	case strings.HasPrefix(p, anchor+"/"):
		rel = strings.Split(p[len(anchor)+1:], "/")
	default:
		return false
	}
	if !pp.anchored {
		if len(rel) == 0 {
			return false
		}
		ok, _ := path.Match(pp.segs[0], rel[len(rel)-1])
		return ok
	}
	return matchSegments(pp.segs, rel)
}

// matchSegments matches path segments against pattern segments, where
// "**" matches any number of segments.
func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			pat = pat[1:]
			if len(pat) == 0 {
				return true
			}
			for i := range len(name) + 1 {
				if matchSegments(pat, name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], name[0]); !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// escapeGlob escapes the glob characters of a literal path segment.
func escapeGlob(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`*?[]\`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// escapePath escapes a slash-separated path for use in a rule, so the
// rule matches only that path. A leading "!" or "#" is escaped as well.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = escapeGlob(s)
	}
	return strings.Join(segs, "/")
}

// excludedBy returns the rule that matches p, evaluating rules in order as
// gitignore evaluates the patterns of one file: the last matching pattern
// wins, a negated pattern re-includes what earlier ones matched, and a
// path inside a matched directory is matched too, whatever later patterns
// say about it. It returns nil if no rule matches.
func excludedBy(rules []*rule, p string, isDir bool) *rule {
	if len(rules) == 0 {
		return nil
	}
	comps := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i := 1; i < len(comps); i++ {
		if r := lastMatch(rules, "/"+strings.Join(comps[:i], "/"), true); r != nil {
			return r
		}
	}
	return lastMatch(rules, p, isDir)
}

func lastMatch(rules []*rule, p string, isDir bool) *rule {
	var m *rule
	for _, r := range rules {
		if r.path.match(p, isDir) {
			if r.path.negate {
				m = nil
			} else {
				m = r
			}
		}
	}
	return m
}

// toSlash converts an absolute native path to the slash-separated form
// rules match. As in Claude Code, a Windows path such as C:\Users\alice
// becomes /c/Users/alice.
func toSlash(p string) string {
	if runtime.GOOS == "windows" {
		vol := filepath.VolumeName(p)
		rest := filepath.ToSlash(p[len(vol):])
		if len(vol) == 2 && vol[1] == ':' {
			return "/" + strings.ToLower(vol[:1]) + rest
		}
		return filepath.ToSlash(vol) + rest
	}
	return p
}

// resolveLinks resolves the symbolic links of an absolute path. For a path
// that does not exist, the links of its deepest existing ancestor are
// resolved.
func resolveLinks(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	return filepath.Join(resolveLinks(parent), filepath.Base(p))
}

// isDir reports whether the native path p is an existing directory.
func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// inside reports whether the slash-separated path p is dir or inside it.
func inside(p, dir string) bool {
	return p == dir || dir == "/" || strings.HasPrefix(p, dir+"/")
}

// protectedDirs and protectedFiles are the paths Claude Code never
// auto-approves writes to, except in bypassPermissions mode; see
// https://code.claude.com/docs/en/permission-modes#protected-paths.
var (
	protectedDirs = map[string]bool{
		".git": true, ".vscode": true, ".idea": true, ".husky": true, ".cargo": true,
		".devcontainer": true, ".yarn": true, ".mvn": true, ".claude": true,
	}
	protectedFiles = map[string]bool{
		".gitconfig": true, ".gitmodules": true,
		".bashrc": true, ".bash_profile": true, ".bash_login": true, ".bash_aliases": true, ".bash_logout": true,
		".zshrc": true, ".zprofile": true, ".zshenv": true, ".zlogin": true, ".zlogout": true, ".profile": true, ".envrc": true,
		".npmrc": true, ".yarnrc": true, ".yarnrc.yml": true, ".pnp.cjs": true, ".pnp.loader.mjs": true, ".pnpmfile.cjs": true,
		"bunfig.toml": true, ".bunfig.toml": true,
		".bazelrc": true, ".bazelversion": true, ".bazeliskrc": true,
		".pre-commit-config.yaml": true, "lefthook.yml": true, "lefthook.yaml": true, ".lefthook.yml": true, ".lefthook.yaml": true,
		"gradle-wrapper.properties": true, "maven-wrapper.properties": true,
		".devcontainer.json": true, ".ripgreprc": true, "pyrightconfig.json": true,
		".mcp.json": true, ".claude.json": true,
	}
)

// protected reports whether rel, a slash-separated path relative to the
// directory it is checked from, is a protected path. Claude's own git
// worktrees under .claude/worktrees are not protected.
func protected(rel string) bool { return protectedPath(rel, caseInsensitive) }

// protectedPath is protected, ignoring case if fold is set.
func protectedPath(rel string, fold bool) bool {
	if fold {
		rel = strings.ToLower(rel)
	}
	comps := strings.Split(strings.Trim(rel, "/"), "/")
	for i, c := range comps {
		next := ""
		if i+1 < len(comps) {
			next = comps[i+1]
		}
		switch {
		case c == ".claude" && next == "worktrees":
			// Claude's own worktrees; see the exceptions in the docs.
		case protectedDirs[c] && i+1 < len(comps):
			return true
		case c == ".config" && next == "git":
			return true
		}
	}
	last := comps[len(comps)-1]
	return protectedFiles[last] || protectedDirs[last]
}
