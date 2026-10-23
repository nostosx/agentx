// Package memorytest checks memory.Store implementations against the
// contract the built-in stores follow.
package memorytest

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nostosx/agentx/memory"
)

// Run runs the conformance tests against stores made by newStore, which
// must return an empty store independent of the others; it is called once
// per subtest and subtests run in parallel. The contract:
//
//   - Put rejects an entry without an ID and otherwise inserts or replaces
//     the entry with its ID; Get and Delete of a missing entry return
//     memory.ErrNotFound.
//   - Every field round-trips: embeddings bit for bit, timestamps to the
//     nanosecond; later changes to entries put or returned are not stored.
//   - List returns the entries matching the filter (exact scope, all
//     tags), oldest CreatedAt first, then by ID.
//   - IDs and scopes are arbitrary UTF-8 strings of up to 255 bytes,
//     compared exactly: case and trailing spaces matter.
//   - A store that implements memory.ScopedDeleter deletes an entry only
//     when its scope matches, atomically.
//   - Entries of a megabyte are stored, and concurrent use is safe.
func Run(t *testing.T, newStore func(t *testing.T) memory.Store) {
	t.Helper()
	tests := []struct {
		name string
		run  func(*testing.T, memory.Store)
	}{
		{"NotFound", testNotFound},
		{"RoundTrip", testRoundTrip},
		{"Isolation", testIsolation},
		{"Replace", testReplace},
		{"List", testList},
		{"IDs", testIDs},
		{"Scopes", testScopes},
		{"DeleteInScope", testDeleteInScope},
		{"LargePayload", testLargePayload},
		{"Concurrency", testConcurrency},
		{"ConcurrentDelete", testConcurrentDelete},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t, newStore(t))
		})
	}
}

func put(t *testing.T, s memory.Store, e *memory.Entry) {
	t.Helper()
	if err := s.Put(t.Context(), e); err != nil {
		t.Fatalf("Put %q = %v", e.ID, err)
	}
}

func get(t *testing.T, s memory.Store, id string) *memory.Entry {
	t.Helper()
	e, err := s.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("Get %q = %v", id, err)
	}
	return e
}

func list(t *testing.T, s memory.Store, f memory.Filter) []*memory.Entry {
	t.Helper()
	es, err := s.List(t.Context(), f)
	if err != nil {
		t.Fatalf("List %+v = %v", f, err)
	}
	return es
}

func ids(es []*memory.Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.ID
	}
	return out
}

// equal compares entries, treating empty and nil collections alike and
// timestamps by instant.
func equal(a, b *memory.Entry) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.ID != b.ID || a.Content != b.Content || a.Scope != b.Scope ||
		!a.CreatedAt.Equal(b.CreatedAt) || !a.UpdatedAt.Equal(b.UpdatedAt) ||
		!slices.Equal(a.Tags, b.Tags) || !maps.Equal(a.Metadata, b.Metadata) || len(a.Embedding) != len(b.Embedding) {
		return false
	}
	for i, v := range a.Embedding {
		if math.Float32bits(v) != math.Float32bits(b.Embedding[i]) {
			return false
		}
	}
	return true
}

func check(t *testing.T, got, want *memory.Entry) {
	t.Helper()
	if !equal(got, want) {
		t.Fatalf("entry = %+v\nwant %+v", got, want)
	}
}

var stamp = time.Date(2024, 5, 6, 7, 8, 9, 123456789, time.FixedZone("UTC+2", 2*3600))

func fullEntry(id string) *memory.Entry {
	return &memory.Entry{
		ID: id, Content: "The user prefers Go. héllo 世界 🚀 NUL:\x00 quote:\" ' \\",
		Tags: []string{"pref", "lang", "日本"}, Scope: "user:42",
		Metadata:  map[string]string{"source": "chat", "empty": "", "unicode": "✓"},
		Embedding: []float32{0, float32(math.Copysign(0, -1)), 1, -1, 0.1, math.MaxFloat32, math.SmallestNonzeroFloat32, -3.4e-20},
		CreatedAt: stamp, UpdatedAt: stamp.Add(time.Hour),
	}
}

func testNotFound(t *testing.T, s memory.Store) {
	ctx := t.Context()
	if _, err := s.Get(ctx, "missing"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "missing"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("Delete missing = %v, want ErrNotFound", err)
	}
	if err := s.Put(ctx, &memory.Entry{Content: "no ID"}); err == nil {
		t.Fatal("Put accepted an entry without an ID")
	}
	if es := list(t, s, memory.Filter{}); len(es) != 0 {
		t.Fatalf("List of an empty store = %v", ids(es))
	}
	put(t, s, &memory.Entry{ID: "deleted"})
	if err := s.Delete(ctx, "deleted"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "deleted"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("Get deleted = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "deleted"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("second Delete = %v, want ErrNotFound", err)
	}
}

func testRoundTrip(t *testing.T, s memory.Store) {
	e := fullEntry("full")
	put(t, s, e)
	check(t, get(t, s, "full"), e)
	if es := list(t, s, memory.Filter{}); len(es) != 1 {
		t.Fatalf("List = %v", ids(es))
	} else {
		check(t, es[0], e)
	}
	// A minimal entry: zero times, no tags, metadata, scope or embedding.
	minimal := &memory.Entry{ID: "minimal"}
	put(t, s, minimal)
	got := get(t, s, "minimal")
	check(t, got, minimal)
	if len(got.Embedding) != 0 || len(got.Tags) != 0 || len(got.Metadata) != 0 {
		t.Fatalf("minimal entry gained values: %+v", got)
	}
}

func testIsolation(t *testing.T, s memory.Store) {
	e := fullEntry("isolated")
	want := fullEntry("isolated")
	put(t, s, e)
	e.Content, e.Tags[0], e.Metadata["source"], e.Embedding[0] = "mutated", "mutated", "mutated", 42
	got := get(t, s, "isolated")
	check(t, got, want)
	got.Tags[0], got.Metadata["source"], got.Embedding[0] = "reader", "reader", 43
	check(t, get(t, s, "isolated"), want)
	listed := list(t, s, memory.Filter{})
	listed[0].Tags[0], listed[0].Metadata["source"], listed[0].Embedding[0] = "lister", "lister", 44
	check(t, get(t, s, "isolated"), want)
}

func testReplace(t *testing.T, s memory.Store) {
	put(t, s, fullEntry("replaced"))
	next := &memory.Entry{ID: "replaced", Content: "new", Scope: "other", Tags: []string{"new"}, CreatedAt: stamp.Add(-time.Hour), UpdatedAt: stamp}
	put(t, s, next)
	check(t, get(t, s, "replaced"), next)
	if es := list(t, s, memory.Filter{Scope: "user:42"}); len(es) != 0 {
		t.Fatalf("replaced entry still listed in its old scope: %v", ids(es))
	}
	if es := list(t, s, memory.Filter{Tags: []string{"pref"}}); len(es) != 0 {
		t.Fatalf("replaced entry still listed with its old tags: %v", ids(es))
	}
	if es := list(t, s, memory.Filter{Scope: "other", Tags: []string{"new"}}); len(es) != 1 {
		t.Fatalf("replaced entry not listed in its new scope: %v", ids(es))
	}
}

func testList(t *testing.T, s memory.Store) {
	base := time.Unix(1_700_000_000, 0).UTC()
	entries := []*memory.Entry{
		{ID: "b", Scope: "one", Tags: []string{"go", "agent"}, CreatedAt: base},
		{ID: "a", Scope: "one", Tags: []string{"agent", "go", "extra"}, CreatedAt: base},
		{ID: "older", Scope: "two", Tags: []string{"go"}, CreatedAt: base.Add(-time.Hour)},
		{ID: "nanos", Scope: "one", CreatedAt: base.Add(time.Nanosecond)},
		{ID: "zone", Scope: "two", Tags: []string{"agent"}, CreatedAt: base.Add(time.Second).In(time.FixedZone("UTC-5", -5*3600))},
		{ID: "zero", Tags: []string{"go"}},
		{ID: "unscoped", Tags: []string{"agent"}, CreatedAt: base.Add(time.Hour)},
	}
	for _, e := range entries {
		put(t, s, e)
	}
	for _, tt := range []struct {
		filter memory.Filter
		want   []string
	}{
		{memory.Filter{}, []string{"zero", "older", "a", "b", "nanos", "zone", "unscoped"}},
		{memory.Filter{Scope: "one"}, []string{"a", "b", "nanos"}},
		{memory.Filter{Scope: "two"}, []string{"older", "zone"}},
		{memory.Filter{Tags: []string{"go"}}, []string{"zero", "older", "a", "b"}},
		{memory.Filter{Tags: []string{"go", "agent"}}, []string{"a", "b"}},
		{memory.Filter{Scope: "one", Tags: []string{"extra"}}, []string{"a"}},
		{memory.Filter{Scope: "two", Tags: []string{"extra"}}, nil},
		{memory.Filter{Scope: "missing"}, nil},
		{memory.Filter{Tags: []string{"missing"}}, nil},
	} {
		if got := ids(list(t, s, tt.filter)); !slices.Equal(got, tt.want) {
			t.Errorf("List %+v = %v, want %v", tt.filter, got, tt.want)
		}
	}
	for _, e := range list(t, s, memory.Filter{}) {
		for _, want := range entries {
			if want.ID == e.ID {
				check(t, e, want)
			}
		}
	}
}

func testIDs(t *testing.T, s memory.Store) {
	valid := []string{"a", "A", "a ", " a", "mem_ABC", "user/42:fact.1", "日本語", "🚀", "a\tb", strings.Repeat("x", 255), strings.Repeat("é", 127)}
	for i, id := range valid {
		put(t, s, &memory.Entry{ID: id, Content: fmt.Sprint(i)})
	}
	for i, id := range valid {
		if got := get(t, s, id); got.ID != id || got.Content != fmt.Sprint(i) {
			t.Fatalf("Get %q = %+v; IDs must be compared exactly", id, got)
		}
	}
	if got := ids(list(t, s, memory.Filter{})); len(got) != len(valid) {
		t.Fatalf("List = %q, want %d entries", got, len(valid))
	}
	if err := s.Delete(t.Context(), "A"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "a ", " a"} {
		get(t, s, id)
	}
}

func testScopes(t *testing.T, s memory.Store) {
	scopes := []string{"project", "Project", "project ", "日本", strings.Repeat("s", 255)}
	for i, scope := range scopes {
		put(t, s, &memory.Entry{ID: fmt.Sprint(i), Scope: scope})
	}
	put(t, s, &memory.Entry{ID: "unscoped"})
	for i, scope := range scopes {
		if got := ids(list(t, s, memory.Filter{Scope: scope})); !slices.Equal(got, []string{fmt.Sprint(i)}) {
			t.Errorf("List in scope %q = %q; scopes must be compared exactly", scope, got)
		}
		if got := get(t, s, fmt.Sprint(i)); got.Scope != scope {
			t.Errorf("scope = %q, want %q", got.Scope, scope)
		}
	}
}

func testDeleteInScope(t *testing.T, s memory.Store) {
	scoped, ok := s.(memory.ScopedDeleter)
	if !ok {
		t.Log("store does not implement memory.ScopedDeleter")
		return
	}
	ctx := t.Context()
	original := fullEntry("fact")
	for _, tt := range []struct {
		name, id, scope string
		allowed         bool
	}{
		{"other scope", "fact", "other", false},
		{"scope differing in case", "fact", "USER:42", false},
		{"missing ID", "missing", "user:42", false},
		{"missing ID without scope", "missing", "", false},
		{"matching scope", "fact", "user:42", true},
		{"no scope", "fact", "", true},
	} {
		put(t, s, original)
		err := scoped.DeleteInScope(ctx, tt.id, tt.scope)
		if tt.allowed {
			if err != nil {
				t.Fatalf("%s: DeleteInScope = %v", tt.name, err)
			}
			if _, err := s.Get(ctx, "fact"); !errors.Is(err, memory.ErrNotFound) {
				t.Fatalf("%s: entry not deleted: %v", tt.name, err)
			}
			continue
		}
		if !errors.Is(err, memory.ErrNotFound) {
			t.Fatalf("%s: DeleteInScope = %v, want ErrNotFound", tt.name, err)
		}
		check(t, get(t, s, "fact"), original)
	}
}

func testLargePayload(t *testing.T, s memory.Store) {
	e := &memory.Entry{ID: "large", Content: strings.Repeat("large memory ✓ ", 1<<16), Metadata: map[string]string{}} // 1 MiB
	e.Embedding = make([]float32, 4096)
	for i := range e.Embedding {
		e.Embedding[i] = float32(math.Sin(float64(i))) * 1e3
	}
	for i := range 200 {
		e.Tags = append(e.Tags, fmt.Sprintf("tag-%d", i))
		e.Metadata[fmt.Sprintf("key-%d", i)] = strings.Repeat("v", 100)
	}
	put(t, s, e)
	check(t, get(t, s, "large"), e)
}

func testConcurrency(t *testing.T, s memory.Store) {
	const workers, rounds = 8, 10
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			own := fmt.Sprintf("own-%d", w)
			for r := range rounds {
				// Every field of a shared entry carries the same number, so a
				// mixed entry is detected.
				n := fmt.Sprint(w*rounds + r)
				shared := &memory.Entry{ID: "shared", Content: n, Scope: n, Tags: []string{n}, Metadata: map[string]string{"n": n}}
				for _, e := range []*memory.Entry{shared, {ID: own, Content: n, Scope: "own"}} {
					if err := s.Put(t.Context(), e); err != nil {
						t.Error(err)
						return
					}
				}
				got, err := s.Get(t.Context(), "shared")
				if err != nil {
					t.Error(err)
					return
				}
				if got.Scope != got.Content || !reflect.DeepEqual(got.Tags, []string{got.Content}) || got.Metadata["n"] != got.Content {
					t.Errorf("Get returned a mixed entry: %+v", got)
					return
				}
				if e, err := s.Get(t.Context(), own); err != nil || e.Content != n {
					t.Errorf("Get %s = %+v, %v; want %s", own, e, err, n)
					return
				}
				if _, err := s.List(t.Context(), memory.Filter{Scope: "own"}); err != nil {
					t.Error(err)
					return
				}
				if r%3 == 0 {
					if err := s.Delete(t.Context(), own); err != nil {
						t.Error(err)
						return
					}
				}
			}
		})
	}
	wg.Wait()
}

func testConcurrentDelete(t *testing.T, s memory.Store) {
	const workers = 8
	scoped, isScoped := s.(memory.ScopedDeleter)
	for round := range 5 {
		put(t, s, &memory.Entry{ID: "contended", Scope: "project"})
		errs := make([]error, workers)
		var wg sync.WaitGroup
		for w := range workers {
			wg.Go(func() {
				if isScoped && w%2 == 0 {
					errs[w] = scoped.DeleteInScope(t.Context(), "contended", "project")
				} else {
					errs[w] = s.Delete(t.Context(), "contended")
				}
			})
		}
		wg.Wait()
		deleted := 0
		for _, err := range errs {
			switch {
			case err == nil:
				deleted++
			case !errors.Is(err, memory.ErrNotFound):
				t.Fatalf("round %d: Delete = %v", round, err)
			}
		}
		if deleted != 1 {
			t.Fatalf("round %d: %d concurrent deletions succeeded, want 1", round, deleted)
		}
	}
}
