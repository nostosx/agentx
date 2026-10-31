package browser_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nostosx/agentx/browser"
)

type session struct{ calls int }

func (s *session) Do(context.Context, browser.Action) (*browser.Result, error) {
	s.calls++
	return &browser.Result{MediaType: "application/octet-stream", Name: "file.txt", Data: []byte("download")}, nil
}
func (*session) Close() error { return nil }

func TestBoundToolAndBinaryLimit(t *testing.T) {
	s := &session{}
	tool, err := browser.Tool(s, 4)
	if err != nil {
		t.Fatal(err)
	}
	if tool.Info().ReadOnly || tool.Info().Concurrent {
		t.Fatal("browser interaction was marked safe for automatic concurrency")
	}
	if _, err := tool.Call(t.Context(), json.RawMessage(`{"operation":"download","selector":"a"}`)); err == nil {
		t.Fatal("oversized download accepted")
	}
	if _, err := tool.Call(t.Context(), json.RawMessage(`{"operation":"navigate","url":"file:///etc/passwd"}`)); err == nil || s.calls != 1 {
		t.Fatal("invalid action reached the session")
	}
}
