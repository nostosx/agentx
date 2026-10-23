package rag

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nostosx/agentx/memory"
)

// SourceInfo records an indexed source.
type SourceInfo struct {
	Source string `json:"source"`
	// Hash identifies the indexed content of the source; see Index.
	Hash      string    `json:"hash"`
	Chunks    int       `json:"chunks"`
	IndexedAt time.Time `json:"indexed_at"`
}

// SourceChunks is the indexed content of a source.
type SourceChunks struct {
	Info   SourceInfo `json:"info"`
	Chunks []*Chunk   `json:"chunks"`
}

// VectorStore stores chunks and their embeddings, grouped by source.
//
// Implementations must be safe for concurrent use. Chunks passed in and
// returned are copies, except that embeddings and values within metadata
// may be shared and must be treated as immutable.
type VectorStore interface {
	// Replace replaces all chunks of each source, recording its info. Each
	// source is replaced atomically.
	Replace(ctx context.Context, sources ...SourceChunks) error
	// Delete removes sources and their chunks. Unknown sources are
	// ignored.
	Delete(ctx context.Context, sources ...string) error
	// Sources returns the indexed sources ordered by name.
	Sources(ctx context.Context) ([]SourceInfo, error)
	// List returns the chunks matching f, ordered by source, document and
	// position.
	List(ctx context.Context, f Filter) ([]*Chunk, error)
	// Search returns up to limit chunks matching f with an embedding,
	// ranked by cosine similarity to vector. A non-positive limit returns
	// all of them.
	Search(ctx context.Context, vector []float32, limit int, f Filter) ([]Hit, error)
}

// Revisioner is implemented by stores that count their changes, such as
// MemoryStore. Keyword caches its index until the revision changes.
type Revisioner interface {
	Revision() uint64
}

// MemoryStore keeps chunks in process memory and searches them
// exhaustively. The zero value is ready to use.
type MemoryStore struct {
	mu       sync.RWMutex
	sources  map[string]*SourceChunks
	revision uint64
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

// Replace implements VectorStore.
func (s *MemoryStore) Replace(_ context.Context, sources ...SourceChunks) error {
	copies := make([]*SourceChunks, len(sources))
	for i, src := range sources {
		c, err := prepareSource(src)
		if err != nil {
			return err
		}
		copies[i] = c
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sources == nil {
		s.sources = map[string]*SourceChunks{}
	}
	for _, c := range copies {
		s.sources[c.Info.Source] = c
	}
	s.revision++
	return nil
}

// prepareSource validates src and returns a copy whose chunks carry its
// source, in document order.
func prepareSource(src SourceChunks) (*SourceChunks, error) {
	if src.Info.Source == "" {
		return nil, errors.New("rag: source has no name")
	}
	c := &SourceChunks{Info: src.Info, Chunks: make([]*Chunk, len(src.Chunks))}
	seen := make(map[string]bool, len(src.Chunks))
	for i, ch := range src.Chunks {
		if ch.ID == "" {
			return nil, fmt.Errorf("rag: chunk of source %s has no ID", src.Info.Source)
		}
		if seen[ch.ID] {
			return nil, fmt.Errorf("rag: duplicate chunk ID %s in source %s", ch.ID, src.Info.Source)
		}
		seen[ch.ID] = true
		cp := ch.clone()
		cp.Source = src.Info.Source
		c.Chunks[i] = cp
	}
	c.Info.Chunks = len(c.Chunks)
	sortChunks(c.Chunks)
	return c, nil
}

func sortChunks(chunks []*Chunk) {
	slices.SortStableFunc(chunks, func(a, b *Chunk) int {
		return cmp.Or(strings.Compare(a.Source, b.Source), strings.Compare(a.DocumentID, b.DocumentID), cmp.Compare(a.Index, b.Index))
	})
}

// Delete implements VectorStore.
func (s *MemoryStore) Delete(_ context.Context, sources ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, src := range sources {
		delete(s.sources, src)
	}
	s.revision++
	return nil
}

// Sources implements VectorStore.
func (s *MemoryStore) Sources(context.Context) ([]SourceInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]SourceInfo, 0, len(s.sources))
	for _, src := range s.sources {
		out = append(out, src.Info)
	}
	slices.SortFunc(out, func(a, b SourceInfo) int { return strings.Compare(a.Source, b.Source) })
	return out, nil
}

// List implements VectorStore.
func (s *MemoryStore) List(_ context.Context, f Filter) ([]*Chunk, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Chunk
	for _, src := range s.sources {
		for _, c := range src.Chunks {
			if f.Match(c) {
				out = append(out, c.clone())
			}
		}
	}
	sortChunks(out)
	return out, nil
}

// Search implements VectorStore.
func (s *MemoryStore) Search(_ context.Context, vector []float32, limit int, f Filter) ([]Hit, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var hits []Hit
	for _, src := range s.sources {
		for _, c := range src.Chunks {
			if len(c.Embedding) == 0 || !f.Match(c) {
				continue
			}
			hits = append(hits, Hit{Chunk: c, Score: memory.Cosine(vector, c.Embedding)})
		}
	}
	hits = topK(hits, limit)
	for i := range hits {
		hits[i].Chunk = hits[i].Chunk.clone()
	}
	return hits, nil
}

// Revision implements Revisioner.
func (s *MemoryStore) Revision() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.revision
}

var (
	_ VectorStore = (*MemoryStore)(nil)
	_ Revisioner  = (*MemoryStore)(nil)
)
