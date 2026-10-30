// Package artifacttest tests implementations of artifact.Store.
package artifacttest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/nostosx/agentx/artifact"
)

// Run exercises persistence, scope isolation, replacement, copies and cancellation.
// Each factory call must return an empty, independently owned store.
func Run(t *testing.T, factory func(*testing.T) artifact.Store) {
	t.Helper()
	t.Run("contract", func(t *testing.T) {
		s := factory(t)
		a := &artifact.Artifact{Scope: "tenant/A", ID: "../same", Name: "image.png", MediaType: "image/png", Data: []byte("first"), Metadata: map[string]string{"source": "model"}}
		if err := s.Put(t.Context(), a); err != nil {
			t.Fatal(err)
		}
		a.Data[0], a.Metadata["source"] = 'X', "changed"
		got, err := s.Get(t.Context(), a.Key)
		if err != nil || string(got.Data) != "first" || got.Metadata["source"] != "model" || got.Name != "image.png" {
			t.Fatalf("Get = %+v, %v", got, err)
		}
		got.Data[0], got.Metadata["source"] = 'Y', "changed"
		again, err := s.Get(t.Context(), a.Key)
		if err != nil || string(again.Data) != "first" || again.Metadata["source"] != "model" {
			t.Fatal("mutable alias returned")
		}
		other := *again
		other.Scope, other.Data = "tenant/a", []byte("other")
		if err := s.Put(t.Context(), &other); err != nil {
			t.Fatal(err)
		}
		again.Data = []byte("replacement")
		if err := s.Put(t.Context(), again); err != nil {
			t.Fatal(err)
		}
		got, err = s.Get(t.Context(), other.Key)
		if err != nil || string(got.Data) != "other" {
			t.Fatalf("scope leaked: %+v, %v", got, err)
		}
		got, err = s.Get(t.Context(), a.Key)
		if err != nil || string(got.Data) != "replacement" {
			t.Fatal("replacement not persisted")
		}
		if err := s.Delete(t.Context(), a.Key); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(t.Context(), a.Key); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Get(t.Context(), a.Key); !errors.Is(err, artifact.ErrNotFound) {
			t.Fatalf("missing = %v", err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := s.Put(ctx, &other); !errors.Is(err, context.Canceled) {
			t.Fatalf("Put cancelled = %v", err)
		}
		if _, err := s.Get(ctx, other.Key); !errors.Is(err, context.Canceled) {
			t.Fatalf("Get cancelled = %v", err)
		}
		if err := s.Delete(ctx, other.Key); !errors.Is(err, context.Canceled) {
			t.Fatalf("Delete cancelled = %v", err)
		}
		if err := s.Put(t.Context(), nil); err == nil {
			t.Fatal("nil artifact accepted")
		}
		if _, err := s.Get(t.Context(), artifact.Key{}); err == nil {
			t.Fatal("empty key accepted")
		}
	})
	t.Run("concurrent replacement", func(t *testing.T) {
		s := factory(t)
		key := artifact.Key{Scope: "concurrent", ID: "same"}
		if err := s.Put(t.Context(), &artifact.Artifact{Key: key, MediaType: "text/plain", Data: []byte("initial"), Metadata: map[string]string{"value": "initial"}}); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for _, value := range []string{"one", "two", "three", "four"} {
			wg.Go(func() {
				for range 5 {
					if err := s.Put(t.Context(), &artifact.Artifact{Key: key, MediaType: "text/plain", Data: []byte(value), Metadata: map[string]string{"value": value}}); err != nil {
						t.Error(err)
						return
					}
					got, err := s.Get(t.Context(), key)
					if err != nil {
						t.Error(err)
						return
					}
					if string(got.Data) != got.Metadata["value"] {
						t.Error("partially replaced artifact")
					}
				}
			})
		}
		wg.Wait()
	})
}
