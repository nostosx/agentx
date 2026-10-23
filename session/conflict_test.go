package session

import (
	"errors"
	"testing"

	"github.com/nostosx/agentx/model"
)

func TestStoresDetectConflicts(t *testing.T) {
	t.Parallel()
	for name, store := range map[string]Store{"memory": NewMemoryStore()} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			s, created, err := GetOrCreate(ctx, store, "shared")
			if err != nil || !created {
				t.Fatal(created, err)
			}
			s.Append(model.UserText("first"))
			if err := store.Save(ctx, s); err != nil {
				t.Fatal(err)
			}
			// Saving the same session again is fine.
			if err := store.Save(ctx, s); err != nil {
				t.Fatal(err)
			}
			a, _ := store.Get(ctx, "shared")
			b, _ := store.Get(ctx, "shared")
			a.Append(model.UserText("from a"))
			if err := store.Save(ctx, a); err != nil {
				t.Fatal(err)
			}
			b.Append(model.UserText("from b"))
			if err := store.Save(ctx, b); !errors.Is(err, ErrConflict) {
				t.Fatalf("stale Save = %v, want ErrConflict", err)
			}
			if err := store.Save(ctx, s); !errors.Is(err, ErrConflict) {
				t.Fatalf("stale Save of saved session = %v, want ErrConflict", err)
			}
			got, _ := store.Get(ctx, "shared")
			if msgs := got.Messages(); len(msgs) != 2 || msgs[1].Text() != "from a" {
				t.Fatalf("stored messages = %v", msgs)
			}
			// A session created while another writer created the same ID.
			fresh, _, _ := GetOrCreate(ctx, store, "raced")
			if err := store.Save(ctx, New("raced")); err != nil {
				t.Fatal(err)
			}
			if err := store.Save(ctx, fresh); !errors.Is(err, ErrConflict) {
				t.Fatalf("Save of raced new session = %v, want ErrConflict", err)
			}
			// Untracked sessions keep overwriting as before.
			if err := store.Save(ctx, New("shared")); err != nil {
				t.Fatalf("untracked Save = %v", err)
			}
		})
	}
}
