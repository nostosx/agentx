package rag_test

import (
	"testing"

	"github.com/nostosx/agentx/rag"
	"github.com/nostosx/agentx/rag/ragtest"
)

func TestMemoryStoreConformance(t *testing.T) {
	t.Parallel()
	ragtest.Run(t, func(*testing.T) rag.VectorStore { return rag.NewMemoryStore() })
}

func TestZeroMemoryStoreConformance(t *testing.T) {
	t.Parallel()
	ragtest.Run(t, func(*testing.T) rag.VectorStore { return &rag.MemoryStore{} })
}
