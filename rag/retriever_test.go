package rag

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
)

// plainStore hides the Revisioner of a store.
type plainStore struct{ VectorStore }

func seedStore(t *testing.T) *MemoryStore {
	t.Helper()
	s := NewMemoryStore()
	put := func(source string, chunks ...*Chunk) {
		if err := s.Replace(t.Context(), SourceChunks{Info: SourceInfo{Source: source}, Chunks: chunks}); err != nil {
			t.Fatal(err)
		}
	}
	put("go.md",
		testChunk(t, "go.md", "g1", "Go channels connect goroutines", map[string]any{"topic": "go"}),
		testChunk(t, "go.md", "g2", "Go modules manage dependencies", map[string]any{"topic": "go"}))
	put("py.md", testChunk(t, "py.md", "p1", "Python asyncio runs coroutines", map[string]any{"topic": "python"}))
	put("zh.md", testChunk(t, "zh.md", "z1", "检索增强生成结合外部知识", nil))
	return s
}

func hitIDs(hits []Hit) []string {
	var ids []string
	for _, h := range hits {
		ids = append(ids, h.Chunk.ID)
	}
	return ids
}

func TestKeyword(t *testing.T) {
	s := seedStore(t)
	for name, store := range map[string]VectorStore{"cached": s, "uncached": plainStore{s}} {
		t.Run(name, func(t *testing.T) {
			k := &Keyword{Store: store}
			hits, err := k.Retrieve(t.Context(), Query{Text: "goroutines channels", Limit: 5})
			if err != nil || !slices.Equal(hitIDs(hits), []string{"g1"}) {
				t.Fatalf("hits = %v, %v", hitIDs(hits), err)
			}
			hits, _ = k.Retrieve(t.Context(), Query{Text: "go python", Limit: 5})
			if len(hits) != 3 {
				t.Fatalf("hits = %v", hitIDs(hits))
			}
			hits, _ = k.Retrieve(t.Context(), Query{Text: "go python", Filter: Filter{Metadata: map[string]any{"topic": "python"}}})
			if !slices.Equal(hitIDs(hits), []string{"p1"}) {
				t.Fatalf("filtered = %v", hitIDs(hits))
			}
			hits, _ = k.Retrieve(t.Context(), Query{Text: "外部知识"})
			if !slices.Equal(hitIDs(hits), []string{"z1"}) {
				t.Fatalf("cjk = %v", hitIDs(hits))
			}
			hits, err = k.Retrieve(t.Context(), Query{Text: "go", MinScore: 100})
			if err != nil || len(hits) != 0 {
				t.Fatalf("min score = %v, %v", hitIDs(hits), err)
			}
			if hits, err := k.Retrieve(t.Context(), Query{Text: "  "}); hits != nil || err != nil {
				t.Fatalf("blank = %v, %v", hits, err)
			}
			// Results are copies.
			hits, _ = k.Retrieve(t.Context(), Query{Text: "asyncio"})
			hits[0].Chunk.Content = "mutated"
			hits, _ = k.Retrieve(t.Context(), Query{Text: "asyncio"})
			if hits[0].Chunk.Content != "Python asyncio runs coroutines" {
				t.Fatal("result aliases the index")
			}
		})
	}
	// Negative parameters select 0: without length normalization the
	// shorter chunk no longer wins.
	if hits, _ := (&Keyword{Store: s, K1: -1, B: -1}).Retrieve(t.Context(), Query{Text: "go"}); len(hits) != 2 || hits[0].Score != hits[1].Score {
		t.Fatalf("flat BM25 = %+v", hits)
	}
	// The cache follows changes to the store, and context is searched.
	k := &Keyword{Store: s}
	if hits, err := k.Retrieve(t.Context(), Query{Text: "rustacean"}); err != nil || len(hits) != 0 {
		t.Fatalf("search before change = %v, %v", hitIDs(hits), err)
	}
	c := testChunk(t, "rs.md", "r1", "ownership and borrowing", nil)
	c.Context = "Rust book, rustacean chapter"
	if err := s.Replace(t.Context(), SourceChunks{Info: SourceInfo{Source: "rs.md"}, Chunks: []*Chunk{c}}); err != nil {
		t.Fatal(err)
	}
	if hits, _ := k.Retrieve(t.Context(), Query{Text: "rustacean"}); !slices.Equal(hitIDs(hits), []string{"r1"}) {
		t.Fatalf("after change = %v", hitIDs(hits))
	}
}

func TestVector(t *testing.T) {
	s := seedStore(t)
	v := &Vector{Store: s, Embedder: &bowEmbedder{}}
	hits, err := v.Retrieve(t.Context(), Query{Text: "go modules dependencies", Limit: 1})
	if err != nil || !slices.Equal(hitIDs(hits), []string{"g2"}) {
		t.Fatalf("hits = %v, %v", hitIDs(hits), err)
	}
	hits, err = v.Retrieve(t.Context(), Query{Text: "go modules dependencies", Filter: Filter{Sources: []string{"py.md"}}})
	if err != nil || len(hits) != 0 {
		t.Fatalf("filtered search = %v, %v", hitIDs(hits), err)
	}
	hits, err = v.Retrieve(t.Context(), Query{Text: "go modules", MinScore: 0.99})
	if err != nil || len(hits) != 0 {
		t.Fatalf("min score = %v, %v", hitIDs(hits), err)
	}
	if _, err := (&Vector{Store: s}).Retrieve(t.Context(), Query{Text: "x"}); err == nil {
		t.Fatal("missing embedder accepted")
	}
	if _, err := (&Vector{Store: s, Embedder: &bowEmbedder{err: errors.New("down")}}).Retrieve(t.Context(), Query{Text: "x"}); err == nil {
		t.Fatal("embedder error ignored")
	}
}

func TestHybrid(t *testing.T) {
	fixed := func(ids ...string) Retriever {
		return RetrieverFunc(func(_ context.Context, q Query) ([]Hit, error) {
			var hits []Hit
			for _, id := range ids {
				hits = append(hits, Hit{Chunk: &Chunk{ID: id}, Score: 1})
			}
			if q.Limit > 0 && len(hits) > q.Limit {
				hits = hits[:q.Limit]
			}
			return hits, nil
		})
	}
	failing := RetrieverFunc(func(context.Context, Query) ([]Hit, error) { return nil, errors.New("down") })
	h := &Hybrid{Retrievers: []Retriever{fixed("a", "b", "c"), fixed("c", "b")}}
	hits, err := h.Retrieve(t.Context(), Query{Limit: 2})
	// c: 1/63 + 1/61 narrowly beats b: 2/62.
	if err != nil || !slices.Equal(hitIDs(hits), []string{"c", "b"}) {
		t.Fatalf("fused = %v, %v", hitIDs(hits), err)
	}
	h.Weights = []float64{3}
	hits, _ = h.Retrieve(t.Context(), Query{})
	if !slices.Equal(hitIDs(hits), []string{"b", "c", "a"}) {
		t.Fatalf("weighted = %v", hitIDs(hits))
	}
	h.Weights = []float64{1, 0}
	hits, _ = h.Retrieve(t.Context(), Query{})
	if !slices.Equal(hitIDs(hits), []string{"a", "b", "c"}) {
		t.Fatalf("zero weight = %v", hitIDs(hits))
	}
	var reported []error
	h = &Hybrid{Retrievers: []Retriever{failing, fixed("x")}, OnError: func(err error) { reported = append(reported, err) }}
	hits, err = h.Retrieve(t.Context(), Query{})
	if err != nil || !slices.Equal(hitIDs(hits), []string{"x"}) || len(reported) != 1 {
		t.Fatalf("partial failure = %v, %v, %v", hitIDs(hits), err, reported)
	}
	if _, err := (&Hybrid{Retrievers: []Retriever{failing, failing}}).Retrieve(t.Context(), Query{}); err == nil {
		t.Fatal("total failure ignored")
	}
	// Without OnError the failure is logged and the query still succeeds.
	if hits, err := (&Hybrid{Retrievers: []Retriever{failing, fixed("x")}}).Retrieve(t.Context(), Query{}); err != nil || len(hits) != 1 {
		t.Fatalf("logged failure = %v, %v", hits, err)
	}
}

func TestLLMReranker(t *testing.T) {
	hits := []Hit{
		{Chunk: &Chunk{ID: "a", Source: "s1", Content: "alpha"}, Score: 3},
		{Chunk: &Chunk{ID: "b", Source: "s2", Content: "beta", Context: "ctx"}, Score: 2},
		{Chunk: &Chunk{ID: "c", Source: "s3", Content: strings.Repeat("g", 50)}, Score: 1},
	}
	m := modeltest.New(modeltest.Reply("Thinking about [1]... Final: [3, 1, 3, 9]"))
	r := &LLMReranker{Model: m, MaxPassageBytes: 10}
	got, err := r.Rerank(t.Context(), "which?", hits)
	if err != nil || !slices.Equal(hitIDs(got), []string{"c", "a"}) || got[0].Score != 1 || got[1].Score != 0.5 {
		t.Fatalf("Rerank = %+v, %v", got, err)
	}
	if hits[2].Score != 1 {
		t.Fatal("input hits modified")
	}
	req := m.Requests()[0]
	user := req.Messages[0].Text()
	if req.System != DefaultRerankPrompt || !strings.Contains(user, "<query>\nwhich?\n</query>") ||
		!strings.Contains(user, "[2] (source: s2)\nctx\n\nbeta") || strings.Contains(user, strings.Repeat("g", 11)) {
		t.Fatalf("request = %s\n%s", req.System, user)
	}
	for _, step := range []modeltest.Step{modeltest.Reply("none of them"), modeltest.Fail(errors.New("down"))} {
		if _, err := (&LLMReranker{Model: modeltest.New(step)}).Rerank(t.Context(), "q", hits); err == nil {
			t.Error("bad reranker reply accepted")
		}
	}
	if got, err := (&LLMReranker{Model: modeltest.New(modeltest.Reply("[]"))}).Rerank(t.Context(), "q", hits); err != nil || len(got) != 0 {
		t.Fatalf("empty ranking = %v, %v", got, err)
	}
	if _, err := (&LLMReranker{}).Rerank(t.Context(), "q", hits); err == nil {
		t.Fatal("missing model accepted")
	}
}

func TestReranked(t *testing.T) {
	var asked int
	first := RetrieverFunc(func(_ context.Context, q Query) ([]Hit, error) {
		asked = q.Limit
		return []Hit{{Chunk: &Chunk{ID: "a"}}, {Chunk: &Chunk{ID: "b"}}, {Chunk: &Chunk{ID: "c"}}}, nil
	})
	reverse := RerankerFunc(func(_ context.Context, _ string, hits []Hit) ([]Hit, error) {
		out := slices.Clone(hits)
		slices.Reverse(out)
		return out, nil
	})
	r := &Reranked{Retriever: first, Reranker: reverse}
	hits, err := r.Retrieve(t.Context(), Query{Text: "q", Limit: 2})
	if err != nil || !slices.Equal(hitIDs(hits), []string{"c", "b"}) || asked != 20 {
		t.Fatalf("Reranked = %v, %v (asked %d)", hitIDs(hits), err, asked)
	}
	r.Candidates = 7
	if _, _ = r.Retrieve(t.Context(), Query{Limit: 10}); asked != 7 {
		t.Fatalf("candidates = %d", asked)
	}
	var reported error
	r = &Reranked{Retriever: first, Reranker: &LLMReranker{Model: modeltest.New(modeltest.Reply("?"))}, OnError: func(err error) { reported = err }}
	hits, err = r.Retrieve(t.Context(), Query{Limit: 2})
	if err != nil || !slices.Equal(hitIDs(hits), []string{"a", "b"}) || reported == nil {
		t.Fatalf("fallback = %v, %v, %v", hitIDs(hits), err, reported)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	slow := RerankerFunc(func(ctx context.Context, _ string, _ []Hit) ([]Hit, error) { return nil, ctx.Err() })
	if _, err := (&Reranked{Retriever: first, Reranker: slow}).Retrieve(ctx, Query{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled = %v", err)
	}
	empty := RetrieverFunc(func(context.Context, Query) ([]Hit, error) { return nil, nil })
	called := false
	never := RerankerFunc(func(context.Context, string, []Hit) ([]Hit, error) { called = true; return nil, nil })
	if hits, err := (&Reranked{Retriever: empty, Reranker: never}).Retrieve(t.Context(), Query{}); err != nil || hits != nil || called {
		t.Fatal("reranker called without candidates")
	}
}

var _ model.Model = (*modeltest.Model)(nil)
