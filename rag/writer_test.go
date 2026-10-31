package rag_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/nostosx/agentx/rag"
)

type partialWriter struct {
	store  *rag.MemoryStore
	fail   string
	writes int
}

func (w *partialWriter) Sources(ctx context.Context) ([]rag.SourceInfo, error) {
	return w.store.Sources(ctx)
}
func (w *partialWriter) Delete(ctx context.Context, s ...string) error {
	return w.store.Delete(ctx, s...)
}
func (w *partialWriter) Write(ctx context.Context, s ...rag.SourceChunks) error {
	for _, src := range s {
		w.writes++
		if src.Info.Source == w.fail {
			if err := w.store.Replace(ctx, rag.SourceChunks{Info: rag.SourceInfo{Source: src.Info.Source}}); err != nil {
				return err
			}
			return errors.New("injected write failure")
		}
		if err := w.store.Replace(ctx, src); err != nil {
			return err
		}
	}
	return nil
}

func TestExternalWriterProgressAndDelta(t *testing.T) {
	ctx := t.Context()
	w := &partialWriter{store: rag.NewMemoryStore(), fail: "b"}
	ix := rag.NewIndex(rag.Config{Writer: w})
	a := &rag.Document{ID: "a", Content: "alpha"}
	b := &rag.Document{ID: "b", Content: "beta"}
	res, err := ix.Add(ctx, a, b)
	if err == nil || !reflect.DeepEqual(res.Added, []string{"a"}) {
		t.Fatalf("result=%+v error=%v", res, err)
	}
	w.fail = ""
	res, err = ix.Add(ctx, a, b)
	if err != nil || !reflect.DeepEqual(res.Unchanged, []string{"a"}) || !reflect.DeepEqual(res.Updated, []string{"b"}) {
		t.Fatalf("retry=%+v error=%v", res, err)
	}
	if ix.Store() != nil {
		t.Fatal("external writer exposed a phantom memory store")
	}
	if _, err := ix.Search(ctx, "alpha", 2); err == nil {
		t.Fatal("ingestion-only index silently queried a memory store")
	}
	if _, err := ix.ApplyChanges(ctx, &rag.Changes{Documents: []*rag.Document{{ID: "c", Content: "gamma"}}}); err != nil {
		t.Fatal(err)
	}
	infos, err := w.Sources(ctx)
	if err != nil || len(infos) != 3 {
		t.Fatalf("partial delta deleted other sources: %v %v", infos, err)
	}
	w.fail = "d"
	if _, err := ix.ApplyChanges(ctx, &rag.Changes{Documents: []*rag.Document{{ID: "d", Content: "delta"}}, DeletedSources: []string{"a"}}); err == nil {
		t.Fatal("write failure accepted")
	}
	infos, _ = w.Sources(ctx)
	found := false
	for _, info := range infos {
		found = found || info.Source == "a"
	}
	if !found {
		t.Fatal("deletions ran after a failed addition")
	}
	w.fail = ""
	if _, err := ix.ApplyChanges(ctx, &rag.Changes{DeletedSources: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	infos, _ = w.Sources(ctx)
	for _, info := range infos {
		if info.Source == "a" {
			t.Fatal("explicit deletion ignored")
		}
	}
}

func TestChangesRejectConflictBeforeMutation(t *testing.T) {
	w := &partialWriter{store: rag.NewMemoryStore()}
	ix := rag.NewIndex(rag.Config{Writer: w})
	if _, err := ix.ApplyChanges(t.Context(), &rag.Changes{Documents: []*rag.Document{{ID: "a", Content: "x"}}, DeletedSources: []string{"a"}}); err == nil || w.writes != 0 {
		t.Fatal("conflicting delta mutated the index")
	}
	ix = rag.NewIndex(rag.Config{Store: w.store, Writer: w})
	if _, err := ix.Add(t.Context(), &rag.Document{ID: "x", Content: "x"}); err == nil {
		t.Fatal("ambiguous backends accepted")
	}
}
