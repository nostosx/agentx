package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type rpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Name         string          `json:"name"`
		URI          string          `json:"uri"`
		Arguments    json.RawMessage `json:"arguments"`
		RequestState string          `json:"requestState"`
		Cursor       string          `json:"cursor"`
		Meta         map[string]any  `json:"_meta"`
	} `json:"params"`
}

func result(id json.RawMessage, v string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, id, v)
}

const discoverResult = `{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{}},` +
	`"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"fake","version":"1.0"}},"ttlMs":0,"cacheScope":"private"}`

// fakeHTTP is a Streamable HTTP server. handle answers requests other than
// server/discover; it returns the response body, or "" to write nothing.
func fakeHTTP(t *testing.T, handle func(w http.ResponseWriter, r *http.Request, req rpcRequest) string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("MCP-Protocol-Version"); got != ProtocolVersion {
			t.Errorf("%s: MCP-Protocol-Version = %q", req.Method, got)
		}
		if got := r.Header.Get("Mcp-Method"); got != req.Method {
			t.Errorf("Mcp-Method = %q, want %q", got, req.Method)
		}
		if req.ID != nil {
			if v := req.Params.Meta[metaProtocolVersion]; v != ProtocolVersion {
				t.Errorf("%s: _meta protocolVersion = %v", req.Method, v)
			}
			if _, ok := req.Params.Meta[metaClientCapabilities]; !ok {
				t.Errorf("%s: _meta lacks clientCapabilities", req.Method)
			}
		}
		if req.Method == "server/discover" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, result(req.ID, discoverResult))
			return
		}
		if body := handle(w, r, req); body != "" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, body)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func connectHTTP(t *testing.T, url string) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Connect(ctx, HTTP(url), WithClientInfo("test", "0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestHTTPDiscoverAndToolCall(t *testing.T) {
	s := fakeHTTP(t, func(w http.ResponseWriter, r *http.Request, req rpcRequest) string {
		switch req.Method {
		case "tools/list":
			return result(req.ID, `{"resultType":"complete","ttlMs":0,"cacheScope":"private","tools":[
				{"name":"sql","inputSchema":{"type":"object","properties":{
					"region":{"type":"string","x-mcp-header":"Region"},
					"opts":{"type":"object","properties":{"limit":{"type":"integer","x-mcp-header":"Limit"}}},
					"query":{"type":"string"}}}},
				{"name":"bad","inputSchema":{"type":"object","properties":{
					"tags":{"type":"array","items":{"type":"string","x-mcp-header":"Tag"}}}}}]}`)
		case "tools/call":
			if got := r.Header.Get("Mcp-Name"); got != "sql" {
				t.Errorf("Mcp-Name = %q", got)
			}
			if got := r.Header.Get("Mcp-Param-Region"); got != "=?base64?5LiK5rW3?=" {
				t.Errorf("Mcp-Param-Region = %q", got)
			}
			if got := r.Header.Get("Mcp-Param-Limit"); got != "10" {
				t.Errorf("Mcp-Param-Limit = %q", got)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\n", `{"jsonrpc":"2.0","method":"notifications/progress","params":{"progress":1}}`)
			fmt.Fprintf(w, "data: %s\n\n", result(req.ID, `{"resultType":"complete","content":[{"type":"text","text":"ok"}]}`))
			return ""
		}
		t.Errorf("unexpected %s", req.Method)
		return ""
	})
	c := connectHTTP(t, s.URL)
	if got := c.Server().ServerInfo.Name; got != "fake" {
		t.Errorf("ServerInfo.Name = %q", got)
	}
	tools, err := c.ListTools(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "sql" {
		t.Fatalf("tools = %+v, want only sql", tools)
	}
	res, err := c.CallTool(t.Context(), "sql", json.RawMessage(`{"region":"上海","opts":{"limit":10},"query":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if FormatContent(res) != "ok" {
		t.Errorf("content = %q", FormatContent(res))
	}
}

func TestHTTPUnsupportedVersion(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32022,"message":"Unsupported protocol version",`+
			`"data":{"supported":["2099-01-01"],"requested":"%s"}}}`, req.ID, ProtocolVersion)
	}))
	defer s.Close()
	_, err := Connect(t.Context(), HTTP(s.URL))
	if err == nil || !strings.Contains(err.Error(), "2099-01-01") {
		t.Fatalf("err = %v", err)
	}
}

func TestHTTPStreamLost(t *testing.T) {
	s := fakeHTTP(t, func(w http.ResponseWriter, _ *http.Request, _ rpcRequest) string {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": keep-alive\n\n")
		return ""
	})
	c := connectHTTP(t, s.URL)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := c.CallTool(ctx, "x", nil)
	if !errors.Is(err, ErrStreamLost) {
		t.Fatalf("err = %v, want ErrStreamLost", err)
	}
}

func TestHTTPCancelClosesStream(t *testing.T) {
	closed := make(chan struct{})
	started := make(chan struct{})
	s := fakeHTTP(t, func(w http.ResponseWriter, r *http.Request, _ rpcRequest) string {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(closed)
		return ""
	})
	c := connectHTTP(t, s.URL)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.CallTool(ctx, "x", nil)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("server stream was not opened")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CallTool did not return after cancellation")
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("server stream was not closed")
	}
}

func TestInputRequiredRetriesWithState(t *testing.T) {
	s := fakeHTTP(t, func(_ http.ResponseWriter, _ *http.Request, req rpcRequest) string {
		if req.Params.RequestState == "" {
			return result(req.ID, `{"resultType":"input_required","requestState":"s1"}`)
		}
		if req.Params.RequestState != "s1" {
			t.Errorf("requestState = %q", req.Params.RequestState)
		}
		return result(req.ID, `{"resultType":"complete","content":[{"type":"text","text":"done"}]}`)
	})
	c := connectHTTP(t, s.URL)
	res, err := c.CallTool(t.Context(), "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if FormatContent(res) != "done" {
		t.Errorf("content = %q", FormatContent(res))
	}

	s2 := fakeHTTP(t, func(_ http.ResponseWriter, _ *http.Request, req rpcRequest) string {
		return result(req.ID, `{"resultType":"input_required","inputRequests":{"a":{"method":"elicitation/create","params":{}}}}`)
	})
	c2 := connectHTTP(t, s2.URL)
	var ie *InputRequiredError
	if _, err := c2.CallTool(t.Context(), "x", nil); !errors.As(err, &ie) || ie.InputRequests["a"] == nil {
		t.Fatalf("err = %v, want InputRequiredError", err)
	}
}

// stdioServer runs handle for each request read from the client.
func stdioServer(t *testing.T, handle func(req rpcRequest, write func(string))) *Client {
	t.Helper()
	var handlers sync.WaitGroup
	// Closing the pipes releases any handler blocked writing a response.
	// Register before pipeServer so its cleanup runs before the join.
	t.Cleanup(handlers.Wait)
	tr := pipeServer(t, func(m wireMessage, write func(string)) {
		if m.Method == "server/discover" {
			write(result(m.ID, discoverResult))
			return
		}
		req := rpcRequest{ID: m.ID, Method: m.Method}
		if len(m.Params) > 0 {
			if err := json.Unmarshal(m.Params, &req.Params); err != nil {
				t.Errorf("decode client request params: %v", err)
				return
			}
		}
		handlers.Go(func() { handle(req, write) })
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Connect(ctx, tr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestStdioCancelSendsNotification(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cancelled := make(chan struct{}, 1)
		c := stdioServer(t, func(req rpcRequest, _ func(string)) {
			if req.Method == "notifications/cancelled" {
				cancelled <- struct{}{}
			}
		})
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		if _, err := c.CallTool(ctx, "slow", nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
		select {
		case <-cancelled:
		case <-time.After(5 * time.Second):
			t.Fatal("no notifications/cancelled")
		}
	})
}

func TestStdioListen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		got := make(chan string, 4)
		tr := pipeServer(t, func(req wireMessage, write func(string)) {
			switch req.Method {
			case "server/discover":
				write(result(req.ID, discoverResult))
			case "subscriptions/listen":
				meta := fmt.Sprintf(`{"io.modelcontextprotocol/subscriptionId":%s}`, req.ID)
				write(`{"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{"_meta":` + meta + `,"notifications":{"toolsListChanged":true}}}`)
				write(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed","params":{"_meta":` + meta + `}}`)
				write(result(req.ID, `{"resultType":"complete","_meta":`+meta+`}`))
			}
		})
		c, err := Connect(t.Context(), tr, OnNotification(func(method string, _ json.RawMessage) { got <- method }))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		if err := c.Listen(t.Context(), Subscription{ToolsListChanged: true}); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"notifications/subscriptions/acknowledged", "notifications/tools/list_changed"} {
			if m := <-got; m != want {
				t.Errorf("notification = %q, want %q", m, want)
			}
		}
	})
}

func TestHeaderValue(t *testing.T) {
	for in, want := range map[string]string{
		"us-west1":           "us-west1",
		"Hello, 世界":          "=?base64?SGVsbG8sIOS4lueVjA==?=",
		" padded ":           "=?base64?IHBhZGRlZCA=?=",
		"line1\nline2":       "=?base64?bGluZTEKbGluZTI=?=",
		"=?base64?literal?=": "=?base64?PT9iYXNlNjQ/bGl0ZXJhbD89?=",
	} {
		if got := headerValue(in); got != want {
			t.Errorf("headerValue(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParamHeadersValidation(t *testing.T) {
	for name, schema := range map[string]string{
		"number type": `{"type":"object","properties":{"a":{"type":"number","x-mcp-header":"A"}}}`,
		"duplicate":   `{"type":"object","properties":{"a":{"type":"string","x-mcp-header":"A"},"b":{"type":"string","x-mcp-header":"a"}}}`,
		"bad token":   `{"type":"object","properties":{"a":{"type":"string","x-mcp-header":"A B"}}}`,
		"under oneOf": `{"type":"object","oneOf":[{"properties":{"a":{"type":"string","x-mcp-header":"A"}}}]}`,
		"root":        `{"type":"object","x-mcp-header":"A"}`,
	} {
		if _, err := paramHeaders(json.RawMessage(schema)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	h, err := paramHeaders(json.RawMessage(`{"type":"object","properties":{"a":{"type":["boolean","null"],"x-mcp-header":"A"}}}`))
	if err != nil || len(h) != 1 {
		t.Fatalf("h = %v, err = %v", h, err)
	}
	if got := paramValues(h, json.RawMessage(`{"a":true}`)).Get("Mcp-Param-A"); got != "true" {
		t.Errorf("Mcp-Param-A = %q", got)
	}
	if got := paramValues(h, json.RawMessage(`{"a":null}`)); len(got) != 0 {
		t.Errorf("null value produced %v", got)
	}
}

// rpcError returns a JSON-RPC error response.
func rpcError(id json.RawMessage, code int, msg string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":%d,"message":%q}}`, id, code, msg)
}

func TestResourcesAndPrompts(t *testing.T) {
	c := stdioServer(t, func(req rpcRequest, write func(string)) {
		switch req.Method {
		case "resources/list":
			write(result(req.ID, map[string]string{
				"":   `{"resources":[{"uri":"file:///a","name":"a"}],"nextCursor":"p2"}`,
				"p2": `{"resources":[{"uri":"file:///b","name":"b","mimeType":"text/plain"}]}`,
			}[req.Params.Cursor]))
		case "resources/read":
			if req.Params.URI != "file:///a" {
				write(rpcError(req.ID, -32002, "resource not found: "+req.Params.URI))
				return
			}
			write(result(req.ID, `{"contents":[{"uri":"file:///a","mimeType":"text/plain","text":"alpha"}]}`))
		case "prompts/list":
			write(result(req.ID, map[string]string{
				"":  `{"prompts":[{"name":"review","arguments":[{"name":"code","required":true}]}],"nextCursor":"n"}`,
				"n": `{"prompts":[{"name":"plain"}]}`,
			}[req.Params.Cursor]))
		case "prompts/get":
			if req.Params.Name == "missing" {
				write(rpcError(req.ID, CodeInvalidParams, "unknown prompt"))
				return
			}
			args := strings.ReplaceAll(string(req.Params.Arguments), `"`, `\"`)
			write(result(req.ID, `{"description":"`+req.Params.Name+`","messages":[{"role":"user","content":{"type":"text","text":"`+args+`"}}]}`))
		}
	})

	resources, err := c.ListResources(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 2 || resources[0].URI != "file:///a" || resources[1].MimeType != "text/plain" {
		t.Errorf("resources = %+v", resources)
	}
	contents, err := c.ReadResource(t.Context(), "file:///a")
	if err != nil {
		t.Fatal(err)
	}
	if len(contents) != 1 || contents[0].Text != "alpha" || contents[0].URI != "file:///a" {
		t.Errorf("contents = %+v", contents)
	}
	var e *Error
	if _, err := c.ReadResource(t.Context(), "file:///z"); !errors.As(err, &e) || e.Code != -32002 {
		t.Errorf("read missing: err = %v", err)
	}

	prompts, err := c.ListPrompts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 2 || prompts[0].Name != "review" || len(prompts[0].Arguments) != 1 || !prompts[0].Arguments[0].Required || prompts[1].Name != "plain" {
		t.Errorf("prompts = %+v", prompts)
	}
	p, err := c.GetPrompt(t.Context(), "review", map[string]string{"code": "x := 1"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Description != "review" || len(p.Messages) != 1 || p.Messages[0].Role != "user" ||
		p.Messages[0].Content.Text != `{"code":"x := 1"}` {
		t.Errorf("prompt = %+v", p)
	}
	// Without arguments, the arguments field is left out.
	if p, err := c.GetPrompt(t.Context(), "plain", nil); err != nil || p == nil || len(p.Messages) != 1 || p.Messages[0].Content.Text != "" {
		t.Errorf("plain prompt = %+v, err = %v", p, err)
	}
	if p, err := c.GetPrompt(t.Context(), "missing", nil); !errors.As(err, &e) || e.Code != CodeInvalidParams || p != nil {
		t.Errorf("missing prompt = %+v, err = %v", p, err)
	}
}

func TestListErrorOnLaterPage(t *testing.T) {
	c := stdioServer(t, func(req rpcRequest, write func(string)) {
		if req.Params.Cursor == "" {
			write(result(req.ID, `{"resources":[{"uri":"file:///a","name":"a"}],"prompts":[{"name":"p"}],"nextCursor":"2"}`))
			return
		}
		write(rpcError(req.ID, CodeInternalError, "page failed"))
	})
	var e *Error
	if got, err := c.ListResources(t.Context()); !errors.As(err, &e) || got != nil {
		t.Errorf("resources = %v, err = %v", got, err)
	}
	if got, err := c.ListPrompts(t.Context()); !errors.As(err, &e) || e.Message != "page failed" || got != nil {
		t.Errorf("prompts = %v, err = %v", got, err)
	}
}

func TestErrorMessages(t *testing.T) {
	e := &Error{Code: CodeMethodNotFound, Message: "method not found", Data: json.RawMessage(`{"x":1}`)}
	if got := e.Error(); got != "mcp: method not found (code -32601)" {
		t.Errorf("Error() = %q", got)
	}
	ie := &InputRequiredError{RequestState: "s"}
	if got := ie.Error(); got != "mcp: server requires client input" {
		t.Errorf("InputRequiredError.Error() = %q", got)
	}
}

func TestWithLoggerReportsRejectedTools(t *testing.T) {
	s := fakeHTTP(t, func(_ http.ResponseWriter, _ *http.Request, req rpcRequest) string {
		return result(req.ID, `{"tools":[{"name":"ok","inputSchema":{"type":"object"}},`+
			`{"name":"bad","inputSchema":{"type":"object","properties":{"a":{"type":"number","x-mcp-header":"A"}}}}]}`)
	})
	var buf bytes.Buffer
	c, err := Connect(t.Context(), HTTP(s.URL), WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	tools, err := c.ListTools(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "ok" {
		t.Errorf("tools = %+v, want only ok", tools)
	}
	if out := buf.String(); !strings.Contains(out, "mcp: rejecting tool") || !strings.Contains(out, "tool=bad") {
		t.Errorf("log = %q", out)
	}
}
