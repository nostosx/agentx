package rag

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/nostosx/agentx/internal/httpclient"

	"github.com/nostosx/agentx"
	"github.com/nostosx/agentx/internal/fsmatch"
)

// Loader loads documents.
//
// Load fails as a whole when any source cannot be read or parsed, rather
// than returning the documents it could load, so that Index.Sync never
// deletes a source because it was temporarily unreadable.
type Loader interface {
	Load(ctx context.Context) ([]*Document, error)
}

// LoaderFunc adapts a function to Loader.
type LoaderFunc func(ctx context.Context) ([]*Document, error)

// Load calls f.
func (f LoaderFunc) Load(ctx context.Context) ([]*Document, error) { return f(ctx) }

// LoadAll loads the documents of every loader in order.
func LoadAll(ctx context.Context, loaders ...Loader) ([]*Document, error) {
	var docs []*Document
	for _, l := range loaders {
		d, err := l.Load(ctx)
		if err != nil {
			return nil, err
		}
		docs = append(docs, d...)
	}
	return docs, nil
}

// DefaultMaxFileSize is the default size limit of FSLoader and URLLoader.
const DefaultMaxFileSize = 10 << 20

// FSLoader loads the files of a file system, such as os.DirFS(dir), that
// have a parser for their extension. Directories named .git are skipped.
//
// Document sources are SourcePrefix followed by the slash-separated path
// relative to Root, which is also stored as MetaPath.
type FSLoader struct {
	FS fs.FS
	// Root is the directory to load, relative to FS. Defaults to ".".
	Root string
	// Include, when non-empty, loads only files matching one of these
	// globs. Patterns without a slash match file names; others match paths
	// relative to Root. "**" matches any number of directories and "{a,b}"
	// alternatives.
	Include []string
	// Exclude skips files and directories matching one of these globs,
	// which work like Include.
	Exclude []string
	// GitIgnore skips paths excluded by .gitignore files within Root.
	// Ignore files above Root and git's global excludes do not apply.
	GitIgnore bool
	// Parsers maps lower-case file extensions, such as ".md", to parsers.
	// Defaults to DefaultParsers. Files without a parser are skipped.
	Parsers map[string]Parser
	// MaxFileSize skips larger files. Defaults to DefaultMaxFileSize.
	MaxFileSize int64
	// SourcePrefix is prepended to document sources, for example to tell
	// apart several directories in one index.
	SourcePrefix string
}

// Load implements Loader.
func (l *FSLoader) Load(ctx context.Context) ([]*Document, error) {
	if l.FS == nil {
		return nil, errors.New("rag: FSLoader requires a file system")
	}
	root := l.Root
	if root == "" {
		root = "."
	}
	include, err := compileGlobs(l.Include)
	if err != nil {
		return nil, err
	}
	exclude, err := compileGlobs(l.Exclude)
	if err != nil {
		return nil, err
	}
	sub, err := fs.Sub(l.FS, root)
	if err != nil {
		return nil, err
	}
	parsers := l.Parsers
	if parsers == nil {
		parsers = DefaultParsers()
	}
	maxSize := l.MaxFileSize
	if maxSize <= 0 {
		maxSize = DefaultMaxFileSize
	}
	var ig *fsmatch.Ignorer
	if l.GitIgnore {
		ig = fsmatch.NewIgnorer(sub)
	}
	var docs []*Document
	err = fs.WalkDir(sub, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if d.IsDir() && d.Name() == ".git" {
			return fs.SkipDir
		}
		if matchAny(exclude, name) || (ig != nil && ig.Ignored(name, d.IsDir())) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || (len(include) > 0 && !matchAny(include, name)) {
			return nil
		}
		p, ok := parsers[strings.ToLower(path.Ext(name))]
		if !ok || p == nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxSize {
			return nil
		}
		data, err := fs.ReadFile(sub, name)
		if err != nil {
			return err
		}
		parsed, err := p.Parse(ctx, l.SourcePrefix+name, data)
		if err != nil {
			return err
		}
		for _, doc := range parsed {
			if doc.Metadata == nil {
				doc.Metadata = map[string]any{}
			}
			doc.Metadata[MetaPath] = name
		}
		docs = append(docs, parsed...)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("rag: load %s: %w", root, err)
	}
	return docs, nil
}

// pathGlob is a compiled Include or Exclude pattern.
type pathGlob struct {
	glob fsmatch.Glob
	// base patterns have no slash and match file names.
	base bool
}

func compileGlobs(patterns []string) ([]pathGlob, error) {
	var out []pathGlob
	for _, p := range patterns {
		g, err := fsmatch.CompileGlob(p)
		if err != nil {
			return nil, fmt.Errorf("rag: %w", err)
		}
		out = append(out, pathGlob{glob: g, base: !strings.Contains(strings.Trim(p, "/"), "/")})
	}
	return out, nil
}

func matchAny(globs []pathGlob, name string) bool {
	for _, g := range globs {
		target := name
		if g.base {
			target = path.Base(name)
		}
		if g.glob.Match(target) {
			return true
		}
	}
	return false
}

// URLLoader loads documents over HTTP. The parser is chosen by media type:
// HTML, Markdown, JSON, JSON Lines and CSV; for other text types, no type
// or application/octet-stream, by the extension of the final URL path as
// in DefaultParsers, falling back to plain text for text types. Other
// media types are an error unless Parser is set.
//
// Document sources are the URLs as given; the final URL after redirects is
// stored as MetaURL and the media type as MetaContentType. The URLs are
// chosen by the application, so unlike the web_fetch tool, URLLoader does
// not refuse private network addresses; set Client to restrict them.
type URLLoader struct {
	URLs []string
	// Client sends the requests. Defaults to a client with a 60 second
	// timeout.
	Client *http.Client
	// Header is added to every request.
	Header http.Header
	// Parser, when set, parses every response.
	Parser Parser
	// MaxBytes fails on larger responses. Defaults to DefaultMaxFileSize.
	MaxBytes int64
}

var defaultHTTPClient = &http.Client{Timeout: 60 * time.Second}

// Load implements Loader.
func (l *URLLoader) Load(ctx context.Context) ([]*Document, error) {
	var docs []*Document
	for _, u := range l.URLs {
		d, err := l.load(ctx, u)
		if err != nil {
			return nil, fmt.Errorf("rag: load %s: %w", u, err)
		}
		docs = append(docs, d...)
	}
	return docs, nil
}

func (l *URLLoader) load(ctx context.Context, rawURL string) ([]*Document, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("only http and https URLs are supported")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "agentx/"+agentx.Version)
	req.Header.Set("Accept", "text/html,text/markdown,text/plain,application/json;q=0.9,*/*;q=0.5")
	httpclient.AddHeaders(req.Header, l.Header)
	client := l.Client
	if client == nil {
		client = defaultHTTPClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	maxBytes := l.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxFileSize
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("response exceeds %d bytes", maxBytes)
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	p := l.Parser
	if p == nil {
		if p = parserFor(mediaType, resp.Request.URL.Path); p == nil {
			return nil, fmt.Errorf("unsupported content type %q", mediaType)
		}
	}
	docs, err := p.Parse(ctx, rawURL, body)
	if err != nil {
		return nil, err
	}
	for _, doc := range docs {
		if doc.Metadata == nil {
			doc.Metadata = map[string]any{}
		}
		doc.Metadata[MetaURL] = resp.Request.URL.String()
		if mediaType != "" {
			doc.Metadata[MetaContentType] = mediaType
		}
	}
	return docs, nil
}

// parserFor chooses a parser by media type and URL path, or returns nil.
func parserFor(mediaType, urlPath string) Parser {
	switch mediaType {
	case "text/html", "application/xhtml+xml":
		return HTMLParser{}
	case "text/markdown", "text/x-markdown":
		return TextParser{Format: "markdown"}
	case "application/json":
		return TextParser{Format: "json"}
	case "application/x-ndjson", "application/jsonl", "application/x-jsonlines":
		return JSONParser{Lines: true}
	case "text/csv":
		return CSVParser{}
	case "text/tab-separated-values":
		return CSVParser{Comma: '\t'}
	}
	textual := mediaType == "" || strings.HasPrefix(mediaType, "text/") || mediaType == "application/octet-stream" ||
		strings.HasSuffix(mediaType, "+json") || strings.HasSuffix(mediaType, "+xml") ||
		mediaType == "application/xml" || mediaType == "application/javascript" || mediaType == "application/yaml"
	if !textual {
		return nil
	}
	if p, ok := DefaultParsers()[strings.ToLower(path.Ext(urlPath))]; ok {
		return p
	}
	if mediaType == "application/octet-stream" {
		return nil
	}
	return TextParser{}
}
