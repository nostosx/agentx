package rag

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nostosx/agentx/internal/htmltext"
)

// Parser turns the bytes of a source into documents. Parsers set the
// document IDs and sources from source.
//
// PDF, Office documents and other binary formats are not supported by the
// standard library; implement Parser with a library of your choice and
// register it in FSLoader.Parsers or set it as URLLoader.Parser.
type Parser interface {
	Parse(ctx context.Context, source string, data []byte) ([]*Document, error)
}

// ParserFunc adapts a function to Parser.
type ParserFunc func(ctx context.Context, source string, data []byte) ([]*Document, error)

// Parse calls f.
func (f ParserFunc) Parse(ctx context.Context, source string, data []byte) ([]*Document, error) {
	return f(ctx, source, data)
}

// TextParser parses text into a single document. It drops a UTF-8 byte
// order mark, converts CRLF line endings to LF and replaces invalid UTF-8.
// Content with NUL bytes is treated as binary and yields no document.
type TextParser struct {
	// Format is stored as MetaFormat. Defaults to "text".
	Format string
	// Language is stored as MetaLanguage when non-empty.
	Language string
}

// Parse implements Parser.
func (p TextParser) Parse(_ context.Context, source string, data []byte) ([]*Document, error) {
	text, ok := decodeText(data)
	if !ok {
		return nil, nil
	}
	format := p.Format
	if format == "" {
		format = "text"
	}
	meta := map[string]any{MetaFormat: format}
	if p.Language != "" {
		meta[MetaLanguage] = p.Language
	}
	return []*Document{{ID: source, Source: source, Content: text, Metadata: meta}}, nil
}

// decodeText normalizes text, reporting false for binary content.
func decodeText(data []byte) (string, bool) {
	if bytes.IndexByte(data, 0) >= 0 {
		return "", false
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	s := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	return s, true
}

// HTMLParser converts HTML to plain text with the converter of the
// web_fetch tool and stores the page title as MetaTitle.
type HTMLParser struct{}

// Parse implements Parser.
func (HTMLParser) Parse(_ context.Context, source string, data []byte) ([]*Document, error) {
	raw, ok := decodeText(data)
	if !ok {
		return nil, nil
	}
	meta := map[string]any{MetaFormat: "html"}
	if title := htmltext.Title(raw); title != "" {
		meta[MetaTitle] = title
	}
	return []*Document{{ID: source, Source: source, Content: htmltext.Text(raw), Metadata: meta}}, nil
}

// JSONParser parses JSON records. A top-level array yields one record per
// element and any other value a single record; with Lines, each non-blank
// line is a record. Fields are named by dot-separated paths into objects,
// such as "author.name".
//
// Document IDs are the source followed by "#" and the IDField value, or
// the 1-based record number, which is also stored as MetaRecord. Records
// without content are skipped.
type JSONParser struct {
	// Lines parses JSON Lines (NDJSON).
	Lines bool
	// ContentFields form the content. One field yields its value, several
	// yield "field: value" lines. Strings are used as-is and other values
	// as JSON. When empty, the content is the whole record as JSON.
	ContentFields []string
	// MetadataFields are copied to the document metadata under their
	// paths.
	MetadataFields []string
	// IDField names a field that identifies records.
	IDField string
}

// Parse implements Parser.
func (p JSONParser) Parse(ctx context.Context, source string, data []byte) ([]*Document, error) {
	var records []any
	if p.Lines {
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(nil, len(data)+1)
		line := 0
		for sc.Scan() {
			line++
			b := bytes.TrimSpace(sc.Bytes())
			if len(b) == 0 {
				records = append(records, nil)
				continue
			}
			var v any
			if err := json.Unmarshal(b, &v); err != nil {
				return nil, fmt.Errorf("rag: parse %s line %d: %w", source, line, err)
			}
			records = append(records, v)
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("rag: parse %s: %w", source, err)
		}
	} else {
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("rag: parse %s: %w", source, err)
		}
		if arr, ok := v.([]any); ok {
			records = arr
		} else {
			records = []any{v}
		}
	}
	var docs []*Document
	for i, rec := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if rec == nil {
			continue
		}
		content := p.content(rec)
		if strings.TrimSpace(content) == "" {
			continue
		}
		meta := map[string]any{MetaFormat: "json", MetaRecord: i + 1}
		for _, f := range p.MetadataFields {
			if v, ok := lookup(rec, f); ok {
				meta[f] = v
			}
		}
		key := strconv.Itoa(i + 1)
		if p.IDField != "" {
			if v, ok := lookup(rec, p.IDField); ok && v != nil {
				key = valueString(v)
			}
		}
		docs = append(docs, &Document{ID: source + "#" + key, Source: source, Content: content, Metadata: meta})
	}
	return docs, nil
}

func (p JSONParser) content(rec any) string {
	switch len(p.ContentFields) {
	case 0:
		return valueString(rec)
	case 1:
		v, _ := lookup(rec, p.ContentFields[0])
		return valueString(v)
	}
	var lines []string
	for _, f := range p.ContentFields {
		if v, ok := lookup(rec, f); ok && v != nil {
			if s := valueString(v); s != "" {
				lines = append(lines, f+": "+s)
			}
		}
	}
	return strings.Join(lines, "\n")
}

// lookup follows a dot-separated path into nested objects.
func lookup(v any, path string) (any, bool) {
	for key := range strings.SplitSeq(path, ".") {
		obj, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if v, ok = obj[key]; !ok {
			return nil, false
		}
	}
	return v, true
}

// valueString renders a JSON value: strings as-is, null as "" and other
// values as JSON.
func valueString(v any) string {
	switch v := v.(type) {
	case nil:
		return ""
	case string:
		return v
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// CSVParser parses CSV with a header row into one document per row. The
// content is "column: value" lines of the content columns with non-empty
// values. Document IDs are the source followed by "#" and the IDColumn
// value, or the 1-based row number, which is also stored as MetaRecord.
type CSVParser struct {
	// Comma is the field delimiter. Defaults to ','.
	Comma rune
	// ContentColumns form the content. Defaults to every column.
	ContentColumns []string
	// MetadataColumns are copied to the document metadata as strings.
	MetadataColumns []string
	// IDColumn names a column that identifies rows.
	IDColumn string
}

// Parse implements Parser.
func (p CSVParser) Parse(ctx context.Context, source string, data []byte) ([]*Document, error) {
	text, ok := decodeText(data)
	if !ok {
		return nil, fmt.Errorf("rag: parse %s: not text", source)
	}
	r := csv.NewReader(strings.NewReader(text))
	if p.Comma != 0 {
		r.Comma = p.Comma
	}
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if errors.Is(err, io.EOF) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("rag: parse %s: %w", source, err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.TrimSpace(h)] = i
	}
	content := p.ContentColumns
	if len(content) == 0 {
		content = header
	}
	for _, c := range append(append(append([]string(nil), content...), p.MetadataColumns...), p.IDColumn) {
		if _, ok := col[strings.TrimSpace(c)]; c != "" && !ok {
			return nil, fmt.Errorf("rag: parse %s: no column %q", source, c)
		}
	}
	field := func(row []string, name string) string {
		if i := col[strings.TrimSpace(name)]; i < len(row) {
			return strings.TrimSpace(row[i])
		}
		return ""
	}
	var docs []*Document
	for n := 1; ; n++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		row, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("rag: parse %s: %w", source, err)
		}
		var lines []string
		for _, c := range content {
			if v := field(row, c); v != "" {
				lines = append(lines, strings.TrimSpace(c)+": "+v)
			}
		}
		if len(lines) == 0 {
			continue
		}
		meta := map[string]any{MetaFormat: "csv", MetaRecord: n}
		for _, c := range p.MetadataColumns {
			meta[c] = field(row, c)
		}
		key := strconv.Itoa(n)
		if p.IDColumn != "" {
			if v := field(row, p.IDColumn); v != "" {
				key = v
			}
		}
		docs = append(docs, &Document{ID: source + "#" + key, Source: source, Content: strings.Join(lines, "\n"), Metadata: meta})
	}
	return docs, nil
}

// DefaultParsers returns the parsers FSLoader uses by file extension: text
// (.txt, .text, .rst, .log, .yaml, .yml, .toml, .xml, .ini, .cfg),
// Markdown (.md, .markdown, .mdx), HTML (.html, .htm, .xhtml), JSON as text
// (.json), JSON Lines with whole records (.jsonl, .ndjson), CSV (.csv) and
// TSV (.tsv), and Go, Python, JavaScript and TypeScript code.
func DefaultParsers() map[string]Parser {
	m := map[string]Parser{}
	for _, ext := range []string{".txt", ".text", ".rst", ".log", ".yaml", ".yml", ".toml", ".xml", ".ini", ".cfg"} {
		m[ext] = TextParser{}
	}
	for _, ext := range []string{".md", ".markdown", ".mdx"} {
		m[ext] = TextParser{Format: "markdown"}
	}
	for _, ext := range []string{".html", ".htm", ".xhtml"} {
		m[ext] = HTMLParser{}
	}
	m[".json"] = TextParser{Format: "json"}
	m[".jsonl"] = JSONParser{Lines: true}
	m[".ndjson"] = JSONParser{Lines: true}
	m[".csv"] = CSVParser{}
	m[".tsv"] = CSVParser{Comma: '\t'}
	for ext, lang := range map[string]string{
		".go": "go", ".py": "python",
		".js": "javascript", ".mjs": "javascript", ".cjs": "javascript", ".jsx": "javascript",
		".ts": "typescript", ".tsx": "typescript",
	} {
		m[ext] = TextParser{Format: "code", Language: lang}
	}
	return m
}
