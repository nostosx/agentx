package tool_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/nostosx/agentx/tool"
)

func rawTool(name string) tool.Tool {
	return tool.New(name, "", nil, func(context.Context, json.RawMessage) (*tool.Result, error) { return tool.Text(name), nil })
}

func TestValidateName(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		valid bool
	}{{"valid_Name-1", true}, {strings.Repeat("a", 64), true}, {"", false}, {strings.Repeat("a", 65), false}, {"a.b", false}, {"工具", false}, {"a b", false}} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tool.ValidateName(tt.name); (err == nil) != tt.valid {
				t.Fatalf("ValidateName(%q)=%v", tt.name, err)
			}
		})
	}
}

func TestRegistryAtomicRegistrationAndCopies(t *testing.T) {
	t.Parallel()
	r, err := tool.NewRegistry(rawTool("z"), rawTool("a"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tools := range [][]tool.Tool{{rawTool("new"), rawTool("z")}, {rawTool("new"), rawTool("new")}, {rawTool("new"), rawTool("bad.name")}} {
		if err := r.Register(tools...); err == nil {
			t.Fatal("invalid batch succeeded")
		}
		if r.Len() != 2 {
			t.Fatalf("batch partially registered: %d", r.Len())
		}
	}
	list := r.List()
	if list[0].Info().Name != "a" || list[1].Info().Name != "z" {
		t.Fatalf("List=%v", list)
	}
	specs := r.Specs()
	if specs[0].Name != "a" || specs[1].Name != "z" {
		t.Fatalf("Specs=%v", specs)
	}
	clone := r.Clone()
	clone.Remove("a")
	if r.Len() != 2 || clone.Len() != 1 {
		t.Fatal("clone aliases registry")
	}
	sub, err := r.Subset("z")
	if err != nil || sub.Len() != 1 {
		t.Fatalf("Subset=%v,%v", sub, err)
	}
	if _, err := r.Subset("missing"); !errors.Is(err, tool.ErrNotFound) {
		t.Fatalf("missing subset=%v", err)
	}
	var zero tool.Registry
	if err := zero.Register(rawTool("zero")); err != nil || zero.Len() != 1 {
		t.Fatalf("zero registry=%v", err)
	}
	var nilRegistry *tool.Registry
	if nilRegistry.Len() != 0 || nilRegistry.List() != nil || nilRegistry.Clone().Len() != 0 {
		t.Fatal("nil registry defaults")
	}
	if _, ok := nilRegistry.Get("x"); ok {
		t.Fatal("nil Get found value")
	}
}

func TestRegistryConcurrentAccess(t *testing.T) {
	t.Parallel()
	r, _ := tool.NewRegistry(rawTool("stable"))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				r.Get("stable")
				r.List()
				r.Specs()
				r.Clone()
				r.Len()
			}
		})
	}
	wg.Go(func() {
		for range 50 {
			if err := r.Register(rawTool("temporary")); err != nil {
				t.Error(err)
			}
			r.Remove("temporary")
		}
	})
	wg.Wait()
}
