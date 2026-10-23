package session

import (
	"context"
	"errors"
	"testing"

	"github.com/nostosx/agentx/model"
)

type failingStore struct {
	Store
	err error
}

func (s failingStore) Get(context.Context, string) (Session, error) { return nil, s.err }

func TestGetOrCreate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	want := New("exists")
	want.Append(model.UserText("persisted"))
	if err := store.Save(ctx, want); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		id      string
		created bool
	}{
		{name: "existing", id: "exists"},
		{name: "missing", id: "new", created: true},
		{name: "generated", created: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, created, err := GetOrCreate(ctx, store, tc.id)
			if err != nil || created != tc.created || s.ID() == "" || (tc.id != "" && s.ID() != tc.id) {
				t.Fatalf("GetOrCreate = %#v, %v, %v", s, created, err)
			}
			if !created && s.Messages()[0].Text() != "persisted" {
				t.Fatal("existing conversation lost")
			}
		})
	}
	wantErr := errors.New("storage unavailable")
	if _, created, err := GetOrCreate(ctx, failingStore{err: wantErr}, "id"); created || !errors.Is(err, wantErr) {
		t.Fatalf("GetOrCreate error = %v, %v", created, err)
	}
}
