package actx

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	// DefaultInstructionMaxBytes is the default total size of instruction
	// files, matching the project_doc_max_bytes default of Codex.
	DefaultInstructionMaxBytes = 32 << 10
	// DefaultImportDepth is the default number of nested @path import hops,
	// matching Claude Code.
	DefaultImportDepth = 4
)

// InstructionConfig configures InstructionFiles.
type InstructionConfig struct {
	// Dir is the working directory instructions are loaded for. Defaults to
	// ".".
	Dir string
	// Names lists the instruction file names checked in each directory, in
	// order of precedence; the first one that exists as a file is loaded
	// and the rest are ignored. Names may contain slash-separated
	// subdirectories, such as ".claude/CLAUDE.md", but must stay inside the
	// directory. Defaults to AGENTS.override.md, AGENTS.md, CLAUDE.md.
	Names []string
	// LocalNames lists files loaded in each directory after the file chosen
	// from Names, all that exist, for example CLAUDE.local.md. None by
	// default.
	LocalNames []string
	// RootMarkers are the entries, of any type, that mark the project root.
	// Directories are searched from the nearest ancestor of Dir containing
	// a marker down to Dir. When no ancestor has a marker, or the list is
	// empty but not nil, only Dir is searched. Defaults to .git.
	RootMarkers []string
	// UserFiles are personal instruction files, such as
	// ~/.codex/AGENTS.md, loaded before the project's, in order. Missing
	// files are skipped. A leading ~ is the user's home directory. No file
	// outside the project is read unless listed here or in ImportDirs.
	UserFiles []string
	// ImportDirs are directories outside the project root from which any
	// instruction file may import. A leading ~ is the user's home
	// directory.
	ImportDirs []string
	// MaxBytes bounds the total size of all files loaded; the content past
	// it is dropped with a note. Defaults to DefaultInstructionMaxBytes; a
	// negative value disables the limit.
	MaxBytes int
	// ImportDepth bounds how many hops of @path imports are followed.
	// Defaults to DefaultImportDepth; a negative value disables imports.
	ImportDepth int
	// Priority orders the sections among those of other providers.
	Priority int
}

var defaultInstructionNames = []string{"AGENTS.override.md", "AGENTS.md", "CLAUDE.md"}

// InstructionFiles returns a provider that adds project instruction files,
// such as AGENTS.md and CLAUDE.md, to the system prompt, the way Codex and
// Claude Code load them.
//
// The project root is the nearest ancestor of cfg.Dir containing one of
// cfg.RootMarkers. In every directory from the root down to cfg.Dir, the
// first of cfg.Names that exists is loaded, followed by any of
// cfg.LocalNames. cfg.UserFiles come first, so instructions closer to
// cfg.Dir are read last. Each file becomes a PlaceSystem section, named and
// labeled with its path: relative to the root for project files, as given
// for user files.
//
// A file may import others with @path outside Markdown code spans and
// fenced code blocks, for example "See @docs/style.md". Paths are relative
// to the importing file, absolute, or start with ~ for the home directory;
// a backslash escapes a space, and quoted paths are not imported. Trailing
// punctuation is dropped when the file with it does not exist. Imported
// files follow the file that imports them, up to cfg.ImportDepth hops. A
// file is loaded at most once, so import cycles end. Missing imports are
// skipped.
//
// Instruction files are untrusted repository content. Project files and
// their imports are read only inside the project root, or cfg.Dir when
// there is none; a user file's imports only inside its directory; both may
// also import from cfg.ImportDirs. Paths that leave these directories,
// through .. or symbolic links, are skipped, as are absolute symbolic
// links.
//
// Files are read again on each run, so edits take effect on the next run;
// changed content also changes the system prompt prefix. Failing to read a
// file that exists returns an error, and the run proceeds without
// instruction files.
func InstructionFiles(cfg InstructionConfig) Provider {
	cfg.Names = slices.Clone(cfg.Names)
	cfg.LocalNames = slices.Clone(cfg.LocalNames)
	cfg.RootMarkers = slices.Clone(cfg.RootMarkers)
	cfg.UserFiles = slices.Clone(cfg.UserFiles)
	cfg.ImportDirs = slices.Clone(cfg.ImportDirs)
	return ProviderFunc(func(ctx context.Context, _ *Input) ([]Section, error) {
		return loadInstructions(ctx, &cfg)
	})
}

// importBase is a directory instruction files may be read from.
type importBase struct {
	dir   string // absolute path
	label string // label prefix of files inside dir
}

type instructionLoader struct {
	cfg       *InstructionConfig
	depth     int
	limit     int
	remaining int // negative when unlimited
	truncated bool
	seen      []os.FileInfo
	roots     map[string]*os.Root
	sections  []Section
}

func loadInstructions(ctx context.Context, cfg *InstructionConfig) ([]Section, error) {
	names := cfg.Names
	if len(names) == 0 {
		names = defaultInstructionNames
	}
	names, err := fileNames(names)
	if err != nil {
		return nil, err
	}
	locals, err := fileNames(cfg.LocalNames)
	if err != nil {
		return nil, err
	}
	dir, err := filepath.Abs(cmp.Or(cfg.Dir, "."))
	if err != nil {
		return nil, fmt.Errorf("instruction files: %w", err)
	}
	if info, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("instruction files: %w", err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("instruction files: %s is not a directory", dir)
	}
	markers := cfg.RootMarkers
	if markers == nil {
		markers = []string{".git"}
	}
	root := projectRoot(dir, markers)

	l := &instructionLoader{cfg: cfg, roots: map[string]*os.Root{}}
	defer l.close()
	l.depth = cfg.ImportDepth
	if l.depth == 0 {
		l.depth = DefaultImportDepth
	}
	l.depth = max(l.depth, 0)
	l.limit = cfg.MaxBytes
	if l.limit == 0 {
		l.limit = DefaultInstructionMaxBytes
	}
	l.remaining = l.limit

	var extra []importBase
	for _, d := range cfg.ImportDirs {
		if abs := expandPath(d); abs != "" {
			extra = append(extra, importBase{dir: abs, label: filepath.Clean(d)})
		}
	}
	for _, f := range cfg.UserFiles {
		if err := l.loadUserFile(ctx, f, extra); err != nil {
			return nil, err
		}
	}

	project := append([]importBase{{dir: root}}, extra...)
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return nil, fmt.Errorf("instruction files: %w", err)
	}
	dirs := []string{"."}
	if rel != "." {
		cur := ""
		for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
			cur = filepath.Join(cur, part)
			dirs = append(dirs, cur)
		}
	}
	for _, d := range dirs {
		for _, n := range names {
			found, err := l.loadProjectFile(ctx, project, filepath.Join(d, n), "project instructions")
			if err != nil {
				return nil, err
			}
			if found {
				break
			}
		}
		for _, n := range locals {
			if _, err := l.loadProjectFile(ctx, project, filepath.Join(d, n), "local instructions"); err != nil {
				return nil, err
			}
		}
	}
	if l.truncated {
		note := fmt.Sprintf("[Instruction files truncated: they exceed the %d-byte limit.]", l.limit)
		if n := len(l.sections); n > 0 {
			l.sections[n-1].Content += "\n\n" + note
		} else {
			l.sections = append(l.sections, Section{Name: "instructions", Content: note, Priority: cfg.Priority})
		}
	}
	return l.sections, ctx.Err()
}

// fileNames validates instruction file names and converts them to the
// native path syntax.
func fileNames(names []string) ([]string, error) {
	out := make([]string, 0, len(names))
	for _, n := range names {
		p := filepath.FromSlash(n)
		if n == "" || !filepath.IsLocal(p) {
			return nil, fmt.Errorf("instruction files: invalid file name %q", n)
		}
		out = append(out, p)
	}
	return out, nil
}

// projectRoot returns the nearest ancestor of dir, inclusive, containing
// one of markers, or dir when there is none.
func projectRoot(dir string, markers []string) string {
	for d := dir; ; {
		for _, m := range markers {
			if _, err := os.Lstat(filepath.Join(d, m)); err == nil {
				return d
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			return dir
		}
		d = parent
	}
}

// expandPath returns the absolute form of p, replacing a leading ~ with the
// home directory. It returns "" when p cannot be resolved.
func expandPath(p string) string {
	p = filepath.FromSlash(p)
	if p == "~" || strings.HasPrefix(p, "~"+string(filepath.Separator)) {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		p = filepath.Join(home, p[1:])
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return ""
	}
	return abs
}

func (l *instructionLoader) close() {
	for _, r := range l.roots {
		r.Close()
	}
}

func (l *instructionLoader) root(dir string) (*os.Root, error) {
	if r, ok := l.roots[dir]; ok {
		return r, nil
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	l.roots[dir] = r
	return r, nil
}

func (l *instructionLoader) done(ctx context.Context) bool { return l.truncated || ctx.Err() != nil }

func (l *instructionLoader) isSeen(info os.FileInfo) bool {
	return slices.ContainsFunc(l.seen, func(s os.FileInfo) bool { return os.SameFile(s, info) })
}

func (l *instructionLoader) loadUserFile(ctx context.Context, name string, extra []importBase) error {
	if l.done(ctx) {
		return nil
	}
	path := expandPath(name)
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("instruction files: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("instruction files: %w", err)
	}
	if !info.Mode().IsRegular() || l.isSeen(info) {
		return nil
	}
	label := filepath.Clean(name)
	bases := append([]importBase{{dir: filepath.Dir(path), label: filepath.Dir(label)}}, extra...)
	return l.add(ctx, f, info, filepath.ToSlash(label), "user instructions", path, bases, 0)
}

// loadProjectFile loads rel inside the project root. It reports whether rel
// is a file, even one already loaded or empty.
func (l *instructionLoader) loadProjectFile(ctx context.Context, bases []importBase, rel, kind string) (bool, error) {
	if l.done(ctx) {
		return false, nil
	}
	r, err := l.root(bases[0].dir)
	if err != nil {
		return false, fmt.Errorf("instruction files: %w", err)
	}
	f, err := r.Open(rel)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return false, fmt.Errorf("instruction files: %w", err)
		}
		// Missing, or a link leaving the root: not an instruction file.
		return false, nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("instruction files: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	if l.isSeen(info) {
		return true, nil
	}
	path := filepath.Join(bases[0].dir, rel)
	return true, l.add(ctx, f, info, filepath.ToSlash(rel), kind, path, bases, 0)
}

// add appends the content of f as a section and follows its imports.
func (l *instructionLoader) add(ctx context.Context, f *os.File, info os.FileInfo, label, kind, path string, bases []importBase, depth int) error {
	l.seen = append(l.seen, info)
	text, err := l.read(f)
	if err != nil {
		return fmt.Errorf("instruction files: read %s: %w", label, err)
	}
	if strings.TrimSpace(text) != "" {
		l.sections = append(l.sections, Section{
			Name:      label,
			Content:   fmt.Sprintf("Contents of %s (%s):\n\n%s", label, kind, text),
			Placement: PlaceSystem,
			Priority:  l.cfg.Priority,
		})
	}
	if depth >= l.depth {
		return nil
	}
	for _, p := range importPaths(text) {
		if l.done(ctx) {
			break
		}
		l.importFile(ctx, p, path, label, bases, depth+1)
	}
	return nil
}

// read reads f within the remaining byte budget.
func (l *instructionLoader) read(f *os.File) (string, error) {
	if l.remaining < 0 {
		b, err := io.ReadAll(f)
		return string(b), err
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(l.remaining)+1))
	if err != nil {
		return "", err
	}
	if len(b) > l.remaining {
		l.truncated = true
		b = cutRune(b[:l.remaining])
	}
	l.remaining -= len(b)
	return string(b), nil
}

// cutRune drops an incomplete UTF-8 sequence at the end of b.
func cutRune(b []byte) []byte {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				return b[:i]
			}
			break
		}
	}
	return b
}

// importFile loads the file imported as p by the file at from. Imports
// that are missing or outside bases are skipped. Trailing punctuation, as
// in "See @a.md.", is dropped when the file with it does not exist.
func (l *instructionLoader) importFile(ctx context.Context, p, from, fromLabel string, bases []importBase, depth int) {
	if l.openImport(ctx, p, from, fromLabel, bases, depth) {
		return
	}
	if trimmed := strings.TrimRight(p, ".,;:!?)"); trimmed != p && trimmed != "" {
		l.openImport(ctx, trimmed, from, fromLabel, bases, depth)
	}
}

// openImport loads an import and reports whether the file exists inside
// bases.
func (l *instructionLoader) openImport(ctx context.Context, p, from, fromLabel string, bases []importBase, depth int) bool {
	target := filepath.FromSlash(p)
	if target == "~" || strings.HasPrefix(target, "~"+string(filepath.Separator)) {
		target = expandPath(target)
	} else if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(from), target)
	}
	if target == "" {
		return false
	}
	target = filepath.Clean(target)
	for _, b := range bases {
		rel, err := filepath.Rel(b.dir, target)
		if err != nil || !filepath.IsLocal(rel) {
			continue
		}
		r, err := l.root(b.dir)
		if err != nil {
			continue
		}
		// os.Root refuses symbolic links that are absolute or leave b.dir.
		f, err := r.Open(rel)
		if err != nil {
			continue
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return true
		}
		if info.Mode().IsRegular() && !l.isSeen(info) {
			label := filepath.ToSlash(filepath.Join(b.label, rel))
			_ = l.add(ctx, f, info, label, "imported by "+fromLabel, target, bases, depth)
		}
		return true
	}
	return false
}

// importPaths returns the @path imports of Markdown text, skipping code
// spans and fenced code blocks.
func importPaths(text string) []string {
	var out []string
	var para strings.Builder
	flush := func() {
		out = scanImports(para.String(), out)
		para.Reset()
	}
	fence := ""
	for line := range strings.Lines(text) {
		if fence != "" {
			if closesFence(line, fence) {
				fence = ""
			}
			continue
		}
		if f := opensFence(line); f != "" {
			flush()
			fence = f
			continue
		}
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		para.WriteString(line)
	}
	flush()
	return out
}

// fenceIndent returns line without up to three spaces of indentation, or
// false when it is indented further.
func fenceIndent(line string) (string, bool) {
	s := strings.TrimRight(line, "\r\n")
	n := len(s) - len(strings.TrimLeft(s, " "))
	return s[n:], n <= 3
}

// opensFence returns the opening fence of a fenced code block, or "".
func opensFence(line string) string {
	s, ok := fenceIndent(line)
	if !ok || s == "" || (s[0] != '`' && s[0] != '~') {
		return ""
	}
	n := len(s) - len(strings.TrimLeft(s, s[:1]))
	if n < 3 || (s[0] == '`' && strings.Contains(s[n:], "`")) {
		return ""
	}
	return s[:n]
}

func closesFence(line, fence string) bool {
	s, ok := fenceIndent(line)
	s = strings.TrimRight(s, " \t")
	return ok && len(s) >= len(fence) && strings.Trim(s, fence[:1]) == ""
}

// scanImports appends the @path imports in a paragraph outside code spans.
func scanImports(s string, out []string) []string {
	for i := 0; i < len(s); {
		switch {
		case s[i] == '`':
			n := backticks(s, i)
			if end := closingBackticks(s, i+n, n); end >= 0 {
				i = end
			} else {
				i += n
			}
		case s[i] == '@' && (i == 0 || isSpace(s[i-1])):
			var p strings.Builder
			j := i + 1
			for j < len(s) && !isSpace(s[j]) {
				if s[j] == '\\' && j+1 < len(s) && s[j+1] == ' ' {
					p.WriteByte(' ')
					j += 2
					continue
				}
				p.WriteByte(s[j])
				j++
			}
			if path := p.String(); path != "" && path[0] != '"' && path[0] != '\'' {
				out = append(out, path)
			}
			i = j
		default:
			i++
		}
	}
	return out
}

func backticks(s string, i int) int {
	n := 0
	for i+n < len(s) && s[i+n] == '`' {
		n++
	}
	return n
}

// closingBackticks returns the end of the next run of exactly n backticks
// at or after i, or -1.
func closingBackticks(s string, i, n int) int {
	for i < len(s) {
		if s[i] != '`' {
			i++
			continue
		}
		k := backticks(s, i)
		if k == n {
			return i + k
		}
		i += k
	}
	return -1
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}
