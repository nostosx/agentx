// Package memory provides long-term memory that persists across sessions,
// with keyword, vector and hybrid retrieval.
package memory

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/nostosx/agentx/tool"
)

// Entry is one remembered fact.
type Entry struct {
	ID        string            `json:"id"`
	Content   string            `json:"content"`
	Tags      []string          `json:"tags,omitempty"`
	Scope     string            `json:"scope,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Embedding []float32         `json:"embedding,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

func (e *Entry) clone() *Entry {
	c := *e
	c.Tags = slices.Clone(e.Tags)
	c.Embedding = slices.Clone(e.Embedding)
	c.Metadata = maps.Clone(e.Metadata)
	return &c
}

// Hit is a retrieved entry with its relevance score.
type Hit struct {
	Entry *Entry
	Score float64
}

// Memory is long-term agent memory.
type Memory interface {
	Remember(ctx context.Context, content string, opts ...RememberOption) (*Entry, error)
	Recall(ctx context.Context, query string, opts ...RecallOption) ([]Hit, error)
	Forget(ctx context.Context, id string) error
}

// RememberOption configures Remember.
type RememberOption func(*Entry)

// WithID sets the entry ID, replacing an existing entry with the same ID.
func WithID(id string) RememberOption { return func(e *Entry) { e.ID = id } }

// WithTags attaches tags.
func WithTags(tags ...string) RememberOption {
	return func(e *Entry) { e.Tags = append(e.Tags, tags...) }
}

// WithScope sets the scope, such as a user or project identifier.
func WithScope(scope string) RememberOption { return func(e *Entry) { e.Scope = scope } }

// WithMetadata attaches a metadata value.
func WithMetadata(key, value string) RememberOption {
	return func(e *Entry) {
		if e.Metadata == nil {
			e.Metadata = map[string]string{}
		}
		e.Metadata[key] = value
	}
}

// RecallOption configures Recall.
type RecallOption func(*Query)

// Limit sets the maximum number of hits. The default is 5.
func Limit(n int) RecallOption { return func(q *Query) { q.Limit = n } }

// InScope restricts recall to a scope.
func InScope(scope string) RecallOption { return func(q *Query) { q.Filter.Scope = scope } }

// Tagged restricts recall to entries with all tags.
func Tagged(tags ...string) RecallOption {
	return func(q *Query) { q.Filter.Tags = append(q.Filter.Tags, tags...) }
}

// MinScore drops hits scoring below s.
func MinScore(s float64) RecallOption { return func(q *Query) { q.MinScore = s } }

// Manager implements Memory over a Store and a Retriever.
type Manager struct {
	store     Store
	retriever Retriever
	embedder  Embedder
	// queryEmbedder embeds recall queries; it defaults to embedder.
	queryEmbedder Embedder
}

// Option configures a Manager.
type Option func(*Manager)

// WithEmbedder embeds entries on write and enables hybrid retrieval unless a
// retriever is set explicitly.
func WithEmbedder(e Embedder) Option { return func(m *Manager) { m.embedder = e } }

// WithRetriever sets the retriever.
func WithRetriever(r Retriever) Option { return func(m *Manager) { m.retriever = r } }

// New returns a Manager. Without options it uses keyword retrieval.
func New(store Store, opts ...Option) *Manager {
	m := &Manager{store: store}
	for _, opt := range opts {
		opt(m)
	}
	if m.retriever == nil {
		kw := &Keyword{Store: store}
		if m.embedder != nil {
			query := m.queryEmbedder
			if query == nil {
				query = m.embedder
			}
			m.retriever = &Hybrid{Retrievers: []Retriever{kw, &Vector{Store: store, Embedder: query}}}
		} else {
			m.retriever = kw
		}
	}
	return m
}

// Store returns the underlying store.
func (m *Manager) Store() Store { return m.store }

// Remember stores content.
func (m *Manager) Remember(ctx context.Context, content string, opts ...RememberOption) (*Entry, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, errors.New("memory: content is empty")
	}
	now := time.Now().UTC()
	e := &Entry{Content: content, CreatedAt: now, UpdatedAt: now}
	for _, opt := range opts {
		opt(e)
	}
	if e.ID == "" {
		e.ID = "mem_" + uuid.NewV7().String()
	} else if old, err := m.store.Get(ctx, e.ID); err == nil {
		e.CreatedAt = old.CreatedAt
	}
	if m.embedder != nil {
		vecs, err := m.embedder.Embed(ctx, []string{content})
		if err != nil {
			return nil, fmt.Errorf("embed memory: %w", err)
		}
		if len(vecs) == 1 {
			e.Embedding = vecs[0]
		}
	}
	if err := m.store.Put(ctx, e); err != nil {
		return nil, err
	}
	return e, nil
}

// Recall returns entries relevant to query.
func (m *Manager) Recall(ctx context.Context, query string, opts ...RecallOption) ([]Hit, error) {
	q := Query{Text: query, Limit: 5}
	for _, opt := range opts {
		opt(&q)
	}
	return m.retriever.Retrieve(ctx, q)
}

// Forget deletes an entry.
func (m *Manager) Forget(ctx context.Context, id string) error { return m.store.Delete(ctx, id) }

// ScopedForgetter is implemented by a Memory that can delete an entry only
// when it belongs to a scope, such as Manager.
type ScopedForgetter interface {
	ForgetInScope(ctx context.Context, id, scope string) error
}

// ScopedDeleter is implemented by a Store that can atomically check the
// scope of an entry and delete it, such as MemoryStore. An empty scope
// deletes any entry.
type ScopedDeleter interface {
	DeleteInScope(ctx context.Context, id, scope string) error
}

// ForgetInScope deletes an entry only when it belongs to scope. An empty
// scope allows deletion of any entry, as with Forget. A non-empty scope
// requires the store to implement ScopedDeleter.
func (m *Manager) ForgetInScope(ctx context.Context, id, scope string) error {
	if scope == "" {
		return m.Forget(ctx, id)
	}
	if scoped, ok := m.store.(ScopedDeleter); ok {
		return scoped.DeleteInScope(ctx, id, scope)
	}
	return errors.New("memory: store does not support scoped deletion")
}

// RecallIn recalls up to limit entries in scope. An empty scope searches all
// entries and a non-positive limit uses the default.
func RecallIn(ctx context.Context, m Memory, scope, query string, limit int) ([]Hit, error) {
	var opts []RecallOption
	if limit > 0 {
		opts = append(opts, Limit(limit))
	}
	if scope != "" {
		opts = append(opts, InScope(scope))
	}
	return m.Recall(ctx, query, opts...)
}

// Format renders hits as a bulleted list for inclusion in a prompt.
func Format(hits []Hit) string {
	var b strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&b, "- [%s] %s", h.Entry.ID, h.Entry.Content)
		if len(h.Entry.Tags) > 0 {
			fmt.Fprintf(&b, " (tags: %s)", strings.Join(h.Entry.Tags, ", "))
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

type rememberArgs struct {
	Content string   `json:"content" jsonschema:"The fact to remember, written so it is understandable without the current conversation."`
	Tags    []string `json:"tags,omitempty" jsonschema:"Optional short tags for later filtering."`
}

type recallArgs struct {
	Query string `json:"query" jsonschema:"What to look for."`
	Limit int    `json:"limit,omitempty" jsonschema:"Maximum number of memories to return (default 5)."`
}

type forgetArgs struct {
	ID string `json:"id" jsonschema:"The ID of the memory to delete, as shown by memory_recall."`
}

// ScopeFunc returns the scope of the memories a call reads and writes,
// for example the user of a multi-tenant server, taken from ctx. An error
// fails the call, so a run whose user is unknown reads and writes nothing.
type ScopeFunc func(ctx context.Context) (string, error)

// FixedScope returns a ScopeFunc that always returns scope.
func FixedScope(scope string) ScopeFunc {
	return func(context.Context) (string, error) { return scope, nil }
}

// Tools returns tools that let a model manage memory: memory_remember,
// memory_recall and memory_forget. Entries are written with the given
// scope, and recall and deletion are restricted to it. With a non-empty
// scope, deletion requires Memory to implement ScopedForgetter; otherwise
// the forget tool returns an error.
func Tools(m Memory, scope string) []tool.Tool { return ScopedTools(m, FixedScope(scope)) }

// ScopedTools is like Tools, with the scope of each call chosen by scope,
// so one agent can serve several users.
func ScopedTools(m Memory, scope ScopeFunc) []tool.Tool {
	remember := tool.Func("memory_remember",
		"Save a durable fact, preference or decision to long-term memory so it is available in future sessions. Do not store secrets.",
		func(ctx context.Context, in rememberArgs) (string, error) {
			s, err := scope(ctx)
			if err != nil {
				return "", err
			}
			opts := []RememberOption{WithTags(in.Tags...)}
			if s != "" {
				opts = append(opts, WithScope(s))
			}
			e, err := m.Remember(ctx, in.Content, opts...)
			if err != nil {
				return "", err
			}
			return "Remembered as " + e.ID, nil
		})
	recall := tool.Func("memory_recall",
		"Search long-term memory for facts relevant to a query.",
		func(ctx context.Context, in recallArgs) (string, error) {
			s, err := scope(ctx)
			if err != nil {
				return "", err
			}
			hits, err := RecallIn(ctx, m, s, in.Query, in.Limit)
			if err != nil {
				return "", err
			}
			if len(hits) == 0 {
				return "No relevant memories found.", nil
			}
			return Format(hits), nil
		}, tool.ReadOnly())
	forget := tool.Func("memory_forget",
		"Delete a memory that is wrong or no longer relevant.",
		func(ctx context.Context, in forgetArgs) (string, error) {
			s, err := scope(ctx)
			if err != nil {
				return "", err
			}
			if s == "" {
				err = m.Forget(ctx, in.ID)
			} else if scoped, ok := m.(ScopedForgetter); ok {
				err = scoped.ForgetInScope(ctx, in.ID, s)
			} else {
				err = errors.New("memory: scoped deletion is not supported")
			}
			if err != nil {
				return "", err
			}
			return "Forgot " + in.ID, nil
		})
	return []tool.Tool{remember, recall, forget}
}

var (
	_ ScopedForgetter = (*Manager)(nil)
	_ ScopedDeleter   = (*MemoryStore)(nil)
)
