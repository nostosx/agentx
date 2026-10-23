package session_test

import (
	"testing"

	"github.com/nostosx/agentx/session"
	"github.com/nostosx/agentx/session/sessiontest"
)

func TestMemoryStoreConformance(t *testing.T) {
	t.Parallel()
	sessiontest.Run(t, func(*testing.T) session.Store { return session.NewMemoryStore() })
}
