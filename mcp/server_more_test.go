package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nostosx/agentx/tool"
)

func TestLegacyServerRequestCancelled(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	handlerDone := make(chan error, 1)
	started := make(chan struct{})
	c := connectStdio(t, f.s, WithProtocolVersion(LegacyProtocolVersion), WithElicitation(func(ctx context.Context, _ *ElicitRequest) (*ElicitResult, error) {
		close(started)
		<-ctx.Done()
		handlerDone <- ctx.Err()
		return nil, ctx.Err()
	}))
	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() {
		_, err := c.CallTool(ctx, "ask", nil)
		errc <- err
	}()
	<-started
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Errorf("CallTool = %v", err)
	}
	// The server cancels its elicitation request when its handler is
	// cancelled.
	select {
	case err := <-handlerDone:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("client handler = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the server did not cancel its request")
	}
}

func TestLegacyURLElicitation(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		link := tool.Func("link", "Link an account.", func(ctx context.Context, _ struct{}) (string, error) {
			if calls.Add(1) == 1 {
				// The user must visit a page first; the server reports
				// completion once the page says so.
				go func() {
					time.Sleep(50 * time.Millisecond)
					_ = CompleteElicitation(ctx, "el-1")
				}()
				return "", URLElicitationRequired("Link your account first.",
					&ElicitRequest{Message: "Open the page", URL: "https://example.com/link", ElicitationID: "el-1"})
			}
			return "linked", nil
		})
		f := newFixture(t, &ServerOptions{Tools: []tool.Tool{link}})
		var urls []string
		c := connectStdio(t, f.s, WithProtocolVersion(LegacyProtocolVersion), WithElicitation(func(_ context.Context, req *ElicitRequest) (*ElicitResult, error) {
			urls = append(urls, req.URL)
			return &ElicitResult{Action: ElicitAccept}, nil
		}, ElicitationURL))
		if out, isErr := callText(t, c, "link", `{}`); out != "linked" || isErr {
			t.Fatalf("link = %q", out)
		}
		if !slices.Equal(urls, []string{"https://example.com/link"}) || calls.Load() != 2 {
			t.Errorf("urls %v, calls %d", urls, calls.Load())
		}
	})
}

func TestURLModeElicitBothGenerations(t *testing.T) {
	visit := tool.Func("visit", "Visit a page.", func(ctx context.Context, _ struct{}) (string, error) {
		res, err := Elicit(ctx, &ElicitRequest{Mode: ElicitationURL, Message: "Please visit", URL: "https://example.com/pay"})
		if err != nil {
			return "", err
		}
		return res.Action, nil
	})
	for _, g := range generations {
		t.Run(g.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, &ServerOptions{Tools: []tool.Tool{visit}})
			var got *ElicitRequest
			c := connectStdio(t, f.s, append(slices.Clone(g.opts), WithElicitation(func(_ context.Context, req *ElicitRequest) (*ElicitResult, error) {
				got = req
				return &ElicitResult{Action: ElicitAccept}, nil
			}, ElicitationURL))...)
			if out, _ := callText(t, c, "visit", `{}`); out != ElicitAccept {
				t.Fatalf("visit = %q", out)
			}
			if got == nil || got.URL != "https://example.com/pay" || (g.name == "modern") != (got.ElicitationID == "") {
				t.Errorf("request = %+v", got)
			}
			// A form-only client lacks URL mode.
			c2 := connectStdio(t, f.s, append(slices.Clone(g.opts), WithElicitation(func(context.Context, *ElicitRequest) (*ElicitResult, error) {
				return &ElicitResult{Action: ElicitAccept}, nil
			}))...)
			_, err := c2.CallTool(t.Context(), "visit", nil)
			if e, ok := errors.AsType[*Error](err); !ok || e.Code != CodeMissingRequiredClientCapability || !strings.Contains(string(e.Data), `"url"`) {
				t.Errorf("form-only client: %v", err)
			}
		})
	}
}

// staticSource is a ResourceSource, ResourceTemplateSource and
// PromptSource.
type staticSource struct{}

func (staticSource) Resources(context.Context) ([]Resource, error) {
	return []Resource{{URI: "mem://a", Name: "a"}}, nil
}

func (staticSource) ReadResource(_ context.Context, uri string) ([]ResourceContents, error) {
	if uri == "mem://a" {
		return []ResourceContents{{URI: uri, Blob: "AAEC", MimeType: "application/octet-stream"}}, nil
	}
	return nil, ErrResourceNotFound
}

func (staticSource) ResourceTemplates(context.Context) ([]ResourceTemplate, error) {
	return []ResourceTemplate{{URITemplate: "mem://{id}", Name: "mem"}}, nil
}

func (staticSource) Prompts(context.Context) ([]Prompt, error) {
	return []Prompt{{Name: "dyn"}}, nil
}

func (staticSource) GetPrompt(ctx context.Context, name string, _ map[string]string) (*GetPromptResult, error) {
	switch name {
	case "dyn":
		return &GetPromptResult{Messages: []PromptMessage{{Role: "user", Content: Content{Type: "text", Text: "dynamic"}}}}, nil
	case "ask":
		res, err := Elicit(ctx, &ElicitRequest{Message: "topic?", RequestedSchema: json.RawMessage(
			`{"type":"object","properties":{"topic":{"type":"string"}}}`)})
		if err != nil {
			return nil, err
		}
		return &GetPromptResult{Messages: []PromptMessage{{Role: "user", Content: Content{Type: "text", Text: res.Content["topic"].(string)}}}}, nil
	case "panic":
		panic("prompt bug")
	}
	return nil, ErrPromptNotFound
}

func TestServerSources(t *testing.T) {
	copts := []ClientOption{WithElicitation(func(context.Context, *ElicitRequest) (*ElicitResult, error) {
		return &ElicitResult{Action: ElicitAccept, Content: map[string]any{"topic": "go"}}, nil
	})}
	opts := &ServerOptions{Resources: staticSource{}, Prompts: staticSource{}}
	forEach(t, opts, copts, func(t *testing.T, f *srvFixture, c *Client, g generation) {
		ctx := t.Context()
		res, err := c.ListResources(ctx)
		if err != nil || len(res) != 2 || res[1].URI != "mem://a" {
			t.Errorf("ListResources = %+v, %v", res, err)
		}
		contents, err := c.ReadResource(ctx, "mem://a")
		if err != nil || contents[0].Blob != "AAEC" {
			t.Errorf("ReadResource = %+v, %v", contents, err)
		}
		templates, err := c.ListResourceTemplates(ctx)
		if err != nil || len(templates) != 2 {
			t.Errorf("templates = %+v, %v", templates, err)
		}
		prompts, err := c.ListPrompts(ctx)
		if err != nil || len(prompts) != 2 || prompts[1].Name != "dyn" {
			t.Errorf("ListPrompts = %+v, %v", prompts, err)
		}
		p, err := c.GetPrompt(ctx, "dyn", nil)
		if err != nil || p.Messages[0].Content.Text != "dynamic" {
			t.Errorf("GetPrompt = %+v, %v", p, err)
		}
		// Prompt handlers ask for input too.
		p, err = c.GetPrompt(ctx, "ask", nil)
		if err != nil || p.Messages[0].Content.Text != "go" {
			t.Errorf("GetPrompt(ask) = %+v, %v", p, err)
		}
		_, err = c.GetPrompt(ctx, "panic", nil)
		if e, ok := errors.AsType[*Error](err); !ok || e.Code != CodeInternalError || !strings.Contains(e.Message, "panicked") {
			t.Errorf("GetPrompt(panic) = %v", err)
		}
	})
}

// stdioExchange writes lines to a server and reads its answers.
type stdioExchange struct {
	w    io.WriteCloser
	r    *bufio.Scanner
	done chan error
}

func newStdioExchange(t *testing.T, s *Server) *stdioExchange {
	t.Helper()
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	x := &stdioExchange{w: cw, r: bufio.NewScanner(cr), done: make(chan error, 1)}
	go func() { x.done <- s.ServeStdio(context.Background(), sr, sw) }()
	t.Cleanup(func() {
		_ = cw.Close()
		if err := <-x.done; err != nil {
			t.Errorf("ServeStdio: %v", err)
		}
		_ = cr.Close()
	})
	return x
}

func (x *stdioExchange) send(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(x.w, line+"\n"); err != nil {
		t.Fatal(err)
	}
}

func (x *stdioExchange) recv(t *testing.T) string {
	t.Helper()
	if !x.r.Scan() {
		t.Fatalf("no message: %v", x.r.Err())
	}
	return x.r.Text()
}

func TestServeStdioProtocolErrors(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	x := newStdioExchange(t, f.s)
	for _, tt := range []struct {
		in   string
		code int
	}{
		{`{not json`, CodeParseError},
		{`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, CodeInvalidRequest},
		{`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, CodeInvalidParams},
		{`{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`, CodeInvalidParams},
		{`{"jsonrpc":"2.0","id":4,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"1.0","io.modelcontextprotocol/clientCapabilities":{}}}}`, CodeUnsupportedProtocolVersion},
		{`{"jsonrpc":"2.0","id":5,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/logLevel":"loud"}}}`, CodeInvalidParams},
		{`{"jsonrpc":"2.0","id":6,"method":"ping","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`, CodeMethodNotFound},
		{`{"jsonrpc":"2.0","id":7,"method":"logging/setLevel","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`, CodeMethodNotFound},
	} {
		x.send(t, tt.in)
		if got := x.recv(t); rpcCode(got) != tt.code {
			t.Errorf("%s: %s, want code %d", tt.in, got, tt.code)
		}
	}
	// A discover answer names the server.
	x.send(t, `{"jsonrpc":"2.0","id":8,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)
	if got := x.recv(t); !strings.Contains(got, `"supportedVersions":["2026-07-28"`) || !strings.Contains(got, `"ttlMs"`) ||
		!strings.Contains(got, `/serverInfo":{"name":"fixture"`) {
		t.Errorf("discover = %s", got)
	}
	// A cancelled request gets no response; a request with an ID in use
	// is refused.
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`
	x.send(t, `{"jsonrpc":"2.0","id":"s","method":"tools/call","params":{"name":"slow",`+meta+`}}`)
	<-f.slowStarted
	x.send(t, `{"jsonrpc":"2.0","id":"s","method":"tools/list","params":{`+meta+`}}`)
	if got := x.recv(t); rpcCode(got) != CodeInvalidRequest || !strings.Contains(got, `"id":"s"`) {
		t.Errorf("duplicate id: %s", got)
	}
	x.send(t, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"s"}}`)
	if err := <-f.slowDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled tool: %v, want context.Canceled", err)
	}
	x.send(t, `{"jsonrpc":"2.0","id":9,"method":"tools/list","params":{`+meta+`}}`)
	if got := x.recv(t); !strings.Contains(got, `"id":9`) {
		t.Errorf("after cancellation: %s", got)
	}
}

func TestAddResourceTemplateErrors(t *testing.T) {
	t.Parallel()
	s := NewServer(Implementation{}, nil)
	for _, tmpl := range []string{"file:///{path", "file:///{?q}", "file:///{a,b}", "file:///{}"} {
		if err := s.AddResourceTemplate(ResourceTemplate{URITemplate: tmpl}, nil); err == nil {
			t.Errorf("%s accepted", tmpl)
		}
	}
	if err := s.AddResourceTemplate(ResourceTemplate{URITemplate: "file:///{+path}"}, func(_ context.Context, uri string, vars map[string]string) ([]ResourceContents, error) {
		return []ResourceContents{{URI: uri, Text: vars["path"]}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.read(t.Context(), "file:///a/b.txt")
	if err != nil || got[0].Text != "a/b.txt" {
		t.Errorf("reserved expansion = %+v, %v", got, err)
	}
	s.RemoveResources("file:///{+path}")
	if _, err := s.read(t.Context(), "file:///a/b.txt"); !errors.Is(err, ErrResourceNotFound) {
		t.Errorf("after removal: %v", err)
	}
}

func TestMCPTool(t *testing.T) {
	t.Parallel()
	bare := tool.New("bare", "No schema.", nil, func(context.Context, json.RawMessage) (*tool.Result, error) { return nil, nil })
	if got := MCPTool(bare); string(got.InputSchema) != `{"type":"object","properties":{}}` || got.Annotations == nil ||
		got.Annotations.DestructiveHint == nil || *got.Annotations.DestructiveHint {
		t.Errorf("MCPTool = %+v", got)
	}
	if _, err := Sample(t.Context(), &SamplingRequest{}); !errors.Is(err, ErrNoRequest) {
		t.Errorf("Sample outside a handler = %v", err)
	}
	if err := Log(t.Context(), LogInfo, "", nil); !errors.Is(err, ErrNoRequest) {
		t.Errorf("Log outside a handler = %v", err)
	}
	if _, ok := CurrentRequest(t.Context()); ok {
		t.Error("CurrentRequest outside a handler")
	}
}

func TestServerRemoveAndAccessors(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	if f.s.Info().Name != "fixture" || f.s.Registry().Len() != 8 {
		t.Fatalf("Info %+v, %d tools", f.s.Info(), f.s.Registry().Len())
	}
	c := connectStdio(t, f.s)
	f.s.RemoveTools("echo", "delete")
	f.s.RemovePrompts("greet")
	tools, err := c.ListTools(t.Context())
	if err != nil || len(tools) != 6 {
		t.Errorf("ListTools = %d, %v", len(tools), err)
	}
	// The prompts capability goes with the last prompt.
	if _, err := c.ListPrompts(t.Context()); err == nil {
		t.Error("prompts/list without prompts succeeded")
	}
}

func TestHTTPServerListenKeepAlive(t *testing.T) {
	t.Parallel()
	_, ts, h := newHTTPFixture(t, nil, &HTTPOptions{KeepAlive: 10 * time.Millisecond})
	body, hdr := modernRequest(1, "subscriptions/listen", map[string]any{"notifications": map[string]any{"toolsListChanged": true}})
	st := openStream(t, http.MethodPost, ts.URL, hdr, body)
	defer st.resp.Body.Close()
	if _, data := st.next(t); !strings.Contains(data, "notifications/subscriptions/acknowledged") || !strings.Contains(data, `subscriptionId":1`) {
		t.Fatalf("acknowledgment %q", data)
	}
	if st.resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Errorf("headers %v", st.resp.Header)
	}
	sawComment := false
	for range 20 {
		line, err := st.r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, ":") {
			sawComment = true
			break
		}
	}
	if !sawComment {
		t.Error("no keep-alive comment")
	}
	// Closing the handler ends the subscription gracefully.
	_ = h.Close()
	for {
		_, data := st.next(t)
		if strings.Contains(data, `"result"`) {
			if !strings.Contains(data, `"resultType":"complete"`) || !strings.Contains(data, `subscriptionId":1`) {
				t.Errorf("final %q", data)
			}
			break
		}
	}
}
