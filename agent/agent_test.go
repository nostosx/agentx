package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nostosx/agentx/actx"
	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/memory"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/skill"
	"github.com/nostosx/agentx/tool"
)

func TestNew(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		id   string
		opts []agent.Option
	}{
		{name: "invalid name", id: "bad name", opts: []agent.Option{agent.WithModel(modeltest.New())}},
		{name: "missing model", id: "assistant"},
		{name: "duplicate tools", id: "assistant", opts: []agent.Option{agent.WithModel(modeltest.New()), agent.WithTools(tool.Func("duplicate", "", func(context.Context, struct{}) (string, error) { return "", nil }), tool.Func("duplicate", "", func(context.Context, struct{}) (string, error) { return "", nil }))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := agent.New(tc.id, tc.opts...); err == nil {
				t.Fatal("New accepted invalid configuration")
			}
		})
	}
	a := agent.MustNew("assistant", agent.WithModel(modeltest.New(modeltest.Reply("answer"))), agent.WithDescription("Helpful assistant"))
	if a.Name() != "assistant" || a.Description() != "Helpful assistant" || a.Runtime() == nil || a.Tools() == nil {
		t.Fatalf("invalid agent: %+v", a)
	}
	if input := agent.Text("question"); input.Message.Role != model.RoleUser || input.Message.Text() != "question" {
		t.Fatalf("Text = %+v", input)
	}
	if answer, err := a.Ask(t.Context(), "question"); err != nil || answer != "answer" {
		t.Fatalf("Ask = %q, %v", answer, err)
	}
}

func TestMustNewPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("MustNew did not panic for missing model")
		}
	}()
	agent.MustNew("assistant")
}

func TestLLMAgentInvalidInputAndAskFailures(t *testing.T) {
	t.Parallel()
	a := agent.MustNew("assistant", agent.WithModel(modeltest.New()))
	for _, in := range []*agent.Input{nil, {}} {
		if _, err := loop.Collect(a.Run(t.Context(), in), nil); err == nil {
			t.Fatal("Run accepted empty input")
		}
	}
	wantErr := errors.New("provider failure")
	a = agent.MustNew("assistant", agent.WithModel(modeltest.New(modeltest.Fail(wantErr))))
	if _, err := a.Ask(t.Context(), "question"); !errors.Is(err, wantErr) {
		t.Fatalf("Ask provider failure = %v", err)
	}
	a = agent.MustNew("assistant", agent.WithModel(modeltest.New(modeltest.Respond(&model.Response{Message: model.AssistantText("partial"), FinishReason: model.FinishPause}))), agent.WithMaxTurns(1))
	if out, err := a.Ask(t.Context(), "question"); out != "partial" || err == nil || !strings.Contains(err.Error(), "max_turns") {
		t.Fatalf("Ask incomplete = %q, %v", out, err)
	}
}

func TestAgentSkillsMemoryAndSubagents(t *testing.T) {
	t.Parallel()
	s, err := skill.New("testing", "Test Go programs", "Use meaningful assertions")
	if err != nil {
		t.Fatal(err)
	}
	mem := memory.New(memory.NewMemoryStore())
	if _, err := mem.Remember(t.Context(), "Go programming preference", memory.WithScope("user")); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Remember(t.Context(), "Go unrelated secret", memory.WithScope("other")); err != nil {
		t.Fatal(err)
	}
	m := modeltest.New(modeltest.Reply("answer"))
	sub := agent.MustNew("researcher", agent.WithModel(modeltest.New()))
	a := agent.MustNew("assistant", agent.WithModel(m), agent.WithSkills(s), agent.WithMemory(mem, "user", 2), agent.WithSubAgents(sub), agent.WithContextProvider(actx.Static("extra", "extra instructions", actx.PlaceSystem)))
	if _, err := a.Ask(t.Context(), "Go"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"skill", "memory_remember", "memory_recall", "memory_forget", "researcher"} {
		if _, ok := a.Tools().Get(name); !ok {
			t.Errorf("agent missing tool %q", name)
		}
	}
	req := m.Requests()[0]
	if !strings.Contains(req.System, "testing") || !strings.Contains(req.System, "extra instructions") || strings.Contains(req.System, "Use meaningful assertions") {
		t.Fatalf("skill system prompt = %q", req.System)
	}
	if text := req.Messages[0].Text(); !strings.Contains(text, "Go programming preference") || strings.Contains(text, "unrelated secret") {
		t.Fatalf("memory reminder = %q", text)
	}
	without := agent.MustNew("assistant", agent.WithModel(modeltest.New()), agent.WithMemory(mem, "user", 0), agent.WithoutMemoryTools())
	if _, ok := without.Tools().Get("memory_recall"); ok {
		t.Fatal("WithoutMemoryTools retained memory tool")
	}
}

func TestMemoryProvider(t *testing.T) {
	t.Parallel()
	m := memory.New(memory.NewMemoryStore())
	p := agent.MemoryProvider(m, "user", 2)
	for _, query := range []string{"  ", "missing"} {
		if sections, err := p.Sections(t.Context(), &actx.Input{Query: query}); err != nil || len(sections) != 0 {
			t.Fatalf("Sections(%q) = %+v, %v", query, sections, err)
		}
	}
}

func TestAsTool(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		step    modeltest.Step
		max     int
		want    string
		isError bool
	}{
		{name: "completed", step: modeltest.Reply("delegated answer"), want: "delegated answer"},
		{name: "partial", step: modeltest.Respond(&model.Response{Message: model.AssistantText("partial answer"), FinishReason: model.FinishPause}), max: 1, want: "Partial answer:\npartial answer", isError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := modeltest.New(tc.step)
			a := agent.MustNew("researcher", agent.WithModel(m), agent.WithMaxTurns(tc.max))
			hooks := hook.NewManager()
			stops := 0
			if err := hooks.OnFunc(hook.SubagentStop, "researcher", func(_ context.Context, ev *hook.Event) (*hook.Result, error) {
				stops++
				if ev.Agent != "researcher" || ev.RunID == "" || ev.Message == "" {
					t.Errorf("subagent stop = %+v", ev)
				}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			wrapped := agent.AsTool(a, agent.ToolOptions{Hooks: hooks})
			if wrapped.Info().Name != "researcher" || !strings.Contains(wrapped.Info().Description, "no knowledge of this conversation") {
				t.Fatalf("tool info = %+v", wrapped.Info())
			}
			res, err := wrapped.Call(t.Context(), json.RawMessage(`{"prompt":"self-contained task"}`))
			if err != nil || res.IsError != tc.isError || !strings.Contains(res.Content, tc.want) || stops != 1 {
				t.Fatalf("Call = %+v, %v; stops = %d", res, err, stops)
			}
			if req := m.Requests()[0]; len(req.Messages) != 1 || req.Messages[0].Text() != "self-contained task" {
				t.Fatalf("delegated request = %+v", req)
			}
			res, err = wrapped.Call(t.Context(), json.RawMessage(`{"prompt":" "}`))
			if err != nil || !res.IsError || len(m.Requests()) != 1 || stops != 1 {
				t.Fatalf("empty prompt = %+v, %v; requests = %d, stops = %d", res, err, len(m.Requests()), stops)
			}
		})
	}
}

func TestAsToolInterruptedAndFailed(t *testing.T) {
	t.Parallel()
	write := tool.Func("write", "write", func(context.Context, struct{}) (string, error) { return "ok", nil }, tool.RequireApproval())
	a := agent.MustNew("researcher", agent.WithModel(modeltest.New(modeltest.CallTools(modeltest.Call{ID: "call", Name: "write"}))), agent.WithTools(write))
	res, err := agent.AsTool(a, agent.ToolOptions{}).Call(t.Context(), json.RawMessage(`{"prompt":"task"}`))
	if err != nil || !res.IsError || !strings.Contains(res.Content, "needs approval") {
		t.Fatalf("interrupted subagent = %+v, %v", res, err)
	}
	wantErr := errors.New("provider failed")
	a = agent.MustNew("researcher", agent.WithModel(modeltest.New(modeltest.Fail(wantErr))))
	if _, err := agent.AsTool(a, agent.ToolOptions{}).Call(t.Context(), json.RawMessage(`{"prompt":"task"}`)); !errors.Is(err, wantErr) {
		t.Fatalf("failed subagent = %v", err)
	}
}
