package memory

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"math"
	"slices"
	"strings"
	"unicode"
)

// Query is a retrieval request.
type Query struct {
	Text   string
	Limit  int
	Filter Filter
	// MinScore drops hits scoring below it. Scores are retriever specific.
	MinScore float64
}

// Retriever finds entries relevant to a query.
type Retriever interface {
	Retrieve(ctx context.Context, q Query) ([]Hit, error)
}

// Embedder converts texts to vectors.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Keyword ranks entries with BM25 over their content and tags. It handles
// CJK text by indexing single characters and character bigrams.
type Keyword struct {
	Store Store
	// K1 and B are the BM25 parameters. Zero selects the defaults, 1.2 and
	// 0.75; a negative value selects 0, so B < 0 turns off document length
	// normalization.
	K1, B float64
}

// Retrieve implements Retriever.
func (k *Keyword) Retrieve(ctx context.Context, q Query) ([]Hit, error) {
	entries, err := k.Store.List(ctx, q.Filter)
	if err != nil {
		return nil, err
	}
	terms := Tokenize(q.Text)
	if len(terms) == 0 || len(entries) == 0 {
		return nil, nil
	}
	k1, b := bm25Param(k.K1, 1.2), bm25Param(k.B, 0.75)
	docs := make([]map[string]int, len(entries))
	lengths := make([]int, len(entries))
	df := map[string]int{}
	total := 0
	for i, e := range entries {
		toks := Tokenize(e.Content + " " + strings.Join(e.Tags, " "))
		tf := map[string]int{}
		for _, t := range toks {
			tf[t]++
		}
		for t := range tf {
			df[t]++
		}
		docs[i], lengths[i] = tf, len(toks)
		total += len(toks)
	}
	avg := float64(total) / float64(len(entries))
	if avg == 0 {
		avg = 1
	}
	n := float64(len(entries))
	unique := map[string]bool{}
	var hits []Hit
	for i, e := range entries {
		score := 0.0
		clear(unique)
		for _, t := range terms {
			if unique[t] {
				continue
			}
			unique[t] = true
			f := float64(docs[i][t])
			if f == 0 {
				continue
			}
			idf := math.Log(1 + (n-float64(df[t])+0.5)/(float64(df[t])+0.5))
			score += idf * f * (k1 + 1) / (f + k1*(1-b+b*float64(lengths[i])/avg))
		}
		if score > 0 && score >= q.MinScore {
			hits = append(hits, Hit{Entry: e, Score: score})
		}
	}
	return topK(hits, q.Limit), nil
}

// Vector ranks entries by cosine similarity between embeddings. Entries
// without an embedding are skipped.
type Vector struct {
	Store    Store
	Embedder Embedder
}

// Retrieve implements Retriever.
func (v *Vector) Retrieve(ctx context.Context, q Query) ([]Hit, error) {
	if v.Embedder == nil {
		return nil, errors.New("memory: vector retriever requires an embedder")
	}
	if strings.TrimSpace(q.Text) == "" {
		return nil, nil
	}
	vecs, err := v.Embedder.Embed(ctx, []string{q.Text})
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 {
		return nil, errors.New("memory: embedder returned an unexpected number of vectors")
	}
	entries, err := v.Store.List(ctx, q.Filter)
	if err != nil {
		return nil, err
	}
	var hits []Hit
	for _, e := range entries {
		if len(e.Embedding) == 0 {
			continue
		}
		s := Cosine(vecs[0], e.Embedding)
		if s > 0 && s >= q.MinScore {
			hits = append(hits, Hit{Entry: e, Score: s})
		}
	}
	return topK(hits, q.Limit), nil
}

// Hybrid fuses several retrievers with reciprocal rank fusion.
//
// A retriever that fails does not fail the recall: Hybrid returns the
// fused hits of the others and reports the failure to OnError, or logs it
// when OnError is nil. Retrieve
// returns an error only when every retriever fails.
type Hybrid struct {
	Retrievers []Retriever
	// K is the RRF constant. Defaults to 60.
	K float64
	// OnError receives the error of each retriever that fails while at
	// least one other succeeds. If nil, the errors are logged as warnings
	// with slog.Default, so a retriever that is permanently broken does not
	// go unnoticed.
	OnError func(err error)
}

// Retrieve implements Retriever.
//
// Query.MinScore is passed to each underlying retriever and compared with
// its own scores, not with the fused score. Scores have different scales
// (BM25 scores are unbounded while cosine similarity is at most 1), so one
// threshold rarely suits every retriever; prefer filtering in the
// retrievers themselves, for example with a wrapping Retriever.
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
	entries := map[string]*Entry{}
	var errs []error
	for _, r := range h.Retrievers {
		hits, err := r.Retrieve(ctx, sub)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for rank, hit := range hits {
			scores[hit.Entry.ID] += 1 / (k + float64(rank+1))
			entries[hit.Entry.ID] = hit.Entry
		}
	}
	if len(errs) > 0 {
		if len(errs) == len(h.Retrievers) {
			return nil, errors.Join(errs...)
		}
		for _, err := range errs {
			if h.OnError != nil {
				h.OnError(err)
			} else {
				slog.Default().WarnContext(ctx, "memory: retriever failed", "error", err)
			}
		}
	}
	hits := make([]Hit, 0, len(scores))
	for id, s := range scores {
		hits = append(hits, Hit{Entry: entries[id], Score: s})
	}
	return topK(hits, q.Limit), nil
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

func topK(hits []Hit, k int) []Hit {
	slices.SortStableFunc(hits, func(a, b Hit) int {
		if c := cmp.Compare(b.Score, a.Score); c != 0 {
			return c
		}
		return strings.Compare(a.Entry.ID, b.Entry.ID)
	})
	if k > 0 && len(hits) > k {
		hits = hits[:k]
	}
	return hits
}

// Cosine returns the cosine similarity of two vectors, or 0 when their
// lengths differ or either is zero.
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// Tokenize lower-cases text and splits it into terms. Runs of letters and
// digits form words; CJK characters produce unigrams and bigrams.
func Tokenize(text string) []string {
	var terms []string
	var word []rune
	var cjk []rune
	flushWord := func() {
		if len(word) > 0 {
			terms = append(terms, string(word))
			word = word[:0]
		}
	}
	flushCJK := func() {
		for i, r := range cjk {
			terms = append(terms, string(r))
			if i+1 < len(cjk) {
				terms = append(terms, string(cjk[i:i+2]))
			}
		}
		cjk = cjk[:0]
	}
	for _, r := range strings.ToLower(text) {
		switch {
		case isCJK(r):
			flushWord()
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushCJK()
			word = append(word, r)
		default:
			flushWord()
			flushCJK()
		}
	}
	flushWord()
	flushCJK()
	return terms
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r)
}
