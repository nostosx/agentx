package rag

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func docsOf(files map[string]string) []*Document {
	var docs []*Document
	for name, content := range files {
		docs = append(docs, &Document{ID: name, Source: name, Content: content})
	}
	slices.SortFunc(docs, func(a, b *Document) int { return strings.Compare(a.ID, b.ID) })
	return docs
}

func TestIndexAddAndSearch(t *testing.T) {
	emb := &bowEmbedder{}
	ix := NewIndex(Config{Embedder: emb, Splitter: &RecursiveSplitter{Size: 12, Overlap: -1}})
	docs := docsOf(map[string]string{
		"go.md":     "Go channels connect goroutines.\n\nGo modules manage dependencies and versions.",
		"python.md": "Python asyncio runs coroutines on an event loop.",
	})
	res, err := ix.Add(t.Context(), docs...)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Added, []string{"go.md", "python.md"}) || res.Chunks != 3 || len(res.Updated)+len(res.Unchanged) != 0 {
		t.Fatalf("result = %+v", res)
	}
	hits, err := ix.Search(t.Context(), "goroutines channels", 2)
	if err != nil || len(hits) == 0 || hits[0].Chunk.Source != "go.md" || !strings.Contains(hits[0].Chunk.Content, "channels") {
		t.Fatalf("hits = %+v, %v", hits, err)
	}
	if len(hits[0].Chunk.Embedding) != 64 {
		t.Fatal("chunk not embedded")
	}
	hits, err = ix.Retrieve(t.Context(), Query{Text: "goroutines coroutines", Filter: Filter{Sources: []string{"go.md"}}})
	if err != nil || len(hits) != 1 {
		t.Fatalf("filtered hits = %+v, %v", hits, err)
	}
	for _, h := range hits {
		if h.Chunk.Source != "go.md" {
			t.Fatalf("filter ignored: %v", h.Chunk.Source)
		}
	}
	// Adding the same content again does nothing.
	before := len(emb.embedded())
	res, err = ix.Add(t.Context(), docs...)
	if err != nil || !slices.Equal(res.Unchanged, []string{"go.md", "python.md"}) || res.Chunks != 0 || len(emb.embedded()) != before {
		t.Fatalf("re-add = %+v, %v", res, err)
	}
	if err := ix.Delete(t.Context(), "go.md"); err != nil {
		t.Fatal(err)
	}
	infos, _ := ix.Store().Sources(t.Context())
	if len(infos) != 1 || infos[0].Source != "python.md" || infos[0].Chunks != 1 || infos[0].IndexedAt.IsZero() {
		t.Fatalf("sources = %+v", infos)
	}
}

func TestIndexSyncIncremental(t *testing.T) {
	emb := &bowEmbedder{}
	ix := NewIndex(Config{Embedder: emb})
	files := map[string]string{"a.md": "alpha", "b.md": "bravo", "c.md": "charlie"}
	res, err := ix.Sync(t.Context(), docsOf(files))
	if err != nil || len(res.Added) != 3 {
		t.Fatalf("first sync = %+v, %v", res, err)
	}
	files["b.md"] = "bravo two"
	delete(files, "c.md")
	files["d.md"] = "delta"
	before := len(emb.embedded())
	res, err = ix.Sync(t.Context(), docsOf(files))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Added, []string{"d.md"}) || !slices.Equal(res.Updated, []string{"b.md"}) ||
		!slices.Equal(res.Unchanged, []string{"a.md"}) || !slices.Equal(res.Deleted, []string{"c.md"}) || res.Chunks != 2 {
		t.Fatalf("second sync = %+v", res)
	}
	if got := emb.embedded()[before:]; !slices.Equal(got, []string{"bravo two", "delta"}) {
		t.Fatalf("re-embedded %q", got)
	}
	if hits, err := ix.Search(t.Context(), "charlie", 5); err != nil || len(hits) != 0 {
		t.Fatalf("search deleted source = %+v, %v", hits, err)
	}
	// Metadata changes count as content changes.
	docs := docsOf(files)
	docs[0].Metadata = map[string]any{"owner": "team"}
	res, _ = ix.Sync(t.Context(), docs)
	if !slices.Equal(res.Updated, []string{"a.md"}) {
		t.Fatalf("metadata change = %+v", res)
	}
	// SyncPrefix limits deletions; an empty sync deletes the rest.
	if _, err := ix.Add(t.Context(), &Document{ID: "web:x", Content: "external"}); err != nil {
		t.Fatal(err)
	}
	res, _ = ix.Sync(t.Context(), nil, SyncPrefix("web:"))
	if !slices.Equal(res.Deleted, []string{"web:x"}) {
		t.Fatalf("prefixed sync = %+v", res)
	}
	res, _ = ix.Sync(t.Context(), nil)
	if len(res.Deleted) != 3 {
		t.Fatalf("empty sync = %+v", res)
	}
}

func TestIndexVersionAndMultiDocumentSources(t *testing.T) {
	store := NewMemoryStore()
	rows := []*Document{
		{ID: "p.csv#2", Source: "p.csv", Content: "name: Gadget"},
		{ID: "p.csv#1", Source: "p.csv", Content: "name: Widget"},
	}
	ix := NewIndex(Config{Store: store})
	res, err := ix.Add(t.Context(), rows...)
	if err != nil || res.Chunks != 2 || !slices.Equal(res.Added, []string{"p.csv"}) {
		t.Fatalf("add = %+v, %v", res, err)
	}
	// Document order does not change the hash.
	if res, _ := ix.Add(t.Context(), rows[1], rows[0]); len(res.Unchanged) != 1 {
		t.Fatalf("reordered = %+v", res)
	}
	all, _ := store.List(t.Context(), Filter{})
	if all[0].DocumentID != "p.csv#1" || all[0].ID != ChunkID("p.csv", "p.csv#1", 0) {
		t.Fatalf("chunks = %+v", all[0])
	}
	// A new version re-indexes unchanged content into the same store.
	ix2 := NewIndex(Config{Store: store, Version: "2"})
	if res, _ := ix2.Add(t.Context(), rows...); !slices.Equal(res.Updated, []string{"p.csv"}) {
		t.Fatalf("version bump = %+v", res)
	}
	// Without an embedder only keyword retrieval runs.
	if hits, err := ix2.Search(t.Context(), "widget", 1); err != nil || len(hits) != 1 || hits[0].Chunk.DocumentID != "p.csv#1" {
		t.Fatalf("keyword = %+v, %v", hits, err)
	}
	for _, bad := range [][]*Document{
		{nil},
		{{Content: "no id"}},
		{{ID: "x", Source: "s"}, {ID: "x", Source: "s"}},
	} {
		if _, err := ix.Add(t.Context(), bad...); err == nil {
			t.Errorf("invalid documents accepted: %+v", bad)
		}
	}
}

func TestIndexBatchesAndFailures(t *testing.T) {
	emb := &bowEmbedder{}
	ix := NewIndex(Config{Embedder: emb, BatchSize: 2, Splitter: &RecursiveSplitter{Size: 2, Overlap: -1}})
	docs := docsOf(map[string]string{"a": "one two three", "b": "four", "c": "five six"})
	res, err := ix.Add(t.Context(), docs...)
	if err != nil || res.Chunks != 4 {
		t.Fatalf("add = %+v, %v", res, err)
	}
	// Batches span sources.
	if len(emb.batches) != 2 || !slices.Equal(emb.batches[1], []string{"four", "five six"}) {
		t.Fatalf("batches = %q", emb.batches)
	}
	// An embedding failure keeps the sources stored before it.
	fail := &bowEmbedder{}
	calls := 0
	failing := embedderFunc(func(ctx context.Context, texts []string) ([][]float32, error) {
		calls++
		if calls > 1 {
			return nil, errors.New("quota")
		}
		return fail.Embed(ctx, texts)
	})
	ix = NewIndex(Config{Embedder: failing, BatchSize: 1})
	res, err = ix.Add(t.Context(), docsOf(map[string]string{"a": "alpha", "b": "bravo"})...)
	if err == nil || !strings.Contains(err.Error(), "quota") || !slices.Equal(res.Added, []string{"a"}) {
		t.Fatalf("failure = %+v, %v", res, err)
	}
	infos, _ := ix.Store().Sources(t.Context())
	if len(infos) != 1 {
		t.Fatalf("stored = %+v", infos)
	}
	// Sync deletes nothing when indexing fails.
	if res, err := ix.Sync(t.Context(), docsOf(map[string]string{"b": "bravo"})); err == nil || len(res.Deleted) != 0 {
		t.Fatalf("failed sync = %+v, %v", res, err)
	}
	short := embedderFunc(func(context.Context, []string) ([][]float32, error) { return nil, nil })
	if _, err := NewIndex(Config{Embedder: short}).Add(t.Context(), &Document{ID: "x", Content: "x"}); err == nil {
		t.Fatal("missing vectors accepted")
	}
	bad := NewIndex(Config{Splitter: &RecursiveSplitter{Size: 1, Overlap: 5}})
	if _, err := bad.Add(t.Context(), &Document{ID: "x", Content: "x"}); err == nil {
		t.Fatal("splitter error ignored")
	}
	enrichErr := NewIndex(Config{Enricher: EnricherFunc(func(context.Context, *Document, []*Chunk) error { return errors.New("enrich") })})
	if _, err := enrichErr.Add(t.Context(), &Document{ID: "x", Content: "x"}); err == nil {
		t.Fatal("enricher error ignored")
	}
}

type embedderFunc func(ctx context.Context, texts []string) ([][]float32, error)

func (f embedderFunc) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return f(ctx, texts)
}

func TestIndexContextualAndReranked(t *testing.T) {
	m := &contextModel{}
	emb := &bowEmbedder{}
	rerank := RerankerFunc(func(_ context.Context, _ string, hits []Hit) ([]Hit, error) {
		out := slices.Clone(hits)
		slices.Reverse(out)
		return out, nil
	})
	ix := NewIndex(Config{
		Embedder: emb,
		Enricher: &ContextualEnricher{Model: m.model()},
		Splitter: &RecursiveSplitter{Size: 6, Overlap: -1},
		Reranker: rerank,
	})
	if _, err := ix.Add(t.Context(), &Document{ID: "doc", Content: "first part here.\n\nsecond part here."}); err != nil {
		t.Fatal(err)
	}
	got := emb.embedded()
	if !slices.Equal(got, []string{"About first part here..\n\nfirst part here.", "About second part here..\n\nsecond part here."}) {
		t.Fatalf("embedded %q", got)
	}
	hits, err := ix.Search(t.Context(), "part", 2)
	if err != nil || len(hits) != 2 {
		t.Fatalf("hits = %+v, %v", hits, err)
	}
	// Both chunks tie on keywords; the reranker reverses the fused order.
	fused, _ := (&Hybrid{Retrievers: []Retriever{&Keyword{Store: ix.Store()}, &Vector{Store: ix.Store(), Embedder: emb}}}).Retrieve(t.Context(), Query{Text: "part", Limit: 20})
	slices.Reverse(fused)
	if !slices.Equal(hitIDs(hits), hitIDs(fused)) {
		t.Fatalf("reranked = %v, fused reversed = %v", hitIDs(hits), hitIDs(fused))
	}
}

func TestIndexCustomRetrieverAndQueryEmbedder(t *testing.T) {
	var asked Query
	r := RetrieverFunc(func(_ context.Context, q Query) ([]Hit, error) { asked = q; return nil, nil })
	ix := NewIndex(Config{Retriever: r})
	if _, err := ix.Search(t.Context(), "q", 4); err != nil || asked.Text != "q" || asked.Limit != 4 {
		t.Fatalf("custom retriever got %+v, %v", asked, err)
	}
	docs, queries := &bowEmbedder{}, &bowEmbedder{}
	ix = NewIndex(Config{Embedder: docs, QueryEmbedder: queries})
	if _, err := ix.Add(t.Context(), &Document{ID: "a", Content: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Search(t.Context(), "alpha", 1); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(queries.embedded(), []string{"alpha"}) || !slices.Equal(docs.embedded(), []string{"alpha"}) {
		t.Fatalf("documents %q, queries %q", docs.embedded(), queries.embedded())
	}
}

func TestIndexPersistentSync(t *testing.T) {
	// The store outlives the indexes, as a persistent store outlives a
	// process.
	store := NewMemoryStore()
	fsys := fstest.MapFS{
		"guide.md": {Data: []byte("# Guide\n\n## Install\n\nRun make install.\n\n## Usage\n\nCall agentx.Run.")},
		"main.go":  {Data: []byte("package main\n\nfunc main() {}\n")},
	}
	open := func() (*Index, *bowEmbedder) {
		emb := &bowEmbedder{}
		return NewIndex(Config{Store: store, Embedder: emb}), emb
	}
	load := func() []*Document {
		docs, err := (&FSLoader{FS: fsys}).Load(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return docs
	}
	ix, _ := open()
	res, err := ix.Sync(t.Context(), load())
	if err != nil || len(res.Added) != 2 {
		t.Fatalf("first sync = %+v, %v", res, err)
	}
	hits, _ := ix.Search(t.Context(), "install", 1)
	if len(hits) != 1 || hits[0].Chunk.Context != "Guide > Install" || !slices.Equal(anyStrings(hits[0].Chunk.Metadata[MetaHeaders]), []string{"Guide", "Install"}) {
		t.Fatalf("hits = %+v", hits)
	}
	// After a restart, unchanged files are not embedded again.
	fsys["guide.md"] = &fstest.MapFile{Data: []byte("# Guide\n\n## Install\n\nRun go install.")}
	ix, emb := open()
	res, err = ix.Sync(t.Context(), load())
	if err != nil || !slices.Equal(res.Updated, []string{"guide.md"}) || !slices.Equal(res.Unchanged, []string{"main.go"}) {
		t.Fatalf("restart sync = %+v, %v", res, err)
	}
	if got := emb.embedded(); len(got) != 1 || !strings.Contains(got[0], "Run go install.") {
		t.Fatalf("embedded after restart %q", got)
	}
	hits, _ = ix.Retrieve(t.Context(), Query{Text: "install", Filter: Filter{Metadata: map[string]any{MetaHeaders: []string{"Guide", "Install"}}}})
	if len(hits) != 1 {
		t.Fatalf("metadata filter after reload = %+v", hits)
	}
}

func anyStrings(v any) []string {
	switch v := v.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, len(v))
		for i, x := range v {
			out[i], _ = x.(string)
		}
		return out
	}
	return nil
}
