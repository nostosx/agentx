package agent

import (
	"sync"
	"testing"

	"github.com/nostosx/agentx/model/modeltest"
)

func TestRunnerReleasesSessionLocks(t *testing.T) {
	t.Parallel()
	steps := make([]modeltest.Step, 40)
	for i := range steps {
		steps[i] = modeltest.Reply("ok")
	}
	runner := NewRunner(MustNew("a", WithModel(modeltest.New(steps...))), nil)
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Go(func() {
			// Half the runs share a session, so some wait for its lock.
			id := "shared"
			if i%2 == 0 {
				id = ""
			}
			if _, err := runner.RunText(t.Context(), id, "hi"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.locks) != 0 {
		t.Fatalf("%d session locks remain", len(runner.locks))
	}
}
