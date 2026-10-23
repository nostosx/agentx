package agent

import (
	"context"

	"github.com/nostosx/agentx/session"
)

// ActiveAgentKey is the session state key holding the name of the agent
// a Runner's session was last handed off to, which handles the session's
// next message; see WithHandoffs. It is absent while the Runner's own
// agent is active. Delete it to return the session to that agent.
const ActiveAgentKey = "agentx.active_agent"

type stateKey struct{}

// ContextWithSessionState returns a context in which SessionState returns
// st. A Runner does this for the runs of its sessions; use it to give
// runs started without a Runner a state, for example for
// WithInstructionTemplate.
func ContextWithSessionState(ctx context.Context, st *session.State) context.Context {
	return context.WithValue(ctx, stateKey{}, st)
}

// SessionState returns the session state carried by ctx and reports false
// if none is present. Tools, hooks, middleware and instruction functions
// can use it to share per-session values.
//
// A Runner supplies its session's state and saves it when the run ends,
// including failures and interruptions. Sub-agents and handoff targets share
// the same state. Its operations are safe for concurrent use, with the
// value-sharing rules of session.State. Store JSON values for persistence
// and leave keys starting with "agentx." to the Runner.
func SessionState(ctx context.Context) (*session.State, bool) {
	st, ok := ctx.Value(stateKey{}).(*session.State)
	return st, ok && st != nil
}
