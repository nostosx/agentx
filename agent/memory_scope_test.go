package agent_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/memory"
	"github.com/nostosx/agentx/model/modeltest"
)

type userKey struct{}

// userScope scopes memories to the user an HTTP middleware would store in
// the request context.
func userScope(ctx context.Context) (string, error) {
	if u, ok := ctx.Value(userKey{}).(string); ok && u != "" {
		return "user:" + u, nil
	}
	return "", errors.New("no user")
}

// One agent serving several users keeps their memories apart, and a run
// without a user reads and writes none.
func TestMemoryScopeFunc(t *testing.T) {
	t.Parallel()
	store := memory.NewMemoryStore()
	mem := memory.New(store)
	m := modeltest.New()
	a := agent.MustNew("assistant", agent.WithModel(m), agent.WithMemoryScopeFunc(mem, userScope, 3))
	run := func(user string, steps ...modeltest.Step) *loop.Result {
		t.Helper()
		m.Append(steps...)
		ctx := t.Context()
		if user != "" {
			ctx = context.WithValue(ctx, userKey{}, user)
		}
		res, err := loop.Collect(a.Run(ctx, agent.Text("tea")), nil)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	toolResult := func(res *loop.Result) string {
		for _, msg := range res.Messages {
			for _, r := range msg.ToolResults() {
				return r.Content
			}
		}
		return ""
	}

	run("alice", modeltest.CallTools(modeltest.Call{ID: "r", Name: "memory_remember", Args: map[string]any{"content": "alice drinks tea"}}),
		modeltest.Reply("noted"))
	if entries, _ := store.List(t.Context(), memory.Filter{Scope: "user:alice"}); len(entries) != 1 {
		t.Fatalf("alice's memories = %+v", entries)
	}

	res := run("bob", modeltest.CallTools(modeltest.Call{ID: "q", Name: "memory_recall", Args: map[string]any{"query": "tea"}}),
		modeltest.Reply("nothing"))
	if got := toolResult(res); got != "No relevant memories found." {
		t.Fatalf("bob recalled %q", got)
	}
	// Automatic recall did not show bob alice's memory either.
	for _, req := range m.Requests()[2:] {
		for _, msg := range req.Messages {
			if strings.Contains(msg.Text(), "alice drinks tea") {
				t.Fatalf("bob's run saw alice's memory: %q", msg.Text())
			}
		}
	}
	// Alice's own runs recall it.
	res = run("alice", modeltest.CallTools(modeltest.Call{ID: "a", Name: "memory_recall", Args: map[string]any{"query": "tea"}}),
		modeltest.Reply("found"))
	if got := toolResult(res); !strings.Contains(got, "alice drinks tea") {
		t.Fatalf("alice recalled %q", got)
	}

	res = run("", modeltest.CallTools(modeltest.Call{ID: "n", Name: "memory_recall", Args: map[string]any{"query": "tea"}}),
		modeltest.Reply("failed"))
	if got := toolResult(res); !strings.Contains(got, "no user") {
		t.Fatalf("recall without a user = %q", got)
	}
}
