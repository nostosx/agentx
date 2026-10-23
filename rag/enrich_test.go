package rag

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nostosx/agentx/model"
)

// contextModel answers with a context naming the chunk it was asked about.
type contextModel struct {
	calls    atomic.Int32
	mu       sync.Mutex
	requests []*model.Request
	fail     string
}

func (m *contextModel) generate(_ context.Context, req *model.Request) (*model.Response, error) {
	m.calls.Add(1)
	m.mu.Lock()
	m.requests = append(m.requests, req)
	m.mu.Unlock()
	user := req.Messages[0].Text()
	_, chunk, _ := strings.Cut(user, "<chunk>\n")
	chunk, _, _ = strings.Cut(chunk, "\n</chunk>")
	if m.fail != "" && chunk == m.fail {
		return nil, errors.New("model down")
	}
	return &model.Response{Message: model.AssistantText(" About " + chunk + ". ")}, nil
}

func (m *contextModel) model() model.Model { return model.Func(m.generate) }

func TestContextualEnricher(t *testing.T) {
	m := &contextModel{}
	e := &ContextualEnricher{Model: m.model(), Concurrency: 2}
	doc := &Document{ID: "d", Content: "Whole document text"}
	newChunks := func() []*Chunk {
		return []*Chunk{{ID: "1", Content: "one"}, {ID: "2", Content: "two", Context: "Header"}, {ID: "3", Content: "three"}}
	}
	chunks := newChunks()
	if err := e.Enrich(t.Context(), doc, chunks); err != nil {
		t.Fatal(err)
	}
	if chunks[0].Context != "About one." || chunks[1].Context != "Header\nAbout two." || chunks[2].Text() != "About three.\n\nthree" {
		t.Fatalf("contexts = %q, %q, %q", chunks[0].Context, chunks[1].Context, chunks[2].Context)
	}
	req := m.requests[0]
	if req.System != "<document>\nWhole document text\n</document>" || !strings.HasSuffix(req.Messages[0].Text(), DefaultContextInstruction) || req.MaxTokens != 200 {
		t.Fatalf("request = %+v", req)
	}
	// Unchanged content is served from the cache.
	if err := e.Enrich(t.Context(), doc, newChunks()); err != nil || m.calls.Load() != 3 {
		t.Fatalf("cached enrich: %v, %d calls", err, m.calls.Load())
	}
	// A changed document invalidates the cache.
	if err := e.Enrich(t.Context(), &Document{ID: "d", Content: "Revised"}, newChunks()[:1]); err != nil || m.calls.Load() != 4 {
		t.Fatalf("changed doc: %v, %d calls", err, m.calls.Load())
	}

	shared := &MapCache{}
	long := &ContextualEnricher{Model: m.model(), Cache: shared, MaxDocumentBytes: 32, Instruction: "Be brief."}
	if err := long.Enrich(t.Context(), &Document{ID: "d", Content: strings.Repeat("Whole document text", 10)}, []*Chunk{{ID: "x", Content: "x"}}); err != nil {
		t.Fatal(err)
	}
	last := m.requests[len(m.requests)-1]
	if !strings.Contains(last.System, "Whole d\n... [document truncated]") || !strings.HasSuffix(last.Messages[0].Text(), "Be brief.") {
		t.Fatalf("truncated request = %+v", last)
	}
	if _, ok := firstValue(shared); !ok {
		t.Fatal("shared cache not used")
	}

	failing := &ContextualEnricher{Model: (&contextModel{fail: "two"}).model()}
	err := failing.Enrich(t.Context(), doc, newChunks())
	if err == nil || !strings.Contains(err.Error(), "chunk 2") || !strings.Contains(err.Error(), "model down") {
		t.Fatalf("failure = %v", err)
	}
	if err := (&ContextualEnricher{}).Enrich(t.Context(), doc, newChunks()); err == nil {
		t.Fatal("missing model accepted")
	}
	empty := &ContextualEnricher{Model: model.Func(func(context.Context, *model.Request) (*model.Response, error) {
		return &model.Response{Message: model.AssistantText("  ")}, nil
	})}
	if err := empty.Enrich(t.Context(), doc, newChunks()); err == nil {
		t.Fatal("empty context accepted")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	cancel()
	if err := e.Enrich(ctx, &Document{ID: "new", Content: "new"}, newChunks()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled = %v", err)
	}
}

func firstValue(c *MapCache) (string, bool) {
	for _, v := range c.m.Values() {
		return v, true
	}
	return "", false
}

func TestContextualEnricherCancelsAndJoinsChunks(t *testing.T) {
	t.Parallel()
	failure := errors.New("provider failed")
	started := make(chan struct{}, 2)
	fail := make(chan struct{})
	exited := make(chan struct{})
	var calls atomic.Int32
	e := &ContextualEnricher{Concurrency: 2, Model: model.Func(func(ctx context.Context, req *model.Request) (*model.Response, error) {
		calls.Add(1)
		started <- struct{}{}
		if strings.Contains(req.Messages[0].Text(), "<chunk>\none\n</chunk>") {
			select {
			case <-fail:
				return nil, failure
			case <-ctx.Done():
				return nil, context.Cause(ctx)
			}
		}
		defer close(exited)
		<-ctx.Done()
		return nil, context.Cause(ctx)
	})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- e.Enrich(ctx, &Document{Content: "doc"}, []*Chunk{
			{ID: "1", Content: "one"}, {ID: "2", Content: "two"}, {ID: "3", Content: "three"},
		})
	}()
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("two chunks did not start")
		}
	}
	close(fail)
	select {
	case err := <-result:
		if !errors.Is(err, failure) || !strings.Contains(err.Error(), "chunk 1") {
			t.Fatalf("Enrich = %v, want chunk 1 failure", err)
		}
	case <-ctx.Done():
		t.Fatal("Enrich did not join canceled chunks")
	}
	select {
	case <-exited:
	default:
		t.Fatal("Enrich returned before the sibling chunk exited")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("model calls = %d, want 2", got)
	}
}

func TestContextualEnricherLargeConcurrency(t *testing.T) {
	t.Parallel()
	m := &contextModel{}
	e := &ContextualEnricher{Model: m.model(), Concurrency: int(^uint(0) >> 1)}
	chunks := []*Chunk{{Content: "one"}}
	if err := e.Enrich(t.Context(), &Document{Content: "doc"}, chunks); err != nil {
		t.Fatal(err)
	}
	if got := chunks[0].Context; got != "About one." {
		t.Fatalf("context = %q", got)
	}
}
