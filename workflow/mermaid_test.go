package workflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nostosx/agentx/workflow"
)

func TestMermaid(t *testing.T) {
	t.Parallel()
	choose := func(context.Context, *workflow.State) ([]string, error) { return nil, nil }
	w := workflow.NewGraph().
		AddFunc("draft", noop, workflow.OnError("apology")).
		AddFunc(`say "hi" <b>`, noop).
		AddNode("fan", workflow.Map("xs", workflow.NodeFunc(noop))).
		AddFunc("apology", noop).
		AddChain(workflow.Start, "draft", `say "hi" <b>`).
		AddBranch(`say "hi" <b>`, choose, "draft", "fan").
		AddEdge("fan", workflow.End).AddEdge("apology", workflow.End).
		MustCompile()
	want := `flowchart TD
    start([start])
    n0["draft"]
    n1["say #quot;hi#quot; #lt;b#gt;"]
    n2[["fan"]]
    n3["apology"]
    end([end])
    n0 -. on error .-> n3
    start --> n0
    n0 --> n1
    n1 -. loop .-> n0
    n1 -.-> n2
    n2 --> end
    n3 --> end
`
	if got := w.Mermaid(); got != want {
		t.Fatalf("Mermaid =\n%s\nwant\n%s", got, want)
	}
}

func TestNestedWorkflowSuspendsAndResumes(t *testing.T) {
	t.Parallel()
	var before, asked atomic.Int32
	child := workflow.NewGraph().
		AddFunc("prep", func(_ context.Context, st *workflow.State) error { before.Add(1); st.Set("prep", "ready"); return nil }).
		AddFunc("ask", func(ctx context.Context, st *workflow.State) error {
			asked.Add(1)
			v, err := workflow.Suspend(ctx, "ok?")
			if err != nil {
				return err
			}
			st.Set("answer", string(v))
			return nil
		}).
		AddChain(workflow.Start, "prep", "ask").MustCompile()
	for _, persisted := range []bool{true, false} {
		before.Store(0)
		asked.Store(0)
		var opts []workflow.Option
		if persisted {
			opts = append(opts, workflow.WithCheckpointer(workflow.NewMemoryCheckpointer()))
		}
		var forwarded []string
		parent := workflow.NewGraph().AddNode("sub", child.AsNode()).AddEdge(workflow.Start, "sub").MustCompile(opts...)
		var s *workflow.Suspension
		for ev, err := range parent.Stream(t.Context(), nil, workflow.WithRunID("outer")) {
			if err != nil {
				t.Fatal(err)
			}
			if ev.Type == workflow.NodeStart {
				forwarded = append(forwarded, ev.Node)
			}
			if ev.Type == workflow.RunEnd && ev.Status == workflow.StatusSuspended {
				s = &workflow.Suspension{RunID: ev.RunID, Pending: ev.Suspended}
			}
		}
		if s == nil || len(s.Pending) != 1 || s.Pending[0].Key != "sub" {
			t.Fatalf("suspension = %+v", s)
		}
		if strings.Join(forwarded, ",") != "sub,sub/prep,sub/ask" {
			t.Fatalf("forwarded node starts = %v", forwarded)
		}
		var nested []*workflow.Pending
		if err := json.Unmarshal(s.Pending[0].Payload, &nested); err != nil || len(nested) != 1 || nested[0].Key != "ask" {
			t.Fatalf("nested pending = %s, %v", s.Pending[0].Payload, err)
		}
		st, err := parent.Resume(t.Context(), "outer", map[string]any{"sub": map[string]any{"ask": "yes"}})
		if err != nil || st.String("answer") != `"yes"` || st.String("prep") != "ready" || before.Load() != 1 || asked.Load() != 2 {
			t.Fatalf("persisted=%v: Resume = %v, %v (prep %d, ask %d)", persisted, st.Snapshot(), err, before.Load(), asked.Load())
		}
	}
	// A nested run outside a workflow returns the suspension as an error.
	if err := child.AsNode().Run(t.Context(), workflow.NewState(nil)); !errors.Is(err, workflow.ErrSuspended) {
		t.Fatalf("standalone nested = %v", err)
	}
}
