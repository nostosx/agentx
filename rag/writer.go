package rag

import (
	"context"
	"errors"
)

// SourceWriter persists an index by source, without promising atomic replacement.
// A failed Write or Delete may have changed the failing source. Implementations
// invalidate its recorded hash before changing chunks, so Add/Sync can retry
// incomplete work. Successful writes record the supplied source hash. Reads may
// observe an incomplete replacement. Applications serialize independent writers
// to the same namespace. VectorStore retains its stronger atomic contract.
type SourceWriter interface {
	Write(ctx context.Context, sources ...SourceChunks) error
	Delete(ctx context.Context, sources ...string) error
	Sources(ctx context.Context) ([]SourceInfo, error)
}

type atomicSourceWriter struct{ VectorStore }

func (w atomicSourceWriter) Write(ctx context.Context, sources ...SourceChunks) error {
	return w.Replace(ctx, sources...)
}

type invalidSourceWriter struct{}

func (invalidSourceWriter) Write(context.Context, ...SourceChunks) error {
	return errors.New("rag: configure Store or Writer, not both")
}

func (invalidSourceWriter) Delete(context.Context, ...string) error {
	return errors.New("rag: configure Store or Writer, not both")
}

func (invalidSourceWriter) Sources(context.Context) ([]SourceInfo, error) {
	return nil, errors.New("rag: configure Store or Writer, not both")
}

// Changes is a complete delta from a ChangeLoader. Cursor is committed by the
// application only after every document and deletion has been applied. A delta
// is not a full snapshot and must not be passed to Index.Sync.
type Changes struct {
	Documents      []*Document `json:"documents"`
	DeletedSources []string    `json:"deleted_sources"`
	Cursor         string      `json:"cursor"`
}

// ChangeLoader reads an authoritative delta since an opaque application-held
// cursor. Permission failures and incomplete listings fail the whole call;
// they must never be reported as deletions. An empty cursor starts a snapshot.
type ChangeLoader interface {
	LoadChanges(ctx context.Context, cursor string) (*Changes, error)
}

// ApplyChanges applies additions before explicit deletions. On failure callers
// retain the old cursor and may retry the delta. It never infers deletions from
// documents missing from a partial listing.
func (ix *Index) ApplyChanges(ctx context.Context, changes *Changes) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if changes == nil {
		return &Result{}, nil
	}
	deleted := make(map[string]bool, len(changes.DeletedSources))
	for _, source := range changes.DeletedSources {
		if source == "" {
			return nil, errors.New("rag: empty deleted source")
		}
		deleted[source] = true
	}
	for _, doc := range changes.Documents {
		if doc != nil && deleted[doc.source()] {
			return nil, errors.New("rag: source occurs in both changed and deleted documents")
		}
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	res, err := ix.add(ctx, changes.Documents)
	if err != nil {
		return res, err
	}
	if err := ix.writer.Delete(ctx, changes.DeletedSources...); err != nil {
		return res, err
	}
	res.Deleted = append([]string(nil), changes.DeletedSources...)
	return res, nil
}
