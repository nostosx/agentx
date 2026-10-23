package rag

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/nostosx/kitx/utf8text"

	"github.com/nostosx/agentx/actx"
	"github.com/nostosx/agentx/tool"
)

// FormatOptions bounds the text rendered by Format.
type FormatOptions struct {
	// MaxChunkBytes bounds each hit's content, including its truncation note.
	// Defaults to 2000.
	MaxChunkBytes int
	// MaxBytes bounds the whole text: hits that would exceed it are
	// replaced by an omission note when space permits, except the first,
	// which is truncated instead. All notes count toward the byte limit.
	// Defaults to 10000.
	MaxBytes int
}

func (o FormatOptions) withDefaults() FormatOptions {
	if o.MaxChunkBytes <= 0 {
		o.MaxChunkBytes = 2000
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = 10000
	}
	return o
}

// Citation identifies a hit included in formatted output.
type Citation struct {
	// N is the number of the hit in the output, starting at 1.
	N      int     `json:"n"`
	ID     string  `json:"id"`
	Source string  `json:"source"`
	Score  float64 `json:"score"`
}

// Format renders hits for a prompt, numbered for citation, each with its
// source, section, chunk ID and score followed by its content.
func Format(hits []Hit, opts FormatOptions) string {
	text, _ := format(hits, opts)
	return text
}

func format(hits []Hit, opts FormatOptions) (string, []Citation) {
	opts = opts.withDefaults()
	var b strings.Builder
	var cites []Citation
	for i, h := range hits {
		c := h.Chunk
		var head strings.Builder
		fmt.Fprintf(&head, "[%d] source: %s", i+1, c.Source)
		if s := section(c); s != "" {
			fmt.Fprintf(&head, " | section: %s", s)
		}
		fmt.Fprintf(&head, " | id: %s | score: %s\n", c.ID, strconv.FormatFloat(h.Score, 'g', 4, 64))
		entry := head.String() + utf8text.Truncate(c.Content, opts.MaxChunkBytes, " ...[truncated]")
		sep := ""
		if i > 0 {
			sep = "\n\n"
		}
		if b.Len()+len(sep)+len(entry) > opts.MaxBytes {
			if i > 0 {
				note := fmt.Sprintf("\n\n(%d more results omitted to bound the output)", len(hits)-i)
				b.WriteString(utf8text.Truncate(note, opts.MaxBytes-b.Len(), ""))
				break
			}
			entry = utf8text.Truncate(entry, opts.MaxBytes, " ...[truncated]")
		}
		b.WriteString(sep)
		b.WriteString(entry)
		cites = append(cites, Citation{N: i + 1, ID: c.ID, Source: c.Source, Score: h.Score})
	}
	return b.String(), cites
}

// section describes where a chunk sits in its document: its Markdown
// header path or its document title.
func section(c *Chunk) string {
	switch hs := c.Metadata[MetaHeaders].(type) {
	case []string:
		return strings.Join(hs, " > ")
	case []any:
		parts := make([]string, 0, len(hs))
		for _, h := range hs {
			parts = append(parts, fmt.Sprint(h))
		}
		return strings.Join(parts, " > ")
	}
	if t, ok := c.Metadata[MetaTitle].(string); ok {
		return t
	}
	return ""
}

// ToolOptions configures Tool.
type ToolOptions struct {
	// Name defaults to "search_knowledge".
	Name string
	// Description defaults to a generic description of a knowledge base
	// search. Describe what the knowledge base holds so the model knows
	// when to search it.
	Description string
	// Limit is the default number of results. Defaults to 5.
	Limit int
	// MaxLimit caps the number of results the model may request. Defaults
	// to 20.
	MaxLimit int
	// Filter restricts every search.
	Filter Filter
	// Format bounds the output.
	Format FormatOptions
}

type searchArgs struct {
	Query  string `json:"query" jsonschema:"What to look for, in natural language or keywords."`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum number of results."`
	Source string `json:"source,omitempty" jsonschema:"Only search sources whose name starts with this prefix, such as a directory or a site."`
}

// Tool returns a read-only tool that searches r, usually an Index, and
// returns the matching chunks numbered for citation with their sources,
// IDs and scores. The citations are also returned in the result metadata
// under "citations" as []Citation.
func Tool(r Retriever, opts ToolOptions) tool.Tool {
	name := opts.Name
	if name == "" {
		name = "search_knowledge"
	}
	desc := opts.Description
	if desc == "" {
		desc = "Search the knowledge base for passages relevant to a query. Results are numbered and name their sources; cite the sources you use."
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 5
	}
	maxLimit := opts.MaxLimit
	if maxLimit <= 0 {
		maxLimit = 20
	}
	return tool.Func(name, desc, func(ctx context.Context, in searchArgs) (*tool.Result, error) {
		if strings.TrimSpace(in.Query) == "" {
			return tool.Errorf("query is empty"), nil
		}
		n := limit
		if in.Limit > 0 {
			n = min(in.Limit, maxLimit)
		}
		f := opts.Filter
		if in.Source != "" {
			// Search the narrower of the two prefixes; disjoint ones match
			// nothing.
			switch {
			case strings.HasPrefix(in.Source, f.SourcePrefix):
				f.SourcePrefix = in.Source
			case !strings.HasPrefix(f.SourcePrefix, in.Source):
				return tool.Text("No results found."), nil
			}
		}
		hits, err := r.Retrieve(ctx, Query{Text: in.Query, Limit: n, Filter: f})
		if err != nil {
			return nil, err
		}
		if len(hits) == 0 {
			return tool.Text("No results found."), nil
		}
		text, cites := format(hits, opts.Format)
		return &tool.Result{Content: text, Metadata: map[string]any{"citations": cites}}, nil
	}, tool.ReadOnly())
}

// Provider returns a context provider that retrieves up to k chunks
// relevant to each run's input from r, usually an Index, and adds them as
// a reminder, as the agent's memory provider does for memories. A
// non-positive k selects 3.
func Provider(r Retriever, k int) actx.Provider {
	if k <= 0 {
		k = 3
	}
	return actx.ProviderFunc(func(ctx context.Context, in *actx.Input) ([]actx.Section, error) {
		if strings.TrimSpace(in.Query) == "" {
			return nil, nil
		}
		hits, err := r.Retrieve(ctx, Query{Text: in.Query, Limit: k})
		if err != nil || len(hits) == 0 {
			return nil, err
		}
		return []actx.Section{{
			Name:      "rag",
			Content:   "Possibly relevant excerpts from the knowledge base; cite their sources when you use them:\n" + Format(hits, FormatOptions{}),
			Placement: actx.PlaceReminder,
		}}, nil
	})
}
