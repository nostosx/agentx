package workflow_test

import (
	"testing"

	"github.com/nostosx/agentx/workflow"
	"github.com/nostosx/agentx/workflow/checkpointtest"
)

func TestMemoryCheckpointerConformance(t *testing.T) {
	t.Parallel()
	checkpointtest.Run(t, func(*testing.T) workflow.Checkpointer { return workflow.NewMemoryCheckpointer() })
}
