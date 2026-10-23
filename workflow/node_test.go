package workflow_test

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/workflow"
)

func TestTemplateAndSequence(t *testing.T) {
	t.Parallel()
	st := workflow.NewState(map[string]any{"name": "Ada", "count": 3})
	if got := workflow.Template("Hello {{name}}: {{count}} {{unknown}}")(st); got != "Hello Ada: 3 {{unknown}}" {
		t.Fatalf("Template=%q", got)
	}
	sentinel := errors.New("stop")
	var order []int
	n := workflow.Sequence(workflow.NodeFunc(func(context.Context, *workflow.State) error { order = append(order, 1); return nil }), workflow.NodeFunc(func(context.Context, *workflow.State) error { order = append(order, 2); return sentinel }), workflow.NodeFunc(func(context.Context, *workflow.State) error { t.Error("sequence continued after error"); return nil }))
	if err := n.Run(t.Context(), st); !errors.Is(err, sentinel) || len(order) != 2 {
		t.Fatalf("Sequence=%v, order=%v", err, order)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := n.Run(ctx, st); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Sequence=%v", err)
	}
}

func TestLoopAndNestedWorkflow(t *testing.T) {
	t.Parallel()
	st := workflow.NewState(nil)
	count := 0
	body := workflow.NodeFunc(func(context.Context, *workflow.State) error { count++; return nil })
	n := workflow.Loop("loop", body, func(context.Context, *workflow.State) (bool, error) { return count < 3, nil }, 3)
	if err := n.Run(t.Context(), st); err != nil || count != 3 || st.String("loop.iteration") != "2" {
		t.Fatalf("Loop=%v count=%d state=%v", err, count, st.Snapshot())
	}
	if err := workflow.Loop("", body, func(context.Context, *workflow.State) (bool, error) { return true, nil }, 1).Run(t.Context(), st); !errors.Is(err, workflow.ErrMaxIterations) {
		t.Fatalf("limit=%v", err)
	}
	child := workflow.NewGraph().AddFunc("child", func(_ context.Context, s *workflow.State) error { s.Set("child", "done"); return nil }).AddEdge(workflow.Start, "child").MustCompile()
	parent := workflow.NewGraph().AddNode("nested", child.AsNode()).AddEdge(workflow.Start, "nested").MustCompile()
	out, err := parent.Run(t.Context(), st)
	if err != nil || out != st || st.String("child") != "done" {
		t.Fatalf("nested=%v, %v", out, err)
	}
}

type scriptedAgent struct {
	run func(context.Context, *agent.Input) iter.Seq2[*loop.Event, error]
}

func (scriptedAgent) Name() string        { return "child" }
func (scriptedAgent) Description() string { return "" }
func (a scriptedAgent) Run(ctx context.Context, input *agent.Input) iter.Seq2[*loop.Event, error] {
	return a.run(ctx, input)
}

func TestAgentNode(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, prompt string
		status       loop.Status
		failure      error
		want         string
	}{
		{"completed", "hello", loop.StatusCompleted, nil, ""},
		{"empty prompt", " \n", loop.StatusCompleted, nil, "empty prompt"},
		{"failed stream", "hello", loop.StatusFailed, errors.New("upstream failed"), "upstream failed"},
		{"interrupted", "hello", loop.StatusInterrupted, nil, "needs approval"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			called := false
			a := scriptedAgent{run: func(_ context.Context, input *agent.Input) iter.Seq2[*loop.Event, error] {
				called = true
				if input.Message.Text() != tt.prompt {
					t.Errorf("prompt=%q", input.Message.Text())
				}
				return func(yield func(*loop.Event, error) bool) {
					if tt.failure != nil {
						yield(nil, tt.failure)
						return
					}
					yield(&loop.Event{Type: loop.EventRunEnd, Result: &loop.Result{Status: tt.status, Output: "answer", Reason: "needs approval"}}, nil)
				}
			}}
			st := workflow.NewState(nil)
			err := workflow.AgentNode(a, func(*workflow.State) string { return tt.prompt }, "output").Run(t.Context(), st)
			if tt.want != "" {
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("AgentNode=%v, want %q", err, tt.want)
				}
				if tt.failure != nil && !errors.Is(err, tt.failure) {
					t.Fatalf("lost cause: %v", err)
				}
				if st.String("output") != "" {
					t.Fatal("failed agent stored output")
				}
				if tt.name == "empty prompt" && called {
					t.Fatal("empty prompt ran agent")
				}
			} else if err != nil || st.String("output") != "answer" {
				t.Fatalf("AgentNode=%v state=%v", err, st.Snapshot())
			}
		})
	}
}
