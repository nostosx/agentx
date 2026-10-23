package memory_test

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/nostosx/agentx/memory"
)

type embedFunc func(context.Context, []string) ([][]float32, error)

func (f embedFunc) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return f(ctx, texts)
}

type retrieveFunc func(context.Context, memory.Query) ([]memory.Hit, error)

func (f retrieveFunc) Retrieve(ctx context.Context, q memory.Query) ([]memory.Hit, error) {
	return f(ctx, q)
}

type failingStore struct {
	memory.Store
	err error
}

func (s failingStore) List(context.Context, memory.Filter) ([]*memory.Entry, error) {
	return nil, s.err
}

func putEntries(t *testing.T, entries ...*memory.Entry) *memory.MemoryStore {
	t.Helper()
	s := memory.NewMemoryStore()
	for _, e := range entries {
		if err := s.Put(t.Context(), e); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestKeywordRetrieve(t *testing.T) {
	t.Parallel()
	s := putEntries(t,
		&memory.Entry{ID: "exact", Content: "golang", Scope: "one", Tags: []string{"agent"}},
		&memory.Entry{ID: "long", Content: "golang and many unrelated words about something else", Scope: "one"},
		&memory.Entry{ID: "other", Content: "golang", Scope: "two"},
		&memory.Entry{ID: "chinese", Content: "上下文工程"},
	)
	k := &memory.Keyword{Store: s}
	hits, err := k.Retrieve(t.Context(), memory.Query{Text: "GOLANG", Filter: memory.Filter{Scope: "one"}})
	if err != nil || len(hits) != 2 || hits[0].Entry.ID != "exact" || hits[0].Score <= hits[1].Score {
		t.Fatalf("keyword ranking = %+v, %v", hits, err)
	}
	duplicate, err := k.Retrieve(t.Context(), memory.Query{Text: "golang golang", Filter: memory.Filter{Scope: "one"}})
	if err != nil || duplicate[0].Score != hits[0].Score {
		t.Error("duplicate query terms changed scores")
	}
	for _, tt := range []struct {
		name  string
		query memory.Query
		want  string
	}{
		{"tag lookup", memory.Query{Text: "agent"}, "exact"},
		{"cjk lookup", memory.Query{Text: "上下文"}, "chinese"},
		{"top limit", memory.Query{Text: "golang", Limit: 1, Filter: memory.Filter{Scope: "one"}}, "exact"},
		{"score cutoff", memory.Query{Text: "golang", MinScore: math.MaxFloat64}, ""},
		{"empty terms", memory.Query{Text: "?! "}, ""},
		{"missing terms", memory.Query{Text: "missing"}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := k.Retrieve(t.Context(), tt.query)
			if err != nil {
				t.Fatal(err)
			}
			if tt.want == "" {
				if len(got) != 0 {
					t.Errorf("got %v, want no hits", got)
				}
				return
			}
			if len(got) != 1 || got[0].Entry.ID != tt.want {
				t.Errorf("got %+v, want %q", got, tt.want)
			}
		})
	}
	wantErr := errors.New("list failed")
	if _, err := (&memory.Keyword{Store: failingStore{err: wantErr}}).Retrieve(t.Context(), memory.Query{Text: "go"}); !errors.Is(err, wantErr) {
		t.Errorf("store error = %v", err)
	}
}

func TestVectorRetrieve(t *testing.T) {
	t.Parallel()
	s := putEntries(t,
		&memory.Entry{ID: "exact", Embedding: []float32{1, 0}, Scope: "one"},
		&memory.Entry{ID: "partial", Embedding: []float32{1, 1}, Scope: "one"},
		&memory.Entry{ID: "negative", Embedding: []float32{-1, 0}},
		&memory.Entry{ID: "missing"},
		&memory.Entry{ID: "wrong-dimension", Embedding: []float32{1}},
		&memory.Entry{ID: "zero", Embedding: []float32{0, 0}},
	)
	e := embedFunc(func(_ context.Context, texts []string) ([][]float32, error) {
		if !reflect.DeepEqual(texts, []string{"query"}) {
			t.Errorf("Embed input = %v", texts)
		}
		return [][]float32{{1, 0}}, nil
	})
	v := &memory.Vector{Store: s, Embedder: e}
	hits, err := v.Retrieve(t.Context(), memory.Query{Text: "query"})
	if err != nil || len(hits) != 2 || hits[0].Entry.ID != "exact" || hits[0].Score != 1 || hits[1].Entry.ID != "partial" {
		t.Fatalf("vector ranking = %+v, %v", hits, err)
	}
	hits, err = v.Retrieve(t.Context(), memory.Query{Text: "query", Limit: 1, MinScore: 0.9, Filter: memory.Filter{Scope: "one"}})
	if err != nil || len(hits) != 1 || hits[0].Entry.ID != "exact" {
		t.Errorf("filtered vector hits = %+v, %v", hits, err)
	}
	if hits, err := v.Retrieve(t.Context(), memory.Query{Text: " \n"}); err != nil || len(hits) != 0 {
		t.Errorf("blank query = %v, %v", hits, err)
	}
	wantErr := errors.New("embed failed")
	for _, tt := range []struct {
		name      string
		retriever memory.Vector
		want      error
	}{
		{"missing embedder", memory.Vector{Store: s}, nil},
		{"embed error", memory.Vector{Store: s, Embedder: embedFunc(func(context.Context, []string) ([][]float32, error) { return nil, wantErr })}, wantErr},
		{"wrong vector count", memory.Vector{Store: s, Embedder: embedFunc(func(context.Context, []string) ([][]float32, error) { return nil, nil })}, nil},
		{"store failure", memory.Vector{Store: failingStore{err: wantErr}, Embedder: e}, wantErr},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.retriever.Retrieve(t.Context(), memory.Query{Text: "query"})
			if err == nil || (tt.want != nil && !errors.Is(err, tt.want)) {
				t.Errorf("error = %v, want failure wrapping %v", err, tt.want)
			}
		})
	}
}

func TestHybridRetrieve(t *testing.T) {
	t.Parallel()
	a, b, c := &memory.Entry{ID: "a"}, &memory.Entry{ID: "b"}, &memory.Entry{ID: "c"}
	var queries []memory.Query
	source := func(hits []memory.Hit) memory.Retriever {
		return retrieveFunc(func(_ context.Context, q memory.Query) ([]memory.Hit, error) {
			queries = append(queries, q)
			return hits, nil
		})
	}
	h := &memory.Hybrid{Retrievers: []memory.Retriever{source([]memory.Hit{{Entry: a}, {Entry: b}}), source([]memory.Hit{{Entry: b}, {Entry: c}})}}
	hits, err := h.Retrieve(t.Context(), memory.Query{Text: "query", Limit: 2, MinScore: 0.5, Filter: memory.Filter{Scope: "project"}})
	if err != nil || len(hits) != 2 || hits[0].Entry.ID != "b" || hits[1].Entry.ID != "a" {
		t.Fatalf("RRF hits = %+v, %v", hits, err)
	}
	if want := 1.0/61 + 1.0/62; math.Abs(hits[0].Score-want) > 1e-12 {
		t.Errorf("fused score = %f, want %f", hits[0].Score, want)
	}
	for _, q := range queries {
		if q.Limit != 6 || q.MinScore != 0.5 || q.Filter.Scope != "project" {
			t.Errorf("sub-query = %+v", q)
		}
	}
	wantErr := errors.New("retrieval failed")
	failing := retrieveFunc(func(context.Context, memory.Query) ([]memory.Hit, error) { return nil, wantErr })
	var reported []error
	h.OnError = func(err error) { reported = append(reported, err) }
	h.Retrievers = append(h.Retrievers, failing)
	hits, err = h.Retrieve(t.Context(), memory.Query{})
	if err != nil || len(hits) != 3 || len(reported) != 1 || !errors.Is(reported[0], wantErr) {
		t.Errorf("partial failure = %+v, %v, reported %v", hits, err, reported)
	}
	other := errors.New("other failure")
	h.Retrievers = []memory.Retriever{failing, retrieveFunc(func(context.Context, memory.Query) ([]memory.Hit, error) { return nil, other })}
	if _, err := h.Retrieve(t.Context(), memory.Query{}); !errors.Is(err, wantErr) || !errors.Is(err, other) {
		t.Errorf("all retrievers failed = %v", err)
	}
}

func TestKeywordZeroB(t *testing.T) {
	t.Parallel()
	store := memory.NewMemoryStore()
	for _, e := range []*memory.Entry{
		{ID: "short", Content: "go"},
		{ID: "long", Content: "go go " + strings.Repeat("filler ", 50)},
	} {
		if err := store.Put(t.Context(), e); err != nil {
			t.Fatal(err)
		}
	}
	rank := func(k *memory.Keyword) string {
		hits, err := k.Retrieve(t.Context(), memory.Query{Text: "go"})
		if err != nil || len(hits) != 2 {
			t.Fatalf("hits = %+v, %v", hits, err)
		}
		return hits[0].Entry.ID
	}
	// The default B penalizes the long entry; without length normalization
	// its higher term frequency wins.
	if got := rank(&memory.Keyword{Store: store}); got != "short" {
		t.Errorf("default B ranks %s first", got)
	}
	if got := rank(&memory.Keyword{Store: store, B: -1}); got != "long" {
		t.Errorf("B < 0 ranks %s first", got)
	}
}

func TestCosine(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		a, b []float32
		want float64
	}{
		{"identical", []float32{1, 2}, []float32{1, 2}, 1},
		{"opposite", []float32{1, 0}, []float32{-1, 0}, -1},
		{"orthogonal", []float32{1, 0}, []float32{0, 1}, 0},
		{"zero", []float32{0, 0}, []float32{1, 2}, 0},
		{"length mismatch", []float32{1}, []float32{1, 2}, 0},
		{"empty", nil, nil, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := memory.Cosine(tt.a, tt.b); math.Abs(got-tt.want) > 1e-12 {
				t.Errorf("Cosine() = %f, want %f", got, tt.want)
			}
		})
	}
}

func TestTokenize(t *testing.T) {
	t.Parallel()
	got := memory.Tokenize("Go123, 上下文! TEST")
	want := []string{"go123", "上", "上下", "下", "下文", "文", "test"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Tokenize() = %v, want %v", got, want)
	}
}

// TestHybridLogsErrorsByDefault changes slog's default logger, so it does
// not run in parallel.
func TestHybridLogsErrorsByDefault(t *testing.T) {
	var logs strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	ok := retrieveFunc(func(context.Context, memory.Query) ([]memory.Hit, error) {
		return []memory.Hit{{Entry: &memory.Entry{ID: "a"}}}, nil
	})
	broken := retrieveFunc(func(context.Context, memory.Query) ([]memory.Hit, error) { return nil, errors.New("index offline") })
	h := &memory.Hybrid{Retrievers: []memory.Retriever{ok, broken}}
	if hits, err := h.Retrieve(t.Context(), memory.Query{}); err != nil || len(hits) != 1 {
		t.Fatalf("hits = %+v, %v", hits, err)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "index offline") {
		t.Errorf("retriever failure not logged: %q", logs.String())
	}
}
