package workflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nostosx/agentx/workflow"
)

func TestMapMergesInItemOrder(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var running, peak atomic.Int32
		body := workflow.NodeFunc(func(ctx context.Context, st *workflow.State) error {
			n := running.Add(1)
			defer running.Add(-1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			i, _ := st.Get("i")
			item := st.String("doc")
			// Later items finish first.
			time.Sleep(time.Duration(5-i.(int)) * time.Millisecond)
			st.Set("results", strings.ToUpper(item))
			st.Set("count", 1)
			return nil
		})
		w := workflow.NewGraph().
			AddNode("upper", workflow.Map("docs", body, workflow.MapConcurrency(2), workflow.MapKeys("doc", "i"))).
			AddEdge(workflow.Start, "upper").
			MustCompile(workflow.Reducer("results", workflow.Append()), workflow.Reducer("count", func(cur, upd any) any {
				n, _ := cur.(int)
				return n + upd.(int)
			}))
		var itemStarts []int
		var st *workflow.State
		init := workflow.NewState(map[string]any{"docs": []string{"a", "b", "c", "d", "e"}})
		for ev, err := range w.Stream(t.Context(), init) {
			if err != nil {
				t.Fatal(err)
			}
			if ev.Type == workflow.NodeStart && ev.Index >= 0 {
				itemStarts = append(itemStarts, ev.Index)
			}
		}
		st = init
		if got, _ := st.Get("results"); !reflect.DeepEqual(got, []any{"A", "B", "C", "D", "E"}) {
			t.Fatalf("results = %v", got)
		}
		if st.String("count") != "5" || peak.Load() != 2 || len(itemStarts) != 5 {
			t.Fatalf("count = %s, peak = %d, item starts = %v", st.String("count"), peak.Load(), itemStarts)
		}
		for _, k := range []string{"doc", "i", "item", "index"} {
			if _, ok := st.Get(k); ok {
				t.Errorf("private key %q committed", k)
			}
		}
	})
}

func TestMapConflictsAndErrors(t *testing.T) {
	t.Parallel()
	set := workflow.NodeFunc(func(_ context.Context, st *workflow.State) error { st.Set("out", st.String("item")); return nil })
	run := func(n workflow.Node, init map[string]any) error {
		_, err := workflow.NewGraph().AddNode("m", n).AddEdge(workflow.Start, "m").MustCompile().Run(t.Context(), workflow.NewState(init))
		return err
	}
	err := run(workflow.Map("xs", set), map[string]any{"xs": []int{1, 2}})
	if c, ok := errors.AsType[*workflow.ConflictError](err); !ok || !slices.Equal(c.Nodes, []string{"m[0]", "m[1]"}) {
		t.Fatalf("conflict = %v", err)
	}
	if err := run(workflow.Map("xs", set), map[string]any{"xs": "not a list"}); err == nil || !strings.Contains(err.Error(), "not a list") {
		t.Fatalf("non-list = %v", err)
	}
	if err := run(workflow.Map("missing", set), nil); err != nil {
		t.Fatalf("empty map = %v", err)
	}
	failing := workflow.NodeFunc(func(ctx context.Context, st *workflow.State) error {
		if i, _ := st.Get("index"); i == 1 {
			return errors.New("bad item")
		}
		<-ctx.Done()
		return ctx.Err()
	})
	if err := run(workflow.Map("xs", failing), map[string]any{"xs": []int{0, 1, 2}}); err == nil || !strings.Contains(err.Error(), "item 1: bad item") {
		t.Fatalf("failing item = %v", err)
	}
	panicking := workflow.NodeFunc(func(context.Context, *workflow.State) error { panic("item panic") })
	if err := run(workflow.Map("xs", panicking), map[string]any{"xs": []int{0}}); err == nil || !strings.Contains(err.Error(), "panic: item panic") {
		t.Fatalf("panicking item = %v", err)
	}
	// Updates of items are committed as values in item order.
	upd := workflow.NodeFunc(func(_ context.Context, st *workflow.State) error {
		st.Update(func(v map[string]any) { v["last"] = v["item"] })
		return nil
	})
	st := workflow.NewState(map[string]any{"xs": []int{1, 2, 3}})
	if err := workflow.Map("xs", upd).Run(t.Context(), st); err != nil || st.String("last") != "3" {
		t.Fatalf("updates = %v, %v", st.Snapshot(), err)
	}
}

func TestMapResumesOnlyUnfinishedItems(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		inner := workflow.NewMemoryCheckpointer()
		cp := &crashCheckpointer{Checkpointer: inner}
		var mu sync.Mutex
		ran := map[int]int{}
		block := make(chan struct{})
		var once sync.Once
		body := func(crash bool) workflow.Node {
			return workflow.NodeFunc(func(ctx context.Context, st *workflow.State) error {
				i, _ := st.Get("index")
				idx := int(toFloat(i))
				mu.Lock()
				ran[idx]++
				mu.Unlock()
				if crash && idx == 2 {
					once.Do(func() { close(block) })
					<-ctx.Done()
					return ctx.Err()
				}
				st.Set("out", fmt.Sprintf("item-%v", st.String("item")))
				return nil
			})
		}
		build := func(crash bool) *workflow.Workflow {
			return workflow.NewGraph().AddNode("m", workflow.Map("xs", body(crash))).
				AddEdge(workflow.Start, "m").MustCompile(workflow.WithCheckpointer(cp), workflow.Reducer("out", workflow.Append()))
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, err := build(true).Run(ctx, workflow.NewState(map[string]any{"xs": []string{"a", "b", "c", "d"}}), workflow.WithRunID("map"))
			done <- err
		}()
		<-block
		// Crash once the other items are checkpointed.
		synctest.Wait()
		cp.crashed.Store(true)
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("interrupted run: %v, want context.Canceled", err)
		}
		c, err := inner.Load(t.Context(), "map")
		if err != nil || len(c.Maps["m"].Done) != 3 || len(c.Maps["m"].Items) != 4 {
			t.Fatalf("checkpoint = %+v, %v", c, err)
		}
		// The list changes before the resume; the captured items are used.
		cp.crashed.Store(false)
		var ckpt workflow.Checkpoint
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &ckpt); err != nil {
			t.Fatal(err)
		}
		ckpt.Values["xs"] = json.RawMessage(`["z"]`)
		if err := inner.Save(t.Context(), &ckpt); err != nil {
			t.Fatal(err)
		}
		st, err := build(false).Resume(t.Context(), "map", nil)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := st.Get("out"); !reflect.DeepEqual(got, []any{"item-a", "item-b", "item-c", "item-d"}) {
			t.Fatalf("out = %v", got)
		}
		if ran[0] != 1 || ran[1] != 1 || ran[2] != 2 || ran[3] != 1 {
			t.Fatalf("item runs = %v", ran)
		}
	})
}

func toFloat(v any) float64 {
	switch v := v.(type) {
	case int:
		return float64(v)
	case float64:
		return v
	}
	return -1
}

func TestMapItemSuspension(t *testing.T) {
	t.Parallel()
	var runs atomic.Int32
	body := workflow.NodeFunc(func(ctx context.Context, st *workflow.State) error {
		runs.Add(1)
		if st.String("item") == "review" {
			v, err := workflow.Suspend(ctx, "check "+st.String("item"))
			if err != nil {
				return err
			}
			st.Set("out", "reviewed:"+string(v))
			return nil
		}
		st.Set("out", st.String("item"))
		return nil
	})
	w := workflow.NewGraph().AddNode("m", workflow.Map("xs", body)).AddEdge(workflow.Start, "m").
		MustCompile(workflow.WithCheckpointer(workflow.NewMemoryCheckpointer()), workflow.Reducer("out", workflow.Append()))
	_, err := w.Run(t.Context(), workflow.NewState(map[string]any{"xs": []string{"a", "review", "b", "review"}}), workflow.WithRunID("items"))
	s := suspension(t, err)
	if len(s.Pending) != 2 || s.Pending[0].Key != "m[1]" || s.Pending[0].Index != 1 || s.Pending[1].Key != "m[3]" || string(s.Pending[0].Payload) != `"check review"` {
		t.Fatalf("pending = %+v", s.Pending)
	}
	_, err = w.Resume(t.Context(), "items", map[string]any{"m[1]": 1})
	if s = suspension(t, err); len(s.Pending) != 1 || s.Pending[0].Key != "m[3]" {
		t.Fatalf("partial = %+v", s.Pending)
	}
	st, err := w.Resume(t.Context(), "items", map[string]any{"m[3]": 3})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Get("out"); !reflect.DeepEqual(got, []any{"a", "reviewed:1", "b", "reviewed:3"}) {
		t.Fatalf("out = %v", got)
	}
	if runs.Load() != 6 {
		t.Fatalf("item runs = %d", runs.Load())
	}
}
