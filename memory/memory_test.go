package memory_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nostosx/agentx/memory"
)

func TestManagerRememberRecallForget(t *testing.T) {
	t.Parallel()
	s := memory.NewMemoryStore()
	m := memory.New(s)
	if m.Store() != s {
		t.Error("Store() did not return configured store")
	}
	if _, err := m.Remember(t.Context(), " \n"); err == nil {
		t.Error("blank memory accepted")
	}
	e, err := m.Remember(t.Context(), "  Go agent design  ", memory.WithID("fact"), memory.WithScope("project"), memory.WithTags("go", "agent"), memory.WithMetadata("source", "test"))
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "fact" || e.Content != "Go agent design" || e.Scope != "project" || e.Metadata["source"] != "test" || !reflect.DeepEqual(e.Tags, []string{"go", "agent"}) || e.CreatedAt.IsZero() || !e.CreatedAt.Equal(e.UpdatedAt) {
		t.Fatalf("Remember() = %+v", e)
	}
	oldTime := time.Unix(1, 0).UTC()
	e.CreatedAt = oldTime
	if err := s.Put(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	replacement, err := m.Remember(t.Context(), "Go tools", memory.WithID("fact"), memory.WithScope("project"), memory.WithTags("go"))
	if err != nil || !replacement.CreatedAt.Equal(oldTime) || !replacement.UpdatedAt.After(oldTime) {
		t.Fatalf("replacement = %+v, %v", replacement, err)
	}
	hits, err := m.Recall(t.Context(), "Go", memory.InScope("project"), memory.Tagged("go"), memory.Limit(1), memory.MinScore(0.01))
	if err != nil || len(hits) != 1 || hits[0].Entry.ID != "fact" {
		t.Fatalf("Recall() = %v, %v", hits, err)
	}
	generated, err := m.Remember(t.Context(), "Generated identifier")
	if err != nil || !strings.HasPrefix(generated.ID, "mem_") {
		t.Errorf("generated memory = %+v, %v", generated, err)
	}
	if err := m.Forget(t.Context(), "fact"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(t.Context(), "fact"); !errors.Is(err, memory.ErrNotFound) {
		t.Errorf("Forget() did not remove entry: %v", err)
	}
}

type putFailStore struct {
	memory.Store
	err error
}

func (s putFailStore) Put(context.Context, *memory.Entry) error { return s.err }

func TestManagerEmbeddingAndFailures(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("backend failed")
	s := memory.NewMemoryStore()
	m := memory.New(s, memory.WithEmbedder(embedFunc(func(_ context.Context, texts []string) ([][]float32, error) { return [][]float32{{1, 0}}, nil })))
	e, err := m.Remember(t.Context(), "fact", memory.WithID("fact"))
	if err != nil || !reflect.DeepEqual(e.Embedding, []float32{1, 0}) {
		t.Fatalf("embedded memory = %+v, %v", e, err)
	}
	if hits, err := m.Recall(t.Context(), "unrelated keyword"); err != nil || len(hits) != 1 || hits[0].Entry.ID != "fact" {
		t.Errorf("default hybrid retrieval = %+v, %v", hits, err)
	}
	m = memory.New(s, memory.WithEmbedder(embedFunc(func(context.Context, []string) ([][]float32, error) { return nil, wantErr })))
	if _, err := m.Remember(t.Context(), "must not persist", memory.WithID("failed")); !errors.Is(err, wantErr) {
		t.Errorf("embedding error = %v", err)
	}
	if _, err := s.Get(t.Context(), "failed"); !errors.Is(err, memory.ErrNotFound) {
		t.Errorf("failed embedding persisted entry: %v", err)
	}
	m = memory.New(putFailStore{Store: s, err: wantErr})
	if _, err := m.Remember(t.Context(), "fact"); !errors.Is(err, wantErr) {
		t.Errorf("store error = %v", err)
	}
}

func TestManagerForgetInScope(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, scope string
		allowed     bool
	}{
		{"matching scope", "project", true},
		{"other scope", "other", false},
		{"unrestricted", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := putEntries(t, &memory.Entry{ID: "fact", Scope: "project", Content: "fact"})
			m := memory.New(s)
			err := m.ForgetInScope(t.Context(), "fact", tt.scope)
			if tt.allowed {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.Get(t.Context(), "fact"); !errors.Is(err, memory.ErrNotFound) {
					t.Errorf("allowed delete did not remove entry: %v", err)
				}
			} else {
				if !errors.Is(err, memory.ErrNotFound) {
					t.Errorf("denied delete = %v, want ErrNotFound", err)
				}
				if _, err := s.Get(t.Context(), "fact"); err != nil {
					t.Errorf("denied delete removed entry: %v", err)
				}
			}
		})
	}
	m := memory.New(memory.NewMemoryStore())
	if err := m.ForgetInScope(t.Context(), "missing", "project"); !errors.Is(err, memory.ErrNotFound) {
		t.Errorf("missing entry error = %v", err)
	}
}

// storeWithoutScopedDelete exposes the original Store contract only.
type storeWithoutScopedDelete struct{ memory.Store }

func TestManagerScopedForgetRequiresConditionalStoreDelete(t *testing.T) {
	t.Parallel()
	s := putEntries(t, &memory.Entry{ID: "fact", Scope: "project", Content: "fact"})
	m := memory.New(storeWithoutScopedDelete{Store: s})
	if err := m.ForgetInScope(t.Context(), "fact", "project"); err == nil {
		t.Error("Store without conditional delete must reject scoped forget")
	}
	if _, err := s.Get(t.Context(), "fact"); err != nil {
		t.Errorf("unsupported scoped delete changed store: %v", err)
	}
	if err := m.ForgetInScope(t.Context(), "fact", ""); err != nil {
		t.Errorf("unrestricted forget must support original Store contract: %v", err)
	}
	if _, err := s.Get(t.Context(), "fact"); !errors.Is(err, memory.ErrNotFound) {
		t.Errorf("unrestricted forget did not delete entry: %v", err)
	}
}

func TestRecallInOptions(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, scope      string
		limit, wantLimit int
	}{
		{"default", "", 0, 5},
		{"scoped limit", "project", 2, 2},
		{"negative limit defaults", "project", -1, 5},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := memory.New(memory.NewMemoryStore(), memory.WithRetriever(retrieveFunc(func(_ context.Context, q memory.Query) ([]memory.Hit, error) {
				if q.Text != "query" || q.Filter.Scope != tt.scope || q.Limit != tt.wantLimit {
					t.Errorf("query = %+v", q)
				}
				return nil, nil
			})))
			if _, err := memory.RecallIn(t.Context(), m, tt.scope, "query", tt.limit); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFormat(t *testing.T) {
	t.Parallel()
	hits := []memory.Hit{{Entry: &memory.Entry{ID: "a", Content: "first", Tags: []string{"go", "agent"}}}, {Entry: &memory.Entry{ID: "b", Content: "second"}}}
	if got, want := memory.Format(hits), "- [a] first (tags: go, agent)\n- [b] second"; got != want {
		t.Errorf("Format() = %q, want %q", got, want)
	}
	if got := memory.Format(nil); got != "" {
		t.Errorf("Format(nil) = %q", got)
	}
}

func TestTools(t *testing.T) {
	t.Parallel()
	s := memory.NewMemoryStore()
	m := memory.New(s)
	tools := memory.Tools(m, "project")
	if len(tools) != 3 || tools[0].Info().Name != "memory_remember" || tools[1].Info().Name != "memory_recall" || tools[2].Info().Name != "memory_forget" {
		t.Fatalf("Tools() = %v", tools)
	}
	if tools[0].Info().ReadOnly || !tools[1].Info().ReadOnly || tools[2].Info().ReadOnly {
		t.Error("incorrect read-only flags")
	}
	result, err := tools[0].Call(t.Context(), json.RawMessage(`{"content":"Go project fact","tags":["go"]}`))
	if err != nil || result.IsError || !strings.HasPrefix(result.Content, "Remembered as mem_") {
		t.Fatalf("remember tool = %+v, %v", result, err)
	}
	entries, _ := s.List(t.Context(), memory.Filter{Scope: "project"})
	if len(entries) != 1 || !reflect.DeepEqual(entries[0].Tags, []string{"go"}) {
		t.Fatalf("remembered entries = %+v", entries)
	}
	if _, err := m.Remember(t.Context(), "Go other secret", memory.WithID("other"), memory.WithScope("other")); err != nil {
		t.Fatal(err)
	}
	result, err = tools[1].Call(t.Context(), json.RawMessage(`{"query":"Go","limit":3}`))
	if err != nil || !strings.Contains(result.Content, "Go project fact") || strings.Contains(result.Content, "other secret") {
		t.Fatalf("scoped recall tool = %+v, %v", result, err)
	}
	args, _ := json.Marshal(map[string]string{"id": entries[0].ID})
	result, err = tools[2].Call(t.Context(), args)
	if err != nil || result.Content != "Forgot "+entries[0].ID {
		t.Fatalf("forget tool = %+v, %v", result, err)
	}
	result, err = tools[1].Call(t.Context(), json.RawMessage(`{"query":"Go"}`))
	if err != nil || result.Content != "No relevant memories found." {
		t.Fatalf("empty recall = %+v, %v", result, err)
	}
}

func TestToolsForgetCannotDeleteAnotherScope(t *testing.T) {
	t.Parallel()
	s := putEntries(t, &memory.Entry{ID: "other-fact", Content: "other project's memory", Scope: "other"})
	tools := memory.Tools(memory.New(s), "project")
	result, err := tools[2].Call(t.Context(), json.RawMessage(`{"id":"other-fact"}`))
	if err == nil && (result == nil || !result.IsError) {
		t.Errorf("scoped forget must reject another scope: result=%+v, error=%v", result, err)
	}
	if _, err := s.Get(t.Context(), "other-fact"); err != nil {
		t.Errorf("scoped forget deleted another project's entry: %v", err)
	}
}

// memoryWithoutScopedDelete deliberately exposes only the original Memory API.
type memoryWithoutScopedDelete struct{ memory.Memory }

func TestToolsScopedForgetRequiresScopedDeleteCapability(t *testing.T) {
	t.Parallel()
	s := putEntries(t, &memory.Entry{ID: "fact", Scope: "project", Content: "fact"})
	m := memoryWithoutScopedDelete{Memory: memory.New(s)}
	scoped := memory.Tools(m, "project")
	result, err := scoped[2].Call(t.Context(), json.RawMessage(`{"id":"fact"}`))
	if err == nil && (result == nil || !result.IsError) {
		t.Errorf("scoped forget without capability must fail closed: result=%+v, error=%v", result, err)
	}
	if _, err := s.Get(t.Context(), "fact"); err != nil {
		t.Errorf("unsupported scoped delete removed entry: %v", err)
	}
	unscoped := memory.Tools(m, "")
	result, err = unscoped[2].Call(t.Context(), json.RawMessage(`{"id":"fact"}`))
	if err != nil || result == nil || result.Content != "Forgot fact" {
		t.Errorf("ordinary forget for custom Memory = %+v, %v", result, err)
	}
}
