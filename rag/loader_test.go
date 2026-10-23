package rag

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func TestTextParser(t *testing.T) {
	docs, err := TextParser{Format: "code", Language: "go"}.Parse(t.Context(), "a.go", []byte("\xef\xbb\xbfline1\r\nline2 \xff"))
	if err != nil || len(docs) != 1 {
		t.Fatalf("Parse = %v, %v", docs, err)
	}
	d := docs[0]
	if d.ID != "a.go" || d.Source != "a.go" || d.Content != "line1\nline2 �" ||
		d.Metadata[MetaFormat] != "code" || d.Metadata[MetaLanguage] != "go" {
		t.Fatalf("doc = %+v", d)
	}
	docs, err = TextParser{}.Parse(t.Context(), "bin", []byte("a\x00b"))
	if err != nil || docs != nil {
		t.Fatalf("binary = %v, %v", docs, err)
	}
	docs, _ = TextParser{}.Parse(t.Context(), "t", []byte("x"))
	if docs[0].Metadata[MetaFormat] != "text" {
		t.Fatalf("default format = %v", docs[0].Metadata)
	}
}

func TestHTMLParser(t *testing.T) {
	docs, err := HTMLParser{}.Parse(t.Context(), "p.html", []byte("<html><head><title>Docs &amp; more</title><script>x()</script></head><body><h1>Hi</h1><p>Body</p></body></html>"))
	if err != nil || len(docs) != 1 {
		t.Fatalf("Parse = %v, %v", docs, err)
	}
	if docs[0].Content != "Docs & more\nHi\nBody" || docs[0].Metadata[MetaTitle] != "Docs & more" || docs[0].Metadata[MetaFormat] != "html" {
		t.Fatalf("doc = %+v", docs[0])
	}
}

func TestJSONParser(t *testing.T) {
	data := []byte(`[{"id":"a","title":"T1","body":"B1","meta":{"lang":"en"}},{"id":"b","title":"T2"},{"other":1},null]`)
	p := JSONParser{ContentFields: []string{"title", "body"}, MetadataFields: []string{"meta.lang"}, IDField: "id"}
	docs, err := p.Parse(t.Context(), "f.json", data)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Fatalf("got %d docs", len(docs))
	}
	if docs[0].ID != "f.json#a" || docs[0].Source != "f.json" || docs[0].Content != "title: T1\nbody: B1" ||
		docs[0].Metadata["meta.lang"] != "en" || docs[0].Metadata[MetaRecord] != 1 {
		t.Fatalf("doc 0 = %+v", docs[0])
	}
	if docs[1].ID != "f.json#b" || docs[1].Content != "title: T2" {
		t.Fatalf("doc 1 = %+v", docs[1])
	}

	lines := []byte("{\"text\":\"one\",\"n\":1}\n\n{\"text\":\"three\"}\n")
	docs, err = JSONParser{Lines: true, ContentFields: []string{"text"}}.Parse(t.Context(), "f.jsonl", lines)
	if err != nil || len(docs) != 2 {
		t.Fatalf("lines = %v, %v", docs, err)
	}
	if docs[1].ID != "f.jsonl#3" || docs[1].Content != "three" || docs[1].Metadata[MetaRecord] != 3 {
		t.Fatalf("line doc = %+v", docs[1])
	}
	// Without content fields the whole record is the content.
	docs, _ = JSONParser{}.Parse(t.Context(), "o.json", []byte(`{"b":2,"a":"x"}`))
	if len(docs) != 1 || docs[0].Content != `{"a":"x","b":2}` || docs[0].ID != "o.json#1" {
		t.Fatalf("object = %+v", docs)
	}
	if _, err := (JSONParser{Lines: true}).Parse(t.Context(), "bad.jsonl", []byte("{}\n{bad")); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("bad line error = %v", err)
	}
	if _, err := (JSONParser{}).Parse(t.Context(), "bad.json", []byte("{")); err == nil {
		t.Fatal("bad JSON accepted")
	}
}

func TestCSVParser(t *testing.T) {
	data := []byte("sku,name,desc,team\nA1,Widget,Small part,core\nB2,Gadget,,ext\nC3,,,core\n")
	docs, err := CSVParser{ContentColumns: []string{"name", "desc"}, MetadataColumns: []string{"team"}, IDColumn: "sku"}.Parse(t.Context(), "p.csv", data)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Fatalf("got %d docs", len(docs))
	}
	if docs[0].ID != "p.csv#A1" || docs[0].Content != "name: Widget\ndesc: Small part" || docs[0].Metadata["team"] != "core" || docs[0].Metadata[MetaRecord] != 1 {
		t.Fatalf("row 1 = %+v", docs[0])
	}
	if docs[1].Content != "name: Gadget" {
		t.Fatalf("row 2 = %+v", docs[1])
	}
	docs, err = CSVParser{Comma: '\t'}.Parse(t.Context(), "p.tsv", []byte("a\tb\n1\t2\n"))
	if err != nil || len(docs) != 1 || docs[0].Content != "a: 1\nb: 2" || docs[0].ID != "p.tsv#1" {
		t.Fatalf("tsv = %+v, %v", docs, err)
	}
	if _, err := (CSVParser{ContentColumns: []string{"missing"}}).Parse(t.Context(), "p.csv", data); err == nil {
		t.Fatal("missing column accepted")
	}
	if docs, err := (CSVParser{}).Parse(t.Context(), "e.csv", nil); err != nil || docs != nil {
		t.Fatalf("empty = %v, %v", docs, err)
	}
}

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"docs/.gitignore":        {Data: []byte("drafts/\n*.tmp.md\n")},
		"docs/guide.md":          {Data: []byte("# Guide\n\nHello")},
		"docs/notes.tmp.md":      {Data: []byte("scratch")},
		"docs/drafts/wip.md":     {Data: []byte("wip")},
		"docs/api/index.html":    {Data: []byte("<title>API</title><p>Endpoints</p>")},
		"docs/data/items.jsonl":  {Data: []byte(`{"x":1}` + "\n" + `{"x":2}`)},
		"docs/img/logo.png":      {Data: []byte("\x89PNG\x00")},
		"docs/main.go":           {Data: []byte("package main")},
		"docs/big.txt":           {Data: []byte(strings.Repeat("b", 100))},
		"docs/.git/HEAD":         {Data: []byte("ref")},
		"docs/.git/config.txt":   {Data: []byte("x")},
		"docs/vendor/lib/lib.go": {Data: []byte("package lib")},
		"other.md":               {Data: []byte("outside root")},
	}
}

func sources(docs []*Document) []string {
	var out []string
	for _, d := range docs {
		out = append(out, d.Source)
	}
	slices.Sort(out)
	return out
}

func TestFSLoader(t *testing.T) {
	l := &FSLoader{FS: testFS(), Root: "docs", GitIgnore: true, MaxFileSize: 50, Exclude: []string{"vendor"}, SourcePrefix: "kb:"}
	docs, err := l.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"kb:api/index.html", "kb:data/items.jsonl", "kb:data/items.jsonl", "kb:guide.md", "kb:main.go"}
	if got := sources(docs); !slices.Equal(got, want) {
		t.Fatalf("sources = %q, want %q", got, want)
	}
	for _, d := range docs {
		switch d.Source {
		case "kb:guide.md":
			if d.Metadata[MetaFormat] != "markdown" || d.Metadata[MetaPath] != "guide.md" || d.ID != "kb:guide.md" {
				t.Errorf("guide = %+v", d)
			}
		case "kb:main.go":
			if d.Metadata[MetaLanguage] != "go" {
				t.Errorf("main.go = %+v", d)
			}
		case "kb:api/index.html":
			if d.Metadata[MetaTitle] != "API" || d.Content != "API\nEndpoints" {
				t.Errorf("html = %+v", d)
			}
		}
	}

	// Without GitIgnore the ignored files load; Include narrows the files.
	l = &FSLoader{FS: testFS(), Root: "docs", Include: []string{"*.md"}}
	docs, err = l.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := sources(docs); !slices.Equal(got, []string{"drafts/wip.md", "guide.md", "notes.tmp.md"}) {
		t.Fatalf("include = %q", got)
	}
	l = &FSLoader{FS: testFS(), Include: []string{"docs/**/*.{md,html}"}, Exclude: []string{"docs/drafts/**"}}
	docs, _ = l.Load(t.Context())
	if got := sources(docs); !slices.Equal(got, []string{"docs/api/index.html", "docs/guide.md", "docs/notes.tmp.md"}) {
		t.Fatalf("path globs = %q", got)
	}
}

func TestFSLoaderCustomParserAndErrors(t *testing.T) {
	pdf := ParserFunc(func(_ context.Context, source string, data []byte) ([]*Document, error) {
		return []*Document{{ID: source, Source: source, Content: "pdf text " + string(data)}}, nil
	})
	fsys := fstest.MapFS{"a.pdf": {Data: []byte("raw")}, "b.md": {Data: []byte("md")}}
	docs, err := (&FSLoader{FS: fsys, Parsers: map[string]Parser{".pdf": pdf}}).Load(t.Context())
	if err != nil || len(docs) != 1 || docs[0].Content != "pdf text raw" || docs[0].Metadata[MetaPath] != "a.pdf" {
		t.Fatalf("custom parser = %+v, %v", docs, err)
	}
	failing := ParserFunc(func(context.Context, string, []byte) ([]*Document, error) { return nil, errors.New("boom") })
	if _, err := (&FSLoader{FS: fsys, Parsers: map[string]Parser{".md": failing}}).Load(t.Context()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("parse error = %v", err)
	}
	if _, err := (&FSLoader{}).Load(t.Context()); err == nil {
		t.Fatal("missing FS accepted")
	}
	if _, err := (&FSLoader{FS: fsys, Include: []string{"[x"}}).Load(t.Context()); err == nil {
		t.Fatal("invalid glob accepted")
	}
	if _, err := (&FSLoader{FS: fsys, Root: "missing"}).Load(t.Context()); err == nil {
		t.Fatal("missing root accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := (&FSLoader{FS: fsys}).Load(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled = %v", err)
	}
}

func TestURLLoader(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Token") != "secret" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<title>Page</title><p>Hello web</p>"))
	})
	mux.HandleFunc("/readme.md", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("# Readme\n\ntext"))
	})
	mux.HandleFunc("/old", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/readme.md", http.StatusFound)
	})
	mux.HandleFunc("/rows", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte("{\"a\":1}\n{\"a\":2}\n"))
	})
	mux.HandleFunc("/image", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG"))
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(strings.Repeat("x", 100)))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	l := &URLLoader{URLs: []string{srv.URL + "/page", srv.URL + "/old", srv.URL + "/rows"}, Header: http.Header{"X-Token": {"secret"}}}
	docs, err := l.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 4 {
		t.Fatalf("got %d docs", len(docs))
	}
	if docs[0].Source != srv.URL+"/page" || docs[0].Content != "Page\nHello web" || docs[0].Metadata[MetaTitle] != "Page" || docs[0].Metadata[MetaContentType] != "text/html" {
		t.Fatalf("page = %+v", docs[0])
	}
	if docs[1].Source != srv.URL+"/old" || docs[1].Metadata[MetaURL] != srv.URL+"/readme.md" || docs[1].Metadata[MetaFormat] != "markdown" {
		t.Fatalf("redirected markdown = %+v", docs[1])
	}
	if docs[3].ID != srv.URL+"/rows#2" {
		t.Fatalf("rows = %+v", docs[3])
	}

	for _, tc := range []struct {
		loader *URLLoader
		want   string
	}{
		{&URLLoader{URLs: []string{srv.URL + "/page"}}, "401"},
		{&URLLoader{URLs: []string{srv.URL + "/image"}}, "unsupported content type"},
		{&URLLoader{URLs: []string{srv.URL + "/big"}, MaxBytes: 10}, "exceeds"},
		{&URLLoader{URLs: []string{"ftp://example.com/x"}}, "only http"},
	} {
		if _, err := tc.loader.Load(t.Context()); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: error = %v, want %q", tc.loader.URLs, err, tc.want)
		}
	}
	// A parser overrides content type detection.
	docs, err = (&URLLoader{URLs: []string{srv.URL + "/image"}, Parser: TextParser{Format: "raw"}, Client: srv.Client()}).Load(t.Context())
	if err != nil || len(docs) != 1 || docs[0].Metadata[MetaFormat] != "raw" || docs[0].Metadata[MetaContentType] != "image/png" {
		t.Fatalf("override = %+v, %v", docs, err)
	}
}

func TestParserFor(t *testing.T) {
	cases := []struct {
		mediaType, path string
		want            Parser
	}{
		{"text/markdown", "/x", TextParser{Format: "markdown"}},
		{"application/json", "/x", TextParser{Format: "json"}},
		{"text/csv", "/x", CSVParser{}},
		{"text/tab-separated-values", "/x", CSVParser{Comma: '\t'}},
		{"text/plain", "/main.py", TextParser{Format: "code", Language: "python"}},
		{"", "/notes", TextParser{}},
		{"application/octet-stream", "/a.md", TextParser{Format: "markdown"}},
		{"application/octet-stream", "/a.bin", nil},
		{"application/pdf", "/a.pdf", nil},
	}
	for _, c := range cases {
		if got := parserFor(c.mediaType, c.path); !reflect.DeepEqual(got, c.want) {
			t.Errorf("parserFor(%q, %q) = %#v, want %#v", c.mediaType, c.path, got, c.want)
		}
	}
}

func TestLoadAll(t *testing.T) {
	a := LoaderFunc(func(context.Context) ([]*Document, error) { return []*Document{{ID: "a"}}, nil })
	b := LoaderFunc(func(context.Context) ([]*Document, error) { return []*Document{{ID: "b"}}, nil })
	docs, err := LoadAll(t.Context(), a, b)
	if err != nil || len(docs) != 2 || docs[1].ID != "b" {
		t.Fatalf("LoadAll = %v, %v", docs, err)
	}
	bad := LoaderFunc(func(context.Context) ([]*Document, error) { return nil, errors.New("x") })
	if docs, err := LoadAll(t.Context(), a, bad); err == nil || docs != nil {
		t.Fatalf("LoadAll with failure = %v, %v", docs, err)
	}
}
