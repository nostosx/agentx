package search_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/search"
)

type capture struct{ requests []search.Request }

func (c *capture) Search(_ context.Context, r search.Request) (*search.Response, error) {
	c.requests = append(c.requests, r)
	return &search.Response{Results: []search.Result{{Title: "source", URL: "https://example.com", Content: "evidence"}}}, nil
}

func TestToolBoundsRequestsAndPreservesAttribution(t *testing.T) {
	s := &capture{}
	tool, err := search.Tool(s, 5)
	if err != nil {
		t.Fatal(err)
	}
	if !tool.Info().ReadOnly {
		t.Fatal("search tool must be read-only")
	}
	result, err := tool.Call(t.Context(), json.RawMessage(`{"query":"query","limit":999}`))
	if err != nil {
		t.Fatal(err)
	}
	var out search.Response
	if err := json.Unmarshal([]byte(result.Content), &out); err != nil {
		t.Fatal(err)
	}
	if len(s.requests) != 1 || s.requests[0].Limit != 5 || out.Results[0].URL != "https://example.com" {
		t.Fatalf("request=%+v response=%+v", s.requests, out)
	}
	if _, err := tool.Call(t.Context(), json.RawMessage(`{"query":" "}`)); err == nil {
		t.Fatal("empty query accepted")
	}
	if len(s.requests) != 1 {
		t.Fatal("invalid query reached searcher")
	}
}

func TestSearchInAgentLoop(t *testing.T) {
	s := &capture{}
	web, err := search.Tool(s, 3)
	if err != nil {
		t.Fatal(err)
	}
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "search-1", Name: "web_search", Args: map[string]any{"query": "golang", "limit": 9}}), modeltest.Reply("done"))
	a := agent.MustNew("research", agent.WithModel(m), agent.WithTools(web))
	if _, err := a.Ask(t.Context(), "Find evidence"); err != nil {
		t.Fatal(err)
	}
	if len(s.requests) != 1 || s.requests[0].Query != "golang" || s.requests[0].Limit != 3 {
		t.Fatalf("requests: %+v", s.requests)
	}
}
