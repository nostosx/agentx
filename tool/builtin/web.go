package builtin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/nostosx/kitx/utf8text"

	"github.com/nostosx/agentx/internal/httpclient"

	"github.com/nostosx/agentx"
	"github.com/nostosx/agentx/internal/htmltext"
	"github.com/nostosx/agentx/tool"
)

// maxFetchBytes caps the response body read by web_fetch.
const maxFetchBytes = 5 << 20

type fetchArgs struct {
	URL string `json:"url" jsonschema:"The http or https URL to fetch."`
}

// WebFetch returns the web_fetch tool. HTML is converted to plain text.
//
// Without Config.HTTPClient, it refuses loopback, private, link-local and
// shared (CGNAT) addresses unless Config.AllowPrivateNetwork is set, and
// gives up on a fetch after 60 seconds. When the environment configures a
// proxy, destinations are checked by name before the request only; see
// publicClient.
func WebFetch(cfg Config) tool.Tool {
	e := newEnv(cfg)
	client, guarded := e.HTTPClient, false
	if client == nil {
		guarded = !e.AllowPrivateNetwork
		client = fetchClient(guarded)
	}
	if e.CheckRedirect != nil {
		client = withRedirectCheck(client, e.CheckRedirect)
	}
	return tool.Func("web_fetch",
		"Fetch a web page or text document by URL. HTML is converted to plain text; long content is truncated.",
		func(ctx context.Context, in fetchArgs) (*tool.Result, error) {
			result, err := e.fetch(ctx, client, guarded, in.URL)
			if result != nil {
				result.Content = utf8text.Truncate(result.Content, e.MaxOutput, "\n... [content truncated]")
			}
			return result, err
		}, tool.ReadOnly())
}

// withRedirectCheck returns a copy of client that calls check after the
// client's own redirect policy, or after Go's default limit of 10
// redirects if it has none.
func withRedirectCheck(client *http.Client, check func(*http.Request, []*http.Request) error) *http.Client {
	c := *client
	own := c.CheckRedirect
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if own != nil {
			if err := own(req, via); err != nil {
				return err
			}
		} else if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return check(req, via)
	}
	return &c
}

func (e *env) fetch(ctx context.Context, client *http.Client, guarded bool, rawURL string) (*tool.Result, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return tool.Errorf("invalid URL %q: only absolute http and https URLs are supported", rawURL), nil
	}
	if guarded {
		if err := checkHost(ctx, u.Hostname()); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return tool.Errorf("fetch %s: %v", u, err), nil
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "agentx/"+agentx.Version)
	req.Header.Set("Accept", "text/html,text/plain,text/markdown,application/json;q=0.9,*/*;q=0.5")
	resp, err := httpclient.ClientOr(client).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return tool.Errorf("fetch %s: %v", u, err), nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes))
	if err != nil {
		return tool.Errorf("read %s: %v", u, err), nil
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return tool.Errorf("fetch %s: %s\n%s", u, resp.Status, utf8text.Truncate(string(body), 1000, "...")), nil
	}

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = http.DetectContentType(body)
	}
	mediaType, _, _ := mime.ParseMediaType(ct)
	var text string
	switch {
	case mediaType == "text/html" || mediaType == "application/xhtml+xml":
		text = htmltext.Text(string(body))
	case isText(mediaType):
		text = strings.ToValidUTF8(string(body), "�")
	default:
		return tool.Errorf("fetch %s: unsupported content type %q", u, ct), nil
	}
	header := fmt.Sprintf("URL: %s\nContent-Type: %s\n\n", resp.Request.URL, mediaType)
	return tool.Text(header + text), nil
}

func isText(mediaType string) bool {
	switch {
	case strings.HasPrefix(mediaType, "text/"),
		strings.HasSuffix(mediaType, "+json"),
		strings.HasSuffix(mediaType, "+xml"):
		return true
	}
	switch mediaType {
	case "application/json", "application/xml", "application/javascript", "application/x-yaml", "application/yaml":
		return true
	}
	return false
}
