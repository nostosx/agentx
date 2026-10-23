package skill

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// DefaultFolders are the skill folders Discover searches in each directory,
// in precedence order: Claude Code's .claude/skills
// (https://code.claude.com/docs/en/skills#choose-where-skills-load), then
// .agents/skills, the interoperable location of Codex
// (https://developers.openai.com/codex/skills) and Gemini CLI
// (https://geminicli.com/docs/cli/skills/).
var DefaultFolders = []string{".claude/skills", ".agents/skills"}

// Scope is where a discovered skill comes from.
type Scope int

const (
	// ScopeManaged is the managed (enterprise) directory.
	ScopeManaged Scope = iota
	// ScopeUser is the user's home directory.
	ScopeUser
	// ScopeProject is the working directory or one of its parents up to
	// the project root.
	ScopeProject
	// ScopeAdditional is an additional directory, like --add-dir.
	ScopeAdditional
	// ScopeNested is a directory below the working directory.
	ScopeNested
)

func (s Scope) String() string {
	switch s {
	case ScopeManaged:
		return "managed"
	case ScopeUser:
		return "user"
	case ScopeProject:
		return "project"
	case ScopeAdditional:
		return "additional"
	case ScopeNested:
		return "nested"
	}
	return fmt.Sprintf("Scope(%d)", int(s))
}

// DiscoverConfig configures Discover.
type DiscoverConfig struct {
	// Dir is the working directory; empty means the process's.
	Dir string
	// Root is the project root: project skills load from Dir and each of
	// its parents up to Root. Empty means the nearest directory, from Dir
	// upwards, that contains .git (a repository or worktree root), or Dir
	// if there is none.
	Root string
	// Home, when set, enables the user's skills in Home/.claude/skills and
	// Home/.agents/skills, for example with os.UserHomeDir. They are off
	// by default, so a program does not pick up skills from the home
	// directory of whoever runs it unless it asks to.
	Home string
	// Managed, when set, is a managed settings directory whose skills take
	// precedence over all others, like Claude Code's enterprise skills.
	Managed string
	// AdditionalDirs are more directories whose skill folders load, like
	// Claude Code's --add-dir, after the project's.
	AdditionalDirs []string
	// Nested also loads the skill folders of directories below Dir. As in
	// Claude Code, such a skill is offered to the model only once it works
	// with a file in that directory (see Scoped), and when its name is
	// taken it is renamed "<dir>:<name>", with dir relative to Dir.
	// Hidden directories and node_modules are not searched.
	Nested bool
	// Folders are the skill folders searched in each directory, in
	// precedence order; nil means DefaultFolders.
	Folders []string
	// ProjectFirst gives project skills precedence over the user's, as
	// Gemini CLI does. By default the user's win, as in Claude Code.
	ProjectFirst bool
	// Strict loads skills with the Strict option.
	Strict bool
}

// Source describes where a discovered skill was found.
type Source struct {
	Scope Scope
	// Folder is the skill folder, such as /repo/.claude/skills.
	Folder string
}

// Conflict reports a skill hidden by one of the same name with higher
// precedence.
type Conflict struct {
	Name string
	// Kept and Shadowed are the directories of the skill that is used and
	// of the one that is not.
	Kept, Shadowed string
}

// Discovery is the result of Discover.
type Discovery struct {
	// Skills are the skills found, one per name, in precedence order.
	Skills []Skill
	// Sources maps skill names to where they were found.
	Sources map[string]Source
	// Conflicts lists skills hidden by others of the same name.
	Conflicts []Conflict
}

// Registry returns a registry of the discovered skills.
func (d *Discovery) Registry() *Registry { return NewRegistry(d.Skills...) }

// Discover finds skills in the standard locations, in precedence order:
//
//  1. Managed: the skill folders of DiscoverConfig.Managed.
//  2. User: Home/.claude/skills and Home/.agents/skills, when Home is set.
//  3. Project: the skill folders of Dir and each parent up to the project
//     root, nearest first.
//  4. Additional directories.
//  5. Nested: the skill folders below Dir, when Nested is set.
//
// Within a directory, .claude/skills comes before .agents/skills. A skill
// whose name was already found is not used and is reported as a Conflict,
// except nested skills, which are renamed; a skill reached through
// several locations, for example by symbolic links to the same directory,
// loads once. Folders named "synced" at the top of a skill folder are
// skipped, as Claude Code keeps claude.ai skills there. Missing folders
// are skipped; the error joins the skills that failed to load, and the
// others are returned.
func Discover(cfg DiscoverConfig) (*Discovery, error) {
	dir := cfg.Dir
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		dir = wd
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	root := cfg.Root
	if root == "" {
		root = projectRoot(dir)
	} else if root, err = filepath.Abs(root); err != nil {
		return nil, err
	}
	folders := cfg.Folders
	if folders == nil {
		folders = DefaultFolders
	}
	in := func(scope Scope, dirs ...string) []Source {
		var out []Source
		for _, d := range dirs {
			for _, f := range folders {
				out = append(out, Source{Scope: scope, Folder: filepath.Join(d, filepath.FromSlash(f))})
			}
		}
		return out
	}

	var sources []Source
	if cfg.Managed != "" {
		sources = append(sources, in(ScopeManaged, cfg.Managed)...)
	}
	var project []string
	for d := dir; ; d = filepath.Dir(d) {
		project = append(project, d)
		if d == root || filepath.Dir(d) == d {
			break
		}
	}
	user := []Source(nil)
	if cfg.Home != "" {
		user = in(ScopeUser, cfg.Home)
	}
	if cfg.ProjectFirst {
		sources = append(append(sources, in(ScopeProject, project...)...), user...)
	} else {
		sources = append(append(sources, user...), in(ScopeProject, project...)...)
	}
	sources = append(sources, in(ScopeAdditional, cfg.AdditionalDirs...)...)

	d := &Discovery{Sources: map[string]Source{}}
	found := map[string]string{} // name -> skill directory
	seen := map[string]bool{}    // real skill directories
	var errs []error
	var opts []LoadOption
	if cfg.Strict {
		opts = append(opts, Strict())
	}
	load := func(src Source, scope string) {
		skills, err := LoadDir(src.Folder, opts...)
		if err != nil {
			errs = append(errs, err)
		}
		for _, s := range skills {
			if rel, err := filepath.Rel(src.Folder, s.Dir()); err == nil {
				if first, _, _ := strings.Cut(filepath.ToSlash(rel), "/"); strings.EqualFold(first, "synced") {
					continue
				}
			}
			real := s.Dir()
			if r, err := filepath.EvalSymlinks(real); err == nil {
				real = r
			}
			if seen[real] {
				continue
			}
			seen[real] = true
			name := s.Metadata().Name
			if scope != "" {
				rel, _ := filepath.Rel(dir, scope)
				ns := &nestedSkill{Skill: s, name: name, scope: scope}
				if _, taken := found[name]; taken {
					ns.name = filepath.ToSlash(rel) + ":" + name
					ns.rel = filepath.ToSlash(rel)
				}
				s, name = ns, ns.name
			}
			if kept, taken := found[name]; taken {
				d.Conflicts = append(d.Conflicts, Conflict{Name: name, Kept: kept, Shadowed: s.Dir()})
				continue
			}
			found[name] = s.Dir()
			d.Skills = append(d.Skills, s)
			d.Sources[name] = src
		}
	}
	for _, src := range sources {
		load(src, "")
	}
	if cfg.Nested {
		for _, sub := range nestedDirs(dir, folders) {
			for _, src := range in(ScopeNested, sub) {
				load(src, sub)
			}
		}
	}
	return d, errors.Join(errs...)
}

// projectRoot returns the nearest directory from dir upwards that contains
// .git, or dir.
func projectRoot(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return dir
		}
	}
}

// maxNestedDepth bounds the search for nested skill folders.
const maxNestedDepth = 16

// nestedDirs returns the directories below dir, in lexical order, that
// have one of the skill folders.
func nestedDirs(dir string, folders []string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		switch {
		case err != nil:
			// An unreadable directory is skipped; WalkDir passes only
			// directory errors.
			return filepath.SkipDir
		case !e.IsDir() || p == dir:
			return nil
		}
		name := e.Name()
		rel, _ := filepath.Rel(dir, p)
		if strings.HasPrefix(name, ".") || name == "node_modules" || strings.Count(filepath.ToSlash(rel), "/") >= maxNestedDepth {
			return filepath.SkipDir
		}
		if slices.ContainsFunc(folders, func(f string) bool {
			info, err := os.Stat(filepath.Join(p, filepath.FromSlash(f)))
			return err == nil && info.IsDir()
		}) {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// nestedSkill is a skill from a directory below the working directory.
type nestedSkill struct {
	Skill
	name  string
	scope string
	// rel is the directory relative to the working directory when the
	// skill was renamed after it.
	rel string
}

func (s *nestedSkill) Metadata() Metadata {
	meta := s.Skill.Metadata()
	meta.Name = s.name
	if s.rel != "" {
		meta.Description = strings.TrimSpace(meta.Description) + " (Applies to files in " + s.rel + ".)"
	}
	return meta
}

// ScopeDir implements Scoped.
func (s *nestedSkill) ScopeDir() string { return s.scope }
