package tool

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/nostosx/agentx/model"
)

// ErrNotFound is returned when a tool is not registered.
var ErrNotFound = errors.New("tool not found")

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// ValidateName reports whether name is accepted by all supported providers.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("invalid tool name %q: use 1-64 letters, digits, underscores or hyphens", name)
	}
	return nil
}

// Registry is a concurrency-safe set of tools keyed by name.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry returns a registry containing tools.
func NewRegistry(tools ...Tool) (*Registry, error) {
	r := &Registry{tools: map[string]Tool{}}
	if err := r.Register(tools...); err != nil {
		return nil, err
	}
	return r, nil
}

// Register adds tools. It fails without registering anything if a name is
// invalid or already registered.
func (r *Registry) Register(tools ...Tool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tools == nil {
		r.tools = map[string]Tool{}
	}
	pending := map[string]bool{}
	for _, t := range tools {
		name := t.Info().Name
		if err := ValidateName(name); err != nil {
			return err
		}
		if _, ok := r.tools[name]; ok || pending[name] {
			return fmt.Errorf("tool %q already registered", name)
		}
		pending[name] = true
	}
	for _, t := range tools {
		r.tools[t.Info().Name] = t
	}
	return nil
}

// Remove deletes tools by name.
func (r *Registry) Remove(names ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range names {
		delete(r.tools, n)
	}
}

// Get returns the named tool.
func (r *Registry) Get(name string) (Tool, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Len returns the number of registered tools.
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}

// List returns the tools sorted by name. The stable order keeps prompts
// byte-identical across requests, which preserves provider prompt caches.
func (r *Registry) List() []Tool {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.SortedFunc(maps.Values(r.tools), func(a, b Tool) int {
		return strings.Compare(a.Info().Name, b.Info().Name)
	})
}

// Specs returns model specifications for all tools, sorted by name.
func (r *Registry) Specs() []*model.ToolSpec {
	tools := r.List()
	specs := make([]*model.ToolSpec, len(tools))
	for i, t := range tools {
		specs[i] = Spec(t)
	}
	return specs
}

// Subset returns a new registry with only the named tools. Unknown names are
// reported as an error.
func (r *Registry) Subset(names ...string) (*Registry, error) {
	sub := &Registry{tools: map[string]Tool{}}
	for _, n := range names {
		t, ok := r.Get(n)
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, n)
		}
		sub.tools[n] = t
	}
	return sub, nil
}

// Clone returns a shallow copy of the registry.
func (r *Registry) Clone() *Registry {
	if r == nil {
		return &Registry{tools: map[string]Tool{}}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	c := &Registry{tools: maps.Clone(r.tools)}
	if c.tools == nil {
		c.tools = map[string]Tool{}
	}
	return c
}
