package memory_test

import (
	"context"
	"testing"

	"github.com/nostosx/agentx/memory"
)

func TestQueryEmbedder(t *testing.T) {
	t.Parallel()
	store := memory.NewMemoryStore()
	var docs, queries []string
	doc := embedFunc(func(_ context.Context, texts []string) ([][]float32, error) {
		docs = append(docs, texts...)
		return [][]float32{{1, 0}}, nil
	})
	query := embedFunc(func(_ context.Context, texts []string) ([][]float32, error) {
		queries = append(queries, texts...)
		return [][]float32{{1, 0}}, nil
	})
	m := memory.New(store, memory.WithEmbedder(doc), memory.WithQueryEmbedder(query))
	if _, err := m.Remember(t.Context(), "zebra stripes"); err != nil {
		t.Fatal(err)
	}
	hits, err := m.Recall(t.Context(), "unrelated words")
	if err != nil || len(hits) != 1 {
		t.Fatalf("Recall = %v, %v", hits, err)
	}
	if len(docs) != 1 || docs[0] != "zebra stripes" || len(queries) != 1 || queries[0] != "unrelated words" {
		t.Fatalf("documents = %v, queries = %v", docs, queries)
	}
}
