package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/model/modeltest"
)

func TestShutdown(t *testing.T) {
	t.Parallel()
	s := New(agent.NewRunner(agent.MustNew("a", agent.WithModel(modeltest.New())), nil))
	release := make(chan struct{})
	s.background(func() {
		<-s.base.Done()
		<-release
	})

	// A task still saving its session outlasts a short deadline.
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown with a short deadline = %v, want DeadlineExceeded", err)
	}

	// Tasks requested once Shutdown started run in their request, with a
	// canceled context, instead of racing with the wait.
	ran := false
	s.background(func() {
		ran = s.base.Err() != nil
	})
	if !ran {
		t.Error("a task after Shutdown must run in the calling goroutine with the base context canceled")
	}

	close(release)
	if err := s.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if err := s.Shutdown(t.Context()); err != nil {
		t.Fatalf("repeated Shutdown = %v", err)
	}
}
