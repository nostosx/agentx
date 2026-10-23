package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nostosx/agentx/tool"
)

// This file tests the server with the package's own client, in process,
// over stdio pipes and Streamable HTTP, with both protocol generations.

type echoArgs struct {
	Text string `json:"text"`
}

// srvFixture is a server with tools, resources and prompts that exercise
// every feature.
type srvFixture struct {
	s *Server
	// slowStarted and slowDone report the slow tool's run; asks counts the
	// runs of the ask tool.
	slowStarted chan struct{}
	slowDone    chan error
	asks        atomic.Int32
}

func newFixture(t *testing.T, opts *ServerOptions) *srvFixture {
	t.Helper()
	f := &srvFixture{slowStarted: make(chan struct{}, 8), slowDone: make(chan error, 8)}
	if opts == nil {
		opts = &ServerOptions{}
	}
	opts.Instructions = "Use echo."
	opts.Tools = append(opts.Tools,
		tool.Func("echo", "Echo text.", func(_ context.Context, in echoArgs) (string, error) {
			return "echo: " + in.Text, nil
		}, tool.ReadOnly()),
		tool.Func("delete", "Delete things.", func(_ context.Context, in echoArgs) (string, error) {
			return "deleted " + in.Text, nil
		}, tool.RequireApproval()),
		tool.Func("ask", "Ask the user twice.", func(ctx context.Context, _ struct{}) (string, error) {
			f.asks.Add(1)
			schema := json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`)
			first, err := Elicit(ctx, &ElicitRequest{Message: "first name?", RequestedSchema: schema})
			if err != nil {
				return "", err
			}
			second, err := Elicit(ctx, &ElicitRequest{Message: "last name?", RequestedSchema: schema})
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("%s %s %v %v", first.Action, second.Action, first.Content["name"], second.Content["name"]), nil
		}),
		tool.Func("sample", "Sample a model.", func(ctx context.Context, in echoArgs) (string, error) {
			res, err := Sample(ctx, &SamplingRequest{MaxTokens: 10,
				Messages: []SamplingMessage{{Role: "user", Content: SamplingContents{{Type: "text", Text: in.Text}}}}})
			if err != nil {
				return "", err
			}
			return res.Model + ": " + res.Content[0].Text, nil
		}),
		tool.Func("roots", "List roots.", func(ctx context.Context, _ struct{}) (string, error) {
			roots, err := ListRoots(ctx)
			if err != nil {
				return "", err
			}
			var uris []string
			for _, r := range roots {
				uris = append(uris, r.URI)
			}
			return strings.Join(uris, ","), nil
		}),
		tool.Func("log", "Log and report progress.", func(ctx context.Context, _ struct{}) (string, error) {
			if err := Log(ctx, LogDebug, "test", "debug message"); err != nil {
				return "", err
			}
			if err := Log(ctx, LogError, "test", map[string]any{"error": "boom"}); err != nil {
				return "", err
			}
			if err := Progress(ctx, 1, 2, "half"); err != nil {
				return "", err
			}
			info, ok := CurrentRequest(ctx)
			if !ok {
				return "", errors.New("no request info")
			}
			return info.ProtocolVersion + " " + info.Client.Name, nil
		}),
		tool.Func("slow", "Wait until cancelled.", func(ctx context.Context, _ struct{}) (string, error) {
			f.slowStarted <- struct{}{}
			<-ctx.Done()
			f.slowDone <- ctx.Err()
			return "", ctx.Err()
		}),
		tool.Func("rpcerr", "Fail with a JSON-RPC error.", func(context.Context, struct{}) (string, error) {
			return "", &Error{Code: -31999, Message: "custom failure"}
		}),
	)
	f.s = NewServer(Implementation{Name: "fixture", Version: "1.2.3"}, opts)
	f.s.AddResource(Resource{URI: "file:///readme.md", Name: "readme", MimeType: "text/markdown"},
		func(_ context.Context, uri string) ([]ResourceContents, error) {
			return []ResourceContents{{URI: uri, MimeType: "text/markdown", Text: "# Hello"}}, nil
		})
	if err := f.s.AddResourceTemplate(ResourceTemplate{URITemplate: "file:///docs/{name}", Name: "docs"},
		func(_ context.Context, uri string, vars map[string]string) ([]ResourceContents, error) {
			if vars["name"] == "missing" {
				return nil, ErrResourceNotFound
			}
			return []ResourceContents{{URI: uri, Text: "doc " + vars["name"]}}, nil
		}); err != nil {
		t.Fatal(err)
	}
	f.s.AddPrompt(Prompt{Name: "greet", Arguments: []PromptArgument{{Name: "who", Required: true}}},
		func(_ context.Context, args map[string]string) (*GetPromptResult, error) {
			return &GetPromptResult{Description: "greeting", Messages: []PromptMessage{
				{Role: "user", Content: Content{Type: "text", Text: "Hello, " + args["who"]}}}}, nil
		})
	t.Cleanup(func() { _ = f.s.Close() })
	return f
}

// connectStdio serves s over in-process pipes and connects a client.
func connectStdio(t *testing.T, s *Server, opts ...ClientOption) *Client {
	t.Helper()
	c, err := dialStdio(t, s, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func dialStdio(t *testing.T, s *Server, opts ...ClientOption) (*Client, error) {
	t.Helper()
	cr, sw := io.Pipe() // server to client
	sr, cw := io.Pipe() // client to server
	done := make(chan error, 1)
	go func() {
		done <- s.ServeStdio(context.Background(), sr, sw)
		_ = sw.Close()
	}()
	tr := NewIOTransport(cr, cw, func() error {
		_ = cw.Close()
		return cr.Close()
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, err := Connect(ctx, tr, append([]ClientOption{WithClientInfo("tester", "1")}, opts...)...)
	t.Cleanup(func() {
		if c != nil {
			_ = c.Close()
		} else {
			_ = tr.Close()
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("ServeStdio: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("ServeStdio did not return")
		}
	})
	return c, err
}

// connectHTTPServer serves s over httptest and connects a client.
func connectHTTPServer(t *testing.T, s *Server, hopts *HTTPOptions, opts ...ClientOption) (*Client, *httptest.Server, *HTTPHandler) {
	t.Helper()
	h := s.HTTPHandler(hopts)
	ts := httptest.NewServer(h)
	t.Cleanup(func() {
		_ = h.Close()
		ts.Close()
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, err := Connect(ctx, HTTP(ts.URL), append([]ClientOption{WithClientInfo("tester", "1")}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, ts, h
}

// connectVia connects a client over transport, "stdio" or "http".
func connectVia(t *testing.T, s *Server, transport string, opts ...ClientOption) *Client {
	t.Helper()
	if transport == "stdio" {
		return connectStdio(t, s, opts...)
	}
	c, _, _ := connectHTTPServer(t, s, nil, opts...)
	return c
}

// generation is a protocol generation the tests run with.
type generation struct {
	name    string
	opts    []ClientOption
	version string
}

var generations = []generation{
	{"modern", nil, ProtocolVersion},
	{"legacy", []ClientOption{WithProtocolVersion(LegacyProtocolVersion)}, LegacyProtocolVersion},
}

// forEach runs fn with clients over both transports and generations.
func forEach(t *testing.T, opts *ServerOptions, copts []ClientOption, fn func(t *testing.T, f *srvFixture, c *Client, g generation)) {
	for _, g := range generations {
		for _, transport := range []string{"stdio", "http"} {
			t.Run(transport+"/"+g.name, func(t *testing.T) {
				t.Parallel()
				var o *ServerOptions
				if opts != nil {
					cp := *opts
					cp.Tools = slices.Clone(opts.Tools)
					o = &cp
				}
				f := newFixture(t, o)
				all := append(slices.Clone(g.opts), copts...)
				fn(t, f, connectVia(t, f.s, transport, all...), g)
			})
		}
	}
}

func callText(t *testing.T, c *Client, name string, args string) (string, bool) {
	t.Helper()
	res, err := c.CallTool(t.Context(), name, json.RawMessage(args))
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	return FormatContent(res), res.IsError
}

func TestServerBasics(t *testing.T) {
	forEach(t, nil, nil, func(t *testing.T, f *srvFixture, c *Client, g generation) {
		if v := c.ProtocolVersion(); v != g.version {
			t.Fatalf("ProtocolVersion = %q, want %q", v, g.version)
		}
		srv := c.Server()
		if srv.ServerInfo.Name != "fixture" || srv.ServerInfo.Version != "1.2.3" || srv.Instructions != "Use echo." {
			t.Errorf("Server() = %+v", srv)
		}
		if !c.capable("tools", "listChanged") || !c.capable("resources", "subscribe") || !c.capable("prompts", "listChanged") ||
			!c.capable("logging", "") {
			t.Errorf("capabilities = %s", srv.Capabilities)
		}
		if err := c.Ping(t.Context()); err != nil {
			t.Errorf("Ping: %v", err)
		}
		tools, err := c.ListTools(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		byName := map[string]Tool{}
		var names []string
		for _, tl := range tools {
			byName[tl.Name] = tl
			names = append(names, tl.Name)
		}
		if !slices.IsSorted(names) || len(names) != 8 {
			t.Errorf("tools = %v", names)
		}
		echo, del := byName["echo"], byName["delete"]
		if !hint(echo.Annotations.ReadOnlyHint) || echo.Annotations.DestructiveHint != nil {
			t.Errorf("echo annotations = %+v", echo.Annotations)
		}
		if hint(del.Annotations.ReadOnlyHint) || !hint(del.Annotations.DestructiveHint) {
			t.Errorf("delete annotations = %+v", del.Annotations)
		}
		if s := byName["sample"].Annotations; s.DestructiveHint == nil || *s.DestructiveHint {
			t.Errorf("sample annotations = %+v", s)
		}
		if !strings.Contains(string(echo.InputSchema), `"text"`) {
			t.Errorf("echo schema = %s", echo.InputSchema)
		}
		if out, isErr := callText(t, c, "echo", `{"text":"hi"}`); out != "echo: hi" || isErr {
			t.Errorf("echo = %q, %v", out, isErr)
		}
		if out, isErr := callText(t, c, "echo", `{"text":5}`); !isErr || !strings.Contains(out, "text") {
			t.Errorf("invalid arguments = %q, %v", out, isErr)
		}
		_, err = c.CallTool(t.Context(), "nope", nil)
		if e, ok := errors.AsType[*Error](err); !ok || e.Code != CodeInvalidParams || !strings.Contains(e.Message, "Unknown tool") {
			t.Errorf("unknown tool: %v", err)
		}
		_, err = c.CallTool(t.Context(), "rpcerr", nil)
		if e, ok := errors.AsType[*Error](err); !ok || e.Code != -31999 {
			t.Errorf("rpcerr: %v", err)
		}
	})
}

func TestServerResourcesAndPrompts(t *testing.T) {
	forEach(t, nil, nil, func(t *testing.T, f *srvFixture, c *Client, g generation) {
		ctx := t.Context()
		res, err := c.ListResources(ctx)
		if err != nil || len(res) != 1 || res[0].URI != "file:///readme.md" {
			t.Fatalf("ListResources = %+v, %v", res, err)
		}
		contents, err := c.ReadResource(ctx, "file:///readme.md")
		if err != nil || len(contents) != 1 || contents[0].Text != "# Hello" {
			t.Fatalf("ReadResource = %+v, %v", contents, err)
		}
		contents, err = c.ReadResource(ctx, "file:///docs/guide")
		if err != nil || len(contents) != 1 || contents[0].Text != "doc guide" {
			t.Fatalf("template ReadResource = %+v, %v", contents, err)
		}
		templates, err := c.ListResourceTemplates(ctx)
		if err != nil || len(templates) != 1 || templates[0].URITemplate != "file:///docs/{name}" {
			t.Fatalf("ListResourceTemplates = %+v, %v", templates, err)
		}
		want := CodeInvalidParams
		if g.name == "legacy" {
			want = CodeResourceNotFound
		}
		for _, uri := range []string{"file:///nothing", "file:///docs/missing", "file:///docs/a/b"} {
			_, err = c.ReadResource(ctx, uri)
			if e, ok := errors.AsType[*Error](err); !ok || e.Code != want {
				t.Errorf("ReadResource(%s) = %v, want code %d", uri, err, want)
			}
		}
		prompts, err := c.ListPrompts(ctx)
		if err != nil || len(prompts) != 1 || prompts[0].Name != "greet" {
			t.Fatalf("ListPrompts = %+v, %v", prompts, err)
		}
		p, err := c.GetPrompt(ctx, "greet", map[string]string{"who": "Ada"})
		if err != nil || p.Description != "greeting" || len(p.Messages) != 1 || p.Messages[0].Content.Text != "Hello, Ada" {
			t.Fatalf("GetPrompt = %+v, %v", p, err)
		}
		_, err = c.GetPrompt(ctx, "greet", nil)
		if e, ok := errors.AsType[*Error](err); !ok || e.Code != CodeInvalidParams {
			t.Errorf("GetPrompt without argument = %v", err)
		}
		_, err = c.GetPrompt(ctx, "other", nil)
		if e, ok := errors.AsType[*Error](err); !ok || e.Code != CodeInvalidParams {
			t.Errorf("GetPrompt(other) = %v", err)
		}
	})
}

func TestServerClientFeatures(t *testing.T) {
	var elicited atomic.Int32
	copts := []ClientOption{
		WithElicitation(func(_ context.Context, req *ElicitRequest) (*ElicitResult, error) {
			n := elicited.Add(1)
			return &ElicitResult{Action: ElicitAccept, Content: map[string]any{"name": fmt.Sprintf("%s-%d", strings.Fields(req.Message)[0], n)}}, nil
		}),
		WithSampling(func(_ context.Context, req *SamplingRequest) (*SamplingResult, error) {
			return &SamplingResult{Model: "m", Content: SamplingContents{{Type: "text", Text: "re " + req.Messages[0].Content[0].Text}}}, nil
		}),
		WithRoots(StaticRoots(Root{URI: "file:///a"}, Root{URI: "file:///b"})),
	}
	forEach(t, nil, copts, func(t *testing.T, f *srvFixture, c *Client, g generation) {
		out, isErr := callText(t, c, "ask", `{}`)
		if isErr || !strings.HasPrefix(out, "accept accept first-") || !strings.Contains(out, " last-") {
			t.Errorf("ask = %q, %v", out, isErr)
		}
		if n := f.asks.Load(); n != 1 {
			t.Errorf("ask ran %d times, want 1", n)
		}
		if out, isErr := callText(t, c, "sample", `{"text":"q"}`); isErr || out != "m: re q" {
			t.Errorf("sample = %q, %v", out, isErr)
		}
		if out, isErr := callText(t, c, "roots", `{}`); isErr || out != "file:///a,file:///b" {
			t.Errorf("roots = %q, %v", out, isErr)
		}
	})
}

func TestServerMissingClientCapability(t *testing.T) {
	forEach(t, nil, nil, func(t *testing.T, f *srvFixture, c *Client, g generation) {
		for _, name := range []string{"sample", "ask", "roots"} {
			_, err := c.CallTool(t.Context(), name, json.RawMessage(`{"text":"x"}`))
			e, ok := errors.AsType[*Error](err)
			if !ok || e.Code != CodeMissingRequiredClientCapability || !strings.Contains(string(e.Data), "requiredCapabilities") {
				t.Errorf("%s: %v", name, err)
			}
		}
	})
}

func TestServerLoggingAndProgress(t *testing.T) {
	for _, g := range generations {
		for _, transport := range []string{"stdio", "http"} {
			t.Run(transport+"/"+g.name, func(t *testing.T) {
				t.Parallel()
				f := newFixture(t, nil)
				var mu sync.Mutex
				var logs []LogMessage
				var progress []string
				c := connectVia(t, f.s, transport, append(slices.Clone(g.opts),
					WithLogLevel(LogInfo),
					WithLogHandler(func(m LogMessage) {
						mu.Lock()
						logs = append(logs, m)
						mu.Unlock()
					}),
					OnNotification(func(method string, params json.RawMessage) {
						if method == "notifications/progress" {
							mu.Lock()
							progress = append(progress, string(params))
							mu.Unlock()
						}
					}))...)
				var out CallToolResult
				err := c.Call(t.Context(), "tools/call", map[string]any{
					"name": "log", "arguments": map[string]any{}, "_meta": map[string]any{"progressToken": "p-1"},
				}, &out)
				if err != nil {
					t.Fatal(err)
				}
				if got := FormatContent(&out); got != g.version+" tester" {
					t.Errorf("log tool = %q", got)
				}
				check := func() bool {
					mu.Lock()
					defer mu.Unlock()
					return len(logs) == 1 && logs[0].Level == LogError && logs[0].Logger == "test" &&
						len(progress) == 1 && strings.Contains(progress[0], `"p-1"`) && strings.Contains(progress[0], `"half"`)
				}
				// Over stdio the notifications may be delivered after the
				// response.
				if !eventually(check) {
					mu.Lock()
					t.Errorf("logs = %+v, progress = %v", logs, progress)
					mu.Unlock()
				}
			})
		}
	}
}

// eventually polls cond for up to two seconds.
func eventually(cond func() bool) bool {
	for range 200 {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

func TestServerCancellation(t *testing.T) {
	forEach(t, nil, nil, func(t *testing.T, f *srvFixture, c *Client, g generation) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		errc := make(chan error, 1)
		go func() {
			_, err := c.CallTool(ctx, "slow", nil)
			errc <- err
		}()
		select {
		case <-f.slowStarted:
		case <-time.After(5 * time.Second):
			t.Fatal("slow tool did not start")
		}
		cancel()
		select {
		case err := <-errc:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("CallTool = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("CallTool did not end after cancellation")
		}
		select {
		case err := <-f.slowDone:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("handler ctx = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the server did not cancel the handler")
		}
		// The connection still works.
		if out, _ := callText(t, c, "echo", `{"text":"after"}`); out != "echo: after" {
			t.Errorf("echo = %q", out)
		}
	})
}

func TestServerPagination(t *testing.T) {
	forEach(t, &ServerOptions{PageSize: 3}, nil, func(t *testing.T, f *srvFixture, c *Client, g generation) {
		tools, err := c.ListTools(t.Context())
		if err != nil || len(tools) != 8 {
			t.Fatalf("ListTools = %d tools, %v", len(tools), err)
		}
		var out map[string]any
		err = c.Call(t.Context(), "tools/list", map[string]any{"cursor": "bogus"}, &out)
		if e, ok := errors.AsType[*Error](err); !ok || e.Code != CodeInvalidParams {
			t.Errorf("invalid cursor = %v", err)
		}
	})
}

// notes records notifications.
type notes struct {
	mu   sync.Mutex
	list []wireNote
}

type wireNote struct {
	method string
	params json.RawMessage
}

func (n *notes) option() ClientOption {
	return OnNotification(func(method string, params json.RawMessage) {
		n.mu.Lock()
		n.list = append(n.list, wireNote{method, params})
		n.mu.Unlock()
	})
}

// wait waits for a notification of method whose params contain substr.
func (n *notes) wait(t *testing.T, method, substr string) wireNote {
	t.Helper()
	var found wireNote
	ok := eventually(func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		for _, w := range n.list {
			if w.method == method && strings.Contains(string(w.params), substr) {
				found = w
				return true
			}
		}
		return false
	})
	if !ok {
		n.mu.Lock()
		defer n.mu.Unlock()
		t.Fatalf("no %s notification with %q; got %v", method, substr, n.list)
	}
	return found
}

func (n *notes) count(method string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	k := 0
	for _, w := range n.list {
		if w.method == method {
			k++
		}
	}
	return k
}

func TestServerListenNotifications(t *testing.T) {
	for _, g := range generations {
		for _, transport := range []string{"stdio", "http"} {
			t.Run(transport+"/"+g.name, func(t *testing.T) {
				t.Parallel()
				f := newFixture(t, nil)
				var n notes
				c := connectVia(t, f.s, transport, append(slices.Clone(g.opts), n.option())...)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				errc := make(chan error, 1)
				go func() {
					errc <- c.Listen(ctx, Subscription{ToolsListChanged: true, PromptsListChanged: true,
						ResourcesListChanged: true, ResourceSubscriptions: []string{"file:///readme.md"}})
				}()
				ack := n.wait(t, "notifications/subscriptions/acknowledged", "toolsListChanged")
				if g.name == "modern" && !strings.Contains(string(ack.params), metaSubscriptionID) {
					t.Errorf("acknowledgment without subscription ID: %s", ack.params)
				}
				if err := f.s.AddTools(tool.Func("extra", "", func(context.Context, struct{}) (string, error) { return "", nil })); err != nil {
					t.Fatal(err)
				}
				tl := n.wait(t, "notifications/tools/list_changed", "")
				if g.name == "modern" && !strings.Contains(string(tl.params), metaSubscriptionID) {
					t.Errorf("notification without subscription ID: %s", tl.params)
				}
				f.s.AddPrompt(Prompt{Name: "p2"}, func(context.Context, map[string]string) (*GetPromptResult, error) { return nil, nil })
				n.wait(t, "notifications/prompts/list_changed", "")
				f.s.RemoveResources("file:///none")
				n.wait(t, "notifications/resources/list_changed", "")
				f.s.ResourceUpdated("file:///other.md")
				f.s.ResourceUpdated("file:///readme.md")
				n.wait(t, "notifications/resources/updated", "file:///readme.md")
				if k := n.count("notifications/resources/updated"); k != 1 {
					t.Errorf("%d resource updates, want only the subscribed one", k)
				}
				tools, err := c.ListTools(t.Context())
				if err != nil || len(tools) != 9 {
					t.Errorf("ListTools after the change = %d, %v", len(tools), err)
				}
				cancel()
				if err := <-errc; !errors.Is(err, context.Canceled) {
					t.Errorf("Listen = %v", err)
				}
			})
		}
	}
}

func TestServerListenGracefulEnd(t *testing.T) {
	for _, transport := range []string{"stdio", "http"} {
		t.Run(transport, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, nil)
			var n notes
			c := connectVia(t, f.s, transport, n.option())
			errc := make(chan error, 1)
			go func() { errc <- c.Listen(t.Context(), Subscription{ToolsListChanged: true}) }()
			n.wait(t, "notifications/subscriptions/acknowledged", "")
			_ = f.s.Close()
			select {
			case err := <-errc:
				if err != nil {
					t.Errorf("Listen = %v, want a graceful end", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Listen did not end")
			}
			n.wait(t, "notifications/cancelled", "server shutting down")
		})
	}
}

func TestServeStdioShutdownEndsListen(t *testing.T) {
	f := newFixture(t, nil)
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan struct{})
	var serveErr error
	go func() {
		defer close(done)
		serveErr = f.s.ServeStdio(ctx, sr, sw)
	}()
	t.Cleanup(func() {
		stop()
		_ = cw.Close()
		_ = cr.Close()
		_ = sr.Close()
		_ = sw.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("ServeStdio did not stop during cleanup")
		}
	})
	var n notes
	c, err := Connect(t.Context(), NewIOTransport(cr, cw, func() error { _ = cw.Close(); return cr.Close() }), n.option())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	errc := make(chan error, 1)
	go func() { errc <- c.Listen(t.Context(), Subscription{ToolsListChanged: true}) }()
	n.wait(t, "notifications/subscriptions/acknowledged", "")
	stop()
	select {
	case err := <-errc:
		if err != nil {
			t.Errorf("Listen = %v, want a graceful end", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Listen did not end after shutdown")
	}
	select {
	case <-done:
		if !errors.Is(serveErr, context.Canceled) {
			t.Errorf("ServeStdio = %v", serveErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeStdio did not end after shutdown")
	}
}
