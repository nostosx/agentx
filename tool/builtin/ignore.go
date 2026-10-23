package builtin

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/nostosx/agentx/internal/fsmatch"
)

// maxIgnoreFile caps the bytes read from an ignore file outside the tree.
const maxIgnoreFile = 1 << 20

// newIgnorer returns an ignorer for the tree fsys. If root, the tree's
// absolute directory, lies in a git work tree, the repository's
// info/exclude file and the .gitignore files between the work tree's top
// and root apply as well. Those files are only read, and only through the
// paths they exclude does their content become visible. The tree root
// itself is never treated as excluded, since it was chosen explicitly.
func newIgnorer(fsys fs.FS, root string) *fsmatch.Ignorer {
	if root == "" {
		return fsmatch.NewIgnorer(fsys)
	}
	return fsmatch.NewIgnorer(fsys, outerIgnoreRules(root)...)
}

// outerIgnoreRules finds the git work tree containing root and returns
// the rules of its info/exclude file and of the .gitignore files above
// root, lowest precedence first. Outside a work tree it returns nil.
func outerIgnoreRules(root string) []fsmatch.OuterRules {
	var ancestors []string // root's ancestors, nearest first
	top := ""
	for dir := root; ; {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			top = dir
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
		ancestors = append(ancestors, dir)
	}
	prefix := func(dir string) string {
		rel, err := filepath.Rel(dir, root)
		if err != nil || rel == "." {
			return ""
		}
		return filepath.ToSlash(rel)
	}
	var out []fsmatch.OuterRules
	if b, ok := readIgnoreFile(gitExcludeFile(top)); ok {
		out = append(out, fsmatch.OuterRules{Prefix: prefix(top), Rules: fsmatch.ParseIgnore(b)})
	}
	for _, dir := range slices.Backward(ancestors) {
		if b, ok := readIgnoreFile(filepath.Join(dir, ".gitignore")); ok {
			out = append(out, fsmatch.OuterRules{Prefix: prefix(dir), Rules: fsmatch.ParseIgnore(b)})
		}
	}
	return out
}

// gitExcludeFile returns the info/exclude file of the repository whose
// work tree top is top, or "" if there is none. A .git file, as in linked
// work trees and submodules, names the git directory, and its commondir
// file the directory holding info/exclude. Both must be local git
// directories; see localGitDir.
func gitExcludeFile(top string) string {
	gitDir := filepath.Join(top, ".git")
	st, err := os.Lstat(gitDir)
	if err != nil {
		return ""
	}
	if st.Mode().IsRegular() {
		b, ok := readIgnoreFile(gitDir)
		dir, found := strings.CutPrefix(strings.TrimSpace(b), "gitdir:")
		if !ok || !found {
			return ""
		}
		if gitDir, ok = localGitDir(top, strings.TrimSpace(dir)); !ok {
			return ""
		}
		if common, ok := readIgnoreFile(filepath.Join(gitDir, "commondir")); ok {
			if gitDir, ok = localGitDir(gitDir, strings.TrimSpace(common)); !ok {
				return ""
			}
		}
	}
	info, ok := resolveLocal(filepath.Join(gitDir, "info"))
	if !ok {
		return ""
	}
	return filepath.Join(info, "exclude")
}

// localGitDir resolves p, a path read from a .git or commondir file,
// against base. It reports whether p names a local git directory: a
// directory holding HEAD, reached without a network path (see
// resolveLocal). Such files come with the directory being searched, which
// may be untrusted.
func localGitDir(base, p string) (string, bool) {
	if p == "" || networkPath(p) {
		return "", false
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	dir, ok := resolveLocal(p)
	if !ok {
		return "", false
	}
	if st, err := os.Lstat(dir); err != nil || !st.IsDir() {
		return "", false
	}
	if st, err := os.Lstat(filepath.Join(dir, "HEAD")); err != nil || st.IsDir() {
		return "", false
	}
	return dir, true
}

// maxSymlinks bounds the symbolic links resolveLocal follows.
const maxSymlinks = 40

// resolveLocal makes p absolute and resolves its symbolic links, one
// component at a time, and reports whether p exists. It refuses a path,
// or a link target, that networkPath rejects before looking it up, since
// looking up such a path can already connect to another host.
func resolveLocal(p string) (string, bool) {
	for range maxSymlinks {
		if networkPath(p) {
			return "", false
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", false
		}
		p = abs
		vol := filepath.VolumeName(p)
		rest := strings.Split(strings.TrimLeft(p[len(vol):], `/\`), string(filepath.Separator))
		cur := vol + string(filepath.Separator)
		link := false
		for i, name := range rest {
			if name == "" {
				continue
			}
			next := filepath.Join(cur, name)
			st, err := os.Lstat(next)
			if err != nil {
				return "", false
			}
			if st.Mode()&fs.ModeSymlink == 0 {
				cur = next
				continue
			}
			target, err := os.Readlink(next)
			if err != nil {
				return "", false
			}
			if networkPath(target) {
				return "", false
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(cur, target)
			}
			p = filepath.Join(append([]string{target}, rest[i+1:]...)...)
			link = true
			break
		}
		if !link {
			return cur, true
		}
	}
	return "", false
}

// networkPath reports whether p looks like a network location: a UNC or
// device path such as \\host\share, //host/share or \\?\UNC\host\share
// on any system, a volume other than a drive letter on Windows, and the
// automounter directories /net and /Network elsewhere.
func networkPath(p string) bool {
	if strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//") {
		return true
	}
	if runtime.GOOS == "windows" {
		// Windows also accepts mixed separators, as in \\/host/share.
		if len(p) >= 2 && os.IsPathSeparator(p[0]) && os.IsPathSeparator(p[1]) {
			return true
		}
		vol := filepath.VolumeName(p)
		return vol != "" && (len(vol) != 2 || vol[1] != ':')
	}
	first, _, _ := strings.Cut(strings.TrimLeft(filepath.Clean(p), "/"), "/")
	return filepath.IsAbs(p) && (first == "net" || first == "Network")
}

// readIgnoreFile reads a small regular file outside the tree. Like git, it
// does not follow a symbolic link.
func readIgnoreFile(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	if st, err := os.Lstat(name); err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	f, err := os.Open(name)
	if err != nil {
		return "", false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxIgnoreFile))
	return string(b), err == nil
}
