package skill

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nostosx/agentx/tool"
)

// ToolName is the name of the tool that loads skills.
const ToolName = "skill"

// Registry holds skills by name.
type Registry struct {
	mu     sync.RWMutex
	skills map[string]Skill
}

// NewRegistry returns a registry containing skills. Later skills with the
// same name replace earlier ones, so project skills can override user skills.
func NewRegistry(skills ...Skill) *Registry {
	r := &Registry{skills: map[string]Skill{}}
	for _, s := range skills {
		r.Register(s)
	}
	return r
}

// Register adds or replaces a skill.
func (r *Registry) Register(s Skill) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.skills == nil {
		r.skills = map[string]Skill{}
	}
	r.skills[s.Metadata().Name] = s
}

// Get returns a skill by name.
func (r *Registry) Get(name string) (Skill, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.skills[name]
	return s, ok
}

// Len returns the number of skills.
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.skills)
}

// List returns skills sorted by name.
func (r *Registry) List() []Skill {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Skill, 0, len(r.skills))
	for _, name := range slices.Sorted(maps.Keys(r.skills)) {
		out = append(out, r.skills[name])
	}
	return out
}

// Names returns the skill names in sorted order.
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Sorted(maps.Keys(r.skills))
}

// maxListingChars bounds each skill's description and when_to_use in the
// listing, as Claude Code truncates them at 1,536 characters.
const maxListingChars = 1536

// Prompt describes the skills offered to the model for the system prompt:
// all skills except those with disable-model-invocation and those that
// apply only to some files (see Conditional). Each skill is listed with its
// description, followed by its when_to_use, cut at 1,536 characters. It is
// empty when there are no such skills. Manager.Prompt also lists the
// conditional skills that apply to a conversation.
func (r *Registry) Prompt() string {
	var skills []Skill
	for _, s := range r.List() {
		if !s.Metadata().DisableModelInvocation && !Conditional(s) {
			skills = append(skills, s)
		}
	}
	return r.prompt(skills)
}

func (r *Registry) prompt(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("The following skills provide specialized instructions. When a task matches a skill's description, call the `")
	b.WriteString(ToolName)
	b.WriteString("` tool with its name, and any arguments, before starting the task, then follow the instructions it returns.\n\n<available_skills>\n")
	for _, s := range skills {
		b.WriteString(listEntry(s.Metadata()))
	}
	b.WriteString("</available_skills>")
	return b.String()
}

// listEntry returns the line of the skill listing for meta.
func listEntry(meta Metadata) string {
	text := strings.Join(strings.Fields(meta.Description+" "+meta.WhenToUse), " ")
	if utf8.RuneCountInString(text) > maxListingChars {
		text = string([]rune(text)[:maxListingChars-1]) + "…"
	}
	name := meta.Name
	if meta.ArgumentHint != "" {
		name += " " + strings.Join(strings.Fields(meta.ArgumentHint), " ")
	}
	return "- " + name + ": " + text + "\n"
}

// Load returns the text given to the model when a skill is invoked without
// arguments, with the defaults of Config.
func (r *Registry) Load(ctx context.Context, name string) (string, error) {
	s, ok := r.Get(name)
	if !ok {
		return "", fmt.Errorf("unknown skill %q; available skills: %s", name, strings.Join(r.Names(), ", "))
	}
	text, err := NewManager(r, Config{}).render(ctx, s, Vars{})
	if err != nil {
		return "", err
	}
	return wrap(s, text), nil
}

// Invoke renders a skill as a user invokes it with arguments, with the
// defaults of Config; see Manager.Invoke. Unlike the skill tool, it
// accepts skills with disable-model-invocation.
func (r *Registry) Invoke(ctx context.Context, name, arguments string) (string, error) {
	return NewManager(r, Config{}).Invoke(ctx, "", Call{Name: name, Arguments: arguments})
}

// Tool returns the tool that invokes skills, with the defaults of Config:
// skills get their arguments and substitutions, and context: fork skills
// run inline. See Manager.Tool.
func (r *Registry) Tool() tool.Tool { return NewManager(r, Config{}).Tool() }
