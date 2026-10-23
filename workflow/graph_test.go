package workflow_test

import (
	"context"
	"strings"
	"testing"

	"github.com/nostosx/agentx/workflow"
)

func noop(context.Context, *workflow.State) error { return nil }

func TestGraphCompileValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		build func() *workflow.Graph
		want  string
	}{
		{"empty", workflow.NewGraph, "no edge from Start"},
		{"reserved name", func() *workflow.Graph { return workflow.NewGraph().AddFunc(workflow.Start, noop) }, "invalid node name"},
		{"empty name", func() *workflow.Graph { return workflow.NewGraph().AddFunc("", noop) }, "invalid node name"},
		{"nil node", func() *workflow.Graph { return workflow.NewGraph().AddNode("a", nil) }, "is nil"},
		{"duplicate", func() *workflow.Graph { return workflow.NewGraph().AddFunc("a", noop).AddFunc("a", noop) }, "duplicate node"},
		{"unknown source", func() *workflow.Graph { return workflow.NewGraph().AddEdge("missing", workflow.End) }, "unknown node"},
		{"unknown destination", func() *workflow.Graph { return workflow.NewGraph().AddEdge(workflow.Start, "missing") }, "unknown node"},
		{"edge into start", func() *workflow.Graph {
			return workflow.NewGraph().AddFunc("a", noop).AddChain(workflow.Start, "a", workflow.Start)
		}, "unknown node"},
		{"edge out of end", func() *workflow.Graph {
			return workflow.NewGraph().AddEdge(workflow.Start, workflow.End).AddEdge(workflow.End, workflow.End)
		}, "unknown node"},
		{"unreachable", func() *workflow.Graph {
			return workflow.NewGraph().AddFunc("orphan", noop).AddEdge(workflow.Start, workflow.End)
		}, "unreachable"},
		{"cycle", func() *workflow.Graph {
			return workflow.NewGraph().AddFunc("a", noop).AddFunc("b", noop).AddChain(workflow.Start, "a", "b", "a")
		}, "cycle a -> b -> a"},
		{"missing condition", func() *workflow.Graph { return workflow.NewGraph().AddBranch(workflow.Start, nil, workflow.End) }, "needs a condition and targets"},
		{"missing targets", func() *workflow.Graph {
			return workflow.NewGraph().AddBranch(workflow.Start, func(context.Context, *workflow.State) ([]string, error) { return nil, nil })
		}, "needs a condition and targets"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.build().Compile()
			if got != nil || err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Compile() = %v, %v; want error containing %q", got, err, tt.want)
			}
		})
	}
}

func TestGraphMustCompilePanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("MustCompile should panic for invalid graph")
		}
	}()
	workflow.NewGraph().MustCompile()
}

func TestCompiledWorkflowIndependentOfBuilder(t *testing.T) {
	t.Parallel()
	var selected, unselected int
	graph := workflow.NewGraph().AddFunc("selected", func(context.Context, *workflow.State) error { selected++; return nil }).AddFunc("unselected", func(context.Context, *workflow.State) error { unselected++; return nil }).AddBranch(workflow.Start, func(context.Context, *workflow.State) ([]string, error) { return []string{"selected"}, nil }, "selected", "unselected")
	compiled := graph.MustCompile()
	graph.AddEdge(workflow.Start, "unselected")
	if _, err := compiled.Run(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if selected != 1 || unselected != 0 {
		t.Fatalf("builder mutation changed compiled workflow: selected=%d unselected=%d", selected, unselected)
	}
}

func TestGraphCopiesBranchAndConditionInputs(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"branch targets", "condition routes"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			ran := false
			g := workflow.NewGraph().AddFunc("selected", func(context.Context, *workflow.State) error { ran = true; return nil })
			if kind == "branch targets" {
				targets := []string{"selected"}
				g.AddBranch(workflow.Start, func(context.Context, *workflow.State) ([]string, error) { return []string{"selected"}, nil }, targets...)
				targets[0] = "unknown"
			} else {
				routes := map[string]string{"route": "selected"}
				g.AddCondition(workflow.Start, func(context.Context, *workflow.State) (string, error) { return "route", nil }, routes)
				routes["route"] = "unknown"
			}
			w, err := g.Compile()
			if err != nil {
				t.Fatalf("input mutation invalidated graph: %v", err)
			}
			if _, err := w.Run(t.Context(), nil); err != nil || !ran {
				t.Fatalf("Run=%v ran=%v", err, ran)
			}
		})
	}
}
