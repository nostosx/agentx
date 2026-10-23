// Package fsmatch matches slash-separated paths against glob patterns and
// .gitignore rules. It is shared by the built-in search tools and the rag
// file loader.
package fsmatch

import (
	"fmt"
	"path"
	"slices"
	"strings"
)

// Glob matches slash-separated paths against patterns in which "**"
// matches any number of path segments, other segments follow path.Match
// and "{a,b}" expands to alternatives.
type Glob [][]string

// CompileGlob compiles pattern.
func CompileGlob(pattern string) (Glob, error) {
	var g Glob
	for _, alt := range expandBraces(pattern) {
		segs := strings.Split(strings.Trim(alt, "/"), "/")
		for _, s := range segs {
			if _, err := path.Match(s, ""); err != nil {
				return nil, fmt.Errorf("invalid pattern %q: %w", pattern, err)
			}
		}
		g = append(g, segs)
	}
	return g, nil
}

// Match reports whether name, a slash-separated path, matches the glob.
func (g Glob) Match(name string) bool {
	segs := strings.Split(name, "/")
	return slices.ContainsFunc(g, func(p []string) bool { return MatchSegments(p, segs) })
}

// MatchSegments matches path segments against pattern segments, where a
// "**" pattern segment matches any number of path segments and others
// follow path.Match.
func MatchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			pat = pat[1:]
			if len(pat) == 0 {
				return true
			}
			for i := range len(name) + 1 {
				if MatchSegments(pat, name[i:]) {
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

func expandBraces(p string) []string {
	i := strings.IndexByte(p, '{')
	if i < 0 {
		return []string{p}
	}
	j := strings.IndexByte(p[i:], '}')
	if j < 0 {
		return []string{p}
	}
	j += i
	var out []string
	for alt := range strings.SplitSeq(p[i+1:j], ",") {
		out = append(out, expandBraces(p[:i]+alt+p[j+1:])...)
	}
	return out
}
