package builtin

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/nostosx/kitx/utf8text"

	"github.com/nostosx/agentx/internal/fileaccess"
	"github.com/nostosx/agentx/internal/fsmatch"
	"github.com/nostosx/agentx/tool"
)

const (
	maxGlobResults     = 100
	maxContextLines    = 10
	defaultGrepResults = 100
	maxGrepResults     = 1000
	maxGrepFileSize    = 20 << 20
	maxGrepLineLength  = 300
)

// skipDir reports whether a directory is skipped by glob and grep.
func skipDir(name string) bool { return name == ".git" || name == "node_modules" }

// walk calls fn for every regular file under base, skipping .git,
// node_modules and, unless includeIgnored is set, paths excluded by
// .gitignore files, including those of the enclosing git work tree. base
// itself is always visited, since it was named explicitly. Paths passed to
// fn are relative to the working directory.
func (e *env) walk(ctx context.Context, fsys fs.FS, base string, includeIgnored bool, fn func(name string, d fs.DirEntry) error) error {
	var ig *fsmatch.Ignorer
	if !includeIgnored {
		ig = newIgnorer(fsys, e.real)
	}
	return fs.WalkDir(fsys, base, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			if name == base {
				return err
			}
			// Unreadable entries below base are skipped, like binary files.
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name != base {
			switch {
			case d.IsDir() && skipDir(d.Name()):
				return fs.SkipDir
			case d.Name() == ".git":
				// The .git file of a linked work tree or submodule.
				return nil
			}
			if ig != nil && ig.Ignored(name, d.IsDir()) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if name != base && !fileaccess.ReadAllowed(ctx, filepath.Join(e.dir, filepath.FromSlash(name))) {
			return nil
		}
		return fn(name, d)
	})
}

// errStop ends a walk early.
var errStop = errors.New("stop")

type globArgs struct {
	Pattern        string `json:"pattern" jsonschema:"Glob pattern matched against paths relative to path, such as \"**/*.go\" or \"src/**/*.{ts,tsx}\". \"**\" matches any number of directories."`
	Path           string `json:"path,omitempty" jsonschema:"Directory to search in (default: the working directory)."`
	IncludeIgnored bool   `json:"include_ignored,omitempty" jsonschema:"Also match files excluded by .gitignore."`
}

// Glob returns the glob tool.
func Glob(cfg Config) tool.Tool {
	e := newEnv(cfg)
	return tool.Func("glob",
		"Find files by name pattern. Returns matching file paths relative to the working directory, most recently modified first. "+
			".git, node_modules and files excluded by .gitignore are skipped.",
		func(ctx context.Context, in globArgs) (*tool.Result, error) {
			g, err := fsmatch.CompileGlob(in.Pattern)
			if err != nil {
				return tool.Errorf("%v", err), nil
			}
			base, err := e.slashRel(in.Path)
			if err != nil {
				return tool.Errorf("%v", err), nil
			}
			root, err := os.OpenRoot(e.dir)
			if err != nil {
				return nil, err
			}
			defer root.Close()

			type match struct {
				name string
				mod  time.Time
			}
			var matches []match
			err = e.walk(ctx, root.FS(), base, in.IncludeIgnored, func(name string, d fs.DirEntry) error {
				rel := name
				if base != "." {
					rel = strings.TrimPrefix(name, base+"/")
				}
				if !g.Match(rel) {
					return nil
				}
				m := match{name: name}
				if info, err := d.Info(); err == nil {
					m.mod = info.ModTime()
				}
				matches = append(matches, m)
				return nil
			})
			if err != nil {
				if ctx.Err() != nil {
					return nil, err
				}
				return tool.Errorf("%v", err), nil
			}
			if len(matches) == 0 {
				return tool.Text("No files found."), nil
			}
			slices.SortFunc(matches, func(a, b match) int {
				return cmp.Or(b.mod.Compare(a.mod), strings.Compare(a.name, b.name))
			})
			var out strings.Builder
			for i, m := range matches[:min(len(matches), maxGlobResults)] {
				if i > 0 {
					out.WriteByte('\n')
				}
				out.WriteString(m.name)
			}
			if n := len(matches) - maxGlobResults; n > 0 {
				fmt.Fprintf(&out, "\n... (%d more files not shown; use a more specific pattern or path)", n)
			}
			return tool.Text(out.String()), nil
		}, tool.ReadOnly())
}

type grepArgs struct {
	Pattern         string `json:"pattern" jsonschema:"Regular expression in Go RE2 syntax."`
	Path            string `json:"path,omitempty" jsonschema:"File or directory to search (default: the working directory)."`
	Include         string `json:"include,omitempty" jsonschema:"Only search files matching this glob, such as \"*.go\" or \"*.{ts,tsx}\". Patterns without a slash match file names; others match paths relative to the working directory."`
	CaseInsensitive bool   `json:"case_insensitive,omitempty" jsonschema:"Match case-insensitively."`
	FilesOnly       bool   `json:"files_only,omitempty" jsonschema:"Return only the paths of matching files."`
	Context         int    `json:"context,omitempty" jsonschema:"Lines of context to show before and after each match (at most 10). Context lines are shown as path-line-text, and separate groups by --."`
	MaxResults      int    `json:"max_results,omitempty" jsonschema:"Maximum number of matching lines, or files with files_only (default 100, at most 1000)."`
	IncludeIgnored  bool   `json:"include_ignored,omitempty" jsonschema:"Also search files excluded by .gitignore."`
}

// Grep returns the grep tool.
func Grep(cfg Config) tool.Tool {
	e := newEnv(cfg)
	return tool.Func("grep",
		"Search file contents with a regular expression. Returns matches as path:line:text. "+
			"Binary files, .git, node_modules and files excluded by .gitignore are skipped.",
		func(ctx context.Context, in grepArgs) (*tool.Result, error) {
			return e.grep(ctx, in)
		}, tool.ReadOnly())
}

func (e *env) grep(ctx context.Context, in grepArgs) (*tool.Result, error) {
	expr := in.Pattern
	if in.CaseInsensitive {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return tool.Errorf("invalid pattern: %v", err), nil
	}
	var include fsmatch.Glob
	if in.Include != "" {
		if include, err = fsmatch.CompileGlob(in.Include); err != nil {
			return tool.Errorf("%v", err), nil
		}
	}
	limit := in.MaxResults
	if limit <= 0 {
		limit = defaultGrepResults
	}
	limit = min(limit, maxGrepResults)
	base, err := e.slashRel(in.Path)
	if err != nil {
		return tool.Errorf("%v", err), nil
	}
	root, err := os.OpenRoot(e.dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	fsys := root.FS()

	contextLines := min(max(in.Context, 0), maxContextLines)
	if in.FilesOnly {
		contextLines = 0
	}
	var out strings.Builder
	results := 0
	truncated := false
	err = e.walk(ctx, fsys, base, in.IncludeIgnored, func(name string, d fs.DirEntry) error {
		if include != nil {
			target := name
			if !strings.Contains(in.Include, "/") {
				target = d.Name()
			}
			if !include.Match(target) {
				return nil
			}
		}
		// Files that vanished or are too large are skipped, not errors.
		if info, err := d.Info(); err != nil || info.Size() > maxGrepFileSize {
			return nil //nolint:nilerr // skipping is the intended handling
		}
		last := 0 // last line written for this file
		return grepFile(fsys, name, re, contextLines, func(n int, line string, isMatch bool) (bool, error) {
			if isMatch {
				if results == limit || out.Len() >= e.MaxOutput {
					truncated = true
					return true, errStop
				}
				results++
				if in.FilesOnly {
					out.WriteString(name + "\n")
					return true, nil
				}
			}
			if contextLines > 0 && out.Len() > 0 && n > last+1 {
				out.WriteString("--\n")
			}
			last = n
			sep := "-"
			if isMatch {
				sep = ":"
			}
			fmt.Fprintf(&out, "%s%s%d%s%s\n", name, sep, n, sep, utf8text.Truncate(line, maxGrepLineLength, "..."))
			return false, nil
		})
	})
	if errors.Is(err, errStop) {
		err = nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		return tool.Errorf("%v", err), nil
	}
	if results == 0 {
		return tool.Text("No matches found."), nil
	}
	s := strings.TrimSuffix(out.String(), "\n")
	if truncated {
		s += fmt.Sprintf("\n... (results limited to %d; narrow the pattern, path or include)", results)
	}
	return tool.Text(s), nil
}

// grepFile calls emit for each matching line of a text file, in order, and
// for up to contextLines lines around each match. emit reports whether to
// skip the rest of the file, and its error ends the walk. Unreadable and
// binary files are skipped.
func grepFile(fsys fs.FS, name string, re *regexp.Regexp, contextLines int, emit func(n int, line string, isMatch bool) (bool, error)) error {
	f, err := fsys.Open(name)
	if err != nil {
		return nil
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64*1024)
	if head, _ := br.Peek(sniffLen); bytes.IndexByte(head, 0) >= 0 {
		return nil
	}
	type numbered struct {
		n    int
		line string
	}
	var before []numbered // lines preceding the next match
	after := 0            // context lines still to show after a match
	for n := 1; ; n++ {
		line, readErr := br.ReadString('\n')
		if line == "" && readErr != nil {
			// End of file; a final newline does not start another line.
			return nil
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case re.MatchString(line):
			for _, b := range before {
				if skip, err := emit(b.n, b.line, false); err != nil || skip {
					return err
				}
			}
			before = before[:0]
			if skip, err := emit(n, line, true); err != nil || skip {
				return err
			}
			after = contextLines
		case after > 0:
			after--
			if skip, err := emit(n, line, false); err != nil || skip {
				return err
			}
		case contextLines > 0:
			if len(before) == contextLines {
				before = append(before[:0], before[1:]...)
			}
			before = append(before, numbered{n, line})
		}
		if readErr != nil {
			return nil
		}
	}
}
