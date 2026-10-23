// Package ragtest checks rag.VectorStore implementations against the
// contract the built-in store follows.
package ragtest

import (
	"context"
	"hash/fnv"
	"slices"
	"testing"

	"github.com/nostosx/agentx/memory"
	"github.com/nostosx/agentx/rag"
)

// Run runs the conformance tests against a store made by newStore, which
// must return an empty store. The contract:
//
//   - Replace stores copies of the chunks of each source, replacing all
//     chunks the source had; it rejects an unnamed source, a chunk without
//     an ID and duplicate chunk IDs within a source. Sources reports each
//     source with its hash and chunk count, ordered by source.
//   - List returns copies of the matching chunks ordered by source,
//     document and index; filters select sources by name or prefix and
//     chunks by metadata, comparing numbers by value.
//   - Search returns the chunks most similar to a vector, best first,
//     limited to limit when it is positive.
//   - Delete removes sources; deleting an unknown source succeeds.
//   - A store that implements rag.Revisioner changes its revision with
//     every change.
func Run(t *testing.T, newStore func(t *testing.T) rag.VectorStore) {
	t.Helper()
	s := newStore(t)
	ctx := t.Context()
	rev := revision(s)
	a1 := chunk(t, "a.md", "a1", "go channels and goroutines", map[string]any{"lang": "en", "n": 1})
	a2 := chunk(t, "a.md", "a2", "python asyncio", map[string]any{"lang": "en", "n": 2})
	b1 := chunk(t, "b/c.md", "b1", "go generics", map[string]any{"lang": "zh"})
	a2.Index = 1
	err := s.Replace(ctx,
		rag.SourceChunks{Info: rag.SourceInfo{Source: "a.md", Hash: "h1"}, Chunks: []*rag.Chunk{a2, a1}},
		rag.SourceChunks{Info: rag.SourceInfo{Source: "b/c.md", Hash: "h2"}, Chunks: []*rag.Chunk{b1}})
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := s.(rag.Revisioner); ok && r.Revision() == rev {
		t.Error("revision did not change")
	}
	// The store keeps its own copies.
	a1.Content, a1.Metadata["lang"] = "changed", "changed"
	infos, _ := s.Sources(ctx)
	if len(infos) != 2 || infos[0].Source != "a.md" || infos[0].Chunks != 2 || infos[0].Hash != "h1" || infos[1].Source != "b/c.md" {
		t.Fatalf("Sources = %+v", infos)
	}
	all, _ := s.List(ctx, rag.Filter{})
	if ids := chunkIDs(all); !slices.Equal(ids, []string{"a1", "a2", "b1"}) || all[0].Content != "go channels and goroutines" || all[0].Metadata["lang"] != "en" {
		t.Fatalf("List = %v", ids)
	}
	all[0].Metadata["lang"] = "mutated"
	for _, tc := range []struct {
		f    rag.Filter
		want []string
	}{
		{rag.Filter{Sources: []string{"b/c.md"}}, []string{"b1"}},
		{rag.Filter{SourcePrefix: "b/"}, []string{"b1"}},
		{rag.Filter{Metadata: map[string]any{"lang": "en"}}, []string{"a1", "a2"}},
		{rag.Filter{Metadata: map[string]any{"n": 2.0}}, []string{"a2"}},
		{rag.Filter{Metadata: map[string]any{"missing": 1}}, nil},
	} {
		got, err := s.List(ctx, tc.f)
		if err != nil || !slices.Equal(chunkIDs(got), tc.want) {
			t.Errorf("List(%+v) = %v, %v", tc.f, chunkIDs(got), err)
		}
	}
	hits, err := s.Search(ctx, embed(t, "go generics"), 2, rag.Filter{})
	if err != nil || len(hits) != 2 || hits[0].Chunk.ID != "b1" || hits[0].Score < 0.99 {
		t.Fatalf("Search = %+v, %v", hits, err)
	}
	hits, _ = s.Search(ctx, embed(t, "go generics"), 0, rag.Filter{Metadata: map[string]any{"lang": "en"}})
	if len(hits) != 2 || hits[0].Chunk.ID != "a1" {
		t.Fatalf("filtered Search = %+v", hits)
	}
	// Replacing a source drops chunks it no longer has.
	if err := s.Replace(ctx, rag.SourceChunks{Info: rag.SourceInfo{Source: "a.md", Hash: "h3"}, Chunks: []*rag.Chunk{chunk(t, "a.md", "a3", "rust", nil)}}); err != nil {
		t.Fatal(err)
	}
	all, _ = s.List(ctx, rag.Filter{})
	if ids := chunkIDs(all); !slices.Equal(ids, []string{"a3", "b1"}) {
		t.Fatalf("after replace = %v", ids)
	}
	rev = revision(s)
	if err := s.Delete(ctx, "b/c.md", "unknown"); err != nil {
		t.Fatal(err)
	}
	if r, ok := s.(rag.Revisioner); ok && r.Revision() == rev {
		t.Error("revision did not change on Delete")
	}
	infos, _ = s.Sources(ctx)
	if len(infos) != 1 || infos[0].Hash != "h3" {
		t.Fatalf("after delete = %+v", infos)
	}
	if err := s.Replace(ctx, rag.SourceChunks{Info: rag.SourceInfo{Source: ""}}); err == nil {
		t.Error("unnamed source accepted")
	}
	dup := rag.SourceChunks{Info: rag.SourceInfo{Source: "d"}, Chunks: []*rag.Chunk{{ID: "x"}, {ID: "x"}}}
	if err := s.Replace(ctx, dup); err == nil {
		t.Error("duplicate chunk IDs accepted")
	}
	if err := s.Replace(ctx, rag.SourceChunks{Info: rag.SourceInfo{Source: "d"}, Chunks: []*rag.Chunk{{}}}); err == nil {
		t.Error("chunk without ID accepted")
	}
	if infos, _ = s.Sources(ctx); len(infos) != 1 {
		t.Errorf("rejected sources were stored: %+v", infos)
	}
}

func revision(s rag.VectorStore) uint64 {
	if r, ok := s.(rag.Revisioner); ok {
		return r.Revision()
	}
	return 0
}

// embed embeds text as a hashed bag of words.
func embed(t *testing.T, text string) []float32 {
	t.Helper()
	v, err := Embedder{}.Embed(t.Context(), []string{text})
	if err != nil {
		t.Fatal(err)
	}
	return v[0]
}

// Embedder embeds texts as 64-dimension hashed bags of words, so texts
// sharing words are similar. It is deterministic and needs no model.
type Embedder struct{}

var _ memory.Embedder = Embedder{}

// Embed implements memory.Embedder.
func (Embedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		v := make([]float32, 64)
		for _, tok := range memory.Tokenize(text) {
			h := fnv.New32a()
			h.Write([]byte(tok))
			v[h.Sum32()%64]++
		}
		out[i] = v
	}
	return out, nil
}

func chunk(t *testing.T, source, id, content string, meta map[string]any) *rag.Chunk {
	return &rag.Chunk{ID: id, DocumentID: source, Source: source, Content: content, Metadata: meta, Embedding: embed(t, content)}
}

func chunkIDs(chunks []*rag.Chunk) []string {
	var ids []string
	for _, c := range chunks {
		ids = append(ids, c.ID)
	}
	return ids
}
