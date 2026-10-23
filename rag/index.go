package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nostosx/agentx/memory"
)

// DefaultBatchSize is the default number of chunks embedded per request.
const DefaultBatchSize = 64

// Config configures an Index.
type Config struct {
	// Store stores the chunks. Defaults to a new MemoryStore.
	Store VectorStore
	// Embedder embeds chunks. Without it chunks are stored without
	// embeddings and only keyword retrieval works.
	Embedder memory.Embedder
	// QueryEmbedder embeds queries. Defaults to Embedder. Use it with
	// embedders that distinguish queries from documents; both must produce
	// vectors in the same space.
	QueryEmbedder memory.Embedder
	// Splitter splits documents. Defaults to AutoSplitter{}.
	Splitter Splitter
	// Enricher, if set, runs on the chunks of each document before they
	// are embedded, such as a ContextualEnricher.
	Enricher Enricher
	// BatchSize is the number of chunks embedded per request. Defaults to
	// DefaultBatchSize.
	BatchSize int
	// Version is included in the content hash of every source. Change it
	// after changing the splitter, enricher or embedder so that the next
	// Add or Sync re-indexes sources whose content did not change.
	Version string
	// Retriever replaces the default retriever: Keyword, fused with Vector
	// by Hybrid when an embedder is set.
	Retriever Retriever
	// Reranker, if set, reranks the candidates of the retriever; see
	// Reranked.
	Reranker Reranker
	// Candidates is the number of hits to rerank; see Reranked.
	Candidates int
	// OnError receives retrieval errors that do not fail a query, such as
	// a failing retriever of Hybrid or a failing Reranker. If nil, they are
	// logged with slog.Default.
	OnError func(err error)
}

// Index embeds and stores the chunks of documents and retrieves them.
//
// Documents are indexed by source: all documents with the same source are
// split, embedded and replaced together. Each source is stored with a
// hash of its documents and Config.Version, so Add and Sync skip sources
// whose content did not change. Adding, syncing and deleting are
// serialized; retrieval may run concurrently with them.
type Index struct {
	cfg       Config
	store     VectorStore
	splitter  Splitter
	retriever Retriever
	mu        sync.Mutex
}

// NewIndex returns an Index.
func NewIndex(cfg Config) *Index {
	ix := &Index{cfg: cfg, store: cfg.Store, splitter: cfg.Splitter}
	if ix.store == nil {
		ix.store = NewMemoryStore()
	}
	if ix.splitter == nil {
		ix.splitter = &AutoSplitter{}
	}
	if ix.cfg.BatchSize <= 0 {
		ix.cfg.BatchSize = DefaultBatchSize
	}
	r := cfg.Retriever
	if r == nil {
		r = &Keyword{Store: ix.store}
		if cfg.Embedder != nil {
			query := cfg.QueryEmbedder
			if query == nil {
				query = cfg.Embedder
			}
			r = &Hybrid{Retrievers: []Retriever{r, &Vector{Store: ix.store, Embedder: query}}, OnError: cfg.OnError}
		}
	}
	if cfg.Reranker != nil {
		r = &Reranked{Retriever: r, Reranker: cfg.Reranker, Candidates: cfg.Candidates, OnError: cfg.OnError}
	}
	ix.retriever = r
	return ix
}

// Store returns the index's store.
func (ix *Index) Store() VectorStore { return ix.store }

// Retrieve implements Retriever with the configured retriever.
func (ix *Index) Retrieve(ctx context.Context, q Query) ([]Hit, error) {
	return ix.retriever.Retrieve(ctx, q)
}

// Search retrieves up to limit chunks relevant to query.
func (ix *Index) Search(ctx context.Context, query string, limit int) ([]Hit, error) {
	return ix.Retrieve(ctx, Query{Text: query, Limit: limit})
}

// Result reports the sources changed by Add or Sync.
type Result struct {
	// Added lists new sources, Updated sources whose content changed,
	// Unchanged sources that were skipped and Deleted removed sources.
	Added, Updated, Unchanged, Deleted []string
	// Chunks is the number of chunks stored.
	Chunks int
}

// Add indexes documents, replacing the chunks of their sources. Sources
// whose content did not change are skipped. On error, the sources stored
// before the error are kept and reported in the result.
func (ix *Index) Add(ctx context.Context, docs ...*Document) (*Result, error) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.add(ctx, docs)
}

// SyncOption configures Sync.
type SyncOption func(*syncConfig)

type syncConfig struct{ prefix string }

// SyncPrefix restricts the deletions of Sync to sources starting with
// prefix, so an index can hold sources from several loaders, for example
// an FSLoader with a SourcePrefix.
func SyncPrefix(prefix string) SyncOption { return func(c *syncConfig) { c.prefix = prefix } }

// Sync makes the index match docs: it adds new sources, re-indexes sources
// whose content changed and deletes indexed sources that are not among
// docs. Unchanged sources are skipped. Deletions happen after all
// additions succeeded.
func (ix *Index) Sync(ctx context.Context, docs []*Document, opts ...SyncOption) (*Result, error) {
	var cfg syncConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	res, err := ix.add(ctx, docs)
	if err != nil {
		return res, err
	}
	keep := map[string]bool{}
	for _, d := range docs {
		keep[d.source()] = true
	}
	infos, err := ix.store.Sources(ctx)
	if err != nil {
		return res, err
	}
	var del []string
	for _, info := range infos {
		if !keep[info.Source] && strings.HasPrefix(info.Source, cfg.prefix) {
			del = append(del, info.Source)
		}
	}
	if len(del) > 0 {
		if err := ix.store.Delete(ctx, del...); err != nil {
			return res, err
		}
		res.Deleted = del
	}
	return res, nil
}

// Delete removes sources from the index.
func (ix *Index) Delete(ctx context.Context, sources ...string) error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.store.Delete(ctx, sources...)
}

// sourceDocs are the documents of one source.
type sourceDocs struct {
	source string
	hash   string
	docs   []*Document
	exists bool
}

func (ix *Index) add(ctx context.Context, docs []*Document) (*Result, error) {
	groups, err := groupDocs(docs)
	if err != nil {
		return nil, err
	}
	infos, err := ix.store.Sources(ctx)
	if err != nil {
		return nil, err
	}
	stored := make(map[string]string, len(infos))
	for _, info := range infos {
		stored[info.Source] = info.Hash
	}
	res := &Result{}
	var pending []*sourceDocs
	for _, g := range groups {
		g.hash = ix.hash(g.docs)
		old, exists := stored[g.source]
		if exists && old == g.hash {
			res.Unchanged = append(res.Unchanged, g.source)
			continue
		}
		g.exists = exists
		pending = append(pending, g)
	}
	// Split, enrich and embed sources in order, storing them in groups of
	// about one embedding batch so a failure keeps the work done before it.
	var batch []SourceChunks
	var batchDocs []*sourceDocs
	var texts []*Chunk
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := ix.embed(ctx, texts); err != nil {
			return err
		}
		if err := ix.store.Replace(ctx, batch...); err != nil {
			return err
		}
		for i, g := range batchDocs {
			if g.exists {
				res.Updated = append(res.Updated, g.source)
			} else {
				res.Added = append(res.Added, g.source)
			}
			res.Chunks += len(batch[i].Chunks)
		}
		batch, batchDocs, texts = nil, nil, nil
		return nil
	}
	for _, g := range pending {
		chunks, err := ix.chunk(ctx, g.docs)
		if err != nil {
			return res, err
		}
		batch = append(batch, SourceChunks{
			Info:   SourceInfo{Source: g.source, Hash: g.hash, Chunks: len(chunks), IndexedAt: time.Now().UTC()},
			Chunks: chunks,
		})
		batchDocs = append(batchDocs, g)
		texts = append(texts, chunks...)
		if len(texts) >= ix.cfg.BatchSize {
			if err := flush(); err != nil {
				return res, err
			}
		}
	}
	if err := flush(); err != nil {
		return res, err
	}
	return res, nil
}

// groupDocs validates documents and groups them by source in order of
// first appearance.
func groupDocs(docs []*Document) ([]*sourceDocs, error) {
	var groups []*sourceDocs
	bySource := map[string]*sourceDocs{}
	ids := map[[2]string]bool{}
	for _, d := range docs {
		if d == nil {
			return nil, errors.New("rag: nil document")
		}
		src, id := d.source(), d.id()
		if src == "" {
			return nil, errors.New("rag: document has neither an ID nor a source")
		}
		if ids[[2]string{src, id}] {
			return nil, fmt.Errorf("rag: duplicate document %s in source %s", id, src)
		}
		ids[[2]string{src, id}] = true
		g := bySource[src]
		if g == nil {
			g = &sourceDocs{source: src}
			bySource[src] = g
			groups = append(groups, g)
		}
		g.docs = append(g.docs, d)
	}
	return groups, nil
}

// hash identifies the content of a source: the index version and the ID,
// content and metadata of each document, in ID order.
func (ix *Index) hash(docs []*Document) string {
	sorted := slices.Clone(docs)
	slices.SortFunc(sorted, func(a, b *Document) int { return strings.Compare(a.id(), b.id()) })
	h := sha256.New()
	write := func(s string) {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	write(ix.cfg.Version)
	for _, d := range sorted {
		write(d.id())
		write(d.Content)
		meta, err := json.Marshal(d.Metadata)
		if err != nil {
			meta = fmt.Append(nil, d.Metadata)
		}
		write(string(meta))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// chunk splits and enriches the documents of a source.
func (ix *Index) chunk(ctx context.Context, docs []*Document) ([]*Chunk, error) {
	var all []*Chunk
	for _, d := range docs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chunks, err := ix.splitter.Split(ctx, d)
		if err != nil {
			return nil, fmt.Errorf("rag: split %s: %w", d.id(), err)
		}
		if ix.cfg.Enricher != nil && len(chunks) > 0 {
			if err := ix.cfg.Enricher.Enrich(ctx, d, chunks); err != nil {
				return nil, err
			}
		}
		all = append(all, chunks...)
	}
	return all, nil
}

// embed sets the embeddings of chunks in batches.
func (ix *Index) embed(ctx context.Context, chunks []*Chunk) error {
	if ix.cfg.Embedder == nil {
		return nil
	}
	for batch := range slices.Chunk(chunks, ix.cfg.BatchSize) {
		texts := make([]string, len(batch))
		for i, c := range batch {
			texts[i] = c.Text()
		}
		vecs, err := ix.cfg.Embedder.Embed(ctx, texts)
		if err != nil {
			return fmt.Errorf("rag: embed chunks: %w", err)
		}
		if len(vecs) != len(batch) {
			return fmt.Errorf("rag: embedder returned %d vectors for %d chunks", len(vecs), len(batch))
		}
		for i, c := range batch {
			c.Embedding = vecs[i]
		}
	}
	return nil
}

var _ Retriever = (*Index)(nil)
