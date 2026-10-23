package skill

import (
	"encoding/json"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nostosx/agentx/internal/toolnames"
	"github.com/nostosx/agentx/model"
)

// maxBraceExpansions bounds the patterns the brace groups of one skill's
// paths expand to, as Claude Code bounds those of path-specific rules
// (https://code.claude.com/docs/en/memory#path-specific-rules). A pattern
// that would exceed it is used unexpanded, so its braces match literally.
const maxBraceExpansions = 1000

// pathGlob matches slash-separated paths relative to the project root. A
// "**" segment matches any number of segments, other segments follow
// path.Match, and "{a,b}" expands to alternatives. A pattern that is not
// valid, such as one with an unclosed "[", matches nothing.
type pathGlob [][]string

func compilePaths(patterns []string) pathGlob {
	var g pathGlob
	budget := maxBraceExpansions
	for _, p := range patterns {
		p = strings.TrimPrefix(strings.TrimSpace(p), "./")
		if p == "" {
			continue
		}
		alts := []string{p}
		if strings.Contains(p, "{") {
			if expanded, ok := expandBraces(p, budget); ok {
				alts = expanded
				budget -= len(expanded)
			}
		}
		for _, alt := range alts {
			segs := strings.Split(strings.Trim(alt, "/"), "/")
			valid := true
			for _, s := range segs {
				if _, err := path.Match(s, ""); err != nil {
					valid = false
				}
			}
			if valid {
				g = append(g, segs)
			}
		}
	}
	return g
}

func (g pathGlob) match(name string) bool {
	segs := strings.Split(strings.Trim(name, "/"), "/")
	return slices.ContainsFunc(g, func(p []string) bool { return matchSegments(p, segs) })
}

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

// expandBraces expands the brace groups of p, reporting false if they
// expand to more than limit patterns.
func expandBraces(p string, limit int) ([]string, bool) {
	i := strings.IndexByte(p, '{')
	if i < 0 {
		return []string{p}, limit >= 1
	}
	j := strings.IndexByte(p[i:], '}')
	if j < 0 {
		return []string{p}, limit >= 1
	}
	j += i
	var out []string
	for alt := range strings.SplitSeq(p[i+1:j], ",") {
		more, ok := expandBraces(p[:i]+alt+p[j+1:], limit-len(out))
		if !ok {
			return nil, false
		}
		out = append(out, more...)
	}
	return out, len(out) <= limit
}

// fileTools maps the tools that read or edit a file to the argument that
// names it: agentx's built-ins and Claude Code's tools, whose use loads
// path-scoped rules and skills.
var fileTools = map[string][]string{
	"read_file":    {"path"},
	"write_file":   {"path"},
	"edit_file":    {"path"},
	"Read":         {"file_path"},
	"Write":        {"file_path"},
	"Edit":         {"file_path"},
	"MultiEdit":    {"file_path"},
	"NotebookEdit": {"notebook_path"},
}

// FilesOf returns the files a tool call reads or edits: the path argument
// of agentx's read_file, write_file and edit_file tools and the file_path
// or notebook_path argument of Claude Code's Read, Write, Edit, MultiEdit
// and NotebookEdit tools, and the files an apply_patch call touches. It is
// the default Config.Files.
func FilesOf(call *model.ToolCall) []string {
	if call.Name == "apply_patch" {
		return toolnames.PatchPaths(call.Arguments)
	}
	keys, ok := fileTools[call.Name]
	if !ok {
		return nil
	}
	var args map[string]any
	if json.Unmarshal(call.Arguments, &args) != nil {
		return nil
	}
	var out []string
	for _, k := range keys {
		if s, ok := args[k].(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// touchedFiles returns the files the tool calls in msgs read or edited.
func touchedFiles(msgs []*model.Message, files func(*model.ToolCall) []string) []string {
	var out []string
	for _, msg := range msgs {
		for _, c := range msg.ToolCalls() {
			out = append(out, files(c)...)
		}
	}
	return out
}

// relPath returns file relative to root in slash form, or false if it is
// outside root. A relative file is taken to be relative to root already.
func relPath(root, file string) (string, bool) {
	if !filepath.IsAbs(file) {
		rel := filepath.ToSlash(filepath.Clean(file))
		return rel, rel != ".." && !strings.HasPrefix(rel, "../")
	}
	if root == "" {
		return "", false
	}
	rel, err := filepath.Rel(root, file)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	return rel, rel != ".." && !strings.HasPrefix(rel, "../")
}

// within reports whether file, relative to root unless absolute, is below
// dir.
func within(root, dir, file string) bool {
	if !filepath.IsAbs(file) && root != "" {
		file = filepath.Join(root, file)
	}
	if filepath.IsAbs(dir) != filepath.IsAbs(file) {
		return false
	}
	rel, err := filepath.Rel(dir, file)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, "../")
}

// Scoped is implemented by skills that apply only to the files below a
// directory, such as the nested skills Discover finds below the working
// directory: like a skill with paths, they are offered to the model once
// it works with a file there.
type Scoped interface {
	// ScopeDir is the directory, absolute or relative to the project root.
	ScopeDir() string
}

// Conditional reports whether s is offered to the model only once it works
// with matching files: it has paths, or it is Scoped.
func Conditional(s Skill) bool {
	if _, ok := s.(Scoped); ok {
		return true
	}
	return len(s.Metadata().Paths) > 0
}

// Applies reports whether a conditional skill applies to one of files, by
// its paths, relative to root, and its scope directory. An unconditional
// skill always applies.
func Applies(s Skill, root string, files []string) bool {
	if !Conditional(s) {
		return true
	}
	var scope string
	if sc, ok := s.(Scoped); ok {
		scope = sc.ScopeDir()
		if !filepath.IsAbs(scope) && root != "" {
			scope = filepath.Join(root, scope)
		}
	}
	g := compilePaths(s.Metadata().Paths)
	for _, f := range files {
		if scope != "" && !within(root, scope, f) {
			continue
		}
		if len(s.Metadata().Paths) == 0 {
			return true
		}
		if rel, ok := relPath(root, f); ok && g.match(rel) {
			return true
		}
	}
	return false
}
