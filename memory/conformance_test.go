package memory_test

import (
	"testing"

	"github.com/nostosx/agentx/memory"
	"github.com/nostosx/agentx/memory/memorytest"
)

func TestMemoryStoreConformance(t *testing.T) {
	t.Parallel()
	memorytest.Run(t, func(*testing.T) memory.Store { return memory.NewMemoryStore() })
}
