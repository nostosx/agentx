package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nostosx/agentx/model"
)

func TestSessionMessageIsolation(t *testing.T) {
	t.Parallel()
	s := New("")
	if s.ID() == "" || s.CreatedAt().IsZero() || s.UpdatedAt().Before(s.CreatedAt()) {
		t.Fatalf("invalid new session: %#v", TakeSnapshot(s))
	}
	msg := model.UserText("original")
	s.Append(msg, nil)
	msg.Parts[0].Text = "caller mutation"
	got := s.Messages()
	if got[0].Text() != "original" || got[1] != nil {
		t.Fatalf("Append = %#v", got)
	}
	got[0].Parts[0].Text = "reader mutation"
	if s.Messages()[0].Text() != "original" {
		t.Fatal("Messages exposes stored content")
	}
	replacement := []*model.Message{model.AssistantText("replacement")}
	s.Replace(replacement)
	replacement[0].Parts[0].Text = "changed"
	if got := s.Messages(); len(got) != 1 || got[0].Text() != "replacement" {
		t.Fatalf("Replace = %#v", got)
	}
	s.Replace(nil)
	if len(s.Messages()) != 0 {
		t.Fatal("Replace(nil) did not clear messages")
	}
}

func TestSessionConcurrentAccess(t *testing.T) {
	t.Parallel()
	s := New("shared")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 20 {
				s.Append(model.UserText("message"))
				s.Messages()
				s.UpdatedAt()
			}
		})
	}
	wg.Wait()
	if got := len(s.Messages()); got != 160 {
		t.Fatalf("message count = %d, want 160", got)
	}
}

func TestMemoryStore(t *testing.T) {
	t.Parallel()
	var store MemoryStore
	testStore(t, &store)
}

func TestMemoryStoreNestedStateIsolation(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	s := New("nested")
	nested := map[string]any{"values": []any{"saved"}}
	s.State().Set("nested", nested)
	if err := store.Save(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	nested["values"].([]any)[0] = "unsaved mutation"
	loaded, err := store.Get(t.Context(), "nested")
	if err != nil {
		t.Fatal(err)
	}
	value, _ := loaded.State().Get("nested")
	values := value.(map[string]any)["values"].([]any)
	if values[0] != "saved" {
		t.Fatalf("saved nested value changed through original: %v", values)
	}
	values[0] = "reader mutation"
	again, err := store.Get(t.Context(), "nested")
	if err != nil {
		t.Fatal(err)
	}
	value, _ = again.State().Get("nested")
	if got := value.(map[string]any)["values"].([]any)[0]; got != "saved" {
		t.Fatalf("stored nested value changed through reader: %v", got)
	}
}

func TestMemoryStoreSerializationFailurePreservesSnapshot(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	s := New("snapshot")
	s.Append(model.UserText("saved"))
	if err := store.Save(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	s.Append(model.AssistantText("unsaved"))
	s.State().Set("bad", make(chan int))
	if err := store.Save(t.Context(), s); err == nil {
		t.Fatal("Save accepted unserializable state")
	}
	loaded, err := store.Get(t.Context(), "snapshot")
	if err != nil || len(loaded.Messages()) != 1 || loaded.Messages()[0].Text() != "saved" {
		t.Fatalf("failed Save replaced previous snapshot: %+v, %v", loaded, err)
	}
	if _, ok := loaded.State().Get("bad"); ok {
		t.Fatal("failed Save leaked unserializable state")
	}
}

func testStore(t *testing.T, store Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.Get(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing = %v", err)
	}
	stamp := time.Unix(100, 0).UTC()
	for _, id := range []string{"b", "a", "newest"} {
		updated := stamp
		if id == "newest" {
			updated = stamp.Add(time.Second)
		}
		s := FromSnapshot(&Snapshot{ID: id, Messages: []*model.Message{model.UserText(id)}, State: map[string]any{"value": "saved"}, CreatedAt: stamp, UpdatedAt: updated})
		if err := store.Save(ctx, s); err != nil {
			t.Fatal(err)
		}
		s.Append(model.AssistantText("unsaved"))
		s.State().Set("value", "unsaved")
	}
	infos, err := store.List(ctx)
	if err != nil || len(infos) != 3 {
		t.Fatalf("List = %#v, %v", infos, err)
	}
	for i, id := range []string{"newest", "a", "b"} {
		if infos[i].ID != id || infos[i].Messages != 1 || !infos[i].CreatedAt.Equal(stamp) {
			t.Fatalf("List[%d] = %#v, want %s with one message", i, infos[i], id)
		}
	}
	got, err := store.Get(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Messages()[0].Text() != "a" || len(got.Messages()) != 1 {
		t.Fatalf("saved messages = %#v", got.Messages())
	}
	if v, _ := got.State().Get("value"); v != "saved" {
		t.Fatalf("saved value = %v", v)
	}
	got.Replace([]*model.Message{model.UserText("reader mutation")})
	got.State().Set("value", "reader mutation")
	again, err := store.Get(ctx, "a")
	if err != nil || again.Messages()[0].Text() != "a" {
		t.Fatalf("Get after reader mutation = %#v, %v", again, err)
	}
	if v, _ := again.State().Get("value"); v != "saved" {
		t.Fatalf("stored state changed through reader: %v", v)
	}
	if err := store.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get deleted = %v", err)
	}
}
