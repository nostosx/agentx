package agent_test

import (
	"context"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/nostosx/agentx/actx"
	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/skill"
	"github.com/nostosx/agentx/tool"
)

func TestOptionsReachModelAndToolExecution(t *testing.T) {
	t.Parallel()
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "call", Name: "echo"}), modeltest.Reply("done"))
	var order []string
	hooks := hook.NewManager()
	if err := hooks.OnFunc(hook.PreModel, "", func(context.Context, *hook.Event) (*hook.Result, error) {
		order = append(order, "first hook")
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	moreHooks := hook.NewManager()
	if err := moreHooks.OnFunc(hook.PreModel, "", func(context.Context, *hook.Event) (*hook.Result, error) {
		order = append(order, "merged hook")
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	s, err := skill.New("testing", "Test Go code", "instructions")
	if err != nil {
		t.Fatal(err)
	}
	reasoning := &model.Reasoning{Effort: "high"}
	choice := &model.ToolChoice{Mode: model.ToolChoiceAuto}
	format := &model.ResponseFormat{Name: "answer", Schema: []byte(`{"type":"object"}`)}
	cp := loop.NewMemoryCheckpointer()
	logger := slog.New(slog.DiscardHandler)
	a := agent.MustNew("assistant",
		agent.WithModel(m), agent.WithInstructions("base instructions"),
		agent.WithHooks(hooks),
		agent.WithHook(hook.PreModel, "", hook.Func(func(context.Context, *hook.Event) (*hook.Result, error) {
			order = append(order, "inline hook")
			return nil, nil
		})),
		agent.WithHooks(moreHooks), agent.WithSkillRegistry(skill.NewRegistry(s)),
		agent.WithContext(actx.Config{Providers: []actx.Provider{actx.Static("context", "configured context", actx.PlaceSystem)}}),
		agent.WithTools(tool.Func("echo", "echo", func(context.Context, struct{}) (string, error) {
			order = append(order, "tool")
			return "raw output", nil
		}, tool.RequireApproval())),
		agent.WithToolMiddleware(func(next tool.Invoker) tool.Invoker {
			return func(ctx context.Context, call *tool.Call) (*tool.Result, error) {
				order = append(order, "middleware")
				res, err := next(ctx, call)
				if err == nil {
					res.Content = "wrapped " + res.Content
				}
				return res, err
			}
		}),
		agent.WithApprover(loop.ApproverFunc(func(context.Context, *loop.ApprovalRequest) (loop.Approval, error) {
			order = append(order, "approve")
			return loop.Approve, nil
		})),
		agent.WithCheckpointer(cp), agent.WithMaxTurns(3), agent.WithMaxTokens(42), agent.WithTemperature(0.25), agent.WithTopP(0.8), agent.WithReasoning(reasoning), agent.WithToolChoice(choice), agent.WithResponseFormat(format),
		agent.WithProviderOptions("openai", map[string]any{"provider_option": true}), agent.WithProviderOptions("openai", map[string]any{"other_option": "value"}), agent.WithSeed(3), agent.WithStreaming(), agent.WithParallelism(2), agent.WithLogger(logger),
	)
	var deltas int
	res, err := loop.Collect(a.Run(t.Context(), agent.Text("question")), func(ev *loop.Event) {
		if ev.Type == loop.EventDelta {
			deltas++
		}
	})
	if err != nil || res.Output != "done" || deltas == 0 {
		t.Fatalf("Run = %+v, %v; deltas = %d", res, err, deltas)
	}
	req := m.Requests()[0]
	if req.MaxTokens != 42 || req.Temperature == nil || *req.Temperature != 0.25 || req.TopP == nil || *req.TopP != 0.8 || !reflect.DeepEqual(req.Reasoning, reasoning) || !reflect.DeepEqual(req.ToolChoice, choice) || !reflect.DeepEqual(req.ResponseFormat, format) || req.ProviderOptions["openai"]["provider_option"] != true || req.ProviderOptions["openai"]["other_option"] != "value" || req.Seed == nil || *req.Seed != 3 {
		t.Fatalf("model options lost: %+v", req)
	}
	for _, text := range []string{"base instructions", "configured context", "testing"} {
		if !strings.Contains(req.System, text) {
			t.Errorf("system prompt %q missing %q", req.System, text)
		}
	}
	wantOrder := []string{"first hook", "inline hook", "merged hook", "approve", "middleware", "tool", "first hook", "inline hook", "merged hook"}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Fatalf("execution order = %v, want %v", order, wantOrder)
	}
	if got := m.Requests()[1].Messages[2].ToolResults()[0].Content; got != "wrapped raw output" {
		t.Fatalf("tool middleware output = %q", got)
	}
	if cfg := a.Runtime().Config(); cfg.MaxTurns != 3 || cfg.Parallelism != 2 || cfg.Logger != logger || cfg.Checkpointer != cp {
		t.Fatalf("runtime options = %+v", cfg)
	}
}

func TestWithHookInvalidMatcherPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("WithHook accepted invalid regexp")
		}
	}()
	agent.MustNew("assistant", agent.WithModel(modeltest.New()), agent.WithHook(hook.Stop, "[", hook.Func(func(context.Context, *hook.Event) (*hook.Result, error) { return nil, nil })))
}

func TestOptionsDoNotModifyCallerObjects(t *testing.T) {
	t.Parallel()
	noop := hook.Func(func(context.Context, *hook.Event) (*hook.Result, error) { return nil, nil })
	hooks := hook.NewManager()
	if err := hooks.On(hook.Stop, "", noop); err != nil {
		t.Fatal(err)
	}
	other := hook.NewManager()
	if err := other.On(hook.PreModel, "", noop); err != nil {
		t.Fatal(err)
	}
	s1, err := skill.New("first", "First skill", "one")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := skill.New("second", "Second skill", "two")
	if err != nil {
		t.Fatal(err)
	}
	reg := skill.NewRegistry(s1)
	agent.MustNew("a", agent.WithModel(modeltest.New()),
		agent.WithHooks(hooks), agent.WithHook(hook.PreToolUse, "", noop), agent.WithHooks(other),
		agent.WithSkillRegistry(reg), agent.WithSkills(s2))
	if hooks.Has(hook.PreToolUse) || hooks.Has(hook.PreModel) || other.Has(hook.Stop) {
		t.Fatal("hook options registered hooks in a caller's manager")
	}
	if reg.Len() != 1 {
		t.Fatalf("skill options registered %d skills in the caller's registry", reg.Len())
	}
}
