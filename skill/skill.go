// Package skill implements Agent Skills: folders of instructions that a
// model discovers by name and description and loads on demand.
//
// A skill is a directory containing a SKILL.md file with YAML frontmatter:
//
//	---
//	name: pdf-tools
//	description: Extract text and tables from PDF files. Use when the user asks about PDFs.
//	allowed-tools: read_file, bash
//	---
//	# PDF tools
//	...
//
// Only the name and description are placed in the system prompt. The body is
// returned when the model invokes the skill tool, which keeps the prompt small
// however many skills are installed.
//
// The frontmatter fields are those of Claude Code
// (https://code.claude.com/docs/en/skills#frontmatter-reference), a
// superset of the Agent Skills specification
// (https://agentskills.io/specification). A Manager gives them effect:
// arguments and substitutions, optional command injection, context: fork
// sub-agents, pre-approved and removed tools, model and effort overrides,
// skill hooks and path-scoped skills.
package skill

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// FileName is the name of the file that defines a skill.
const FileName = "SKILL.md"

// Metadata is the frontmatter of a skill. Field names follow Claude Code's
// frontmatter reference; fields of the Agent Skills specification are
// included.
type Metadata struct {
	// Name is the skill name, by default the name of its directory.
	Name string
	// Description says what the skill does and when to use it. When the
	// frontmatter has none, the loader uses the first non-empty line of
	// the body, as Claude Code does.
	Description string
	// WhenToUse is "when_to_use": more context for when to invoke the
	// skill, appended to Description in the skill listing.
	WhenToUse string
	// ArgumentHint is "argument-hint", such as "[issue-number]", shown to
	// users and to the model.
	ArgumentHint string
	// Arguments names the positional arguments, so $name expands to the
	// argument at that name's position.
	Arguments []string
	// DisableModelInvocation hides the skill from the model: it is not
	// listed and the skill tool refuses it, but users can invoke it; see
	// Registry.Invoke.
	DisableModelInvocation bool
	// UserInvocable is false when "user-invocable: false" hides the skill
	// from users; the model can still invoke it. Nil means true.
	UserInvocable *bool
	// AllowedTools are tool rules, such as "read_file" or
	// "Bash(git add *)", that need no approval while the skill is active.
	// They do not restrict other tools.
	AllowedTools []string
	// DisallowedTools are tool rules removed from the model while the skill
	// is active.
	DisallowedTools []string
	// Model is the model to use while the skill is active, or for its
	// sub-agent with context: fork. "inherit" keeps the current model.
	Model string
	// Effort is the reasoning effort while the skill is active: low,
	// medium, high, xhigh or max.
	Effort string
	// Context is "fork" to run the skill in an isolated sub-agent.
	Context string
	// Agent names the sub-agent of a context: fork skill; the default is
	// general-purpose.
	Agent string
	// Background is false when a forked skill should block the turn that
	// invoked it. agentx always waits for a forked skill's result, as
	// Claude Code does in the Agent SDK; the field is informational.
	Background *bool
	// Hooks holds hooks, in the format of the "hooks" block of a Claude
	// Code settings file, that are registered for the session when the
	// skill is invoked. Values are JSON types.
	Hooks map[string]any
	// Paths are glob patterns: a skill with paths is offered to the model
	// only once it works with matching files.
	Paths []string
	// Shell is the shell of injected commands: bash (the default) or
	// powershell.
	Shell string
	// License, Compatibility and Metadata are fields of the Agent Skills
	// specification that agentx does not act on. Metadata values are
	// strings, []any or map[string]any.
	License       string
	Compatibility string
	Metadata      map[string]any
	// Extra holds frontmatter keys that are not recognized. Sequences are
	// joined with ", " and mappings encoded as JSON.
	Extra map[string]string
}

// Fork reports whether the skill runs in a sub-agent (context: fork).
func (m Metadata) Fork() bool { return strings.EqualFold(strings.TrimSpace(m.Context), "fork") }

// IsUserInvocable reports whether users may invoke the skill.
func (m Metadata) IsUserInvocable() bool { return m.UserInvocable == nil || *m.UserInvocable }

// Skill is a loadable unit of instructions.
type Skill interface {
	Metadata() Metadata
	// Content returns the instructions without frontmatter.
	Content(ctx context.Context) (string, error)
	// Dir is the skill's base directory, used to resolve bundled files. It is
	// empty for skills that are not backed by a directory.
	Dir() string
}

var namePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// reservedName is the name Claude Code reserves for skills synced from
// claude.ai.
const reservedName = "anthropic-skills"

// Validate checks metadata against the rules of Claude Code
// (https://code.claude.com/docs/en/skills#characters-a-skill-name-cant-hold):
// a name is required and may hold any printable characters except "<" and
// ">"; it may not hold control, format (zero-width or bidirectional),
// private-use or unassigned characters, or blanks other than single spaces
// between words, and it may not be "anthropic-skills" or start with
// "anthropic-skills:". A description is required. ValidateStrict applies
// the Agent Skills specification instead.
func (m Metadata) Validate() error {
	if err := checkName(m.Name); err != nil {
		return err
	}
	if strings.TrimSpace(m.Description) == "" {
		return fmt.Errorf("skill %q: description is required", m.Name)
	}
	return nil
}

func checkName(name string) error {
	switch {
	case name == "":
		return errors.New("skill: name is required")
	case name == reservedName || strings.HasPrefix(name, reservedName+":"):
		return fmt.Errorf("skill %q: the name is reserved", name)
	case strings.TrimSpace(name) != name || strings.Contains(name, "  "):
		return fmt.Errorf("skill %q: name may only contain single spaces between words", name)
	case !utf8.ValidString(name):
		return fmt.Errorf("skill %q: name is not valid UTF-8", name)
	}
	for _, r := range name {
		switch {
		case r == '<' || r == '>':
			return fmt.Errorf("skill %q: name may not contain %q", name, r)
		case unicode.IsSpace(r) && r != ' ', !unicode.IsGraphic(r), unicode.Is(unicode.Cf, r):
			// IsGraphic excludes control, private-use and unassigned
			// characters; Cf holds zero-width and bidirectional controls.
			return fmt.Errorf("skill %q: name contains the character %U", name, r)
		}
	}
	return nil
}

// ValidateStrict checks metadata against the Agent Skills specification
// (https://agentskills.io/specification): a name of 1 to 64 lowercase
// letters, digits and single hyphens, neither leading nor trailing; a
// description of 1 to 1024 characters; and a compatibility of at most 500
// characters. Loading with Strict also requires the name to match the
// skill's directory.
func (m Metadata) ValidateStrict() error {
	switch {
	case m.Name == "":
		return errors.New("skill: name is required")
	case len(m.Name) > 64:
		return fmt.Errorf("skill %q: name exceeds 64 characters", m.Name)
	case !namePattern.MatchString(m.Name):
		return fmt.Errorf("skill %q: name must contain only lowercase letters, digits and single hyphens", m.Name)
	case strings.TrimSpace(m.Description) == "":
		return fmt.Errorf("skill %q: description is required", m.Name)
	case utf8.RuneCountInString(m.Description) > 1024:
		return fmt.Errorf("skill %q: description exceeds 1024 characters", m.Name)
	case utf8.RuneCountInString(m.Compatibility) > 500:
		return fmt.Errorf("skill %q: compatibility exceeds 500 characters", m.Name)
	}
	return nil
}

// static is a skill whose content is held in memory.
type static struct {
	meta    Metadata
	content string
	dir     string
}

func (s *static) Metadata() Metadata                      { return s.meta }
func (s *static) Content(context.Context) (string, error) { return s.content, nil }
func (s *static) Dir() string                             { return s.dir }

// New returns an in-memory skill.
func New(name, description, content string, allowedTools ...string) (Skill, error) {
	return Define(Metadata{Name: name, Description: description, AllowedTools: allowedTools}, content)
}

// Define returns an in-memory skill with the given metadata. An empty
// description defaults to the first non-empty line of content.
func Define(meta Metadata, content string) (Skill, error) {
	content = strings.TrimSpace(content)
	if strings.TrimSpace(meta.Description) == "" {
		meta.Description = firstLine(content)
	}
	if err := meta.Validate(); err != nil {
		return nil, err
	}
	return &static{meta: meta, content: content}, nil
}

// firstLine returns the first non-empty line of s.
func firstLine(s string) string {
	for line := range strings.Lines(s) {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// fsSkill reads its body lazily from a file system.
type fsSkill struct {
	meta Metadata
	fsys fs.FS
	path string
	dir  string
}

func (s *fsSkill) Metadata() Metadata { return s.meta }
func (s *fsSkill) Dir() string        { return s.dir }

func (s *fsSkill) Content(context.Context) (string, error) {
	b, err := fs.ReadFile(s.fsys, s.path)
	if err != nil {
		return "", err
	}
	_, body, err := Parse(string(b))
	if err != nil {
		return "", err
	}
	return body, nil
}
