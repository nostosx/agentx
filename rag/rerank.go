package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/nostosx/kitx/utf8text"

	"github.com/nostosx/agentx/model"
)

// Reranker reorders candidate hits by relevance to a query. It may drop
// hits it judges irrelevant.
type Reranker interface {
	Rerank(ctx context.Context, query string, hits []Hit) ([]Hit, error)
}

// RerankerFunc adapts a function to Reranker.
type RerankerFunc func(ctx context.Context, query string, hits []Hit) ([]Hit, error)

// Rerank calls f.
func (f RerankerFunc) Rerank(ctx context.Context, query string, hits []Hit) ([]Hit, error) {
	return f(ctx, query, hits)
}

// Reranked retrieves candidates with Retriever and reorders them with
// Reranker, the second stage of the retrieval pipeline recommended by
// Anthropic's contextual retrieval study.
//
// When the reranker fails, Reranked keeps the first-stage order and
// reports the error to OnError, or logs it with slog.Default when OnError
// is nil. Query.MinScore applies to the first stage.
type Reranked struct {
	Retriever Retriever
	Reranker  Reranker
	// Candidates is the number of first-stage hits to rerank. Defaults to
	// four times the limit, and at least 20.
	Candidates int
	OnError    func(err error)
}

// Retrieve implements Retriever.
func (r *Reranked) Retrieve(ctx context.Context, q Query) ([]Hit, error) {
	sub := q
	sub.Limit = r.Candidates
	if sub.Limit <= 0 {
		sub.Limit = max(4*q.Limit, 20)
	}
	hits, err := r.Retriever.Retrieve(ctx, sub)
	if err != nil || len(hits) == 0 {
		return hits, err
	}
	reranked, err := r.Reranker.Rerank(ctx, q.Text, hits)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		report(ctx, r.OnError, "rag: reranking failed", err)
		reranked = hits
	}
	if q.Limit > 0 && len(reranked) > q.Limit {
		reranked = reranked[:q.Limit]
	}
	return reranked, nil
}

// DefaultRerankPrompt is the system prompt of LLMReranker.
const DefaultRerankPrompt = `You rank search results. Given a query and numbered passages, judge how well each passage helps answer the query.

Reply with only a JSON array of the numbers of the relevant passages, most relevant first, for example [3, 1]. Leave out passages that do not help answer the query, and reply [] if none does.`

// LLMReranker reranks hits by asking a model to order them by relevance.
// Hits the model leaves out are dropped. Scores become 1 for the first hit
// and decrease linearly with rank.
type LLMReranker struct {
	Model model.Model
	// Prompt is the system prompt. Defaults to DefaultRerankPrompt.
	Prompt string
	// MaxPassageBytes truncates each passage. Defaults to 2000.
	MaxPassageBytes int
	// MaxTokens bounds the reply. Defaults to 256.
	MaxTokens int
}

var reRanking = regexp.MustCompile(`\[[\s\d,]*\]`)

// Rerank implements Reranker.
func (r *LLMReranker) Rerank(ctx context.Context, query string, hits []Hit) ([]Hit, error) {
	if r.Model == nil {
		return nil, errors.New("rag: LLMReranker requires a model")
	}
	if len(hits) == 0 {
		return nil, nil
	}
	prompt := r.Prompt
	if prompt == "" {
		prompt = DefaultRerankPrompt
	}
	limit := r.MaxPassageBytes
	if limit <= 0 {
		limit = 2000
	}
	maxTokens := r.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 256
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<query>\n%s\n</query>\n\n<passages>\n", query)
	for i, h := range hits {
		fmt.Fprintf(&b, "[%d] (source: %s)\n%s\n\n", i+1, h.Chunk.Source, utf8text.Truncate(h.Chunk.Text(), limit, " ..."))
	}
	b.WriteString("</passages>")
	resp, err := r.Model.Generate(ctx, &model.Request{
		System:    prompt,
		Messages:  []*model.Message{model.UserText(b.String())},
		MaxTokens: maxTokens,
	})
	if err != nil {
		return nil, err
	}
	text := resp.Text()
	// The last array wins, so a reply that thinks aloud before answering
	// is understood.
	matches := reRanking.FindAllString(text, -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("rag: reranker reply has no ranking: %q", utf8text.Truncate(text, 200, "..."))
	}
	var order []int
	if err := json.Unmarshal([]byte(matches[len(matches)-1]), &order); err != nil {
		return nil, fmt.Errorf("rag: decode reranker reply: %w", err)
	}
	seen := map[int]bool{}
	var ranked []Hit
	for _, n := range order {
		if n < 1 || n > len(hits) || seen[n] {
			continue
		}
		seen[n] = true
		ranked = append(ranked, hits[n-1])
	}
	for i := range ranked {
		ranked[i].Score = 1 - float64(i)/float64(len(ranked))
	}
	return ranked, nil
}

var (
	_ Retriever = (*Reranked)(nil)
	_ Reranker  = (*LLMReranker)(nil)
)
