package rag_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nostosx/agentx/actx"
	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/rag"
	"github.com/nostosx/agentx/tool"
)

func newIndex(t *testing.T) *rag.Index {
	t.Helper()
	ix := rag.NewIndex(rag.Config{})
	docs := []*rag.Document{
		{ID: "docs/install.md", Content: "# Install\n\nRun go get to install agentx.", Metadata: map[string]any{rag.MetaFormat: "markdown"}},
		{ID: "docs/usage.md", Content: "Create an agent with agent.New and install tools.", Metadata: map[string]any{rag.MetaFormat: "markdown"}},
		{ID: "blog/news.md", Content: "Release notes: install the new version today."},
	}
	if _, err := ix.Add(t.Context(), docs...); err != nil {
		t.Fatal(err)
	}
	return ix
}

func call(t *testing.T, tl tool.Tool, args map[string]any) *tool.Result {
	t.Helper()
	b, _ := json.Marshal(args)
	res, err := tl.Call(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestTool(t *testing.T) {
	ix := newIndex(t)
	tl := rag.Tool(ix, rag.ToolOptions{})
	info := tl.Info()
	if info.Name != "search_knowledge" || !info.ReadOnly || info.Parameters == nil {
		t.Fatalf("info = %+v", info)
	}
	res := call(t, tl, map[string]any{"query": "install agentx"})
	if res.IsError || !strings.HasPrefix(res.Content, "[1] source: docs/install.md | section: Install | id: c_") ||
		!strings.Contains(res.Content, "\n# Install\n\nRun go get to install agentx.") || !strings.Contains(res.Content, "[3] source: ") {
		t.Fatalf("content = %q", res.Content)
	}
	cites, _ := res.Metadata["citations"].([]rag.Citation)
	if len(cites) != 3 || cites[0].N != 1 || cites[0].Source != "docs/install.md" || cites[0].Score <= cites[2].Score {
		t.Fatalf("citations = %+v", cites)
	}
	res = call(t, tl, map[string]any{"query": "install", "limit": 1, "source": "blog/"})
	if !strings.HasPrefix(res.Content, "[1] source: blog/news.md") || strings.Contains(res.Content, "[2]") {
		t.Fatalf("filtered = %q", res.Content)
	}
	if res := call(t, tl, map[string]any{"query": "  "}); !res.IsError {
		t.Fatal("empty query accepted")
	}
	if res := call(t, tl, map[string]any{"query": "nonexistentword"}); res.Content != "No results found." {
		t.Fatalf("no results = %q", res.Content)
	}

	// Options restrict and bound the output.
	tl = rag.Tool(ix, rag.ToolOptions{
		Name: "docs", Description: "Search the docs.", Limit: 1, MaxLimit: 2,
		Filter: rag.Filter{SourcePrefix: "docs/"},
		Format: rag.FormatOptions{MaxChunkBytes: 24, MaxBytes: 1000},
	})
	if tl.Info().Name != "docs" || tl.Info().Description != "Search the docs." {
		t.Fatalf("info = %+v", tl.Info())
	}
	res = call(t, tl, map[string]any{"query": "install", "limit": 10})
	if strings.Count(res.Content, "source: docs/") != 2 || strings.Contains(res.Content, "blog/") || !strings.Contains(res.Content, " ...[truncated]") {
		t.Fatalf("restricted = %q", res.Content)
	}
	if res := call(t, tl, map[string]any{"query": "install", "source": "docs/usage"}); !strings.Contains(res.Content, "docs/usage.md") || strings.Contains(res.Content, "install.md") {
		t.Fatalf("narrower prefix = %q", res.Content)
	}
	if res := call(t, tl, map[string]any{"query": "install", "source": "blog/"}); res.Content != "No results found." {
		t.Fatalf("disjoint prefix = %q", res.Content)
	}
	failing := rag.RetrieverFunc(func(context.Context, rag.Query) ([]rag.Hit, error) { return nil, errors.New("index down") })
	if _, err := rag.Tool(failing, rag.ToolOptions{}).Call(t.Context(), json.RawMessage(`{"query":"x"}`)); err == nil {
		t.Fatal("retriever error hidden")
	}
}

func TestFormatBounds(t *testing.T) {
	var hits []rag.Hit
	for i := range 5 {
		hits = append(hits, rag.Hit{Chunk: &rag.Chunk{ID: "c" + string(rune('0'+i)), Source: "s", Content: strings.Repeat("x", 100), Metadata: map[string]any{rag.MetaTitle: "Page"}}, Score: 1})
	}
	out := rag.Format(hits, rag.FormatOptions{MaxBytes: 360})
	if !strings.Contains(out, "| section: Page |") || !strings.Contains(out, "(3 more results omitted to bound the output)") || strings.Contains(out, "[3]") {
		t.Fatalf("Format = %q", out)
	}
	out = rag.Format(hits[:1], rag.FormatOptions{MaxBytes: 50})
	if len(out) > 50 || !strings.HasPrefix(out, "[1] source: s") {
		t.Fatalf("truncated first hit = %q", out)
	}
	headers := rag.Hit{Chunk: &rag.Chunk{ID: "h", Source: "s", Content: "c", Metadata: map[string]any{rag.MetaHeaders: []any{"A", "B"}}}, Score: 0.5}
	if out := rag.Format([]rag.Hit{headers}, rag.FormatOptions{}); out != "[1] source: s | section: A > B | id: h | score: 0.5\nc" {
		t.Fatalf("decoded headers = %q", out)
	}
}

func TestProvider(t *testing.T) {
	ix := newIndex(t)
	p := rag.Provider(ix, 1)
	sections, err := p.Sections(t.Context(), &actx.Input{Query: "how do I install agentx?"})
	if err != nil || len(sections) != 1 {
		t.Fatalf("Sections = %+v, %v", sections, err)
	}
	s := sections[0]
	if s.Placement != actx.PlaceReminder || s.Name != "rag" || !strings.Contains(s.Content, "docs/install.md") || strings.Contains(s.Content, "[2]") {
		t.Fatalf("section = %+v", s)
	}
	for _, q := range []string{"  ", "zzzz"} {
		if sections, err := p.Sections(t.Context(), &actx.Input{Query: q}); err != nil || len(sections) != 0 {
			t.Fatalf("Sections(%q) = %+v, %v", q, sections, err)
		}
	}
	sections, _ = rag.Provider(ix, 0).Sections(t.Context(), &actx.Input{Query: "install"})
	if !strings.Contains(sections[0].Content, "[3]") {
		t.Fatalf("default k = %q", sections[0].Content)
	}
}

func TestAgentIntegration(t *testing.T) {
	ix := newIndex(t)
	m := modeltest.New(
		modeltest.CallTools(modeltest.Call{ID: "1", Name: "search_knowledge", Args: map[string]any{"query": "agent.New"}}),
		modeltest.Reply("Use agent.New [docs/usage.md]."),
	)
	a := agent.MustNew("assistant", agent.WithModel(m),
		agent.WithTools(rag.Tool(ix, rag.ToolOptions{})),
		agent.WithContextProvider(rag.Provider(ix, 2)))
	out, err := a.Ask(t.Context(), "How do I install agentx?")
	if err != nil || out != "Use agent.New [docs/usage.md]." {
		t.Fatalf("Ask = %q, %v", out, err)
	}
	reqs := m.Requests()
	if !strings.Contains(reqs[0].Messages[len(reqs[0].Messages)-1].Text(), "Possibly relevant excerpts from the knowledge base") {
		t.Fatalf("first request lacks retrieved context: %+v", reqs[0].Messages)
	}
	var result string
	for _, msg := range reqs[1].Messages {
		for _, r := range msg.ToolResults() {
			result = r.Content
		}
	}
	if !strings.Contains(result, "docs/usage.md") {
		t.Fatalf("tool result = %q", result)
	}
}

var _ model.Model = (*modeltest.Model)(nil)

func TestFormatHonorsTotalByteBudget(t *testing.T) {
	t.Parallel()
	hits := []rag.Hit{
		{Chunk: &rag.Chunk{ID: "a", Source: "你好", Content: strings.Repeat("🙂中文", 30)}, Score: 1},
		{Chunk: &rag.Chunk{ID: "b", Source: "世界", Content: strings.Repeat("b", 100)}, Score: 0.5},
	}
	for _, budget := range []int{1, 2, 3, 10, 50, 100, 150, 200, 300} {
		out := rag.Format(hits, rag.FormatOptions{MaxChunkBytes: 80, MaxBytes: budget})
		if len(out) > budget || !utf8.ValidString(out) {
			t.Errorf("budget %d: %d bytes, valid=%v: %q", budget, len(out), utf8.ValidString(out), out)
		}
	}
}
