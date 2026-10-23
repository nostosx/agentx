package session

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nostosx/agentx/model"
)

func TestSnapshotRoundTrip(t *testing.T) {
	t.Parallel()
	s := New("conversation")
	s.Append(model.UserText("hello"))
	s.State().Set("language", "en")
	snap := TakeSnapshot(s)
	restored := FromSnapshot(snap)
	if restored.ID() != s.ID() || !restored.CreatedAt().Equal(s.CreatedAt()) || !restored.UpdatedAt().Equal(s.UpdatedAt()) {
		t.Fatalf("snapshot lost identity or timestamps: %#v", TakeSnapshot(restored))
	}
	snap.Messages[0].Parts[0].Text = "mutated"
	snap.State["language"] = "other"
	if restored.Messages()[0].Text() != "hello" {
		t.Fatal("restored messages alias snapshot")
	}
	if got, _ := restored.State().Get("language"); got != "en" {
		t.Fatalf("restored state = %v, want en", got)
	}
	if s.Messages()[0].Text() != "hello" {
		t.Fatal("snapshot messages alias original session")
	}
}

func TestState(t *testing.T) {
	t.Parallel()
	var s State
	var missing string
	if ok, err := s.Decode("missing", &missing); ok || err != nil {
		t.Fatalf("Decode missing = %v, %v", ok, err)
	}
	s.Set("profile", map[string]any{"name": "Ada", "age": 37})
	var profile struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}
	if ok, err := s.Decode("profile", &profile); !ok || err != nil || profile.Name != "Ada" || profile.Age != 37 {
		t.Fatalf("Decode profile = %+v, %v, %v", profile, ok, err)
	}
	copy := s.All()
	delete(copy, "profile")
	if _, ok := s.Get("profile"); !ok {
		t.Fatal("All exposes the state's map")
	}
	s.Set("invalid", make(chan int))
	if ok, err := s.Decode("invalid", &profile); !ok || err == nil {
		t.Fatalf("Decode unserializable = %v, %v", ok, err)
	}
	s.Set("wrong-type", "string")
	if ok, err := s.Decode("wrong-type", &profile); !ok || err == nil {
		t.Fatalf("Decode incompatible type = %v, %v", ok, err)
	}
	s.Delete("profile")
	if _, ok := s.Get("profile"); ok {
		t.Fatal("Delete retained key")
	}
}

func TestStateConcurrentAccess(t *testing.T) {
	t.Parallel()
	var s State
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Go(func() {
			key := fmt.Sprint(i)
			for n := range 25 {
				s.Set("value", i)
				s.Get("value")
				s.Set(key, n)
				if got, ok := s.Get(key); !ok || got != n {
					t.Errorf("Get(%q) = %v, %v; want %d", key, got, ok, n)
				}
				if got := s.All()[key]; got != n {
					t.Errorf("All()[%q] = %v, want %d", key, got, n)
				}
				s.Delete(key)
				if _, ok := s.Get(key); ok {
					t.Errorf("Delete(%q) retained key", key)
				}
			}
		})
	}
	wg.Wait()
	s.Delete("value")
	if got := s.All(); len(got) != 0 {
		t.Fatalf("state after concurrent deletes = %v", got)
	}
}

func TestNewID(t *testing.T) {
	t.Parallel()
	a, b := NewID(), NewID()
	if a == "" || a == b || !idPattern.MatchString(a) {
		t.Fatalf("invalid generated identifiers: %q, %q", a, b)
	}
}

func TestSnapshotPreservesEmptySession(t *testing.T) {
	t.Parallel()
	snap := &Snapshot{ID: "empty", CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(2, 0)}
	s := FromSnapshot(snap)
	s.State().Set("ready", true)
	if got := s.Messages(); len(got) != 0 {
		t.Fatalf("Messages = %#v", got)
	}
	if got := TakeSnapshot(s); !reflect.DeepEqual(got.State, map[string]any{"ready": true}) {
		t.Fatalf("State = %#v", got.State)
	}
}
