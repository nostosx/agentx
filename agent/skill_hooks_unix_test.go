//go:build !windows

package agent_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/session"
	"github.com/nostosx/agentx/skill"
)

func TestSkillHooks(t *testing.T) {
	t.Parallel()
	counter := filepath.Join(t.TempDir(), "count")
	doc := `---
name: guard
description: Guards dangerous tools
hooks:
  PreToolUse:
    - matcher: danger
      hooks:
        - type: command
          command: "echo blocked by guard >&2; exit 2"
---
Guarding.`
	meta, body, err := skill.Parse(doc)
	if err != nil {
		t.Fatal(err)
	}
	guard := defineSkill(t, meta, body)
	count := defineSkill(t, skill.Metadata{Name: "count", Hooks: map[string]any{
		"PostToolUse": []any{map[string]any{"matcher": "danger", "hooks": []any{
			map[string]any{"type": "command", "command": "echo x >> " + counter, "once": true},
		}}},
		"Stop": []any{map[string]any{"hooks": []any{map[string]any{"type": "bogus"}}}},
	}}, "Counting.")

	var calls atomic.Int32
	approve := agent.WithApprover(loop.ApproverFunc(func(context.Context, *loop.ApprovalRequest) (loop.Approval, error) {
		return loop.Approve, nil
	}))
	var userHook atomic.Int32
	userHooks := hook.NewManager()
	_ = userHooks.OnFunc(hook.PreToolUse, "danger", func(context.Context, *hook.Event) (*hook.Result, error) {
		userHook.Add(1)
		return nil, nil
	})
	newAgent := func(m *modeltest.Model) *agent.LLMAgent {
		return agent.MustNew("assistant", agent.WithModel(m), agent.WithTools(dangerTool(&calls)), approve,
			agent.WithHooks(userHooks), agent.WithSkills(guard, count))
	}
	store := session.NewMemoryStore()
	m := modeltest.New(
		modeltest.CallTools(modeltest.Call{ID: "c1", Name: "danger"}),
		modeltest.CallTools(invoke("c2", "guard", "")),
		modeltest.CallTools(modeltest.Call{ID: "c3", Name: "danger"}),
		modeltest.Reply("first"),
		// The hook lasts for the session.
		modeltest.CallTools(modeltest.Call{ID: "c4", Name: "danger"}),
		modeltest.Reply("second"),
		// Not for other sessions.
		modeltest.CallTools(modeltest.Call{ID: "c5", Name: "danger"}),
		modeltest.Reply("other"),
	)
	a := newAgent(m)
	runner := agent.NewRunner(a, store)
	res, err := runner.RunText(t.Context(), "s1", "go")
	if err != nil || calls.Load() != 1 {
		t.Fatalf("run = %+v, %v; calls = %d", res, err, calls.Load())
	}
	results := toolResults(res.Messages)
	if r := results["c3"]; !r.IsError || !strings.Contains(r.Content, "blocked by guard") {
		t.Errorf("guarded call = %+v", r)
	}
	if res, err = runner.RunText(t.Context(), "s1", "again"); err != nil || !toolResults(res.Messages)["c4"].IsError || calls.Load() != 1 {
		t.Errorf("later run = %+v, %v; calls = %d", res, err, calls.Load())
	}
	if res, err = runner.RunText(t.Context(), "s2", "other"); err != nil || toolResults(res.Messages)["c5"].IsError || calls.Load() != 2 {
		t.Errorf("other session = %+v, %v; calls = %d", res, err, calls.Load())
	}
	if userHook.Load() != 4 {
		t.Errorf("user hook ran %d times", userHook.Load())
	}
	// The hooks the user passed are not modified.
	if userHooks.Has(hook.SessionEnd) {
		t.Error("skill dispatchers were added to the caller's manager")
	}

	// After a restart, the hooks of the skills invoked in the session are
	// registered again from the conversation.
	m2 := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c6", Name: "danger"}), modeltest.Reply("restarted"))
	res, err = agent.NewRunner(newAgent(m2), store).RunText(t.Context(), "s1", "restart")
	if err != nil || !toolResults(res.Messages)["c6"].IsError || calls.Load() != 2 {
		t.Errorf("restarted run = %+v, %v; calls = %d", res, err, calls.Load())
	}

	// A once hook runs once; ending the session forgets the hooks.
	m3 := modeltest.New(
		modeltest.CallTools(invoke("c7", "count", "")),
		modeltest.CallTools(modeltest.Call{ID: "c8", Name: "danger"}),
		modeltest.CallTools(modeltest.Call{ID: "c9", Name: "danger"}),
		modeltest.Reply("counted"),
	)
	runner3 := agent.NewRunner(newAgent(m3), nil)
	if _, err := runner3.RunText(t.Context(), "s3", "count"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(counter)
	if err != nil || string(data) != "x\n" {
		t.Errorf("once hook output = %q, %v", data, err)
	}
	if err := runner3.EndSession(t.Context(), "s3", "clear"); err != nil {
		t.Fatal(err)
	}
}

func TestSkillHooksWithoutSession(t *testing.T) {
	t.Parallel()
	guard := defineSkill(t, skill.Metadata{Name: "guard", Hooks: map[string]any{
		"PreToolUse": []any{map[string]any{"matcher": "danger", "hooks": []any{
			map[string]any{"type": "command", "command": "exit 2"},
		}}},
	}}, "Guarding.")
	var calls atomic.Int32
	m := modeltest.New(
		modeltest.CallTools(invoke("c1", "guard", "")),
		modeltest.CallTools(modeltest.Call{ID: "c2", Name: "danger"}),
		modeltest.Reply("first"),
		modeltest.CallTools(modeltest.Call{ID: "c3", Name: "danger"}),
	)
	a := agent.MustNew("assistant", agent.WithModel(m), agent.WithTools(dangerTool(&calls)), agent.WithSkills(guard))
	res, err := loop.Collect(a.Run(t.Context(), agent.Text("go")), nil)
	if err != nil || !toolResults(res.Messages)["c2"].IsError {
		t.Fatalf("run = %+v, %v", res, err)
	}
	// Without a session the hooks last for the run: the next run needs
	// approval instead of being blocked.
	res, err = loop.Collect(a.Run(t.Context(), agent.Text("again")), nil)
	if err != nil || res.Status != loop.StatusInterrupted {
		t.Fatalf("next run = %+v, %v", res, err)
	}
}
