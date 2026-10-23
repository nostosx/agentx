package workflow_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/nostosx/agentx/workflow"
)

// parallel builds a graph in which Start fans out to the given nodes.
func parallel(nodes map[string]func(context.Context, *workflow.State) error) *workflow.Graph {
	g := workflow.NewGraph()
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for k := range nodes {
			if !yield(k) {
				return
			}
		}
	}) {
		g.AddFunc(name, nodes[name]).AddEdge(workflow.Start, name).AddEdge(name, workflow.End)
	}
	return g
}

func TestParallelWritesConflictWithoutReducer(t *testing.T) {
	t.Parallel()
	g := parallel(map[string]func(context.Context, *workflow.State) error{
		"a": func(_ context.Context, st *workflow.State) error { st.Set("out", "a"); return nil },
		"b": func(_ context.Context, st *workflow.State) error { st.Set("out", "b"); return nil },
	})
	_, err := g.MustCompile().Run(t.Context(), nil)
	conflict, ok := errors.AsType[*workflow.ConflictError](err)
	if !ok || conflict.Key != "out" || !reflect.DeepEqual(conflict.Nodes, []string{"a", "b"}) {
		t.Fatalf("Run = %v", err)
	}
	// Deleting counts as a write too.
	g = parallel(map[string]func(context.Context, *workflow.State) error{
		"a": func(_ context.Context, st *workflow.State) error { st.Set("out", "a"); return nil },
		"b": func(_ context.Context, st *workflow.State) error { st.Delete("out"); return nil },
	})
	if _, err := g.MustCompile().Run(t.Context(), nil); !errors.As(err, &conflict) {
		t.Fatalf("delete conflict = %v", err)
	}
}

func TestReducersMergeParallelWrites(t *testing.T) {
	t.Parallel()
	g := parallel(map[string]func(context.Context, *workflow.State) error{
		"a": func(_ context.Context, st *workflow.State) error {
			st.Set("items", "a1")
			st.Set("items", "a2")
			// The node sees its own updates merged on the committed value.
			if got := st.String("items"); !strings.HasPrefix(got, "[x ") || !strings.HasSuffix(got, " a1 a2]") {
				t.Errorf("node view = %s", got)
			}
			return nil
		},
		"b": func(_ context.Context, st *workflow.State) error { st.Set("items", []any{"b1", "b2"}); return nil },
		"c": func(_ context.Context, st *workflow.State) error { st.Set("total", 2); return nil },
		"d": func(_ context.Context, st *workflow.State) error { st.Set("total", 3); return nil },
	})
	sum := workflow.Reducer("total", func(cur, upd any) any {
		n, _ := cur.(int)
		return n + upd.(int)
	})
	st, err := g.MustCompile(workflow.Reducer("items", workflow.Append()), sum).Run(t.Context(), workflow.NewState(map[string]any{"items": []any{"x"}}))
	if err != nil {
		t.Fatal(err)
	}
	items, _ := st.Get("items")
	got := items.([]any)
	if len(got) != 5 || got[0] != "x" || !slices.Contains(got, "a1") || !slices.Contains(got, "a2") ||
		!slices.Contains(got, "b1") || !slices.Contains(got, "b2") ||
		slices.Index(got, "a1") > slices.Index(got, "a2") || slices.Index(got, "b1") > slices.Index(got, "b2") {
		t.Fatalf("items = %v", got)
	}
	if st.String("total") != "5" {
		t.Fatalf("total = %v", st.String("total"))
	}
}

func TestAppendReducer(t *testing.T) {
	t.Parallel()
	app := workflow.Append()
	for _, tt := range []struct {
		cur, upd, want any
	}{
		{nil, "a", []any{"a"}},
		{nil, []any{"a", "b"}, []any{"a", "b"}},
		{[]string{"a"}, "b", []string{"a", "b"}},
		{[]string{"a"}, []string{"b", "c"}, []string{"a", "b", "c"}},
		{[]string{"a"}, 1, []any{"a", 1}},
		{"solo", "b", []any{"solo", "b"}},
		{[]any{1}, []any{2}, []any{1, 2}},
	} {
		if got := app(tt.cur, tt.upd); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("Append(%v, %v) = %#v, want %#v", tt.cur, tt.upd, got, tt.want)
		}
	}
	cur := make([]string, 1, 4)
	cur[0] = "a"
	_ = app(cur, "b")
	if cur[:2][1] != "" {
		t.Fatal("Append wrote into the current slice")
	}
}

func TestOrderedNodesOverwriteAndUpdatesCompose(t *testing.T) {
	t.Parallel()
	inc := func(_ context.Context, st *workflow.State) error {
		st.Update(func(v map[string]any) { v["count"] = v["count"].(int) + 1 })
		return nil
	}
	w := workflow.NewGraph().
		AddFunc("first", func(_ context.Context, st *workflow.State) error { st.Set("out", "first"); return nil }).
		AddFunc("second", func(_ context.Context, st *workflow.State) error { st.Set("out", "second"); return nil }).
		AddFunc("inc1", inc).AddFunc("inc2", inc).AddFunc("inc3", inc).
		AddChain(workflow.Start, "first", "second", workflow.End).
		AddEdge(workflow.Start, "inc1").AddEdge(workflow.Start, "inc2").AddEdge(workflow.Start, "inc3").
		MustCompile()
	st, err := w.Run(t.Context(), workflow.NewState(map[string]any{"count": 0}))
	if err != nil || st.String("out") != "second" || st.String("count") != "3" {
		t.Fatalf("Run = %v, %v", st.Snapshot(), err)
	}
}

func TestFailedAttemptWritesAreDiscarded(t *testing.T) {
	t.Parallel()
	attempts := 0
	w := workflow.NewGraph().AddFunc("a", func(_ context.Context, st *workflow.State) error {
		attempts++
		st.Set("log", attempts)
		if attempts == 1 {
			st.Set("partial", true)
			return errors.New("transient")
		}
		return nil
	}, workflow.Retry(2, 0)).AddEdge(workflow.Start, "a").MustCompile(workflow.Reducer("log", workflow.Append()))
	st, err := w.Run(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Get("partial"); ok || st.String("log") != "[2]" {
		t.Fatalf("state = %v", st.Snapshot())
	}
}

func TestNodeViewIsolation(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		release, read := make(chan struct{}), make(chan struct{})
		w := workflow.NewGraph().
			AddFunc("writer", func(_ context.Context, st *workflow.State) error {
				st.Set("k", "v")
				st.Delete("gone")
				if _, ok := st.Get("gone"); ok {
					t.Error("deleted key visible to its node")
				}
				if snap := st.Snapshot(); snap["k"] != "v" || snap["gone"] != nil {
					t.Errorf("snapshot = %v", snap)
				}
				close(release)
				<-read
				return nil
			}).
			AddFunc("reader", func(_ context.Context, st *workflow.State) error {
				<-release
				if _, ok := st.Get("gone"); !ok {
					t.Error("uncommitted delete visible to a parallel node")
				}
				if _, ok := st.Get("k"); ok {
					t.Error("uncommitted write visible to a parallel node")
				}
				close(read)
				return nil
			}).
			AddEdge(workflow.Start, "writer").AddEdge(workflow.Start, "reader").MustCompile()
		st, err := w.Run(t.Context(), workflow.NewState(map[string]any{"gone": 1}))
		if err != nil || st.String("k") != "v" {
			t.Fatalf("Run = %v, %v", st.Snapshot(), err)
		}
		if _, ok := st.Get("gone"); ok {
			t.Fatal("delete not committed")
		}
	})
}

func TestNestedWorkflowReducers(t *testing.T) {
	t.Parallel()
	child := parallel(map[string]func(context.Context, *workflow.State) error{
		"x": func(_ context.Context, st *workflow.State) error { st.Set("items", "x"); return nil },
		"y": func(_ context.Context, st *workflow.State) error { st.Set("items", "y"); return nil },
	}).MustCompile()
	parent := workflow.NewGraph().
		AddNode("nested", child.AsNode()).
		AddFunc("other", func(_ context.Context, st *workflow.State) error { st.Set("items", "o"); return nil }).
		AddEdge(workflow.Start, "nested").AddEdge(workflow.Start, "other").
		MustCompile(workflow.Reducer("items", workflow.Append()))
	st, err := parent.Run(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	items, _ := st.Get("items")
	got := slices.Clone(items.([]any))
	slices.SortFunc(got, func(a, b any) int {
		switch {
		case a.(string) < b.(string):
			return -1
		case a.(string) > b.(string):
			return 1
		}
		return 0
	})
	if !reflect.DeepEqual(got, []any{"o", "x", "y"}) {
		t.Fatalf("items = %v", items)
	}
}

func TestNestedWorkflowCommitsEveryWriteKind(t *testing.T) {
	t.Parallel()
	child := workflow.NewGraph().
		AddFunc("set", func(_ context.Context, st *workflow.State) error { st.Set("items", "c"); st.Delete("gone"); return nil }).
		AddFunc("upd", func(_ context.Context, st *workflow.State) error {
			st.Update(func(v map[string]any) { v["n"] = v["n"].(int) + 1; delete(v, "gone2") })
			return nil
		}).
		AddFunc("map", func(ctx context.Context, st *workflow.State) error {
			return workflow.Map("xs", workflow.NodeFunc(func(_ context.Context, st *workflow.State) error {
				st.Update(func(v map[string]any) { v["last"] = v["item"] })
				return nil
			})).Run(ctx, st)
		}).
		AddChain(workflow.Start, "set", "upd", "map").MustCompile()
	parent := workflow.NewGraph().AddNode("nested", child.AsNode()).
		AddFunc("check", func(_ context.Context, st *workflow.State) error {
			if _, ok := st.Get("gone"); ok {
				t.Error("delete lost")
			}
			return nil
		}).
		AddChain(workflow.Start, "nested", "check").
		MustCompile(workflow.Reducer("items", workflow.Append()))
	st, err := parent.Run(t.Context(), workflow.NewState(map[string]any{"items": []any{"p"}, "gone": 1, "gone2": 2, "n": 1, "xs": []any{"a", "b"}}))
	if err != nil {
		t.Fatal(err)
	}
	if st.String("items") != "[p c]" || st.String("n") != "2" || st.String("last") != "b" {
		t.Fatalf("state = %v", st.Snapshot())
	}
	for _, k := range []string{"gone", "gone2"} {
		if _, ok := st.Get(k); ok {
			t.Errorf("%s not deleted", k)
		}
	}
	if msg := (&workflow.ConflictError{Key: "k", Nodes: []string{"a", "b"}}).Error(); !strings.Contains(msg, `"k"`) || !strings.Contains(msg, "a and b") {
		t.Fatalf("ConflictError = %q", msg)
	}
}
