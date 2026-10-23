package memory_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nostosx/agentx/memory"
)

func TestMemoryStore(t *testing.T) {
	t.Parallel()
	var s memory.MemoryStore
	ctx := t.Context()
	if err := s.Put(ctx, &memory.Entry{}); err == nil {
		t.Fatal("Put accepted an empty ID")
	}
	for _, op := range []struct {
		name string
		run  func() error
	}{
		{"get missing", func() error { _, err := s.Get(ctx, "missing"); return err }},
		{"delete missing", func() error { return s.Delete(ctx, "missing") }},
	} {
		t.Run(op.name, func(t *testing.T) {
			if err := op.run(); !errors.Is(err, memory.ErrNotFound) {
				t.Errorf("error = %v", err)
			}
		})
	}
	e := &memory.Entry{ID: "b", Content: "original", Tags: []string{"go", "agent"}, Scope: "one", Metadata: map[string]string{"key": "original"}, Embedding: []float32{1, 2}, CreatedAt: time.Unix(10, 0)}
	if err := s.Put(ctx, e); err != nil {
		t.Fatal(err)
	}
	e.Content, e.Tags[0], e.Metadata["key"], e.Embedding[0] = "mutated", "changed", "changed", 0
	got, err := s.Get(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "original" || got.Tags[0] != "go" || got.Metadata["key"] != "original" || got.Embedding[0] != 1 {
		t.Fatalf("store retained caller's mutable state: %+v", got)
	}
	got.Tags[0], got.Metadata["key"], got.Embedding[0] = "changed", "changed", 0
	again, _ := s.Get(ctx, "b")
	if again.Tags[0] != "go" || again.Metadata["key"] != "original" || again.Embedding[0] != 1 {
		t.Error("Get leaked mutable stored state")
	}
	for _, entry := range []*memory.Entry{
		{ID: "a", Scope: "one", Tags: []string{"go", "agent"}, CreatedAt: time.Unix(10, 0)},
		{ID: "older", Scope: "two", Tags: []string{"go"}, CreatedAt: time.Unix(5, 0)},
	} {
		if err := s.Put(ctx, entry); err != nil {
			t.Fatal(err)
		}
	}
	all, _ := s.List(ctx, memory.Filter{})
	if ids := entryIDs(all); !reflect.DeepEqual(ids, []string{"older", "a", "b"}) {
		t.Errorf("List order = %v", ids)
	}
	filtered, _ := s.List(ctx, memory.Filter{Scope: "one", Tags: []string{"go", "agent"}})
	if ids := entryIDs(filtered); !reflect.DeepEqual(ids, []string{"a", "b"}) {
		t.Errorf("filtered List = %v", ids)
	}
	filtered[1].Metadata["key"] = "mutated list"
	again, _ = s.Get(ctx, "b")
	if again.Metadata["key"] != "original" {
		t.Error("List leaked mutable stored state")
	}
	if err := s.Delete(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "b"); !errors.Is(err, memory.ErrNotFound) {
		t.Errorf("deleted entry returned error %v", err)
	}
}

func entryIDs(entries []*memory.Entry) []string {
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	return ids
}

func TestMemoryStoreConcurrentAccess(t *testing.T) {
	t.Parallel()
	s := memory.NewMemoryStore()
	var wg sync.WaitGroup
	for _, id := range []string{"a", "b", "c", "d"} {
		wg.Go(func() {
			for range 30 {
				if err := s.Put(t.Context(), &memory.Entry{ID: id}); err != nil {
					t.Error(err)
				}
				if _, err := s.Get(t.Context(), id); err != nil {
					t.Error(err)
				}
				if _, err := s.List(t.Context(), memory.Filter{}); err != nil {
					t.Error(err)
				}
				if err := s.Delete(t.Context(), id); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
}

type scopedStore interface {
	memory.Store
	DeleteInScope(context.Context, string, string) error
}

func testDeleteInScope(t *testing.T, newStore func(*testing.T) scopedStore) {
	t.Helper()
	for _, tt := range []struct {
		name, id, scope string
		allowed         bool
	}{
		{"matching scope", "fact", "project", true},
		{"other scope", "fact", "other", false},
		{"empty scope", "fact", "", true},
		{"missing ID", "missing", "project", false},
		{"missing ID empty scope", "missing", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			original := &memory.Entry{ID: "fact", Scope: "project", Content: "durable fact", Tags: []string{"go"}}
			if err := s.Put(t.Context(), original); err != nil {
				t.Fatal(err)
			}
			err := s.DeleteInScope(t.Context(), tt.id, tt.scope)
			if tt.allowed {
				if err != nil {
					t.Fatalf("allowed DeleteInScope() = %v", err)
				}
			} else if !errors.Is(err, memory.ErrNotFound) {
				t.Errorf("denied DeleteInScope() = %v, want ErrNotFound", err)
			}
			got, err := s.Get(t.Context(), "fact")
			if tt.allowed {
				if !errors.Is(err, memory.ErrNotFound) {
					t.Errorf("store retained deleted entry: %+v, %v", got, err)
				}
			} else if err != nil || !reflect.DeepEqual(got, original) {
				t.Errorf("store changed entry after denied deletion: %+v, %v", got, err)
			}
		})
	}
}

func TestMemoryStoreDeleteInScope(t *testing.T) {
	t.Parallel()
	testDeleteInScope(t, func(*testing.T) scopedStore {
		return memory.NewMemoryStore()
	})
}
