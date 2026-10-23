package memory

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"

	"github.com/nostosx/kitx/syncmap"
)

// ErrNotFound is returned when an entry does not exist.
var ErrNotFound = errors.New("memory not found")

// Filter selects entries.
type Filter struct {
	// Scope matches entries with exactly this scope when non-empty.
	Scope string
	// Tags matches entries that carry all of these tags.
	Tags []string
}

// Match reports whether e satisfies the filter.
func (f Filter) Match(e *Entry) bool {
	if f.Scope != "" && e.Scope != f.Scope {
		return false
	}
	for _, t := range f.Tags {
		if !slices.Contains(e.Tags, t) {
			return false
		}
	}
	return true
}

// Store persists memory entries.
type Store interface {
	// Put inserts or replaces an entry by ID.
	Put(ctx context.Context, e *Entry) error
	// Get returns an entry or ErrNotFound.
	Get(ctx context.Context, id string) (*Entry, error)
	Delete(ctx context.Context, id string) error
	// List returns matching entries, oldest first.
	List(ctx context.Context, f Filter) ([]*Entry, error)
}

// MemoryStore keeps entries in process memory. The zero value is ready to
// use.
type MemoryStore struct {
	entries syncmap.Map[string, *Entry]
	mu      sync.Mutex
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

// Put implements Store.
func (s *MemoryStore) Put(_ context.Context, e *Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.ID == "" {
		return errors.New("memory: entry has no ID")
	}
	s.entries.Set(e.ID, e.clone())
	return nil
}

// Get implements Store.
func (s *MemoryStore) Get(_ context.Context, id string) (*Entry, error) {
	e, ok := s.entries.Get(id)
	if !ok {
		return nil, ErrNotFound
	}
	return e.clone(), nil
}

// Delete implements Store.
func (s *MemoryStore) Delete(ctx context.Context, id string) error {
	return s.DeleteInScope(ctx, id, "")
}

// DeleteInScope atomically checks the scope and deletes an entry. An empty
// scope allows deletion of any entry.
func (s *MemoryStore) DeleteInScope(_ context.Context, id, scope string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries.Get(id)
	if !ok || (scope != "" && e.Scope != scope) {
		return ErrNotFound
	}
	s.entries.Delete(id)
	return nil
}

// List implements Store.
func (s *MemoryStore) List(_ context.Context, f Filter) ([]*Entry, error) {
	var out []*Entry
	for _, e := range s.entries.Values() {
		if f.Match(e) {
			out = append(out, e.clone())
		}
	}
	sortEntries(out)
	return out, nil
}

func sortEntries(es []*Entry) {
	slices.SortFunc(es, func(a, b *Entry) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
}
