package rag

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"slices"
	"sync"
	"testing"

	"github.com/nostosx/agentx/memory"
)

// bowEmbedder embeds text as a hashed bag of words and records its
// batches.
type bowEmbedder struct {
	mu      sync.Mutex
	batches [][]string
	err     error
}

func (e *bowEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return nil, e.err
	}
	e.batches = append(e.batches, slices.Clone(texts))
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

func (e *bowEmbedder) embedded() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Concat(e.batches...)
}

func embed(t *testing.T, text string) []float32 {
	t.Helper()
	v, err := (&bowEmbedder{}).Embed(t.Context(), []string{text})
	if err != nil {
		t.Fatal(err)
	}
	return v[0]
}

func testChunk(t *testing.T, source, id, content string, meta map[string]any) *Chunk {
	return &Chunk{ID: id, DocumentID: source, Source: source, Content: content, Metadata: meta, Embedding: embed(t, content)}
}

func TestFilterMatch(t *testing.T) {
	c := &Chunk{Source: "docs/a.md", Metadata: map[string]any{"n": 2, "tags": []string{"x", "y"}, "ok": true}}
	var decoded map[string]any
	b, _ := json.Marshal(c.Metadata)
	_ = json.Unmarshal(b, &decoded)
	d := &Chunk{Source: "docs/a.md", Metadata: decoded}
	for _, tc := range []struct {
		f    Filter
		want bool
	}{
		{Filter{}, true},
		{Filter{Sources: []string{"docs/a.md", "x"}}, true},
		{Filter{Sources: []string{"x"}}, false},
		{Filter{SourcePrefix: "docs/"}, true},
		{Filter{SourcePrefix: "src/"}, false},
		{Filter{Metadata: map[string]any{"n": int64(2), "ok": true}}, true},
		{Filter{Metadata: map[string]any{"n": 3}}, false},
		{Filter{Metadata: map[string]any{"tags": []string{"x", "y"}}}, true},
		{Filter{Metadata: map[string]any{"tags": []any{"x"}}}, false},
		{Filter{Metadata: map[string]any{"n": "2"}}, false},
	} {
		if got := tc.f.Match(c); got != tc.want {
			t.Errorf("%+v.Match(original) = %v", tc.f, got)
		}
		if got := tc.f.Match(d); got != tc.want {
			t.Errorf("%+v.Match(decoded) = %v", tc.f, got)
		}
	}
}
