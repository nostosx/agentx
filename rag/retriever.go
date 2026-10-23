package rag

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"

	"github.com/nostosx/agentx/memory"
)

// Query is a retrieval request.
type Query struct {
	Text string
	// Limit is the maximum number of hits; non-positive means no limit.
	Limit  int
	Filter Filter
	// MinScore drops hits scoring below it. Scores are retriever specific.
	MinScore float64
}

// Retriever finds chunks relevant to a query.
type Retriever interface {
	Retrieve(ctx context.Context, q Query) ([]Hit, error)
}

// RetrieverFunc adapts a function to Retriever.
type RetrieverFunc func(ctx context.Context, q Query) ([]Hit, error)

// Retrieve calls f.
func (f RetrieverFunc) Retrieve(ctx context.Context, q Query) ([]Hit, error) { return f(ctx, q) }

// Vector ranks chunks by the cosine similarity of their embeddings to the
// query's, as computed by the store. Hits with a non-positive similarity
// are dropped.
type Vector struct {
	Store VectorStore
	// Embedder embeds the query. It must produce vectors in the same space
	// as the embedder of the index.
	Embedder memory.Embedder
}

// Retrieve implements Retriever.
func (v *Vector) Retrieve(ctx context.Context, q Query) ([]Hit, error) {
	if v.Embedder == nil {
		return nil, errors.New("rag: vector retriever requires an embedder")
	}
	if strings.TrimSpace(q.Text) == "" {
		return nil, nil
	}
	vecs, err := v.Embedder.Embed(ctx, []string{q.Text})
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 {
		return nil, errors.New("rag: embedder returned an unexpected number of vectors")
	}
	hits, err := v.Store.Search(ctx, vecs[0], q.Limit, q.Filter)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(hits, func(h Hit) bool { return h.Score <= 0 || h.Score < q.MinScore }), nil
}

// Keyword ranks chunks with BM25 over Chunk.Text, tokenized like memory
// (words, and unigrams and bigrams of CJK characters).
//
// When the store implements Revisioner, Keyword caches its inverted index
// until the store changes, and term statistics then come from all chunks
// rather than only those matching the filter. Otherwise it lists and
// indexes the matching chunks for every query. Neither Store nor the
// Keyword may be changed or copied after first use.
type Keyword struct {
	Store VectorStore
	// K1 and B are the BM25 parameters. Zero selects the defaults, 1.2 and
	// 0.75; a negative value selects 0.
	K1, B float64

	mu    sync.Mutex
	cache *bm25Index
	rev   uint64
}

type posting struct {
	doc  int
	freq int
}

// bm25Index is an inverted index over chunks.
type bm25Index struct {
	chunks   []*Chunk
	lengths  []int
	postings map[string][]posting
	avg      float64
}

func newBM25Index(chunks []*Chunk) *bm25Index {
	ix := &bm25Index{chunks: chunks, lengths: make([]int, len(chunks)), postings: map[string][]posting{}}
	total := 0
	for i, c := range chunks {
		toks := memory.Tokenize(c.Text())
		tf := map[string]int{}
		for _, t := range toks {
			tf[t]++
		}
		for t, f := range tf {
			ix.postings[t] = append(ix.postings[t], posting{i, f})
		}
		ix.lengths[i] = len(toks)
		total += len(toks)
	}
	ix.avg = 1
	if len(chunks) > 0 && total > 0 {
		ix.avg = float64(total) / float64(len(chunks))
	}
	return ix
}

// Retrieve implements Retriever.
func (k *Keyword) Retrieve(ctx context.Context, q Query) ([]Hit, error) {
	terms := memory.Tokenize(q.Text)
	if len(terms) == 0 {
		return nil, nil
	}
	ix, filtered, err := k.index(ctx, q.Filter)
	if err != nil {
		return nil, err
	}
	k1, b := bm25Param(k.K1, 1.2), bm25Param(k.B, 0.75)
	n := float64(len(ix.chunks))
	scores := map[int]float64{}
	seen := map[string]bool{}
	for _, t := range terms {
		if seen[t] {
			continue
		}
		seen[t] = true
		ps := ix.postings[t]
		if len(ps) == 0 {
			continue
		}
		df := float64(len(ps))
		idf := math.Log(1 + (n-df+0.5)/(df+0.5))
		for _, p := range ps {
			f := float64(p.freq)
			scores[p.doc] += idf * f * (k1 + 1) / (f + k1*(1-b+b*float64(ix.lengths[p.doc])/ix.avg))
		}
	}
	var hits []Hit
	for doc, s := range scores {
		c := ix.chunks[doc]
		if s <= 0 || s < q.MinScore || (!filtered && !q.Filter.Match(c)) {
			continue
		}
		hits = append(hits, Hit{Chunk: c, Score: s})
	}
	hits = topK(hits, q.Limit)
	for i := range hits {
		hits[i].Chunk = hits[i].Chunk.clone()
	}
	return hits, nil
}

// index returns the index to search and whether it holds only chunks that
// match f.
func (k *Keyword) index(ctx context.Context, f Filter) (*bm25Index, bool, error) {
	rs, ok := k.Store.(Revisioner)
	if !ok {
		chunks, err := k.Store.List(ctx, f)
		if err != nil {
			return nil, false, err
		}
		return newBM25Index(chunks), true, nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	// Read the revision before listing, so a change made meanwhile leaves
	// the cache stale rather than marking stale content as current.
	rev := rs.Revision()
	if k.cache != nil && k.rev == rev {
		return k.cache, false, nil
	}
	chunks, err := k.Store.List(ctx, Filter{})
	if err != nil {
		return nil, false, err
	}
	k.cache, k.rev = newBM25Index(chunks), rev
	return k.cache, false, nil
}

// bm25Param resolves a BM25 parameter: zero selects def and a negative
// value selects 0.
func bm25Param(v, def float64) float64 {
	switch {
	case v == 0:
		return def
	case v < 0:
		return 0
	}
	return v
}

// Hybrid fuses several retrievers with reciprocal rank fusion: a chunk
// scores the sum of Weight/(K+rank) over the retrievers that return it.
//
// A retriever that fails does not fail the query: Hybrid fuses the hits of
// the others and reports the failure to OnError, or logs it with
// slog.Default when OnError is nil. Retrieve returns an error only when
// every retriever fails.
type Hybrid struct {
	Retrievers []Retriever
	// Weights scale the contribution of each retriever. Missing weights
	// are 1.
	Weights []float64
	// K is the RRF constant. Defaults to 60.
	K float64
	// OnError receives the error of each retriever that fails while
	// another succeeds.
	OnError func(err error)
}

// Retrieve implements Retriever. Each retriever is asked for three times
// the limit and Query.MinScore applies to each retriever's own scores, not
// to the fused score.
func (h *Hybrid) Retrieve(ctx context.Context, q Query) ([]Hit, error) {
	k := h.K
	if k == 0 {
		k = 60
	}
	sub := q
	if sub.Limit > 0 {
		sub.Limit *= 3
	}
	scores := map[string]float64{}
	chunks := map[string]*Chunk{}
	var errs []error
	for i, r := range h.Retrievers {
		hits, err := r.Retrieve(ctx, sub)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		w := 1.0
		if i < len(h.Weights) {
			w = h.Weights[i]
		}
		for rank, hit := range hits {
			scores[hit.Chunk.ID] += w / (k + float64(rank+1))
			if _, ok := chunks[hit.Chunk.ID]; !ok {
				chunks[hit.Chunk.ID] = hit.Chunk
			}
		}
	}
	if len(errs) > 0 {
		if len(errs) == len(h.Retrievers) {
			return nil, errors.Join(errs...)
		}
		for _, err := range errs {
			report(ctx, h.OnError, "rag: retriever failed", err)
		}
	}
	hits := make([]Hit, 0, len(scores))
	for id, s := range scores {
		hits = append(hits, Hit{Chunk: chunks[id], Score: s})
	}
	return topK(hits, q.Limit), nil
}

// report passes err to onError, or logs it as a warning.
func report(ctx context.Context, onError func(error), msg string, err error) {
	if onError != nil {
		onError(err)
		return
	}
	slog.Default().WarnContext(ctx, msg, "error", err)
}

// topK sorts hits by descending score, then chunk ID, and keeps the first
// k when k is positive.
func topK(hits []Hit, k int) []Hit {
	slices.SortStableFunc(hits, func(a, b Hit) int {
		return cmp.Or(cmp.Compare(b.Score, a.Score), strings.Compare(a.Chunk.ID, b.Chunk.ID))
	})
	if k > 0 && len(hits) > k {
		hits = hits[:k]
	}
	return hits
}

var (
	_ Retriever = (*Vector)(nil)
	_ Retriever = (*Keyword)(nil)
	_ Retriever = (*Hybrid)(nil)
)
