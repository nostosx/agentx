package web_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nostosx/agentx/web"
)

type readerFunc func(context.Context, string) (*web.Document, error)

func (f readerFunc) Read(ctx context.Context, u string) (*web.Document, error) { return f(ctx, u) }

func TestReaderToolBoundsAndAttribution(t *testing.T) {
	doc := &web.Document{URL: "https://example.com/final", Title: "title", Text: strings.Repeat("文", 20), Markdown: strings.Repeat("文", 20)}
	tool, err := web.Tool(readerFunc(func(context.Context, string) (*web.Document, error) { return doc, nil }), 5)
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.Call(t.Context(), json.RawMessage(`{"url":"https://example.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "https://example.com/final") || strings.Contains(result.Content, strings.Repeat("文", 6)) {
		t.Fatal(result.Content)
	}
	if len([]rune(doc.Text)) != 20 {
		t.Fatal("tool mutated the reader's document")
	}
	if _, err := tool.Call(t.Context(), json.RawMessage(`{"url":"https://user:secret@example.com"}`)); err == nil {
		t.Fatal("credential-bearing URL accepted")
	}
}

func TestLoaderFailsAsWhole(t *testing.T) {
	r := readerFunc(func(_ context.Context, u string) (*web.Document, error) {
		if strings.HasSuffix(u, "bad") {
			return nil, errors.New("failure")
		}
		return &web.Document{URL: u, Markdown: "body"}, nil
	})
	l := web.Loader{Reader: r, URLs: []string{"https://example.com/good", "https://example.com/bad"}}
	docs, err := l.Load(t.Context())
	if err == nil || docs != nil {
		t.Fatal("partial snapshot returned")
	}
}
