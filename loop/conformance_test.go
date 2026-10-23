package loop_test

import (
	"testing"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/loop/checkpointtest"
)

func TestMemoryCheckpointerConformance(t *testing.T) {
	t.Parallel()
	checkpointtest.Run(t, func(*testing.T) loop.Checkpointer { return loop.NewMemoryCheckpointer() })
}
