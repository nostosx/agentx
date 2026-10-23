package workflow_test

import (
	"sync"
	"testing"

	"github.com/nostosx/agentx/workflow"
)

func TestStateCopiesAndConcurrentUpdates(t *testing.T) {
	t.Parallel()
	init := map[string]any{"count": 0, "name": "test"}
	st := workflow.NewState(init)
	init["name"] = "changed"
	if got := st.String("name"); got != "test" {
		t.Fatalf("initial map aliases state: %q", got)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			for range 100 {
				st.Update(func(v map[string]any) { v["count"] = v["count"].(int) + 1 })
				_ = st.Snapshot()
			}
		})
	}
	wg.Wait()
	if got := st.String("count"); got != "2000" {
		t.Fatalf("count = %q", got)
	}
	snap := st.Snapshot()
	snap["name"] = "other"
	if st.String("name") != "test" {
		t.Fatal("snapshot aliases state")
	}
	st.Delete("name")
	if _, ok := st.Get("name"); ok || st.String("name") != "" {
		t.Fatal("Delete did not remove value")
	}
	var zero workflow.State
	zero.Set("value", 1)
	zero.Update(func(v map[string]any) { v["other"] = true })
	if zero.String("other") != "true" {
		t.Fatal("zero state does not support Update")
	}
}

func TestStateDecode(t *testing.T) {
	t.Parallel()
	st := workflow.NewState(map[string]any{"item": map[string]any{"name": "hello"}, "bad": make(chan int)})
	var dst struct {
		Name string `json:"name"`
	}
	if ok, err := st.Decode("item", &dst); !ok || err != nil || dst.Name != "hello" {
		t.Fatalf("Decode = %v, %v, %+v", ok, err, dst)
	}
	if ok, err := st.Decode("missing", &dst); ok || err != nil {
		t.Fatalf("missing Decode = %v, %v", ok, err)
	}
	if ok, err := st.Decode("bad", &dst); !ok || err == nil {
		t.Fatalf("unsupported Decode = %v, %v", ok, err)
	}
	if ok, err := st.Decode("item", 1); !ok || err == nil {
		t.Fatalf("invalid destination Decode = %v, %v", ok, err)
	}
	if v, ok, err := st.Value[struct{ Name string }]("item"); !ok || err != nil || v.Name != "hello" {
		t.Fatalf("Value = %+v, %v, %v", v, ok, err)
	}
	if n, ok, err := st.Value[int]("missing"); ok || err != nil || n != 0 {
		t.Fatalf("missing Value = %v, %v, %v", n, ok, err)
	}
	if _, ok, err := st.Value[int]("item"); !ok || err == nil {
		t.Fatalf("mismatched Value = %v, %v", ok, err)
	}
}
