package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nostosx/agentx/tool"
)

// rawResponse is an HTTP response with its body read.
type rawResponse struct {
	status int
	header http.Header
	body   string
}

// rawDo sends an HTTP request to the handler.
func rawDo(t *testing.T, method, url string, header map[string]string, body string) rawResponse {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return rawResponse{resp.StatusCode, resp.Header, string(b)}
}

// modernRequest returns the body and headers of a modern request.
func modernRequest(id int, method string, params map[string]any) (string, map[string]string) {
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = map[string]any{metaProtocolVersion: ProtocolVersion, metaClientCapabilities: map[string]any{}}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	h := map[string]string{"MCP-Protocol-Version": ProtocolVersion, "Mcp-Method": method}
	if name, ok := params["name"].(string); ok {
		h["Mcp-Name"] = name
	}
	if uri, ok := params["uri"].(string); ok {
		h["Mcp-Name"] = uri
	}
	return string(b), h
}

// rpcCode returns the JSON-RPC error code of a response body, or 0.
func rpcCode(body string) int {
	var m message
	if json.Unmarshal([]byte(strings.TrimSpace(body)), &m) != nil || m.Error == nil {
		return 0
	}
	return m.Error.Code
}

func newHTTPFixture(t *testing.T, opts ServerOptions, hopts HTTPOptions) (*srvFixture, *httptest.Server, *HTTPHandler) {
	t.Helper()
	f := newFixture(t, opts)
	h := f.s.HTTPHandler(hopts)
	ts := httptest.NewServer(h)
	t.Cleanup(func() {
		_ = h.Close()
		ts.Close()
	})
	return f, ts, h
}

func TestHTTPServerModernValidation(t *testing.T) {
	t.Parallel()
	_, ts, _ := newHTTPFixture(t, ServerOptions{}, HTTPOptions{})
	body, h := modernRequest(1, "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"text": "x"}})
	with := func(change func(map[string]string)) map[string]string {
		c := maps.Clone(h)
		change(c)
		return c
	}
	for _, tt := range []struct {
		name   string
		header map[string]string
		body   string
		status int
		code   int
	}{
		{"valid", h, body, 200, 0},
		{"missing version header", with(func(m map[string]string) { delete(m, "MCP-Protocol-Version") }), body, 400, CodeHeaderMismatch},
		{"version header mismatch", with(func(m map[string]string) { m["MCP-Protocol-Version"] = LegacyProtocolVersion }), body, 400, CodeHeaderMismatch},
		{"missing method header", with(func(m map[string]string) { delete(m, "Mcp-Method") }), body, 400, CodeHeaderMismatch},
		{"method header mismatch", with(func(m map[string]string) { m["Mcp-Method"] = "tools/list" }), body, 400, CodeHeaderMismatch},
		{"missing name header", with(func(m map[string]string) { delete(m, "Mcp-Name") }), body, 400, CodeHeaderMismatch},
		{"name header mismatch", with(func(m map[string]string) { m["Mcp-Name"] = "other" }), body, 400, CodeHeaderMismatch},
		{"base64 name header", with(func(m map[string]string) { m["Mcp-Name"] = "=?base64?ZWNobw==?=" }), body, 200, 0},
		{"invalid base64 name", with(func(m map[string]string) { m["Mcp-Name"] = "=?base64?***?=" }), body, 400, CodeHeaderMismatch},
		{"missing meta", h, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`, 400, CodeInvalidParams},
		{"unsupported version", with(func(m map[string]string) { m["MCP-Protocol-Version"] = "2099-01-01" }),
			strings.Replace(body, ProtocolVersion, "2099-01-01", 1), 400, CodeUnsupportedProtocolVersion},
		{"batch", h, "[" + body + "]", 400, CodeInvalidRequest},
		{"parse error", h, "{", 400, CodeParseError},
		{"response", h, `{"jsonrpc":"2.0","id":1,"result":{}}`, 400, CodeInvalidRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := rawDo(t, http.MethodPost, ts.URL, tt.header, tt.body)
			if r.status != tt.status || rpcCode(r.body) != tt.code {
				t.Fatalf("status %d, body %s; want %d, code %d", r.status, r.body, tt.status, tt.code)
			}
		})
	}
	r := rawDo(t, http.MethodPost, ts.URL, h, strings.Replace(body, ProtocolVersion, "2099-01-01", 1))
	if !strings.Contains(r.body, `"supported":["2026-07-28","2025-11-25"`) || !strings.Contains(r.body, `"requested":"2099-01-01"`) {
		t.Errorf("unsupported version body = %s", r.body)
	}
	body, h = modernRequest(2, "nope/nothing", nil)
	if r := rawDo(t, http.MethodPost, ts.URL, h, body); r.status != 404 || rpcCode(r.body) != CodeMethodNotFound {
		t.Errorf("unknown method: %d %s", r.status, r.body)
	}
	// A notification is accepted without a body.
	if r := rawDo(t, http.MethodPost, ts.URL, h, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`); r.status != 202 || r.body != "" {
		t.Errorf("notification: %d %q", r.status, r.body)
	}
	if r := rawDo(t, http.MethodPost, ts.URL, map[string]string{"Content-Type": "text/plain"}, body); r.status != 415 {
		t.Errorf("content type: %d", r.status)
	}
	for _, m := range []string{http.MethodGet, http.MethodDelete} {
		if r := rawDo(t, m, ts.URL, nil, ""); r.status != 405 {
			t.Errorf("%s without session: %d", m, r.status)
		}
	}
	if r := rawDo(t, http.MethodPut, ts.URL, nil, ""); r.status != 405 || r.header.Get("Allow") == "" {
		t.Errorf("PUT: %d", r.status)
	}
	// A JSON answer without notifications, carrying the server's identity.
	body, h = modernRequest(3, "tools/list", nil)
	r = rawDo(t, http.MethodPost, ts.URL, h, body)
	if r.header.Get("Content-Type") != "application/json" || !strings.Contains(r.body, `"resultType":"complete"`) ||
		!strings.Contains(r.body, `"ttlMs":0`) || !strings.Contains(r.body, `"cacheScope":"private"`) ||
		!strings.Contains(r.body, `"io.modelcontextprotocol/serverInfo":{"name":"fixture"`) {
		t.Errorf("tools/list: %s %s", r.header.Get("Content-Type"), r.body)
	}
	// Session headers are ignored for modern requests.
	h["Mcp-Session-Id"] = "whatever"
	if r := rawDo(t, http.MethodPost, ts.URL, h, body); r.status != 200 || r.header.Get("Mcp-Session-Id") != "" {
		t.Errorf("modern request with a session header: %d %v", r.status, r.header)
	}
}

func TestHTTPServerParamHeaders(t *testing.T) {
	t.Parallel()
	schema := json.RawMessage(`{"type":"object","properties":{
		"region":{"type":"string","x-mcp-header":"Region"},
		"limit":{"type":"integer","x-mcp-header":"Limit"},
		"query":{"type":"string"}}}`)
	sql := tool.New("sql", "Run SQL.", nil, func(_ context.Context, args json.RawMessage) (*tool.Result, error) {
		return tool.Text(string(args)), nil
	}, func(i *tool.Info) { i.RawParameters = schema })
	f, ts, _ := newHTTPFixture(t, ServerOptions{Tools: []tool.Tool{sql}}, HTTPOptions{})
	_ = f
	c := connectHTTP(t, ts.URL)
	if _, err := c.ListTools(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The client mirrors the parameters, base64-encoding the region.
	out, isErr := callText(t, c, "sql", `{"region":"上海","limit":42,"query":"q"}`)
	if isErr || !strings.Contains(out, "上海") {
		t.Fatalf("sql = %q, %v", out, isErr)
	}
	body, h := modernRequest(1, "tools/call", map[string]any{"name": "sql",
		"arguments": map[string]any{"region": "eu", "limit": 42, "query": "q"}})
	for _, tt := range []struct {
		name   string
		params map[string]string
		status int
	}{
		{"missing", map[string]string{"Mcp-Param-Limit": "42"}, 400},
		{"mismatch", map[string]string{"Mcp-Param-Region": "us", "Mcp-Param-Limit": "42"}, 400},
		{"integer mismatch", map[string]string{"Mcp-Param-Region": "eu", "Mcp-Param-Limit": "41"}, 400},
		{"numeric equality", map[string]string{"Mcp-Param-Region": "eu", "Mcp-Param-Limit": "42.0"}, 200},
		{"base64", map[string]string{"Mcp-Param-Region": "=?base64?ZXU=?=", "Mcp-Param-Limit": "42"}, 200},
		{"invalid characters", map[string]string{"Mcp-Param-Region": "eü", "Mcp-Param-Limit": "42"}, 400},
	} {
		t.Run(tt.name, func(t *testing.T) {
			hh := maps.Clone(h)
			maps.Copy(hh, tt.params)
			r := rawDo(t, http.MethodPost, ts.URL, hh, body)
			if r.status != tt.status || (tt.status == 400 && rpcCode(r.body) != CodeHeaderMismatch) {
				t.Fatalf("%d %s", r.status, r.body)
			}
		})
	}
}

func TestHTTPServerOriginAndHost(t *testing.T) {
	t.Parallel()
	_, ts, _ := newHTTPFixture(t, ServerOptions{}, HTTPOptions{AllowedOrigins: []string{"https://app.example"}})
	body, h := modernRequest(1, "tools/list", nil)
	for _, tt := range []struct {
		origin string
		status int
	}{
		{"https://evil.example", 403},
		{"http://localhost:3000", 200},
		{"http://127.0.0.1:9", 200},
		{"https://app.example", 200},
		{"null", 403},
	} {
		hh := map[string]string{"Origin": tt.origin}
		maps.Copy(hh, h)
		r := rawDo(t, http.MethodPost, ts.URL, hh, body)
		if r.status != tt.status {
			t.Errorf("Origin %s: %d %s", tt.origin, r.status, r.body)
		}
		if tt.status == 200 && r.header.Get("Access-Control-Allow-Origin") != tt.origin {
			t.Errorf("Origin %s: CORS header %q", tt.origin, r.header.Get("Access-Control-Allow-Origin"))
		}
		if tt.status == 403 && (strings.Contains(r.body, `"id"`) || rpcCode(r.body) != CodeInvalidRequest) {
			t.Errorf("Origin %s: body %s", tt.origin, r.body)
		}
	}
	r := rawDo(t, http.MethodOptions, ts.URL, map[string]string{"Origin": "https://app.example",
		"Access-Control-Request-Headers": "mcp-method, content-type"}, "")
	if r.status != 204 || r.header.Get("Access-Control-Allow-Headers") != "mcp-method, content-type" ||
		!strings.Contains(r.header.Get("Access-Control-Expose-Headers"), "Mcp-Session-Id") {
		t.Errorf("preflight: %d %v", r.status, r.header)
	}

	_, hts, _ := newHTTPFixture(t, ServerOptions{}, HTTPOptions{AllowedHosts: []string{"localhost:1"}})
	if r := rawDo(t, http.MethodPost, hts.URL, h, body); r.status != 403 {
		t.Errorf("Host check: %d", r.status)
	}
}

func TestHTTPServerSessions(t *testing.T) {
	t.Parallel()
	_, ts, h := newHTTPFixture(t, ServerOptions{}, HTTPOptions{})
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"raw","version":"1"}}}`
	r := rawDo(t, http.MethodPost, ts.URL, nil, init)
	sid := r.header.Get("Mcp-Session-Id")
	if r.status != 200 || !validSessionID(sid) || !strings.Contains(r.body, `"protocolVersion":"2025-06-18"`) ||
		strings.Contains(r.body, "resultType") {
		t.Fatalf("initialize: %d %q %s", r.status, sid, r.body)
	}
	// An unknown version is answered with the newest.
	r = rawDo(t, http.MethodPost, ts.URL, nil, strings.Replace(init, "2025-06-18", "1999-01-01", 1))
	if !strings.Contains(r.body, `"protocolVersion":"`+LegacyProtocolVersion+`"`) {
		t.Errorf("initialize with an unknown version: %s", r.body)
	}
	hdr := map[string]string{"Mcp-Session-Id": sid, "MCP-Protocol-Version": "2025-06-18"}
	if r := rawDo(t, http.MethodPost, ts.URL, hdr, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); r.status != 202 {
		t.Fatalf("initialized: %d", r.status)
	}
	r = rawDo(t, http.MethodPost, ts.URL, hdr, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if r.status != 200 || r.header.Get("Content-Type") != "text/event-stream" || !strings.Contains(r.body, `"echo"`) ||
		!strings.Contains(r.body, "id: ") {
		t.Errorf("tools/list in session: %d %s", r.status, r.body)
	}
	if r := rawDo(t, http.MethodPost, ts.URL, nil, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`); r.status != 400 {
		t.Errorf("request without a session or meta: %d %s", r.status, r.body)
	}
	if r := rawDo(t, http.MethodPost, ts.URL, map[string]string{"Mcp-Session-Id": "nope"}, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`); r.status != 404 {
		t.Errorf("unknown session: %d", r.status)
	}
	bad := map[string]string{"Mcp-Session-Id": sid, "MCP-Protocol-Version": LegacyProtocolVersion}
	if r := rawDo(t, http.MethodPost, ts.URL, bad, `{"jsonrpc":"2.0","id":4,"method":"tools/list"}`); r.status != 400 {
		t.Errorf("version mismatch: %d", r.status)
	}
	if r := rawDo(t, http.MethodPost, ts.URL, hdr, init); r.status != 400 {
		t.Errorf("initialize in a session: %d", r.status)
	}
	get := map[string]string{"Mcp-Session-Id": sid, "Accept": "application/json"}
	if r := rawDo(t, http.MethodGet, ts.URL, get, ""); r.status != 406 {
		t.Errorf("GET without text/event-stream: %d", r.status)
	}
	if r := rawDo(t, http.MethodGet, ts.URL, map[string]string{"Mcp-Session-Id": sid, "Last-Event-ID": "99.1"}, ""); r.status != 404 {
		t.Errorf("GET resuming an unknown stream: %d", r.status)
	}
	if r := rawDo(t, http.MethodDelete, ts.URL, hdr, ""); r.status != 204 {
		t.Errorf("DELETE: %d", r.status)
	}
	if r := rawDo(t, http.MethodPost, ts.URL, hdr, `{"jsonrpc":"2.0","id":5,"method":"tools/list"}`); r.status != 404 {
		t.Errorf("request after DELETE: %d", r.status)
	}
	if r := rawDo(t, http.MethodDelete, ts.URL, hdr, ""); r.status != 404 {
		t.Errorf("second DELETE: %d", r.status)
	}
	h.mu.Lock()
	n := len(h.sessions)
	h.mu.Unlock()
	if n != 1 {
		t.Errorf("%d sessions left, want the second initialize's", n)
	}
}

// sseReader reads the events of a stream.
type sseReader struct {
	resp *http.Response
	r    *bufio.Reader
}

func openStream(t *testing.T, method, url string, header map[string]string, body string) *sseReader {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("stream: %d %s", resp.StatusCode, b)
	}
	return &sseReader{resp: resp, r: bufio.NewReader(resp.Body)}
}

// next returns the ID and data of the next event.
func (s *sseReader) next(t *testing.T) (id, data string) {
	t.Helper()
	got := false
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			t.Fatalf("read event: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if got {
				return id, data
			}
		case strings.HasPrefix(line, "id: "):
			id, got = line[4:], true
		case strings.HasPrefix(line, "data: "):
			data, got = line[6:], true
		case strings.HasPrefix(line, "data:"):
			got = true
		}
	}
}

func TestHTTPServerResumption(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	gate := tool.Func("gate", "Wait for the test.", func(ctx context.Context, _ struct{}) (string, error) {
		if err := Log(ctx, LogError, "gate", "waiting"); err != nil {
			return "", err
		}
		<-release
		return "released", nil
	})
	_, ts, _ := newHTTPFixture(t, ServerOptions{Tools: []tool.Tool{gate}}, HTTPOptions{})
	t.Cleanup(unblock)
	r := rawDo(t, http.MethodPost, ts.URL, nil,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`)
	sid := r.header.Get("Mcp-Session-Id")
	hdr := map[string]string{"Mcp-Session-Id": sid, "MCP-Protocol-Version": LegacyProtocolVersion}
	rawDo(t, http.MethodPost, ts.URL, hdr, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	rawDo(t, http.MethodPost, ts.URL, hdr, `{"jsonrpc":"2.0","id":2,"method":"logging/setLevel","params":{"level":"info"}}`)
	st := openStream(t, http.MethodPost, ts.URL, hdr, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"gate"}}`)
	t.Cleanup(func() { _ = st.resp.Body.Close() })
	prime, data := st.next(t)
	if data != "" || !strings.HasSuffix(prime, ".0") {
		t.Fatalf("priming event %q %q", prime, data)
	}
	logID, data := st.next(t)
	if !strings.Contains(data, "notifications/message") {
		t.Fatalf("log event %q", data)
	}
	// The connection breaks; the request goes on.
	st.resp.Body.Close()
	unblock()
	// Resuming after the priming event replays the log message too.
	again := openStream(t, http.MethodGet, ts.URL, map[string]string{"Mcp-Session-Id": sid, "Last-Event-ID": prime}, "")
	defer again.resp.Body.Close()
	if id, data := again.next(t); id != logID || !strings.Contains(data, "notifications/message") {
		t.Fatalf("replayed %q %q", id, data)
	}
	if _, data := again.next(t); !strings.Contains(data, `"released"`) || !strings.Contains(data, `"id":3`) {
		t.Fatalf("response %q", data)
	}
}

// countingHandler counts the requests of each method.
type countingHandler struct {
	h      http.Handler
	mu     sync.Mutex
	counts map[string]int
}

func (c *countingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.Method
	if r.Header.Get("Last-Event-ID") != "" {
		key += " resume"
	}
	c.mu.Lock()
	c.counts[key]++
	c.mu.Unlock()
	c.h.ServeHTTP(w, r)
}

func (c *countingHandler) count(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[key]
}

func TestHTTPServerPollingWithClient(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		slow := tool.Func("snooze", "Sleep.", func(ctx context.Context, _ struct{}) (string, error) {
			select {
			case <-time.After(300 * time.Millisecond):
				return "awake", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		})
		f := newFixture(t, ServerOptions{Tools: []tool.Tool{slow}})
		h := f.s.HTTPHandler(HTTPOptions{PollInterval: 40 * time.Millisecond, RetryInterval: 10 * time.Millisecond})
		ch := &countingHandler{h: h, counts: map[string]int{}}
		ts := httptest.NewTestServer(t, ch)
		defer h.Close()
		c, err := Connect(t.Context(), serverHTTP(ts), WithProtocolVersion(LegacyProtocolVersion))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if out, isErr := callText(t, c, "snooze", `{}`); out != "awake" || isErr {
			t.Fatalf("snooze = %q", out)
		}
		if ch.count("GET resume") == 0 {
			t.Error("the client did not resume the polled stream")
		}
	})
}

func TestHTTPServerSessionExpiryAndRenewal(t *testing.T) {
	t.Parallel()
	f := newFixture(t, ServerOptions{})
	c, _, h := connectHTTPServer(t, f.s, HTTPOptions{SessionTimeout: 30 * time.Millisecond}, WithProtocolVersion(LegacyProtocolVersion))
	ok := eventually(func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.sessions) == 0
	})
	if !ok {
		t.Fatal("the idle session did not expire")
	}
	// The client starts a new session on 404.
	if out, _ := callText(t, c, "echo", `{"text":"again"}`); out != "echo: again" {
		t.Errorf("echo = %q", out)
	}
}

func TestHTTPServerStandaloneStream(t *testing.T) {
	t.Parallel()
	f, ts, _ := newHTTPFixture(t, ServerOptions{}, HTTPOptions{})
	r := rawDo(t, http.MethodPost, ts.URL, nil,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`)
	sid := r.header.Get("Mcp-Session-Id")
	hdr := map[string]string{"Mcp-Session-Id": sid, "MCP-Protocol-Version": LegacyProtocolVersion}
	rawDo(t, http.MethodPost, ts.URL, hdr, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	st := openStream(t, http.MethodGet, ts.URL, hdr, "")
	defer st.resp.Body.Close()
	if _, data := st.next(t); data != "" {
		t.Fatalf("priming event %q", data)
	}
	if r := rawDo(t, http.MethodGet, ts.URL, hdr, ""); r.status != http.StatusConflict {
		t.Errorf("second GET stream: %d", r.status)
	}
	f.s.ToolsChanged()
	if _, data := st.next(t); !strings.Contains(data, "notifications/tools/list_changed") {
		t.Errorf("notification %q", data)
	}
}

func TestHTTPServerModernOnly(t *testing.T) {
	t.Parallel()
	f := newFixture(t, ServerOptions{ModernOnly: true})
	c, ts, _ := connectHTTPServer(t, f.s, HTTPOptions{})
	if c.ProtocolVersion() != ProtocolVersion || len(c.Server().SupportedVersions) != 1 {
		t.Errorf("version %s, supported %v", c.ProtocolVersion(), c.Server().SupportedVersions)
	}
	_, err := Connect(t.Context(), HTTP(ts.URL), WithProtocolVersion(LegacyProtocolVersion))
	if err == nil || !strings.Contains(err.Error(), ProtocolVersion) {
		t.Errorf("legacy client: %v", err)
	}
	r := rawDo(t, http.MethodGet, ts.URL, map[string]string{"Mcp-Session-Id": "x"}, "")
	if r.status != 405 {
		t.Errorf("GET: %d", r.status)
	}
	// Over stdio too.
	if _, err := dialStdio(t, f.s, WithProtocolVersion(LegacyProtocolVersion)); err == nil || !strings.Contains(err.Error(), ProtocolVersion) {
		t.Errorf("legacy stdio client: %v", err)
	}
}

// switchHandler sends the first request to a and the others to b.
type switchHandler struct {
	a, b http.Handler
	n    atomic.Int32
}

func (s *switchHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Mcp-Method") == "tools/call" && s.n.Add(1) == 1 {
		s.a.ServeHTTP(w, r)
		return
	}
	s.b.ServeHTTP(w, r)
}

func TestInputRequiredReplayOnAnotherInstance(t *testing.T) {
	t.Parallel()
	key := []byte("shared state key")
	f1 := newFixture(t, ServerOptions{StateKey: key})
	f2 := newFixture(t, ServerOptions{StateKey: key})
	h1, h2 := f1.s.HTTPHandler(HTTPOptions{}), f2.s.HTTPHandler(HTTPOptions{})
	ts := httptest.NewServer(&switchHandler{a: h1, b: h2})
	defer ts.Close()
	var prompts []string
	c, err := Connect(t.Context(), HTTP(ts.URL), WithElicitation(func(_ context.Context, req *ElicitRequest) (*ElicitResult, error) {
		prompts = append(prompts, req.Message)
		return &ElicitResult{Action: ElicitAccept, Content: map[string]any{"name": strings.Fields(req.Message)[0]}}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	out, isErr := callText(t, c, "ask", `{}`)
	if isErr || out != "accept accept first last" {
		t.Fatalf("ask = %q", out)
	}
	// The first instance asked the first question; the second, which
	// replayed the handler with the first answer, asked the second.
	if f1.asks.Load() != 1 || f2.asks.Load() != 1 || len(prompts) != 2 {
		t.Errorf("runs %d and %d, prompts %v", f1.asks.Load(), f2.asks.Load(), prompts)
	}
	// The first instance still waits for its retry until the timeout.
	f1.s.mu.Lock()
	n := len(f1.s.conts)
	f1.s.mu.Unlock()
	if n != 1 {
		t.Errorf("first instance holds %d handlers", n)
	}
}

func TestInputRequiredStateValidation(t *testing.T) {
	t.Parallel()
	f, ts, _ := newHTTPFixture(t, ServerOptions{InputTimeout: time.Hour}, HTTPOptions{})
	call := func(id int, args map[string]any, extra map[string]any) map[string]any {
		params := map[string]any{"name": "ask", "arguments": args}
		maps.Copy(params, extra)
		body, h := modernRequest(id, "tools/call", params)
		b := strings.Replace(body, `clientCapabilities":{}`, `clientCapabilities":{"elicitation":{}}`, 1)
		r := rawDo(t, http.MethodPost, ts.URL, h, b)
		var m struct {
			Result map[string]any `json:"result"`
			Error  *Error         `json:"error"`
		}
		if err := json.Unmarshal([]byte(r.body), &m); err != nil {
			t.Fatalf("%d %s", r.status, r.body)
		}
		if m.Error != nil {
			return map[string]any{"error": m.Error.Code}
		}
		return m.Result
	}
	first := call(1, map[string]any{}, nil)
	if first["resultType"] != "input_required" || first["requestState"] == nil {
		t.Fatalf("first = %v", first)
	}
	reqs, _ := first["inputRequests"].(map[string]any)
	if len(reqs) != 1 || reqs["1"] == nil {
		t.Fatalf("inputRequests = %v", reqs)
	}
	state := first["requestState"].(string)
	answer := map[string]any{"1": map[string]any{"action": "accept", "content": map[string]any{"name": "Ada"}}}
	for _, tt := range []struct {
		name  string
		args  map[string]any
		extra map[string]any
	}{
		{"tampered", map[string]any{}, map[string]any{"requestState": state[:len(state)-2] + "AA", "inputResponses": answer}},
		{"other arguments", map[string]any{"x": 1}, map[string]any{"requestState": state, "inputResponses": answer}},
		{"garbage", map[string]any{}, map[string]any{"requestState": "garbage", "inputResponses": answer}},
	} {
		if got := call(2, tt.args, tt.extra); got["error"] != CodeInvalidParams {
			t.Errorf("%s: %v", tt.name, got)
		}
	}
	// A retry without the answer is asked again.
	again := call(3, map[string]any{}, map[string]any{"requestState": state})
	if again["resultType"] != "input_required" {
		t.Fatalf("retry without answer = %v", again)
	}
	state = again["requestState"].(string)
	second := call(4, map[string]any{}, map[string]any{"requestState": state, "inputResponses": answer})
	if second["resultType"] != "input_required" || second["inputRequests"].(map[string]any)["2"] == nil {
		t.Fatalf("second = %v", second)
	}
	final := call(5, map[string]any{}, map[string]any{"requestState": second["requestState"],
		"inputResponses": map[string]any{"2": map[string]any{"action": "decline"}}})
	if final["resultType"] != "complete" || !strings.Contains(fmt.Sprint(final["content"]), "accept decline Ada") {
		t.Fatalf("final = %v", final)
	}
	if f.asks.Load() != 1 {
		t.Errorf("ask ran %d times", f.asks.Load())
	}
}

func TestInputRequiredExpiry(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, ServerOptions{InputTimeout: 50 * time.Millisecond})
		ts := httptest.NewTestServer(t, f.s.HTTPHandler(HTTPOptions{}))
		slowUser := func(ctx context.Context, _ *ElicitRequest) (*ElicitResult, error) {
			time.Sleep(200 * time.Millisecond)
			return &ElicitResult{Action: ElicitAccept, Content: map[string]any{"name": "x"}}, nil
		}
		c, err := Connect(t.Context(), serverHTTP(ts), WithElicitation(slowUser))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_, err = c.CallTool(t.Context(), "ask", nil)
		if e, ok := errors.AsType[*Error](err); !ok || e.Code != CodeInvalidParams || !strings.Contains(e.Message, "requestState") {
			t.Fatalf("CallTool after the input timeout = %v", err)
		}
		ok := eventually(func() bool {
			f.s.mu.Lock()
			defer f.s.mu.Unlock()
			return len(f.s.conts) == 0
		})
		if !ok {
			t.Error("the expired handler was kept")
		}
	})
}

func TestInputRequiredCancelledByClient(t *testing.T) {
	t.Parallel()
	f := newFixture(t, ServerOptions{})
	started := make(chan struct{})
	block := func(ctx context.Context, _ *ElicitRequest) (*ElicitResult, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	c, _, _ := connectHTTPServer(t, f.s, HTTPOptions{}, WithElicitation(block))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		_, err := c.CallTool(ctx, "ask", nil)
		errc <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("elicitation did not start")
	}
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("CallTool = %v, want cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CallTool did not end after cancellation")
	}
	// The handler waits for the retry until the input timeout.
	f.s.mu.Lock()
	n := len(f.s.conts)
	f.s.mu.Unlock()
	if n != 1 {
		t.Errorf("%d waiting handlers", n)
	}
	_ = f.s.Close()
}
