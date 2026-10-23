package mcp

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HTTPOptions configures the Streamable HTTP handler of a Server.
type HTTPOptions struct {
	// AllowedOrigins lists the Origin header values accepted, such as
	// "https://app.example.com"; "*" accepts any. Requests from loopback
	// origins (localhost, 127.0.0.1, [::1]) are always accepted. Requests
	// with any other Origin are refused with 403 Forbidden, which protects
	// local servers from DNS rebinding attacks; requests without an Origin,
	// which do not come from browsers, are accepted.
	AllowedOrigins []string
	// AllowedHosts, if set, lists the Host header values accepted, such as
	// "localhost:8080". Servers bound to a loopback address should set it,
	// as a second defense against DNS rebinding.
	AllowedHosts []string
	// Auth, if set, requires bearer tokens; see AuthOptions.
	Auth *AuthOptions
	// SessionTimeout ends sessions of initialization-based revisions that
	// were idle that long, without requests or open streams. Defaults to
	// 30 minutes; negative never ends them.
	SessionTimeout time.Duration
	// PollInterval, if positive, closes the SSE connections of sessions
	// after that long, sending a retry field first, without ending their
	// streams: the client reconnects with Last-Event-ID, as revision
	// 2025-11-25 allows servers to do to avoid long-lived connections.
	PollInterval time.Duration
	// RetryInterval is the reconnection time sent on those streams.
	// Defaults to 1 second.
	RetryInterval time.Duration
	// KeepAlive is the interval of the comments sent on quiet long-lived
	// streams: subscriptions/listen responses and session GET streams.
	// Defaults to 25 seconds; negative disables them.
	KeepAlive time.Duration
	// MaxBodyBytes bounds the size of a POST body. Defaults to 4 MiB.
	MaxBodyBytes int64
	// MaxStreamEvents bounds the events kept per session stream for
	// resumption. Defaults to 1024; older events cannot be replayed.
	MaxStreamEvents int
}

// HTTPHandler serves a Server over Streamable HTTP. Mount it at the MCP
// endpoint, such as "/mcp".
//
// Requests with the protocol version in _meta follow
// https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http:
// each POST is answered with JSON or, once the handler sends notifications,
// with an SSE stream, and closing the stream cancels the request. The
// handler checks that MCP-Protocol-Version, Mcp-Method, Mcp-Name and the
// Mcp-Param headers of x-mcp-header tool parameters match the body.
//
// An initialize request starts a session of an initialization-based
// revision
// (https://modelcontextprotocol.io/specification/2025-11-25/basic/transports#streamable-http):
// the response carries its Mcp-Session-Id, requests are answered with
// resumable SSE streams whose events have IDs, GET opens a stream for
// notifications that belong to no request, GET with Last-Event-ID resumes
// any stream of the session, and DELETE ends it. In a session, closing a
// stream does not cancel its request; notifications/cancelled does.
type HTTPHandler struct {
	s        *Server
	opts     HTTPOptions
	shutdown chan struct{}

	mu       sync.Mutex
	sessions map[string]*httpSession
	closed   bool
}

// HTTPHandler returns a Streamable HTTP handler of the server. opts may be
// nil.
func (s *Server) HTTPHandler(opts *HTTPOptions) *HTTPHandler {
	h := &HTTPHandler{s: s, shutdown: make(chan struct{}), sessions: map[string]*httpSession{}}
	if opts != nil {
		h.opts = *opts
	}
	o := &h.opts
	if o.SessionTimeout == 0 {
		o.SessionTimeout = 30 * time.Minute
	}
	if o.RetryInterval <= 0 {
		o.RetryInterval = time.Second
	}
	if o.KeepAlive == 0 {
		o.KeepAlive = 25 * time.Second
	}
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = 4 << 20
	}
	if o.MaxStreamEvents <= 0 {
		o.MaxStreamEvents = 1024
	}
	return h
}

// Close ends the subscriptions/listen streams gracefully and ends every
// session.
func (h *HTTPHandler) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	close(h.shutdown)
	sessions := make([]*httpSession, 0, len(h.sessions))
	for _, hs := range h.sessions {
		sessions = append(sessions, hs)
	}
	h.mu.Unlock()
	for _, hs := range sessions {
		hs.ss.close()
	}
	return nil
}

// ServeHTTP implements http.Handler.
func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.serveMetadata(w, r) {
		return
	}
	if len(h.opts.AllowedHosts) > 0 && !slices.Contains(h.opts.AllowedHosts, r.Host) {
		writeError(w, http.StatusForbidden, nil, &Error{Code: CodeInvalidRequest, Message: "Forbidden: invalid Host header"})
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		if !h.originAllowed(origin) {
			writeError(w, http.StatusForbidden, nil, &Error{Code: CodeInvalidRequest, Message: "Forbidden: invalid Origin header"})
			return
		}
		hdr := w.Header()
		hdr.Set("Access-Control-Allow-Origin", origin)
		hdr.Add("Vary", "Origin")
		hdr.Set("Access-Control-Expose-Headers", "Mcp-Session-Id, WWW-Authenticate")
		if r.Method == http.MethodOptions {
			hdr.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
				hdr.Set("Access-Control-Allow-Headers", req)
			}
			hdr.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	token, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodPost:
		h.post(w, r, token)
	case http.MethodGet:
		h.get(w, r, token)
	case http.MethodDelete:
		h.delete(w, r, token)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeError(w, http.StatusMethodNotAllowed, nil, &Error{Code: CodeInvalidRequest, Message: "Method not allowed"})
	}
}

// originAllowed reports whether a request from origin is accepted.
func (h *HTTPHandler) originAllowed(origin string) bool {
	if slices.Contains(h.opts.AllowedOrigins, "*") || slices.Contains(h.opts.AllowedOrigins, origin) {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && isLoopback(u.Hostname())
}

// isLoopback reports whether host names the local machine.
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// writeError writes a JSON-RPC error response with an HTTP status.
// Without an id, the response has none.
func writeError(w http.ResponseWriter, status int, id json.RawMessage, e *Error) {
	writeJSON(w, status, encode(id, nil, e))
}

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// post serves a POST.
func (h *HTTPHandler) post(w http.ResponseWriter, r *http.Request, token *TokenInfo) {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, nil, &Error{Code: CodeInvalidRequest, Message: "Content-Type must be application/json"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.opts.MaxBodyBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, nil, &Error{Code: CodeInvalidRequest, Message: "Request body too large"})
		return
	}
	body = bytes.TrimSpace(body)
	if len(body) > 0 && body[0] == '[' {
		writeError(w, http.StatusBadRequest, nil, &Error{Code: CodeInvalidRequest, Message: "batches are not supported"})
		return
	}
	var m message
	if err := json.Unmarshal(body, &m); err != nil || m.JSONRPC != "2.0" {
		writeError(w, http.StatusBadRequest, nil, &Error{Code: CodeParseError, Message: "Parse error"})
		return
	}
	sid := r.Header.Get("Mcp-Session-Id")
	switch {
	case isModern(m.Params):
		// A modern request ignores any session ID.
		h.postModern(w, r, &m, token)
	case sid != "" && !h.s.opts.ModernOnly:
		h.postSession(w, r, sid, &m, token)
	case m.Method == "initialize" && len(m.ID) > 0:
		h.initialize(w, r, &m, token)
	case m.Method != "" || h.s.opts.ModernOnly:
		h.postModern(w, r, &m, token)
	default:
		writeError(w, http.StatusBadRequest, nil, &Error{Code: CodeInvalidRequest, Message: "Bad Request: missing Mcp-Session-Id"})
	}
}

// postModern serves a POST of ProtocolVersion.
func (h *HTTPHandler) postModern(w http.ResponseWriter, r *http.Request, m *message, token *TokenInfo) {
	switch {
	case m.Method == "":
		writeError(w, http.StatusBadRequest, nil, &Error{Code: CodeInvalidRequest, Message: "clients must not send responses"})
		return
	case len(m.ID) == 0:
		// This revision defines no client notifications over HTTP; closing
		// the response stream cancels a request.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	c := &incoming{id: m.ID, method: m.Method, params: m.Params, token: token, shutdown: h.shutdown}
	if e := h.s.prepareModern(c); e != nil {
		writeError(w, http.StatusBadRequest, m.ID, e)
		return
	}
	if e := h.checkHeaders(r, c); e != nil {
		writeError(w, http.StatusBadRequest, m.ID, e)
		return
	}
	if !h.s.knows(c, c.method) {
		writeError(w, http.StatusNotFound, m.ID, methodNotFound(c.method))
		return
	}
	if !h.authorizeOperation(w, c) {
		return
	}
	lw := &lazyWriter{w: w, rc: http.NewResponseController(w)}
	c.send = lw.send
	if c.method == "subscriptions/listen" {
		lw.start()
		stop := h.keepAlive(lw.comment)
		defer stop()
	}
	res, err := h.s.serve(r.Context(), c)
	if errors.Is(err, errNoResponse) || r.Context().Err() != nil {
		lw.close()
		return
	}
	status := http.StatusOK
	if e, ok := errors.AsType[*Error](err); ok && e.Code == CodeMissingRequiredClientCapability {
		status = http.StatusBadRequest
	}
	lw.finish(encode(c.id, res, err), status)
}

// keepAlive calls fn at the keep-alive interval until stop is called.
func (h *HTTPHandler) keepAlive(fn func()) (stop func()) {
	if h.opts.KeepAlive < 0 {
		return func() {}
	}
	t := time.NewTicker(h.opts.KeepAlive)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-t.C:
				fn()
			case <-done:
				return
			}
		}
	}()
	return func() {
		t.Stop()
		close(done)
	}
}

// checkHeaders checks the request metadata headers of a modern request
// against its body.
func (h *HTTPHandler) checkHeaders(r *http.Request, c *incoming) *Error {
	mismatch := func(format string, args ...any) *Error {
		return &Error{Code: CodeHeaderMismatch, Message: "Header mismatch: " + fmt.Sprintf(format, args...)}
	}
	switch v := r.Header.Get("MCP-Protocol-Version"); {
	case v == "":
		return mismatch("missing MCP-Protocol-Version header")
	case v != ProtocolVersion:
		return mismatch("MCP-Protocol-Version header value %q does not match body value %q", v, ProtocolVersion)
	}
	switch v := r.Header.Get("Mcp-Method"); {
	case v == "":
		return mismatch("missing Mcp-Method header")
	case v != c.method:
		return mismatch("Mcp-Method header value %q does not match body value %q", v, c.method)
	}
	var p struct {
		Name      string          `json:"name"`
		URI       string          `json:"uri"`
		Arguments json.RawMessage `json:"arguments"`
	}
	_ = json.Unmarshal(c.params, &p)
	switch c.method {
	case "tools/call", "prompts/get", "resources/read":
		want := p.Name
		if c.method == "resources/read" {
			want = p.URI
		}
		raw := r.Header.Get("Mcp-Name")
		if raw == "" {
			return mismatch("missing Mcp-Name header")
		}
		got, err := decodeHeaderValue(raw)
		if err != nil {
			return mismatch("Mcp-Name header: %v", err)
		}
		if got != want {
			return mismatch("Mcp-Name header value %q does not match body value %q", got, want)
		}
	}
	if c.method != "tools/call" {
		return nil
	}
	t, ok := h.s.reg.Get(p.Name)
	if !ok {
		return nil
	}
	// A tool with invalid annotations has no headers to check; clients
	// reject it.
	headers, _ := paramHeaders(MCPTool(t).InputSchema)
	if len(headers) == 0 {
		return nil
	}
	values := paramBodyValues(headers, p.Arguments)
	for _, ph := range headers {
		raw := r.Header.Get("Mcp-Param-" + ph.name)
		body, present := values[ph.name]
		if !present {
			continue
		}
		if raw == "" {
			return mismatch("missing Mcp-Param-%s header", ph.name)
		}
		got, err := decodeHeaderValue(raw)
		if err != nil {
			return mismatch("Mcp-Param-%s header: %v", ph.name, err)
		}
		if !sameParam(got, body) {
			return mismatch("Mcp-Param-%s header value %q does not match body value %v", ph.name, got, body)
		}
	}
	return nil
}

// paramBodyValues returns the primitive argument values that x-mcp-header
// annotations mirror, by header name.
func paramBodyValues(headers []paramHeader, args json.RawMessage) map[string]any {
	d := json.NewDecoder(bytes.NewReader(args))
	d.UseNumber()
	var root any
	if d.Decode(&root) != nil {
		return nil
	}
	out := map[string]any{}
	for _, ph := range headers {
		v := root
		for _, k := range ph.path {
			m, _ := v.(map[string]any)
			v = m[k]
		}
		switch v.(type) {
		case string, bool, json.Number:
			out[ph.name] = v
		}
	}
	return out
}

// sameParam compares a decoded header value with a body value, integers
// numerically.
func sameParam(header string, body any) bool {
	switch b := body.(type) {
	case string:
		return header == b
	case bool:
		return header == strconv.FormatBool(b)
	case json.Number:
		hf, err1 := strconv.ParseFloat(header, 64)
		bf, err2 := b.Float64()
		return err1 == nil && err2 == nil && hf == bf && !math.IsInf(hf, 0)
	}
	return false
}

// decodeHeaderValue decodes a Mcp-Name or Mcp-Param header value, which may
// use the =?base64?...?= form.
func decodeHeaderValue(v string) (string, error) {
	if strings.HasPrefix(v, "=?base64?") && strings.HasSuffix(v, "?=") && len(v) >= len("=?base64??=") {
		b, err := base64.StdEncoding.DecodeString(v[len("=?base64?") : len(v)-len("?=")])
		if err != nil {
			return "", errors.New("invalid base64 value")
		}
		return string(b), nil
	}
	for i := range len(v) {
		if c := v[i]; c != '\t' && (c < 0x20 || c > 0x7e) {
			return "", errors.New("invalid characters")
		}
	}
	return v, nil
}

// lazyWriter answers a modern request with JSON, or with an SSE stream
// once a message related to the request is sent before the response.
type lazyWriter struct {
	mu   sync.Mutex
	w    http.ResponseWriter
	rc   *http.ResponseController
	sse  bool
	done bool
}

// startLocked switches to SSE. The caller holds mu, or nobody else uses lw yet.
func (lw *lazyWriter) startLocked() {
	if lw.sse {
		return
	}
	lw.sse = true
	h := lw.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	lw.w.WriteHeader(http.StatusOK)
	_ = lw.rc.Flush()
}

func (lw *lazyWriter) start() {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	lw.startLocked()
}

func (lw *lazyWriter) send(msg []byte) error {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if lw.done {
		return ErrClosed
	}
	lw.startLocked()
	_ = lw.rc.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := fmt.Fprintf(lw.w, "data: %s\n\n", msg); err != nil {
		return err
	}
	return lw.rc.Flush()
}

func (lw *lazyWriter) comment() {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if lw.done || !lw.sse {
		return
	}
	_ = lw.rc.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, _ = io.WriteString(lw.w, ":\n\n")
	_ = lw.rc.Flush()
}

func (lw *lazyWriter) finish(msg []byte, status int) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if lw.done {
		return
	}
	lw.done = true
	if lw.sse {
		_, _ = fmt.Fprintf(lw.w, "data: %s\n\n", msg)
		_ = lw.rc.Flush()
		return
	}
	writeJSON(lw.w, status, msg)
}

func (lw *lazyWriter) close() {
	lw.mu.Lock()
	lw.done = true
	lw.mu.Unlock()
}

// writeTimeout bounds a write to a stream whose client stopped reading.
const writeTimeout = 30 * time.Second

// httpSession is a Streamable HTTP session of an initialization-based
// revision.
type httpSession struct {
	h  *HTTPHandler
	ss *session
	// ctx ends with the session; handlers run in it, not in their POST's
	// context.
	ctx    context.Context
	cancel context.CancelFunc

	mu         sync.Mutex
	streams    map[string]*eventStream
	order      []string
	nextStream int
	standalone *eventStream
	inflight   map[string]*inflight
	active     int
	timer      *time.Timer
}

func (h *HTTPHandler) lookup(sid string, token *TokenInfo) *httpSession {
	h.mu.Lock()
	hs := h.sessions[sid]
	h.mu.Unlock()
	if hs == nil || hs.ss.principal != principalOf(token) {
		// A session is bound to the principal that created it.
		return nil
	}
	return hs
}

func principalOf(t *TokenInfo) string {
	if t == nil {
		return ""
	}
	return t.Subject
}

// initialize starts a session.
func (h *HTTPHandler) initialize(w http.ResponseWriter, r *http.Request, m *message, token *TokenInfo) {
	hs := &httpSession{h: h, streams: map[string]*eventStream{}, inflight: map[string]*inflight{}}
	ss := newSession(h.s, hs.notify)
	c := &incoming{id: m.ID, method: m.Method, params: m.Params, token: token}
	if !h.authorizeOperation(w, c) {
		return
	}
	res, err := h.s.initialize(c, ss)
	if err != nil {
		writeError(w, http.StatusBadRequest, m.ID, toRPCError(err))
		return
	}
	ss.id, ss.principal = newSessionID(), principalOf(token)
	hs.ss = ss
	hs.ctx, hs.cancel = context.WithCancel(context.WithoutCancel(r.Context()))
	ss.onClose = append(ss.onClose, hs.closed)
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		ss.close()
		writeError(w, http.StatusServiceUnavailable, m.ID, &Error{Code: CodeInternalError, Message: "server shutting down"})
		return
	}
	h.sessions[ss.id] = hs
	h.mu.Unlock()
	hs.arm()
	w.Header().Set("Mcp-Session-Id", ss.id)
	writeJSON(w, http.StatusOK, encode(m.ID, res, nil))
}

// arm (re)starts the idle timer.
func (hs *httpSession) arm() {
	d := hs.h.opts.SessionTimeout
	if d < 0 {
		return
	}
	hs.mu.Lock()
	defer hs.mu.Unlock()
	if hs.timer != nil {
		hs.timer.Stop()
	}
	hs.timer = time.AfterFunc(d, func() {
		hs.mu.Lock()
		busy := hs.active > 0 || len(hs.inflight) > 0
		hs.mu.Unlock()
		if busy {
			hs.arm()
			return
		}
		hs.ss.close()
	})
}

// closed releases the session's resources once it ended.
func (hs *httpSession) closed() {
	hs.h.mu.Lock()
	if hs.h.sessions[hs.ss.id] == hs {
		delete(hs.h.sessions, hs.ss.id)
	}
	hs.h.mu.Unlock()
	hs.cancel()
	hs.mu.Lock()
	if hs.timer != nil {
		hs.timer.Stop()
	}
	streams := make([]*eventStream, 0, len(hs.streams))
	for _, st := range hs.streams {
		streams = append(streams, st)
	}
	if hs.standalone != nil {
		streams = append(streams, hs.standalone)
	}
	hs.mu.Unlock()
	for _, st := range streams {
		st.wakeAll()
	}
}

// notify sends a message related to no request on the GET stream, if the
// client opened one; otherwise the message is dropped.
func (hs *httpSession) notify(msg []byte) error {
	hs.mu.Lock()
	st := hs.standalone
	hs.mu.Unlock()
	if st == nil {
		return nil
	}
	return st.add(msg)
}

// primes reports whether the session's streams start with a priming event,
// which revision 2025-11-25 introduced: an event without data that gives
// the client an ID to resume from. Clients of earlier revisions may not
// expect events without data.
func (hs *httpSession) primes() bool { return hs.ss.version == LegacyProtocolVersion }

// newStream creates a stream of the session.
func (hs *httpSession) newStream() *eventStream {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	hs.nextStream++
	st := newEventStream(strconv.Itoa(hs.nextStream), hs.h.opts.MaxStreamEvents, hs.primes())
	hs.streams[st.id] = st
	hs.order = append(hs.order, st.id)
	// Bound the streams kept for resumption.
	for len(hs.order) > 256 {
		delete(hs.streams, hs.order[0])
		hs.order = hs.order[1:]
	}
	return st
}

func (hs *httpSession) stream(id string) *eventStream {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	if hs.standalone != nil && hs.standalone.id == id {
		return hs.standalone
	}
	return hs.streams[id]
}

// streamRetention is how long a stream whose events were all written is
// kept: a write may succeed although the client already disconnected, and
// the client then resumes the stream.
const streamRetention = time.Minute

// delivered forgets a stream whose events were all written, after
// streamRetention.
func (hs *httpSession) delivered(st *eventStream) {
	time.AfterFunc(streamRetention, func() {
		hs.mu.Lock()
		defer hs.mu.Unlock()
		if hs.streams[st.id] == st {
			delete(hs.streams, st.id)
			hs.order = slices.DeleteFunc(hs.order, func(id string) bool { return id == st.id })
		}
	})
}

// postSession serves a POST in a session.
func (h *HTTPHandler) postSession(w http.ResponseWriter, r *http.Request, sid string, m *message, token *TokenInfo) {
	hs := h.lookup(sid, token)
	if hs == nil {
		writeError(w, http.StatusNotFound, m.ID, &Error{Code: CodeInvalidRequest, Message: "Session not found"})
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && v != hs.ss.version {
		writeError(w, http.StatusBadRequest, m.ID, &Error{Code: CodeInvalidRequest,
			Message: fmt.Sprintf("Bad Request: MCP-Protocol-Version %q is not the session's %q", v, hs.ss.version)})
		return
	}
	hs.arm()
	switch {
	case m.Method == "" && len(m.ID) > 0:
		hs.ss.resolve(m.ID, m)
		w.WriteHeader(http.StatusAccepted)
		return
	case m.Method != "" && len(m.ID) == 0:
		hs.notification(m.Method, m.Params)
		w.WriteHeader(http.StatusAccepted)
		return
	case m.Method == "":
		writeError(w, http.StatusBadRequest, nil, &Error{Code: CodeInvalidRequest, Message: "Invalid request"})
		return
	case m.Method == "initialize":
		writeError(w, http.StatusBadRequest, m.ID, &Error{Code: CodeInvalidRequest, Message: "session already initialized"})
		return
	}
	c := &incoming{id: m.ID, method: m.Method, params: m.Params, token: token, sess: hs.ss,
		caps: hs.ss.caps, info: hs.ss.info, shutdown: h.shutdown}
	if !h.authorizeOperation(w, c) {
		return
	}
	key := idKey(m.ID)
	ctx, cancel := context.WithCancel(hs.ctx)
	in := &inflight{cancel: cancel}
	hs.mu.Lock()
	if _, dup := hs.inflight[key]; dup {
		hs.mu.Unlock()
		cancel()
		writeError(w, http.StatusBadRequest, m.ID, &Error{Code: CodeInvalidRequest, Message: "duplicate request id"})
		return
	}
	hs.inflight[key] = in
	hs.mu.Unlock()
	st := hs.newStream()
	c.send = st.add
	go func() {
		defer func() {
			hs.mu.Lock()
			delete(hs.inflight, key)
			hs.mu.Unlock()
			cancel()
		}()
		res, err := h.s.serve(ctx, c)
		if errors.Is(err, errNoResponse) || in.cancelled.Load() {
			st.finish(nil)
			return
		}
		st.finish(encode(c.id, res, err))
	}()
	h.pump(w, r, hs, st, -1)
}

func (hs *httpSession) notification(method string, params json.RawMessage) {
	switch method {
	case "notifications/initialized":
		hs.ss.markInitialized()
	case "notifications/cancelled":
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(params, &p) != nil || len(p.RequestID) == 0 {
			return
		}
		hs.mu.Lock()
		in := hs.inflight[idKey(p.RequestID)]
		hs.mu.Unlock()
		if in != nil {
			in.cancelled.Store(true)
			in.cancel()
		}
	}
}

// get serves a GET: in a session, a stream for messages that belong to no
// request, or the resumption of a stream with Last-Event-ID.
func (h *HTTPHandler) get(w http.ResponseWriter, r *http.Request, token *TokenInfo) {
	sid := r.Header.Get("Mcp-Session-Id")
	if h.s.opts.ModernOnly || sid == "" {
		// ProtocolVersion has no GET stream.
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, nil, &Error{Code: CodeInvalidRequest, Message: "Method not allowed"})
		return
	}
	hs := h.lookup(sid, token)
	if hs == nil {
		writeError(w, http.StatusNotFound, nil, &Error{Code: CodeInvalidRequest, Message: "Session not found"})
		return
	}
	if !acceptsEventStream(r) {
		writeError(w, http.StatusNotAcceptable, nil, &Error{Code: CodeInvalidRequest, Message: "Accept must include text/event-stream"})
		return
	}
	hs.arm()
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		id, seq, ok := parseEventID(last)
		st := hs.stream(id)
		if !ok || st == nil || !st.resumable(seq) {
			writeError(w, http.StatusNotFound, nil, &Error{Code: CodeInvalidRequest, Message: "Stream not found or no longer resumable"})
			return
		}
		h.pump(w, r, hs, st, seq)
		return
	}
	hs.mu.Lock()
	if hs.standalone != nil && hs.standalone.attached() {
		hs.mu.Unlock()
		writeError(w, http.StatusConflict, nil, &Error{Code: CodeInvalidRequest, Message: "Conflict: only one GET stream per session"})
		return
	}
	hs.nextStream++
	st := newEventStream(strconv.Itoa(hs.nextStream), h.opts.MaxStreamEvents, hs.primes())
	st.open = true
	hs.standalone = st
	hs.mu.Unlock()
	h.pump(w, r, hs, st, -1)
}

func acceptsEventStream(r *http.Request) bool {
	for _, v := range r.Header.Values("Accept") {
		for part := range strings.SplitSeq(v, ",") {
			mt, _, _ := mime.ParseMediaType(strings.TrimSpace(part))
			if mt == "text/event-stream" || mt == "*/*" || mt == "text/*" {
				return true
			}
		}
	}
	return false
}

// delete ends a session.
func (h *HTTPHandler) delete(w http.ResponseWriter, r *http.Request, token *TokenInfo) {
	sid := r.Header.Get("Mcp-Session-Id")
	if h.s.opts.ModernOnly || sid == "" {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, nil, &Error{Code: CodeInvalidRequest, Message: "Method not allowed"})
		return
	}
	hs := h.lookup(sid, token)
	if hs == nil {
		writeError(w, http.StatusNotFound, nil, &Error{Code: CodeInvalidRequest, Message: "Session not found"})
		return
	}
	hs.ss.close()
	w.WriteHeader(http.StatusNoContent)
}

// eventStream is a stream of a session: the events of a request's
// response, or of the GET stream. It keeps its events so that a client can
// resume it after the connection broke.
type eventStream struct {
	id  string
	max int
	// open marks the GET stream, which never finishes.
	open bool

	mu     sync.Mutex
	events []streamEvent
	next   int
	ended  bool
	wake   chan struct{}
	gen    int
	writer bool
}

type streamEvent struct {
	seq  int
	data []byte
}

func newEventStream(id string, max int, prime bool) *eventStream {
	st := &eventStream{id: id, max: max, wake: make(chan struct{}), next: 1}
	if prime {
		// The priming event gives the client an event ID before any
		// message.
		st.events = append(st.events, streamEvent{seq: 0})
	}
	return st
}

func (st *eventStream) signalLocked() {
	close(st.wake)
	st.wake = make(chan struct{})
}

func (st *eventStream) wakeAll() {
	st.mu.Lock()
	st.signalLocked()
	st.mu.Unlock()
}

// add appends a message.
func (st *eventStream) add(msg []byte) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.ended {
		return ErrClosed
	}
	st.events = append(st.events, streamEvent{seq: st.next, data: msg})
	st.next++
	if len(st.events) > st.max {
		st.events = st.events[len(st.events)-st.max:]
	}
	st.signalLocked()
	return nil
}

// finish appends the final message, if any, and ends the stream.
func (st *eventStream) finish(msg []byte) {
	if msg != nil {
		_ = st.add(msg)
	}
	st.mu.Lock()
	st.ended = true
	st.signalLocked()
	st.mu.Unlock()
}

// resumable reports whether the events after seq are all kept.
func (st *eventStream) resumable(seq int) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return seq >= 0 && seq < st.next && (len(st.events) == 0 || seq >= st.events[0].seq-1)
}

func (st *eventStream) attached() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.writer
}

// attach makes a new connection the stream's writer, detaching any other.
func (st *eventStream) attach() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.gen++
	st.writer = true
	st.signalLocked()
	return st.gen
}

func (st *eventStream) detach(gen int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.gen == gen {
		st.writer = false
	}
}

// since returns the events after seq, whether the stream ended, the
// channel that signals a change, and whether gen is still the writer.
func (st *eventStream) since(seq, gen int) ([]streamEvent, bool, <-chan struct{}, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	i, _ := slices.BinarySearchFunc(st.events, seq+1, func(e streamEvent, s int) int { return e.seq - s })
	return slices.Clone(st.events[i:]), st.ended, st.wake, st.gen == gen
}

func parseEventID(id string) (stream string, seq int, ok bool) {
	stream, n, ok := strings.Cut(id, ".")
	if !ok {
		return "", 0, false
	}
	seq, err := strconv.Atoi(n)
	return stream, seq, err == nil
}

// pump writes a session stream to a connection, from the event after seq,
// until the stream ends, the client disconnects, another connection resumes
// the stream, the poll interval passes or the session ends.
func (h *HTTPHandler) pump(w http.ResponseWriter, r *http.Request, hs *httpSession, st *eventStream, seq int) {
	hs.mu.Lock()
	hs.active++
	hs.mu.Unlock()
	defer func() {
		hs.mu.Lock()
		hs.active--
		hs.mu.Unlock()
		hs.arm()
	}()
	rc := http.NewResponseController(w)
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	gen := st.attach()
	defer st.detach(gen)
	var poll <-chan time.Time
	if h.opts.PollInterval > 0 {
		t := time.NewTimer(h.opts.PollInterval)
		defer t.Stop()
		poll = t.C
	}
	var keep <-chan time.Time
	if h.opts.KeepAlive > 0 {
		t := time.NewTicker(h.opts.KeepAlive)
		defer t.Stop()
		keep = t.C
	}
	retry := h.opts.RetryInterval.Milliseconds()
	first := true
	for {
		events, ended, wake, current := st.since(seq, gen)
		if !current {
			return
		}
		var buf bytes.Buffer
		for _, ev := range events {
			fmt.Fprintf(&buf, "id: %s.%d\n", st.id, ev.seq)
			if first {
				fmt.Fprintf(&buf, "retry: %d\n", retry)
				first = false
			}
			fmt.Fprintf(&buf, "data: %s\n\n", ev.data)
			seq = ev.seq
		}
		if buf.Len() > 0 {
			_ = rc.SetWriteDeadline(time.Now().Add(writeTimeout))
			if _, err := w.Write(buf.Bytes()); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
		if ended && !st.open {
			hs.delivered(st)
			return
		}
		select {
		case <-wake:
		case <-r.Context().Done():
			return
		case <-hs.ctx.Done():
			return
		case <-poll:
			// Close the connection, not the stream: the client resumes.
			_, _ = fmt.Fprintf(w, "retry: %d\n\n", retry)
			_ = rc.Flush()
			return
		case <-keep:
			_ = rc.SetWriteDeadline(time.Now().Add(writeTimeout))
			if _, err := io.WriteString(w, ":\n\n"); err != nil {
				return
			}
			_ = rc.Flush()
		}
	}
}

// ProtectedResourceMetadataURL returns the URL at which RFC 9728 serves the
// Protected Resource Metadata of resource: the well-known path inserted
// between its host and path, as in
// https://example.com/.well-known/oauth-protected-resource/mcp for
// https://example.com/mcp.
func ProtectedResourceMetadataURL(resource string) string {
	u, err := url.Parse(resource)
	if err != nil {
		return ""
	}
	path := strings.TrimSuffix(u.EscapedPath(), "/")
	return (&url.URL{Scheme: u.Scheme, Host: u.Host}).String() + "/.well-known/oauth-protected-resource" + path
}

// serveMetadata serves the Protected Resource Metadata when the handler is
// mounted where its well-known path arrives too.
func (h *HTTPHandler) serveMetadata(w http.ResponseWriter, r *http.Request) bool {
	a := h.opts.Auth
	if a == nil || a.Metadata == nil || r.Method != http.MethodGet && r.Method != http.MethodOptions {
		return false
	}
	u, err := url.Parse(cmp.Or(a.MetadataURL, ProtectedResourceMetadataURL(a.Metadata.Resource)))
	if err != nil || u.EscapedPath() != r.URL.EscapedPath() {
		return false
	}
	ProtectedResourceMetadataHandler(a.Metadata).ServeHTTP(w, r)
	return true
}
