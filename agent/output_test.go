package agent_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/tool"
)

type Report struct {
	Title string   `json:"title" jsonschema:"Short title"`
	Tags  []string `json:"tags,omitempty"`
}

type page[T any] struct {
	Items []T `json:"items"`
}

func TestWithOutputAndAskAs(t *testing.T) {
	t.Parallel()
	m := modeltest.New(
		modeltest.Reply("Here is the report."),
		modeltest.Reply("```json\n{\"title\":\"Q3\",\"tags\":[\"finance\"]}\n```"),
	)
	a := agent.MustNew("reporter", agent.WithModel(m), agent.WithOutput[*Report](agent.OutputRetries(1)))
	got, err := agent.AskAs[*Report](t.Context(), a, "report")
	if err != nil || got == nil || got.Title != "Q3" || len(got.Tags) != 1 {
		t.Fatalf("AskAs = %+v, %v", got, err)
	}
	f := m.Requests()[0].ResponseFormat
	if f == nil || f.Name != "Report" || !f.Strict || !strings.Contains(string(f.Schema), `"title"`) {
		t.Fatalf("response format = %+v", f)
	}
	// Generic type names are not valid schema names.
	m = modeltest.New(modeltest.Reply(`{"items":[1,2]}`))
	b := agent.MustNew("pager", agent.WithModel(m), agent.WithOutput[page[int]](agent.OutputName("page")))
	res, err := loop.Collect(b.Run(t.Context(), agent.Text("q")), nil)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := res.DecodeAs[page[int]](); err != nil || len(p.Items) != 2 || m.Requests()[0].ResponseFormat.Name != "page" {
		t.Fatalf("Output = %+v, %v", p, err)
	}
	c := agent.MustNew("pager", agent.WithModel(modeltest.New()), agent.WithOutput[page[int]]())
	if name := c.Runtime().Config().ResponseFormat.Name; name != "output" {
		t.Fatalf("default name = %q", name)
	}
}

func TestWithOutputFailures(t *testing.T) {
	t.Parallel()
	if _, err := agent.New("bad", agent.WithModel(modeltest.New()), agent.WithOutput[[]string]()); err == nil {
		t.Fatal("New accepted a non-object output")
	}
	a := agent.MustNew("reporter", agent.WithModel(modeltest.New(modeltest.Reply("no"))), agent.WithOutput[Report](agent.OutputRetries(-1)))
	if _, err := agent.AskAs[Report](t.Context(), a, "report"); !errors.As(err, new(*loop.OutputError)) {
		t.Fatalf("AskAs = %v", err)
	}
	if _, err := (&loop.Result{Status: loop.StatusMaxTurns}).DecodeAs[Report](); err == nil {
		t.Fatal("Output decoded an unfinished run")
	}
}

func TestLimitAndStopOptions(t *testing.T) {
	t.Parallel()
	final := tool.Func("final", "final", func(_ context.Context, in struct {
		Answer string `json:"answer"`
	}) (string, error) {
		return in.Answer, nil
	})
	echo := tool.Func("echo", "echo", func(context.Context, struct{}) (string, error) { return "ok", nil })
	call := func(id, name string) modeltest.Step {
		return modeltest.CallTools(modeltest.Call{ID: id, Name: name, Args: map[string]string{"answer": id}})
	}

	a := agent.MustNew("a", agent.WithModel(modeltest.New(call("a", "final"))), agent.WithTools(final), agent.WithStopAtTools("final"))
	if out, err := a.Ask(t.Context(), "q"); err != nil || out != "a" {
		t.Fatalf("stop at tool: %q, %v", out, err)
	}

	var turns []int
	a = agent.MustNew("a", agent.WithModel(modeltest.New(call("a", "echo"))), agent.WithTools(echo),
		agent.WithStopWhen(func(s *loop.Step) bool { turns = append(turns, s.Turn); return true }))
	if res, err := loop.Collect(a.Run(t.Context(), agent.Text("q")), nil); err != nil || res.Status != loop.StatusCompleted || len(turns) != 1 {
		t.Fatalf("stop when: %+v, %v", res, err)
	}

	a = agent.MustNew("a", agent.WithModel(modeltest.New(call("a", "echo"), call("b", "echo"))), agent.WithTools(echo),
		agent.WithLimits(loop.Limits{Requests: 1}))
	if res, err := loop.Collect(a.Run(t.Context(), agent.Text("q")), nil); err != nil || res.Status != loop.StatusLimitExceeded {
		t.Fatalf("limits: %+v, %v", res, err)
	}
	if _, err := a.Ask(t.Context(), "q"); err == nil || !strings.Contains(err.Error(), "limit_exceeded") {
		t.Fatalf("Ask = %v", err)
	}

	// Input overrides the agent's limits and turn limit.
	a = agent.MustNew("a", agent.WithModel(modeltest.New(call("a", "echo"), call("b", "echo"), call("c", "echo"))), agent.WithTools(echo),
		agent.WithLimits(loop.Limits{Requests: 1}))
	res, err := loop.Collect(a.Run(t.Context(), &agent.Input{Message: model.UserText("q"), Limits: &loop.Limits{}, MaxTurns: 2}), nil)
	if err != nil || res.Status != loop.StatusMaxTurns || res.Usage.Requests != 2 {
		t.Fatalf("overrides: %+v, %v", res, err)
	}

	a = agent.MustNew("a", agent.WithModel(modeltest.New(call("a", "echo"))), agent.WithTools(echo),
		agent.WithBudget(func(u model.Usage) error {
			if u.Requests > 0 {
				return errors.New("spent")
			}
			return nil
		}))
	if res, err := loop.Collect(a.Run(t.Context(), agent.Text("q")), nil); err != nil || res.Status != loop.StatusLimitExceeded || res.Reason != "budget: spent" {
		t.Fatalf("budget: %+v, %v", res, err)
	}
}

func TestSubAgentRequestsCountTowardParentLimits(t *testing.T) {
	t.Parallel()
	echo := tool.Func("echo", "echo", func(context.Context, struct{}) (string, error) { return "ok", nil })
	sub := agent.MustNew("helper", agent.WithTools(echo), agent.WithModel(modeltest.New(
		modeltest.CallTools(modeltest.Call{ID: "s", Name: "echo"}),
		modeltest.Reply("help"),
	)))
	parent := agent.MustNew("coordinator", agent.WithModel(modeltest.New(
		modeltest.CallTools(modeltest.Call{ID: "c", Name: "helper", Args: map[string]string{"prompt": "help"}}),
		modeltest.Reply("done"),
	)), agent.WithSubAgents(sub), agent.WithLimits(loop.Limits{Requests: 3}))
	res, err := loop.Collect(parent.Run(t.Context(), agent.Text("q")), nil)
	if err != nil || res.Status != loop.StatusLimitExceeded || res.Usage.Requests != 3 || res.Usage.ToolCalls != 2 {
		t.Fatalf("Run = %+v, %v", res, err)
	}
}
