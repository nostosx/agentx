package web_test

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/nostosx/agentx/web"
)

func TestToolBoundsMetadataAndRejectsUserinfo(t *testing.T) {
	doc := &web.Document{URL: "https://example.com", Metadata: map[string]string{"untrusted": strings.Repeat("x", 10000)}}
	operation, err := web.Tool(readerFunc(func(context.Context, string) (*web.Document, error) { return doc, nil }), 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := operation.Call(t.Context(), json.RawMessage(`{"url":"https://example.com"}`)); err == nil {
		t.Fatal("unbounded metadata accepted")
	}
	credentialURL := (&url.URL{Scheme: "https", Host: "example.com", User: url.User("fixture")}).String()
	arguments, err := json.Marshal(map[string]string{"url": credentialURL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := operation.Call(t.Context(), arguments); err == nil {
		t.Fatal("URL credentials accepted")
	}
}
