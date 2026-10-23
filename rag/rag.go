// Package rag implements retrieval-augmented generation: loading documents,
// splitting them into chunks, embedding and indexing the chunks, and
// retrieving them for agents through a search tool or a context provider.
//
// The pipeline follows the component split of frameworks such as Eino
// (loader, parser, transformer, indexer, retriever):
//
//   - A Loader reads documents from files (FSLoader) or URLs (URLLoader).
//     A Parser turns bytes into documents: text, Markdown, HTML, JSON, JSON
//     Lines and CSV are built in. PDF and other binary formats need an
//     external parser; register one in FSLoader.Parsers or implement Loader.
//   - A Splitter cuts documents into chunks: RecursiveSplitter,
//     MarkdownSplitter, CodeSplitter and AutoSplitter, which picks one of the
//     others per document. Sizes are measured in tokens by a pluggable
//     LengthFunc that defaults to the estimate of the context package.
//   - An Index embeds chunks in batches with a memory.Embedder and stores
//     them in a VectorStore, such as MemoryStore. Sync re-indexes only
//     sources whose content changed and deletes sources that disappeared.
//     An optional Enricher, such as ContextualEnricher, adds a short
//     LLM-written context to each chunk before it is embedded.
//   - Retrievers rank chunks: Vector (cosine similarity), Keyword (BM25),
//     Hybrid (reciprocal rank fusion) and Reranked, which reorders the
//     candidates of another retriever with a Reranker such as LLMReranker.
//   - Tool exposes an index to a model as a read-only search tool, and
//     Provider retrieves relevant chunks automatically for each run.
package rag

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// Metadata keys set by the loaders, parsers and splitters of this package.
const (
	// MetaFormat is the document format: "text", "markdown", "html",
	// "json", "csv" or "code".
	MetaFormat = "format"
	// MetaLanguage is the programming language of a code document, such as
	// "go", "python", "javascript" or "typescript".
	MetaLanguage = "language"
	// MetaPath is the slash-separated path of a file relative to the root
	// of its FSLoader.
	MetaPath = "path"
	// MetaURL is the final URL of a document loaded by URLLoader.
	MetaURL = "url"
	// MetaContentType is the media type of a document loaded by URLLoader.
	MetaContentType = "content_type"
	// MetaTitle is the title of an HTML document.
	MetaTitle = "title"
	// MetaRecord is the 1-based number of a JSON Lines line, JSON array
	// element or CSV row.
	MetaRecord = "record"
	// MetaHeaders is the path of Markdown headers above a chunk, as a
	// []string from the outermost header.
	MetaHeaders = "headers"
)

// Document is a unit of loaded content, such as a file, a web page or a
// CSV row.
type Document struct {
	// ID identifies the document. It defaults to Source.
	ID      string `json:"id"`
	Content string `json:"content"`
	// Metadata is copied to every chunk of the document.
	Metadata map[string]any `json:"metadata,omitempty"`
	// Source names where the document came from, such as a file path or a
	// URL. Documents with the same source are indexed and replaced
	// together; a source defaults to the document ID.
	Source string `json:"source,omitempty"`
}

// source returns the document's source, or its ID when the source is
// empty.
func (d *Document) source() string {
	if d.Source != "" {
		return d.Source
	}
	return d.ID
}

// id returns the document's ID, or its source when the ID is empty.
func (d *Document) id() string {
	if d.ID != "" {
		return d.ID
	}
	return d.Source
}

// Chunk is a piece of a document, the unit that is embedded, stored and
// retrieved.
type Chunk struct {
	// ID is stable: it is derived from the source, the document ID and the
	// offset, so re-splitting unchanged content yields the same IDs.
	ID         string `json:"id"`
	DocumentID string `json:"document_id"`
	Source     string `json:"source,omitempty"`
	// Index is the position of the chunk within its document.
	Index int `json:"index"`
	// Offset is the byte offset of Content within the document content.
	Offset  int    `json:"offset"`
	Content string `json:"content"`
	// Context situates the chunk within its document, such as a Markdown
	// header path or text written by ContextualEnricher. It is prepended to
	// Content for embedding and keyword search; see Text.
	Context  string         `json:"context,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	// Embedding is set by the Index. Stores share it between the copies
	// they return, so it must not be modified.
	Embedding []float32 `json:"embedding,omitempty"`
}

// Text returns the text that is embedded and searched: Context and
// Content separated by a blank line.
func (c *Chunk) Text() string {
	if c.Context == "" {
		return c.Content
	}
	return c.Context + "\n\n" + c.Content
}

// clone copies the chunk and its metadata map. The embedding is shared.
func (c *Chunk) clone() *Chunk {
	cp := *c
	cp.Metadata = maps.Clone(c.Metadata)
	return &cp
}

// ChunkID returns the stable ID of the chunk of a document that starts at
// offset: a hash of the source, the document ID and the offset.
func ChunkID(source, documentID string, offset int) string {
	h := sha256.New()
	h.Write([]byte(source))
	h.Write([]byte{0})
	h.Write([]byte(documentID))
	h.Write([]byte{0})
	h.Write([]byte(strconv.Itoa(offset)))
	return "c_" + hex.EncodeToString(h.Sum(nil))[:16]
}

// Hit is a retrieved chunk with its relevance score. Scores are retriever
// specific.
type Hit struct {
	Chunk *Chunk
	Score float64
}

// Filter selects chunks. The zero value matches every chunk.
type Filter struct {
	// Sources matches chunks whose source is one of these when non-empty.
	Sources []string
	// SourcePrefix matches chunks whose source starts with it.
	SourcePrefix string
	// Metadata matches chunks whose metadata has every key with an equal
	// value. Numbers are compared by value whatever their type, and slices
	// element by element, so values decoded from JSON match the originals.
	Metadata map[string]any
}

// Match reports whether c satisfies the filter.
func (f Filter) Match(c *Chunk) bool {
	if len(f.Sources) > 0 && !slices.Contains(f.Sources, c.Source) {
		return false
	}
	if !strings.HasPrefix(c.Source, f.SourcePrefix) {
		return false
	}
	for k, want := range f.Metadata {
		got, ok := c.Metadata[k]
		if !ok || !equalValues(got, want) {
			return false
		}
	}
	return true
}

// equalValues compares metadata values, treating numbers of any type as
// equal when their values are and slices as equal when their elements are.
func equalValues(a, b any) bool {
	if x, ok := toFloat(a); ok {
		y, ok := toFloat(b)
		return ok && x == y
	}
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if va.Kind() == reflect.Slice && vb.Kind() == reflect.Slice {
		if va.Len() != vb.Len() {
			return false
		}
		for i := range va.Len() {
			if !equalValues(va.Index(i).Interface(), vb.Index(i).Interface()) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a, b)
}

func toFloat(v any) (float64, bool) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return float64(rv.Uint()), true
	case reflect.Float32, reflect.Float64:
		return rv.Float(), true
	}
	return 0, false
}
