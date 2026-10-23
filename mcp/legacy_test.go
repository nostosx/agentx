package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// wireMessage is any JSON-RPC message seen by a fake server.
type wireMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *Error          `json:"error"`
}

func (m wireMessage) param(key string) string {
	var p map[string]any
	_ = json.Unmarshal(m.Params, &p)
	s, _ := p[key].(string)
	return s
}

// hasProtocolMeta reports whether a request carries the per-request
// protocol fields of ProtocolVersion.
func (m wireMessage) hasProtocolMeta() bool {
	var p struct {
		Meta map[string]any `json:"_meta"`
	}
	_ = json.Unmarshal(m.Params, &p)
	_, ok := p.Meta[metaProtocolVersion]
	return ok
}

const legacyCaps = `{"tools":{"listChanged":true},"resources":{"subscribe":true},"prompts":{}}`

func initResult(id json.RawMessage, version, caps string) string {
	return result(id, fmt.Sprintf(`{"protocolVersion":%q,"capabilities":%s,`+
		`"serverInfo":{"name":"legacy","version":"0.9"},"instructions":"be nice"}`, version, caps))
}

// pipeServer returns a transport connected to serve, which receives every
// message the client writes.
func pipeServer(t *testing.T, serve func(m wireMessage, write func(string))) *IOTransport {
	t.Helper()
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	var mu sync.Mutex
	write := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprint(sw, s+"\n")
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		_ = cw.Close()
		_ = cr.Close()
		_ = sr.Close()
		_ = sw.Close()
		<-done
	})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(sr)
		for sc.Scan() {
			var m wireMessage
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				continue
			}
			serve(m, write)
		}
		_ = sw.Close()
	}()
	return NewIOTransport(cr, cw, func() error {
		_ = cw.Close()
		return cr.Close()
	})
}

// legacyStdio is a fake server implementing an initialization-based
// revision over stdio.
type legacyStdio struct {
	t *testing.T
	// version answers initialize; caps are the capabilities.
	version, caps string
	// discover answers server/discover, or stays silent if empty.
	discover func(id json.RawMessage) string
	// handle answers other requests.
	handle func(m wireMessage, write func(string))

	mu          sync.Mutex
	received    []wireMessage
	initialized bool
	responses   chan wireMessage
}

func (s *legacyStdio) serve(m wireMessage, write func(string)) {
	s.mu.Lock()
	s.received = append(s.received, m)
	initialized := s.initialized
	s.mu.Unlock()
	switch {
	case m.Method == "" && len(m.ID) > 0:
		s.responses <- m
	case m.Method == "server/discover":
		if s.discover != nil {
			if r := s.discover(m.ID); r != "" {
				write(r)
			}
		}
	case m.Method == "initialize":
		if m.hasProtocolMeta() {
			s.t.Errorf("initialize carries _meta protocol fields")
		}
		write(initResult(m.ID, s.version, s.caps))
	case m.Method == "notifications/initialized":
		s.mu.Lock()
		s.initialized = true
		s.mu.Unlock()
	case !initialized && len(m.ID) > 0:
		write(rpcError(m.ID, CodeInvalidRequest, "not initialized"))
	default:
		if len(m.ID) > 0 && m.hasProtocolMeta() {
			s.t.Errorf("%s carries _meta protocol fields", m.Method)
		}
		if s.handle != nil {
			s.handle(m, write)
		}
	}
}

func (s *legacyStdio) methods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, m := range s.received {
		if m.Method != "" {
			out = append(out, m.Method)
		}
	}
	return out
}

func newLegacyStdio(t *testing.T) *legacyStdio {
	return &legacyStdio{
		t: t, version: LegacyProtocolVersion, caps: legacyCaps,
		responses: make(chan wireMessage, 8),
		discover: func(id json.RawMessage) string {
			return rpcError(id, CodeMethodNotFound, "Method not found")
		},
	}
}

func (s *legacyStdio) connect(t *testing.T, opts ...ClientOption) (*Client, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, err := Connect(ctx, pipeServer(t, s.serve), opts...)
	if c != nil {
		t.Cleanup(func() { _ = c.Close() })
	}
	return c, err
}

func TestLegacyStdioFallback(t *testing.T) {
	s := newLegacyStdio(t)
	s.handle = func(m wireMessage, write func(string)) {
		switch m.Method {
		case "tools/list":
			write(result(m.ID, `{"tools":[{"name":"echo","inputSchema":{"type":"object"}}]}`))
		case "tools/call":
			// Server requests and notifications before the response.
			write(`{"jsonrpc":"2.0","id":"s1","method":"ping"}`)
			write(`{"jsonrpc":"2.0","id":"s2","method":"sampling/createMessage","params":{}}`)
			write(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`)
			write(result(m.ID, `{"content":[{"type":"text","text":"hi"}]}`))
		}
	}
	notes := make(chan string, 4)
	c, err := s.connect(t, OnNotification(func(method string, _ json.RawMessage) { notes <- method }))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.ProtocolVersion(); got != LegacyProtocolVersion {
		t.Errorf("ProtocolVersion = %q", got)
	}
	srv := c.Server()
	if srv.ServerInfo.Name != "legacy" || srv.Instructions != "be nice" ||
		!slices.Equal(srv.SupportedVersions, []string{LegacyProtocolVersion}) {
		t.Errorf("Server = %+v", srv)
	}
	tools, err := c.ListTools(t.Context())
	if err != nil || len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v, err = %v", tools, err)
	}
	res, err := c.CallTool(t.Context(), "echo", nil)
	if err != nil || FormatContent(res) != "hi" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if got := s.methods(); len(got) < 3 || !slices.Equal(got[:3], []string{"server/discover", "initialize", "notifications/initialized"}) {
		t.Errorf("handshake = %v", got)
	}
	answers := map[string]wireMessage{}
	for range 2 {
		select {
		case m := <-s.responses:
			answers[idKey(m.ID)] = m
		case <-time.After(5 * time.Second):
			t.Fatal("server request not answered")
		}
	}
	if m := answers["s1"]; string(m.Result) != "{}" || m.Error != nil {
		t.Errorf("ping answer = %+v", m)
	}
	if m := answers["s2"]; m.Error == nil || m.Error.Code != CodeMethodNotFound {
		t.Errorf("sampling answer = %+v", m)
	}
	if m := <-notes; m != "notifications/tools/list_changed" {
		t.Errorf("notification = %q", m)
	}
}

func TestLegacyStdioSilentProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newLegacyStdio(t)
		s.discover = nil // a server that ignores requests before initialize
		start := time.Now()
		c, err := s.connect(t, WithProbeTimeout(100*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed != 100*time.Millisecond {
			t.Errorf("fallback took %v, want probe timeout 100ms", elapsed)
		}
		if c.ProtocolVersion() != LegacyProtocolVersion {
			t.Errorf("ProtocolVersion = %q", c.ProtocolVersion())
		}
		// The unanswered probe is not cancelled.
		if slices.Contains(s.methods(), "notifications/cancelled") {
			t.Errorf("methods = %v", s.methods())
		}
	})
}

func TestLegacyNegotiation(t *testing.T) {
	t.Run("older version", func(t *testing.T) {
		s := newLegacyStdio(t)
		s.version = "2025-06-18"
		c, err := s.connect(t)
		if err != nil {
			t.Fatal(err)
		}
		if c.ProtocolVersion() != "2025-06-18" {
			t.Errorf("ProtocolVersion = %q", c.ProtocolVersion())
		}
	})
	t.Run("unsupported version", func(t *testing.T) {
		s := newLegacyStdio(t)
		s.version = "2024-01-01"
		_, err := s.connect(t)
		if err == nil || !strings.Contains(err.Error(), `unsupported protocol version "2024-01-01"`) {
			t.Fatalf("err = %v", err)
		}
		if slices.Contains(s.methods(), "notifications/initialized") {
			t.Error("initialized sent after an unsupported version")
		}
	})
	t.Run("initialize error lists versions", func(t *testing.T) {
		var offers []string
		tr := pipeServer(t, func(m wireMessage, write func(string)) {
			switch m.Method {
			case "server/discover":
				write(rpcError(m.ID, CodeInvalidParams, "unknown method"))
			case "initialize":
				var p struct {
					ProtocolVersion string `json:"protocolVersion"`
				}
				_ = json.Unmarshal(m.Params, &p)
				offers = append(offers, p.ProtocolVersion)
				if p.ProtocolVersion != "2025-03-26" {
					write(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32602,"message":"Unsupported protocol version",`+
						`"data":{"supported":["2025-03-26"],"requested":%q}}}`, m.ID, p.ProtocolVersion))
					return
				}
				write(initResult(m.ID, "2025-03-26", `{"tools":{}}`))
			}
		})
		c, err := Connect(t.Context(), tr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if !slices.Equal(offers, []string{LegacyProtocolVersion, "2025-03-26"}) || c.ProtocolVersion() != "2025-03-26" {
			t.Errorf("offers = %v, version = %q", offers, c.ProtocolVersion())
		}
	})
	t.Run("modern server without our version", func(t *testing.T) {
		s := newLegacyStdio(t)
		s.discover = func(id json.RawMessage) string {
			return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32022,"message":"Unsupported protocol version",`+
				`"data":{"supported":["2099-01-01"]}}}`, id)
		}
		_, err := s.connect(t)
		if err == nil || !strings.Contains(err.Error(), "2099-01-01") {
			t.Fatalf("err = %v", err)
		}
		if slices.Contains(s.methods(), "initialize") {
			t.Error("fell back to initialize after a modern error")
		}
	})
	t.Run("modern error is not a fallback", func(t *testing.T) {
		s := newLegacyStdio(t)
		s.discover = func(id json.RawMessage) string {
			return rpcError(id, CodeMissingRequiredClientCapability, "need roots")
		}
		if _, err := s.connect(t); err == nil || slices.Contains(s.methods(), "initialize") {
			t.Fatalf("err = %v, methods = %v", err, s.methods())
		}
	})
	t.Run("dual-era server offering a legacy version", func(t *testing.T) {
		s := newLegacyStdio(t)
		s.discover = func(id json.RawMessage) string {
			return result(id, `{"supportedVersions":["2025-06-18"],"capabilities":{}}`)
		}
		s.version = "2025-06-18"
		c, err := s.connect(t)
		if err != nil {
			t.Fatal(err)
		}
		if c.ProtocolVersion() != "2025-06-18" {
			t.Errorf("ProtocolVersion = %q", c.ProtocolVersion())
		}
	})
}

func TestWithProtocolVersion(t *testing.T) {
	t.Run("legacy pin skips the probe", func(t *testing.T) {
		s := newLegacyStdio(t)
		c, err := s.connect(t, WithProtocolVersion(LegacyProtocolVersion))
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(s.methods(), "server/discover") || c.ProtocolVersion() != LegacyProtocolVersion {
			t.Errorf("methods = %v, version = %q", s.methods(), c.ProtocolVersion())
		}
	})
	t.Run("legacy pin requires that version", func(t *testing.T) {
		s := newLegacyStdio(t)
		s.version = "2025-06-18"
		if _, err := s.connect(t, WithProtocolVersion(LegacyProtocolVersion)); err == nil {
			t.Fatal("no error")
		}
	})
	t.Run("modern pin does not fall back", func(t *testing.T) {
		s := newLegacyStdio(t)
		_, err := s.connect(t, WithProtocolVersion(ProtocolVersion))
		if err == nil || !strings.Contains(err.Error(), "WithProtocolVersion") || slices.Contains(s.methods(), "initialize") {
			t.Fatalf("err = %v, methods = %v", err, s.methods())
		}
	})
	t.Run("unknown version", func(t *testing.T) {
		_, err := Connect(t.Context(), pipeServer(t, func(wireMessage, func(string)) {}), WithProtocolVersion("1.0"))
		if err == nil || !strings.Contains(err.Error(), `"1.0"`) {
			t.Fatalf("err = %v", err)
		}
	})
	if got := SupportedProtocolVersions(); got[0] != ProtocolVersion || !slices.Contains(got, "2025-06-18") {
		t.Errorf("SupportedProtocolVersions = %v", got)
	}
}

func TestLegacyCapabilities(t *testing.T) {
	s := newLegacyStdio(t)
	s.caps = `{"resources":{}}`
	c, err := s.connect(t)
	if err != nil {
		t.Fatal(err)
	}
	if tools, err := c.ListTools(t.Context()); err != nil || len(tools) != 0 {
		t.Errorf("tools = %v, err = %v", tools, err)
	}
	if prompts, err := c.ListPrompts(t.Context()); err != nil || len(prompts) != 0 {
		t.Errorf("prompts = %v, err = %v", prompts, err)
	}
	if _, err := c.CallTool(t.Context(), "x", nil); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("CallTool err = %v", err)
	}
	if _, err := c.GetPrompt(t.Context(), "x", nil); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("GetPrompt err = %v", err)
	}
	if err := c.Listen(t.Context(), Subscription{ResourceSubscriptions: []string{"file:///a"}}); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Listen err = %v", err)
	}
	for _, m := range s.methods() {
		if strings.HasPrefix(m, "tools/") || strings.HasPrefix(m, "prompts/") || strings.HasPrefix(m, "resources/") {
			t.Errorf("sent %s", m)
		}
	}
}

func TestLegacyStdioListen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newLegacyStdio(t)
		s.handle = func(m wireMessage, write func(string)) {
			switch m.Method {
			case "resources/subscribe":
				write(result(m.ID, `{}`))
				write(fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/resources/updated","params":{"uri":%q}}`, m.param("uri")))
			case "resources/unsubscribe":
				write(result(m.ID, `{}`))
			}
		}
		type note struct {
			method string
			params json.RawMessage
		}
		notes := make(chan note, 4)
		c, err := s.connect(t, OnNotification(func(method string, params json.RawMessage) { notes <- note{method, params} }))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			done <- c.Listen(ctx, Subscription{ToolsListChanged: true, PromptsListChanged: true, ResourceSubscriptions: []string{"file:///a"}})
		}()
		got := map[string]json.RawMessage{}
		for range 2 {
			n := <-notes
			got[n.method] = n.params
		}
		if _, ok := got["notifications/resources/updated"]; !ok {
			t.Errorf("notifications = %v", got)
		}
		var ack struct {
			Notifications Subscription `json:"notifications"`
		}
		_ = json.Unmarshal(got["notifications/subscriptions/acknowledged"], &ack)
		// The server advertises listChanged for tools only.
		if !ack.Notifications.ToolsListChanged || ack.Notifications.PromptsListChanged ||
			!slices.Equal(ack.Notifications.ResourceSubscriptions, []string{"file:///a"}) {
			t.Errorf("ack = %s", got["notifications/subscriptions/acknowledged"])
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("Listen = %v", err)
		}
		if !slices.Contains(s.methods(), "resources/unsubscribe") {
			t.Errorf("methods = %v", s.methods())
		}
	})
}

func TestLegacyStdioCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cancelled := make(chan string, 1)
		s := newLegacyStdio(t)
		s.handle = func(m wireMessage, _ func(string)) {
			if m.Method == "notifications/cancelled" {
				var p struct {
					RequestID json.RawMessage `json:"requestId"`
				}
				_ = json.Unmarshal(m.Params, &p)
				cancelled <- string(p.RequestID)
			}
		}
		c, err := s.connect(t)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		if _, err := c.CallTool(ctx, "slow", nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
		select {
		case id := <-cancelled:
			if id == "" {
				t.Error("no requestId")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no notifications/cancelled")
		}
	})
}

// legacyHTTP is a fake Streamable HTTP server implementing an
// initialization-based revision with sessions.
type legacyHTTP struct {
	t       *testing.T
	version string
	caps    string
	// probe answers server/discover before initialize; it writes the
	// whole response.
	probe func(w http.ResponseWriter, m wireMessage)
	// handle answers requests in a session; "" means 202.
	handle func(m wireMessage) string
	// noStream refuses GET with 405.
	noStream bool
	// refuseResume refuses GET requests with a Last-Event-ID with 405.
	refuseResume bool
	// initGate, if set, holds back the answers to initialize requests
	// after the first; initStarted receives the session ID they will get.
	initGate    chan struct{}
	initStarted chan string

	mu        sync.Mutex
	sessions  int
	live      map[string]bool
	posts     []wireMessage
	postedIn  []string // the session of each post
	deleted   []string
	responses chan wireMessage
	push      chan string
	streams   chan string // session of each opened GET stream
	resumes   chan string // Last-Event-ID of each resumed GET stream
}

func newLegacyHTTP(t *testing.T) *legacyHTTP {
	return &legacyHTTP{
		t: t, version: LegacyProtocolVersion, caps: legacyCaps,
		live:      map[string]bool{},
		responses: make(chan wireMessage, 8),
		push:      make(chan string, 8),
		streams:   make(chan string, 8),
		resumes:   make(chan string, 8),
		probe: func(w http.ResponseWriter, m wireMessage) {
			// The TypeScript SDK's answer to requests without a session.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"jsonrpc":"2.0","error":{"code":-32000,"message":"Bad Request: Server not initialized"},"id":null}`)
		},
	}
}

// start serves s on an in-memory network; connect to it with
// serverHTTP.
func (s *legacyHTTP) start() *httptest.Server {
	return httptest.NewTestServer(s.t, s)
}

func (s *legacyHTTP) expireAll() {
	s.mu.Lock()
	clear(s.live)
	s.mu.Unlock()
}

func (s *legacyHTTP) methods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, m := range s.posts {
		out = append(out, m.Method)
	}
	return out
}

func (s *legacyHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t := s.t
	session := r.Header.Get("Mcp-Session-Id")
	s.mu.Lock()
	live := s.live[session]
	s.mu.Unlock()
	if r.Method != http.MethodPost {
		switch {
		case session == "":
			http.Error(w, "no session", http.StatusBadRequest)
		case !live:
			http.Error(w, "session not found", http.StatusNotFound)
		case r.Method == http.MethodDelete:
			s.mu.Lock()
			s.deleted = append(s.deleted, session)
			delete(s.live, session)
			s.mu.Unlock()
		case r.Method == http.MethodGet && s.noStream:
			w.WriteHeader(http.StatusMethodNotAllowed)
		case r.Method == http.MethodGet:
			if v := r.Header.Get("MCP-Protocol-Version"); v != s.version {
				t.Errorf("GET MCP-Protocol-Version = %q", v)
			}
			if id := r.Header.Get("Last-Event-ID"); id != "" {
				s.resumes <- id
				if s.refuseResume {
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "retry: 10\nid: prime\ndata:\n\n")
			w.(http.Flusher).Flush()
			s.streams <- session
			for {
				select {
				case msg := <-s.push:
					if msg == "" {
						return // the server closes the stream
					}
					fmt.Fprintf(w, "data: %s\n\n", msg)
					w.(http.Flusher).Flush()
				case <-r.Context().Done():
					return
				}
			}
		}
		return
	}
	var m wireMessage
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.posts = append(s.posts, m)
	s.postedIn = append(s.postedIn, session)
	s.mu.Unlock()
	// The probe is a request of ProtocolVersion.
	if m.Method != "server/discover" && (r.Header.Get("Mcp-Method") != "" || r.Header.Get("Mcp-Name") != "") {
		t.Errorf("%s: modern standard headers sent", m.Method)
	}
	switch {
	case m.Method == "initialize":
		if session != "" {
			t.Errorf("initialize sent with session %q", session)
		}
		if v := r.Header.Get("MCP-Protocol-Version"); v != "" {
			t.Errorf("initialize sent with MCP-Protocol-Version %q", v)
		}
		s.mu.Lock()
		s.sessions++
		id := fmt.Sprintf("sess-%d", s.sessions)
		renewal := s.sessions > 1
		s.mu.Unlock()
		if renewal && s.initGate != nil {
			s.initStarted <- id
			<-s.initGate
		}
		s.mu.Lock()
		s.live[id] = true
		s.mu.Unlock()
		w.Header().Set("Mcp-Session-Id", id)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, initResult(m.ID, s.version, s.caps))
		return
	case session == "":
		s.probe(w, m)
		return
	case !live:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"jsonrpc":"2.0","error":{"code":-32001,"message":"Session not found"},"id":null}`)
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != s.version {
		t.Errorf("%s: MCP-Protocol-Version = %q, want %q", m.Method, v, s.version)
	}
	if len(m.ID) > 0 && m.Method != "" && m.hasProtocolMeta() {
		t.Errorf("%s carries _meta protocol fields", m.Method)
	}
	if m.Method == "" {
		s.responses <- m
	}
	body := ""
	if m.Method != "" && len(m.ID) > 0 && s.handle != nil {
		body = s.handle(m)
	}
	if body == "" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	// Answer on an SSE stream with a priming event, as 2025-11-25 servers do.
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "id: 1\ndata:\n\nid: 2\ndata: %s\n\n", body)
}

// serverHTTP returns a Streamable HTTP transport to srv, which may serve
// on an in-memory network.
func serverHTTP(srv *httptest.Server) *HTTPTransport {
	client := srv.Client() // sets srv.URL
	tr := HTTP(srv.URL)
	tr.Client = client
	return tr
}

func connectLegacyHTTP(t *testing.T, srv *httptest.Server, opts ...ClientOption) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, err := Connect(ctx, serverHTTP(srv), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestLegacyHTTPSession(t *testing.T) {
	s := newLegacyHTTP(t)
	s.handle = func(m wireMessage) string {
		switch m.Method {
		case "tools/list":
			// x-mcp-header annotations do not apply to earlier revisions.
			return result(m.ID, `{"tools":[{"name":"sql","inputSchema":{"type":"object","properties":{`+
				`"a":{"type":"number","x-mcp-header":"A"}}}}]}`)
		case "tools/call":
			return result(m.ID, `{"content":[{"type":"text","text":"ok"}]}`)
		}
		return ""
	}
	srv := s.start()
	notes := make(chan string, 8)
	c := connectLegacyHTTP(t, srv, OnNotification(func(method string, _ json.RawMessage) { notes <- method }))
	if c.ProtocolVersion() != LegacyProtocolVersion {
		t.Fatalf("ProtocolVersion = %q", c.ProtocolVersion())
	}
	if got := s.methods(); !slices.Equal(got, []string{"server/discover", "initialize", "notifications/initialized"}) {
		t.Errorf("handshake = %v", got)
	}
	tools, err := c.ListTools(t.Context())
	if err != nil || len(tools) != 1 {
		t.Fatalf("tools = %+v, err = %v", tools, err)
	}
	res, err := c.CallTool(t.Context(), "sql", json.RawMessage(`{"a":1}`))
	if err != nil || FormatContent(res) != "ok" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}

	// Listen opens the session's GET stream, which carries notifications
	// and server requests.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Listen(ctx, Subscription{ToolsListChanged: true}) }()
	if got := <-s.streams; got != "sess-1" {
		t.Errorf("GET session = %q", got)
	}
	if m := <-notes; m != "notifications/subscriptions/acknowledged" {
		t.Errorf("notification = %q", m)
	}
	s.push <- `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`
	s.push <- `{"jsonrpc":"2.0","id":7,"method":"ping"}`
	if m := <-notes; m != "notifications/tools/list_changed" {
		t.Errorf("notification = %q", m)
	}
	select {
	case m := <-s.responses:
		if string(m.ID) != "7" || string(m.Result) != "{}" {
			t.Errorf("ping answer = %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ping not answered")
	}
	// The server closes the connection; the client resumes the stream
	// after the event it received last.
	s.push <- ""
	select {
	case id := <-s.resumes:
		if id != "prime" {
			t.Errorf("Last-Event-ID = %q", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream not resumed")
	}
	if got := <-s.streams; got != "sess-1" {
		t.Errorf("resumed GET session = %q", got)
	}
	s.push <- `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`
	if m := <-notes; m != "notifications/tools/list_changed" {
		t.Errorf("notification after resumption = %q", m)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Listen = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Listen did not return")
	}

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Equal(s.deleted, []string{"sess-1"}) {
		t.Errorf("deleted = %v", s.deleted)
	}
}

func TestLegacyHTTPSessionExpired(t *testing.T) {
	s := newLegacyHTTP(t)
	s.handle = func(m wireMessage) string {
		return result(m.ID, `{"tools":[]}`)
	}
	srv := s.start()
	c := connectLegacyHTTP(t, srv)
	if _, err := c.ListTools(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.expireAll()
	if _, err := c.ListTools(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := s.methods(); !slices.Equal(got, []string{
		"server/discover", "initialize", "notifications/initialized", "tools/list",
		"tools/list", "initialize", "notifications/initialized", "tools/list",
	}) {
		t.Errorf("methods = %v", got)
	}
	// Listen renews the session too.
	s.expireAll()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Listen(ctx, Subscription{}) }()
	if got := <-s.streams; got != "sess-3" {
		t.Errorf("GET session = %q", got)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Listen = %v", err)
	}
	_ = c.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Equal(s.deleted, []string{"sess-3"}) {
		t.Errorf("deleted = %v", s.deleted)
	}
}

func TestLegacyHTTPNoStream(t *testing.T) {
	s := newLegacyHTTP(t)
	s.noStream = true
	c := connectLegacyHTTP(t, s.start())
	if err := c.Listen(t.Context(), Subscription{}); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Listen = %v", err)
	}
}

func TestLegacyHTTPCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newLegacyHTTP(t)
		cancelled := make(chan struct{})
		s.handle = func(m wireMessage) string {
			if m.Method == "tools/call" {
				select {
				case <-cancelled:
				case <-time.After(5 * time.Second):
				}
			}
			return ""
		}
		var once sync.Once
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				body, _ := io.ReadAll(r.Body)
				if strings.Contains(string(body), "notifications/cancelled") {
					once.Do(func() { close(cancelled) })
				}
				r.Body = io.NopCloser(strings.NewReader(string(body)))
			}
			s.ServeHTTP(w, r)
		}))
		c := connectLegacyHTTP(t, srv)
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

func TestHTTPProbeOutcomes(t *testing.T) {
	plain := func(status int) func(http.ResponseWriter, wireMessage) {
		return func(w http.ResponseWriter, _ wireMessage) { http.Error(w, "nope", status) }
	}
	for name, tc := range map[string]struct {
		probe    func(http.ResponseWriter, wireMessage)
		fallback bool
	}{
		"400 legacy error":  {fallback: true},
		"404 without body":  {probe: plain(http.StatusNotFound), fallback: true},
		"405 without body":  {probe: plain(http.StatusMethodNotAllowed), fallback: true},
		"401 is not legacy": {probe: plain(http.StatusUnauthorized)},
		"500 is not legacy": {probe: plain(http.StatusInternalServerError)},
		"stateless method not found": {fallback: true, probe: func(w http.ResponseWriter, _ wireMessage) {
			// A stateless server without the request's ID in its error.
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"jsonrpc":"2.0","error":{"code":-32601,"message":"Method not found"},"id":null}`)
		}},
		"header mismatch is modern": {probe: func(w http.ResponseWriter, m wireMessage) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, rpcError(m.ID, CodeHeaderMismatch, "Header mismatch"))
		}},
	} {
		t.Run(name, func(t *testing.T) {
			s := newLegacyHTTP(t)
			if tc.probe != nil {
				s.probe = tc.probe
			}
			c, err := Connect(t.Context(), serverHTTP(s.start()))
			if tc.fallback {
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				if c.ProtocolVersion() != LegacyProtocolVersion {
					t.Errorf("ProtocolVersion = %q", c.ProtocolVersion())
				}
				return
			}
			if err == nil || slices.Contains(s.methods(), "initialize") {
				t.Fatalf("err = %v, methods = %v", err, s.methods())
			}
		})
	}
}

func TestHTTPStatusErrorMessage(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream down", http.StatusBadGateway)
	}))
	defer s.Close()
	_, err := Connect(t.Context(), HTTP(s.URL))
	var he *HTTPStatusError
	if !errors.As(err, &he) || he.StatusCode != http.StatusBadGateway || !strings.Contains(err.Error(), "http 502: upstream down") {
		t.Fatalf("err = %v", err)
	}
}

func TestModernHTTPSendsNoSession(t *testing.T) {
	var methods []string
	var mu sync.Mutex
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()
		var req rpcRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		// A modern server ignores session IDs and never mints one.
		w.Header().Set("Mcp-Session-Id", "ignored")
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Mcp-Session-Id") != "" {
			t.Errorf("%s: session id sent", req.Method)
		}
		if req.Method == "server/discover" {
			fmt.Fprint(w, result(req.ID, discoverResult))
			return
		}
		fmt.Fprint(w, result(req.ID, `{"tools":[]}`))
	}))
	defer s.Close()
	c := connectHTTP(t, s.URL)
	if c.ProtocolVersion() != ProtocolVersion {
		t.Errorf("ProtocolVersion = %q", c.ProtocolVersion())
	}
	if _, err := c.ListTools(t.Context()); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	mu.Lock()
	defer mu.Unlock()
	if slices.Contains(methods, http.MethodDelete) {
		t.Errorf("methods = %v", methods)
	}
}
