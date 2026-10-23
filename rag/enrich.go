package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/nostosx/kitx/syncmap"
	"github.com/nostosx/kitx/utf8text"

	"github.com/nostosx/agentx/model"

	"github.com/nostosx/kitx/parallel"
)

// Enricher adds information to the chunks of a document before they are
// embedded, typically by setting Chunk.Context.
type Enricher interface {
	Enrich(ctx context.Context, doc *Document, chunks []*Chunk) error
}

// EnricherFunc adapts a function to Enricher.
type EnricherFunc func(ctx context.Context, doc *Document, chunks []*Chunk) error

// Enrich calls f.
func (f EnricherFunc) Enrich(ctx context.Context, doc *Document, chunks []*Chunk) error {
	return f(ctx, doc, chunks)
}

// Cache stores generated text by key. Implementations must be safe for
// concurrent use.
type Cache interface {
	Get(key string) (string, bool)
	Put(key, value string)
}

// MapCache is an in-memory Cache. The zero value is ready to use.
type MapCache struct {
	m syncmap.Map[string, string]
}

// Get implements Cache.
func (c *MapCache) Get(key string) (string, bool) { return c.m.Get(key) }

// Put implements Cache.
func (c *MapCache) Put(key, value string) { c.m.Set(key, value) }

// DefaultContextInstruction is the instruction of ContextualEnricher, from
// Anthropic's contextual retrieval prompt.
const DefaultContextInstruction = "Please give a short succinct context to situate this chunk within the overall document for the purposes of improving search retrieval of the chunk. Answer only with the succinct context and nothing else."

// ContextualEnricher implements contextual retrieval: a model writes a
// short context that situates each chunk within its document, and the
// context is prepended to the chunk for embedding and keyword search (see
// Chunk.Text), after any context the splitter set.
//
// The document is sent as the system prompt and the chunk as the user
// message, so the requests for the chunks of one document share a prefix
// that providers with prompt caching can reuse. Contexts are cached by a
// hash of the instruction, the document and the chunk, so re-indexing
// unchanged content does not call the model again.
type ContextualEnricher struct {
	Model model.Model
	// Instruction follows the chunk in the user message. Defaults to
	// DefaultContextInstruction.
	Instruction string
	// MaxDocumentBytes truncates long documents. Defaults to 100000, about
	// 25000 tokens of English text.
	MaxDocumentBytes int
	// MaxTokens bounds each context. Defaults to 200.
	MaxTokens int
	// Concurrency is the number of chunks enriched in parallel. Defaults to
	// 4.
	Concurrency int
	// Cache stores contexts. Defaults to a MapCache owned by the enricher;
	// supply a persistent cache to reuse contexts across processes.
	Cache Cache

	once     sync.Once
	ownCache *MapCache
}

func (e *ContextualEnricher) cache() Cache {
	if e.Cache != nil {
		return e.Cache
	}
	e.once.Do(func() { e.ownCache = &MapCache{} })
	return e.ownCache
}

// Enrich implements Enricher. It fails when any context cannot be
// generated.
func (e *ContextualEnricher) Enrich(ctx context.Context, doc *Document, chunks []*Chunk) error {
	if e.Model == nil {
		return errors.New("rag: ContextualEnricher requires a model")
	}
	if len(chunks) == 0 {
		return nil
	}
	instruction := e.Instruction
	if instruction == "" {
		instruction = DefaultContextInstruction
	}
	maxDoc := e.MaxDocumentBytes
	if maxDoc <= 0 {
		maxDoc = 100000
	}
	system := "<document>\n" + utf8text.Truncate(doc.Content, maxDoc, "\n... [document truncated]") + "\n</document>"
	workers := e.Concurrency
	if workers <= 0 {
		workers = 4
	}
	return parallel.ForEach(ctx, chunks, workers, func(ctx context.Context, c *Chunk) error {
		text, err := e.context(ctx, system, instruction, c.Content)
		if err != nil {
			return fmt.Errorf("rag: contextualize chunk %s of %s: %w", c.ID, c.Source, err)
		}
		if c.Context == "" {
			c.Context = text
		} else {
			c.Context += "\n" + text
		}
		return nil
	})
}

func (e *ContextualEnricher) context(ctx context.Context, system, instruction, chunk string) (string, error) {
	h := sha256.New()
	for _, s := range []string{instruction, system, chunk} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	key := hex.EncodeToString(h.Sum(nil))
	cache := e.cache()
	if text, ok := cache.Get(key); ok {
		return text, nil
	}
	maxTokens := e.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 200
	}
	user := "Here is the chunk we want to situate within the whole document\n<chunk>\n" + chunk + "\n</chunk>\n\n" + instruction
	resp, err := e.Model.Generate(ctx, &model.Request{
		System:    system,
		Messages:  []*model.Message{model.UserText(user)},
		MaxTokens: maxTokens,
	})
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(resp.Text())
	if text == "" {
		return "", errors.New("model returned an empty context")
	}
	cache.Put(key, text)
	return text, nil
}

var _ Enricher = (*ContextualEnricher)(nil)
