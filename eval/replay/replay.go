// Package replay records HTTP interactions to a cassette file and replays
// them, so tests of providers, agents and evaluations run deterministically
// without network access or API keys.
//
// A Recorder is an http.RoundTripper. In record mode it sends requests to
// the real transport and saves each request and response; in replay mode
// it answers requests from the cassette and sends nothing. Pass its client
// to a provider, such as one from github.com/nostosx/agentx-contrib/provider:
//
//	rec := replay.NewTest(t, "testdata/weather.json")
//	m, _ := openai.New(openai.WithHTTPClient(rec.Client()))
//
// The mode comes from the AGENTX_REPLAY environment variable unless
// WithMode sets it: "replay" (the default), "record", "auto" (replay if
// the cassette exists, otherwise record) or "passthrough". Record a
// cassette once with real credentials, AGENTX_REPLAY=record go test, and
// inspect it for sensitive data before committing it; later runs replay it.
//
// Default redaction replaces selected header and query-parameter values,
// such as Authorization, x-api-key and key, with "REDACTED". Bodies and
// other fields are preserved; use WithRedactHeaders, WithRedactQuery and
// WithFilter to remove additional sensitive data. Responses streamed as
// Server-Sent Events are saved event by event and replayed one event per
// read, so stream parsing is exercised as with a real server.
package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// Mode selects whether a Recorder records or replays.
type Mode string

const (
	// ModeReplay answers requests from the cassette and fails requests it
	// has no answer for.
	ModeReplay Mode = "replay"
	// ModeRecord sends requests to the real transport and saves the
	// interactions, replacing the cassette.
	ModeRecord Mode = "record"
	// ModeAuto replays if the cassette exists and records otherwise.
	ModeAuto Mode = "auto"
	// ModePassthrough sends requests to the real transport without
	// recording.
	ModePassthrough Mode = "passthrough"
)

// EnvMode is the environment variable that sets the default mode.
const EnvMode = "AGENTX_REPLAY"

// Redacted replaces redacted values.
const Redacted = "REDACTED"

// CassetteVersion is the version of the cassette format.
const CassetteVersion = 1

// DefaultRedactHeaders are the headers whose values are redacted by
// default: the credentials of the agentx providers and common cookies.
var DefaultRedactHeaders = []string{
	"Authorization", "Proxy-Authorization", "X-Api-Key", "Api-Key", "X-Goog-Api-Key",
	"X-Amz-Security-Token", "Cookie", "Set-Cookie",
}

// DefaultRedactQuery are the query parameters whose values are redacted by
// default.
var DefaultRedactQuery = []string{"key", "api_key", "api-key", "access_token"}

// ModeFromEnv returns the mode EnvMode sets, or ModeReplay if it is unset.
func ModeFromEnv() Mode {
	if m := os.Getenv(EnvMode); m != "" {
		return Mode(strings.ToLower(strings.TrimSpace(m)))
	}
	return ModeReplay
}

// Cassette is the content of a cassette file.
type Cassette struct {
	Version      int            `json:"version"`
	Interactions []*Interaction `json:"interactions"`
}

// Interaction is a recorded request and its response.
type Interaction struct {
	Request  Request  `json:"request"`
	Response Response `json:"response"`
}

// Request is a recorded request.
type Request struct {
	Method string      `json:"method"`
	URL    string      `json:"url"`
	Header http.Header `json:"header,omitempty"`
	// Body is the body if it is valid UTF-8; BodyBase64 holds it otherwise.
	Body       string `json:"body,omitempty"`
	BodyBase64 []byte `json:"body_base64,omitempty"`
}

// Response is a recorded response.
type Response struct {
	StatusCode int         `json:"status_code"`
	Header     http.Header `json:"header,omitempty"`
	// Body is the body if it is valid UTF-8 and not an event stream;
	// BodyBase64 holds other bodies.
	Body       string `json:"body,omitempty"`
	BodyBase64 []byte `json:"body_base64,omitempty"`
	// Events holds an event stream (text/event-stream), split after each
	// event; their concatenation is the body.
	Events []string `json:"events,omitempty"`
}

// BodyBytes returns the body of the request.
func (r *Request) BodyBytes() []byte {
	if r.BodyBase64 != nil {
		return r.BodyBase64
	}
	return []byte(r.Body)
}

// SetBody stores body in Body or BodyBase64.
func (r *Request) SetBody(body []byte) {
	r.Body, r.BodyBase64 = encodeBody(body)
}

// BodyBytes returns the body of the response.
func (r *Response) BodyBytes() []byte {
	switch {
	case r.Events != nil:
		return []byte(strings.Join(r.Events, ""))
	case r.BodyBase64 != nil:
		return r.BodyBase64
	}
	return []byte(r.Body)
}

// SetBody stores body in Body, BodyBase64 or, for an event stream as the
// Content-Type header declares it, Events.
func (r *Response) SetBody(body []byte) {
	r.Body, r.BodyBase64, r.Events = "", nil, nil
	if isEventStream(r.Header) && utf8.Valid(body) && len(body) > 0 {
		r.Events = splitEvents(string(body))
		return
	}
	r.Body, r.BodyBase64 = encodeBody(body)
}

func encodeBody(body []byte) (string, []byte) {
	if utf8.Valid(body) {
		return string(body), nil
	}
	return "", bytes.Clone(body)
}

func isEventStream(h http.Header) bool {
	ct := strings.ToLower(h.Get("Content-Type"))
	return strings.HasPrefix(ct, "text/event-stream")
}

// splitEvents splits an event stream after each blank line.
func splitEvents(s string) []string {
	sep := "\n\n"
	if strings.Contains(s, "\r\n\r\n") {
		sep = "\r\n\r\n"
	}
	parts := strings.SplitAfter(s, sep)
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// Matcher reports whether a request matches a recorded one. The request
// is redacted and filtered as it would be when recorded.
type Matcher func(req, recorded *Request) bool

// DefaultMatcher matches requests with the same method, the same URL with
// query parameters in any order, and the same body, compared as JSON
// values if both are JSON. Headers are not compared.
func DefaultMatcher(req, recorded *Request) bool {
	return req.Method == recorded.Method && sameURL(req.URL, recorded.URL) && sameBody(req.BodyBytes(), recorded.BodyBytes())
}

// MatchMethodURL matches requests by method and URL only, ignoring the
// body.
func MatchMethodURL(req, recorded *Request) bool {
	return req.Method == recorded.Method && sameURL(req.URL, recorded.URL)
}

func sameURL(a, b string) bool {
	if a == b {
		return true
	}
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	if err1 != nil || err2 != nil {
		return false
	}
	qa, qb := ua.Query(), ub.Query()
	ua.RawQuery, ub.RawQuery = "", ""
	return ua.String() == ub.String() && reflect.DeepEqual(qa, qb)
}

func sameBody(a, b []byte) bool {
	if bytes.Equal(a, b) {
		return true
	}
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

// Option configures a Recorder.
type Option func(*Recorder)

// WithMode sets the mode instead of EnvMode.
func WithMode(m Mode) Option { return func(r *Recorder) { r.mode = m } }

// WithTransport sets the transport that sends real requests in record and
// passthrough modes; it defaults to http.DefaultTransport.
func WithTransport(t http.RoundTripper) Option { return func(r *Recorder) { r.next = t } }

// WithRedactHeaders redacts the values of more headers, in requests and
// responses, in addition to DefaultRedactHeaders.
func WithRedactHeaders(names ...string) Option {
	return func(r *Recorder) { r.headers = append(r.headers, names...) }
}

// WithRedactQuery redacts the values of more query parameters in addition
// to DefaultRedactQuery.
func WithRedactQuery(names ...string) Option {
	return func(r *Recorder) { r.query = append(r.query, names...) }
}

// WithMatcher sets how requests are matched to recorded ones; it defaults
// to DefaultMatcher.
func WithMatcher(m Matcher) Option { return func(r *Recorder) { r.match = m } }

// WithFilter calls fn on each interaction before it is saved, for example
// to remove secrets or volatile values from bodies. In replay mode it is
// called on each request, in an interaction with an empty response, before
// matching, so requests compare as they were saved.
func WithFilter(fn func(*Interaction)) Option { return func(r *Recorder) { r.filter = fn } }

// Recorder records and replays HTTP interactions. It is safe for
// concurrent use.
type Recorder struct {
	path    string
	mode    Mode
	next    http.RoundTripper
	headers []string
	query   []string
	match   Matcher
	filter  func(*Interaction)

	mu       sync.Mutex
	replay   []*Interaction
	used     []bool
	recorded []*recording
}

// recording is an interaction being recorded.
type recording struct {
	in   *Interaction
	body bytes.Buffer
}

// New returns a recorder for the cassette at path. In replay mode, the
// cassette must exist. In record mode, call Save to write it.
func New(path string, opts ...Option) (*Recorder, error) {
	r := &Recorder{
		path:    path,
		mode:    ModeFromEnv(),
		next:    http.DefaultTransport,
		headers: slices.Clone(DefaultRedactHeaders),
		query:   slices.Clone(DefaultRedactQuery),
		match:   DefaultMatcher,
	}
	for _, opt := range opts {
		opt(r)
	}
	if r.mode == ModeAuto {
		r.mode = ModeRecord
		if _, err := os.Stat(path); err == nil {
			r.mode = ModeReplay
		}
	}
	switch r.mode {
	case ModeReplay:
		c, err := Load(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("replay: cassette %s does not exist; record it with %s=record", path, EnvMode)
			}
			return nil, err
		}
		r.replay = c.Interactions
		r.used = make([]bool, len(c.Interactions))
	case ModeRecord, ModePassthrough:
	default:
		return nil, fmt.Errorf("replay: unknown mode %q; want replay, record, auto or passthrough", r.mode)
	}
	return r, nil
}

// NewTest returns a recorder for a test. It fails the test if the recorder
// cannot be created, and when the test ends, it saves the cassette in
// record mode and, in replay mode, fails the test if interactions of the
// cassette were not used.
func NewTest(t testing.TB, path string, opts ...Option) *Recorder {
	t.Helper()
	r, err := New(path, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() && r.mode == ModeRecord {
			// Keep the previous cassette rather than saving a broken one.
			return
		}
		if err := r.Save(); err != nil {
			t.Error(err)
		}
		if n := len(r.Unused()); n > 0 && !t.Failed() {
			t.Errorf("replay: %d interactions of %s were not used", n, path)
		}
	})
	return r
}

// Load reads a cassette file.
func Load(path string) (*Cassette, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Cassette
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("replay: cassette %s: %w", path, err)
	}
	if c.Version != CassetteVersion {
		return nil, fmt.Errorf("replay: cassette %s has version %d, want %d", path, c.Version, CassetteVersion)
	}
	return &c, nil
}

// Mode returns the mode the recorder runs in; ModeAuto is resolved.
func (r *Recorder) Mode() Mode { return r.mode }

// Client returns an HTTP client that uses the recorder.
func (r *Recorder) Client() *http.Client { return &http.Client{Transport: r} }

// Interactions returns the interactions recorded so far in record mode, or
// those of the cassette in replay mode.
func (r *Recorder) Interactions() []*Interaction {
	r.mu.Lock()
	if r.mode == ModeReplay {
		defer r.mu.Unlock()
		return slices.Clone(r.replay)
	}
	out := make([]*Interaction, len(r.recorded))
	for i, rec := range r.recorded {
		out[i] = rec.snapshot()
	}
	r.mu.Unlock()
	if r.filter != nil {
		for _, in := range out {
			r.filter(in)
		}
	}
	return out
}

// Unused returns the interactions of the cassette that no request matched
// in replay mode.
func (r *Recorder) Unused() []*Interaction {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*Interaction
	for i, in := range r.replay {
		if !r.used[i] {
			out = append(out, in)
		}
	}
	return out
}

// Save writes the recorded interactions to the cassette in record mode,
// replacing it; it does nothing in other modes. A response body is saved
// as far as it was read.
func (r *Recorder) Save() error {
	if r.mode != ModeRecord {
		return nil
	}
	c := Cassette{Version: CassetteVersion, Interactions: r.Interactions()}
	if c.Interactions == nil {
		c.Interactions = []*Interaction{}
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(c); err != nil {
		return fmt.Errorf("replay: %w", err)
	}
	if err := os.WriteFile(r.path, b.Bytes(), 0o644); err != nil {
		return fmt.Errorf("replay: %w", err)
	}
	return nil
}

// RoundTrip implements http.RoundTripper.
func (r *Recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	if r.mode == ModePassthrough {
		return r.next.RoundTrip(req)
	}
	if err := req.Context().Err(); err != nil {
		closeBody(req)
		return nil, err
	}
	body, err := readBody(req)
	if err != nil {
		return nil, err
	}
	in := &Interaction{Request: r.requestOf(req, body)}
	if r.mode == ModeReplay {
		return r.replayRequest(req, in)
	}

	out := req.Clone(req.Context())
	out.Body = io.NopCloser(bytes.NewReader(body))
	out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	out.ContentLength = int64(len(body))
	if req.Body == nil || req.Body == http.NoBody {
		out.Body, out.GetBody = req.Body, nil
	}
	resp, err := r.next.RoundTrip(out)
	if err != nil {
		return nil, err
	}
	in.Response = Response{StatusCode: resp.StatusCode, Header: r.redactHeader(resp.Header)}
	rec := &recording{in: in}
	r.mu.Lock()
	r.recorded = append(r.recorded, rec)
	r.mu.Unlock()
	resp.Body = &recordingBody{ReadCloser: resp.Body, r: r, rec: rec}
	return resp, nil
}

func closeBody(req *http.Request) {
	if req.Body != nil {
		req.Body.Close()
	}
}

func readBody(req *http.Request) ([]byte, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	defer req.Body.Close()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, fmt.Errorf("replay: read the request body: %w", err)
	}
	return body, nil
}

// requestOf returns the recorded form of a request.
func (r *Recorder) requestOf(req *http.Request, body []byte) Request {
	u := req.URL.Clone()
	if q := u.Query(); len(q) > 0 {
		changed := false
		for _, name := range r.query {
			if _, ok := q[name]; ok {
				q[name] = []string{Redacted}
				changed = true
			}
		}
		if changed {
			u.RawQuery = q.Encode()
		}
	}
	rq := Request{Method: req.Method, URL: u.String(), Header: r.redactHeader(req.Header)}
	if rq.Method == "" {
		rq.Method = http.MethodGet
	}
	rq.SetBody(body)
	return rq
}

func (r *Recorder) redactHeader(h http.Header) http.Header {
	if len(h) == 0 {
		return nil
	}
	out := h.Clone()
	for k := range out {
		// Keys set without canonicalization are redacted too.
		if slices.ContainsFunc(r.headers, func(name string) bool { return strings.EqualFold(name, k) }) {
			out[k] = []string{Redacted}
		}
	}
	return out
}

// replayRequest answers a request from the cassette.
func (r *Recorder) replayRequest(req *http.Request, in *Interaction) (*http.Response, error) {
	if r.filter != nil {
		r.filter(in)
	}
	r.mu.Lock()
	var found *Interaction
	for i, rec := range r.replay {
		if !r.used[i] && r.match(&in.Request, &rec.Request) {
			r.used[i] = true
			found = rec
			break
		}
	}
	r.mu.Unlock()
	if found == nil {
		return nil, &NoMatchError{Method: in.Request.Method, URL: in.Request.URL, Cassette: r.path}
	}
	rs := found.Response
	header := rs.Header.Clone()
	if header == nil {
		header = http.Header{}
	}
	body := rs.BodyBytes()
	resp := &http.Response{
		Status:     strconv.Itoa(rs.StatusCode) + " " + http.StatusText(rs.StatusCode),
		StatusCode: rs.StatusCode,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     header,
		Request:    req,
	}
	if rs.Events != nil {
		header.Del("Content-Length")
		resp.ContentLength = -1
		resp.Body = &eventBody{ctx: req.Context(), events: slices.Clone(rs.Events)}
	} else {
		if header.Get("Content-Length") != "" {
			header.Set("Content-Length", strconv.Itoa(len(body)))
		}
		resp.ContentLength = int64(len(body))
		resp.Body = io.NopCloser(bytes.NewReader(body))
	}
	return resp, nil
}

// NoMatchError reports a request that matches no unused interaction of the
// cassette in replay mode.
type NoMatchError struct {
	Method, URL, Cassette string
}

func (e *NoMatchError) Error() string {
	return fmt.Sprintf("replay: no interaction of %s matches %s %s; record it again with %s=record", e.Cassette, e.Method, e.URL, EnvMode)
}

// recordingBody records a response body as it is read.
type recordingBody struct {
	io.ReadCloser
	r   *Recorder
	rec *recording
}

func (b *recordingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.r.mu.Lock()
		b.rec.body.Write(p[:n])
		b.r.mu.Unlock()
	}
	return n, err
}

// snapshot copies the interaction with the body read so far and the header
// and query redactions already applied. The caller applies any filter after
// releasing the recorder's lock, which must be held during snapshot.
func (rec *recording) snapshot() *Interaction {
	in := *rec.in
	in.Request.Header = in.Request.Header.Clone()
	in.Response.Header = in.Response.Header.Clone()
	in.Response.SetBody(rec.body.Bytes())
	return &in
}

// eventBody replays an event stream one event per read.
type eventBody struct {
	ctx    context.Context
	mu     sync.Mutex
	events []string
	rest   string
}

func (b *eventBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	if b.rest == "" {
		if len(b.events) == 0 {
			return 0, io.EOF
		}
		b.rest, b.events = b.events[0], b.events[1:]
	}
	n := copy(p, b.rest)
	b.rest = b.rest[n:]
	return n, nil
}

func (b *eventBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events, b.rest = nil, ""
	return nil
}
