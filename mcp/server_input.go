package mcp

import (
	"bytes"
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"
)

// This file lets handlers ask the client for input: Sample, Elicit and
// ListRoots. In a session of an initialization-based revision they send the
// client a request and wait for its response. With ProtocolVersion servers
// send no requests; the server answers the client's request with an
// input_required result instead, and the client retries the request with
// the answers
// (https://modelcontextprotocol.io/specification/2026-07-28/basic/patterns/mrtr).
//
// The handler does not see the difference: it blocks in Sample until the
// retry brings the answer. Meanwhile it waits on the server, whose
// requestState names it. If the retry reaches a server that does not hold
// the waiting handler, such as another instance behind a load balancer
// sharing ServerOptions.StateKey, or the same server after a restart, the
// handler runs again from the start and its earlier requests are answered
// from the requestState, which carries every answer so far. Handlers that
// ask for input should therefore ask in a deterministic order, and be safe
// to run again up to their last request.

// ErrNoRequest is returned by the handler functions, such as Sample and
// Log, when ctx does not belong to a request served by a Server.
var ErrNoRequest = errors.New("mcp: not called from a request handler")

// MissingCapabilityError is returned by Sample, Elicit and ListRoots when
// the client did not declare the capability the input request needs. A
// handler that fails with it, or returns an error result, makes the server
// answer with the MissingRequiredClientCapability error (-32021) instead.
// It wraps errors.ErrUnsupported.
type MissingCapabilityError struct {
	Method string
	// Required is the missing capability in the form of
	// ClientCapabilities, such as {"sampling": {}}.
	Required map[string]any
}

func (e *MissingCapabilityError) Error() string {
	return fmt.Sprintf("mcp: %s: the client did not declare the required capability", e.Method)
}

// Unwrap returns errors.ErrUnsupported.
func (e *MissingCapabilityError) Unwrap() error { return errors.ErrUnsupported }

// invocation is the run of a handler for a request. With ProtocolVersion a
// run may span the request and its retries.
type invocation struct {
	s  *Server
	mu sync.Mutex
	// cur is the request whose stream carries the run's messages, or nil
	// while it waits for a retry; origin is the request that started it.
	cur, origin *incoming
	// k holds the input state with ProtocolVersion.
	k *continuation
	// missing collects the client capabilities the handler needed.
	missing map[string]any
}

type invocationKey struct{}

func invocationFrom(ctx context.Context) *invocation {
	inv, _ := ctx.Value(invocationKey{}).(*invocation)
	return inv
}

func (inv *invocation) current() *incoming {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	return inv.cur
}

func (inv *invocation) attach(c *incoming) {
	inv.mu.Lock()
	inv.cur = c
	inv.mu.Unlock()
}

func (inv *invocation) need(caps map[string]any) {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	if inv.missing == nil {
		inv.missing = map[string]any{}
	}
	maps.Copy(inv.missing, caps)
}

// finish turns the outcome of a handler that needed a capability the
// client lacks and failed into the MissingRequiredClientCapability error.
func (inv *invocation) finish(res any, err error) (any, error) {
	inv.mu.Lock()
	missing := inv.missing
	inv.mu.Unlock()
	if missing == nil || (err == nil && !isErrorResult(res)) {
		return res, err
	}
	if _, ok := errors.AsType[*Error](err); ok {
		return res, err
	}
	data, _ := json.Marshal(map[string]any{"requiredCapabilities": missing})
	return nil, &Error{Code: CodeMissingRequiredClientCapability, Message: "Missing required client capability", Data: data}
}

func isErrorResult(res any) bool {
	switch r := res.(type) {
	case map[string]any:
		b, _ := r["isError"].(bool)
		return b
	case toolResult:
		return r.IsError
	}
	return false
}

// handlerFunc is the part of a request a handler serves.
type handlerFunc func(ctx context.Context, c *incoming) (any, error)

// run runs fn, converting a panic into an error.
func run(ctx context.Context, c *incoming, fn handlerFunc) (res any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("mcp: %s handler panicked: %v", c.method, r)
		}
	}()
	return fn(ctx, c)
}

// withInput serves a request whose handler may ask for client input.
func (s *Server) withInput(ctx context.Context, c *incoming, fn handlerFunc) (any, error) {
	if !c.modern {
		inv := &invocation{s: s, cur: c, origin: c}
		return inv.finish(run(context.WithValue(ctx, invocationKey{}, inv), c, fn))
	}
	var p struct {
		InputResponses map[string]json.RawMessage `json:"inputResponses"`
		RequestState   *string                    `json:"requestState"`
	}
	if err := json.Unmarshal(c.params, &p); err != nil {
		return nil, invalidParams("invalid params: %v", err)
	}
	digest := paramsDigest(c.method, c.params)
	principal := c.principal()
	answers := map[string]answer{}
	fresh := map[string]answer{}
	var st *requestState
	if p.RequestState != nil {
		var err error
		st, err = s.openState(*p.RequestState)
		if err != nil || st.Method != c.method || st.Digest != digest || st.Principal != principal {
			return nil, invalidParams("invalid requestState")
		}
		maps.Copy(answers, st.Answers)
		for key, d := range st.Pending {
			if raw, ok := p.InputResponses[key]; ok && len(raw) > 0 && string(raw) != "null" {
				a := answer{Digest: d, Result: raw}
				answers[key], fresh[key] = a, a
			}
		}
	}
	var k *continuation
	if st != nil && st.Handle != "" {
		k = s.claim(st.Handle, digest, principal)
	}
	if k != nil {
		// Restore the request before an answer wakes the handler, which may ask again.
		k.inv.attach(c)
		k.deliver(fresh)
	} else {
		k = s.start(ctx, c, answers, digest, principal, fn)
	}
	return s.drive(ctx, c, k)
}

// answer is the client's answer to an input request, with the digest of
// the request.
type answer struct {
	Digest string          `json:"d"`
	Result json.RawMessage `json:"r"`
}

// pendingInput is an input request waiting for the client's answer.
type pendingInput struct {
	method string
	params json.RawMessage
	digest string
	ch     chan json.RawMessage
}

// continuation is a handler run that may wait for client input across the
// retries of a request.
type continuation struct {
	id, digest, principal string
	inv                   *invocation
	cancel                context.CancelFunc
	signal                chan struct{}
	done                  chan struct{}
	res                   any
	err                   error

	mu      sync.Mutex
	seq     int
	answers map[string]answer
	pending map[string]*pendingInput
	busy    bool
	timer   *time.Timer
}

// start runs fn as a new continuation that already knows answers.
func (s *Server) start(ctx context.Context, c *incoming, answers map[string]answer, digest, principal string, fn handlerFunc) *continuation {
	inv := &invocation{s: s, cur: c, origin: c}
	kctx, cancel := context.WithCancel(context.WithValue(context.WithoutCancel(ctx), invocationKey{}, inv))
	k := &continuation{
		id: uuid.New().String(), digest: digest, principal: principal, inv: inv, cancel: cancel,
		signal: make(chan struct{}, 1), done: make(chan struct{}),
		answers: answers, pending: map[string]*pendingInput{}, busy: true,
	}
	inv.k = k
	s.mu.Lock()
	s.conts[k.id] = k
	s.mu.Unlock()
	go func() {
		defer close(k.done)
		defer cancel()
		k.res, k.err = inv.finish(run(kctx, c, fn))
	}()
	return k
}

// claim returns the continuation a retry names, if this server holds it
// waiting for input for the same request and principal.
func (s *Server) claim(id, digest, principal string) *continuation {
	s.mu.Lock()
	k := s.conts[id]
	s.mu.Unlock()
	if k == nil || k.digest != digest || k.principal != principal {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.busy || (k.timer != nil && !k.timer.Stop()) {
		// Another retry holds it, or it expired.
		return nil
	}
	select {
	case <-k.done:
		return nil
	default:
	}
	k.busy = true
	return k
}

func (s *Server) forget(k *continuation) {
	s.mu.Lock()
	if s.conts[k.id] == k {
		delete(s.conts, k.id)
	}
	s.mu.Unlock()
}

// deliver hands answers to the input requests waiting for them.
func (k *continuation) deliver(answers map[string]answer) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, key := range slices.Sorted(maps.Keys(answers)) {
		a := answers[key]
		p := k.pending[key]
		if p == nil || p.digest != a.Digest {
			continue
		}
		k.answers[key] = a
		delete(k.pending, key)
		p.ch <- a.Result
	}
}

// ask waits for the client's answer to an input request.
func (k *continuation) ask(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	d := inputDigest(method, params)
	k.mu.Lock()
	k.seq++
	key := strconv.Itoa(k.seq)
	if a, ok := k.answers[key]; ok && a.Digest == d {
		k.mu.Unlock()
		return a.Result, nil
	}
	p := &pendingInput{method: method, params: params, digest: d, ch: make(chan json.RawMessage, 1)}
	k.pending[key] = p
	k.mu.Unlock()
	select {
	case k.signal <- struct{}{}:
	default:
	}
	select {
	case r := <-p.ch:
		return r, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (k *continuation) hasPending() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.pending) > 0
}

// drive waits for the continuation to finish or to need input, which it
// returns as an input_required result.
func (s *Server) drive(ctx context.Context, c *incoming, k *continuation) (any, error) {
	k.inv.attach(c)
	for {
		select {
		case <-k.done:
			s.forget(k)
			return k.res, k.err
		default:
		}
		if k.hasPending() {
			return s.suspend(c, k), nil
		}
		select {
		case <-k.done:
		case <-k.signal:
		case <-ctx.Done():
			// The client cancelled the request, and with it the run.
			k.cancel()
			s.forget(k)
			return nil, errNoResponse
		}
	}
}

// suspend detaches the continuation from the request, keeps it for the
// retry and returns the input_required result.
func (s *Server) suspend(c *incoming, k *continuation) any {
	k.inv.attach(nil)
	k.mu.Lock()
	requests := map[string]any{}
	pending := map[string]string{}
	for key, p := range k.pending {
		requests[key] = map[string]any{"method": p.method, "params": p.params}
		pending[key] = p.digest
	}
	answers := maps.Clone(k.answers)
	k.busy = false
	k.timer = time.AfterFunc(s.opts.InputTimeout, func() {
		k.cancel()
		s.forget(k)
	})
	k.mu.Unlock()
	state := s.seal(&requestState{
		Handle: k.id, Method: c.method, Digest: k.digest, Principal: k.principal,
		Expires: time.Now().Add(s.opts.InputTimeout).UnixMilli(), Answers: answers, Pending: pending,
	})
	return map[string]any{
		"resultType":    "input_required",
		"inputRequests": requests,
		"requestState":  state,
		"_meta":         map[string]any{metaServerInfo: s.info},
	}
}

// requestState is the state an input_required result hands the client. It
// is authenticated with the server's key, bound to the method, parameters
// and principal of the request and limited in time, as the security
// requirements of MRTR ask.
type requestState struct {
	Handle    string            `json:"h,omitempty"`
	Method    string            `json:"m"`
	Digest    string            `json:"d"`
	Principal string            `json:"p,omitempty"`
	Expires   int64             `json:"e"`
	Answers   map[string]answer `json:"a,omitempty"`
	Pending   map[string]string `json:"q,omitempty"`
}

const stateLabel = "io.agentx/mcp-request-state\x00"

func (s *Server) mac(payload []byte) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(stateLabel))
	m.Write(payload)
	return m.Sum(nil)
}

func (s *Server) seal(st *requestState) string {
	payload, _ := json.Marshal(st)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(s.mac(payload))
}

func (s *Server) openState(token string) (*requestState, error) {
	p, m, ok := strings.Cut(token, ".")
	if !ok {
		return nil, errors.New("malformed state")
	}
	payload, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return nil, err
	}
	sum, err := base64.RawURLEncoding.DecodeString(m)
	if err != nil || !hmac.Equal(sum, s.mac(payload)) {
		return nil, errors.New("state does not verify")
	}
	var st requestState
	if err := json.Unmarshal(payload, &st); err != nil {
		return nil, err
	}
	if time.Now().UnixMilli() > st.Expires {
		return nil, errors.New("state expired")
	}
	return &st, nil
}

// canonical re-encodes JSON with sorted object keys.
func canonical(raw json.RawMessage) []byte {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if d.Decode(&v) != nil {
		return raw
	}
	b, _ := json.Marshal(v)
	return b
}

// paramsDigest identifies a request by its method and parameters, without
// its _meta and the fields of MRTR retries.
func paramsDigest(method string, params json.RawMessage) string {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(params, &fields)
	delete(fields, "_meta")
	delete(fields, "inputResponses")
	delete(fields, "requestState")
	b, _ := json.Marshal(fields)
	return inputDigest(method, b)
}

func inputDigest(method string, params json.RawMessage) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write(canonical(params))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// principal returns the subject of the request's token, if any.
func (c *incoming) principal() string {
	if c.token == nil {
		return ""
	}
	return c.token.Subject
}

// clientCan reports whether the client of the request ctx belongs to
// declared the capability an input request needs.
func clientCan(ctx context.Context, method string, params any) bool {
	inv := invocationFrom(ctx)
	if inv == nil {
		return false
	}
	c := inv.current()
	return c != nil && c.caps.missing(method, params) == nil
}

// input asks the client for input: a request of method with params, whose
// result is decoded into out.
func input(ctx context.Context, method string, params, out any) error {
	inv := invocationFrom(ctx)
	if inv == nil {
		return ErrNoRequest
	}
	c := inv.current()
	if c == nil {
		return ErrNoRequest
	}
	if req := c.caps.missing(method, params); req != nil {
		inv.need(req)
		return &MissingCapabilityError{Method: method, Required: req}
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	var res json.RawMessage
	if c.sess != nil {
		res, err = c.sess.request(ctx, c.send, method, raw)
	} else {
		res, err = inv.k.ask(ctx, method, raw)
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(res, out); err != nil {
		return fmt.Errorf("mcp: decode %s result: %w", method, err)
	}
	return nil
}

// Sample asks the client to sample from its language model, from a tool,
// resource or prompt handler of a Server. It needs the client's sampling
// capability, and sampling.tools for requests with tools; without it, it
// returns a *MissingCapabilityError. Sampling is deprecated as of
// ProtocolVersion but still part of it.
func Sample(ctx context.Context, req *SamplingRequest) (*SamplingResult, error) {
	if req == nil {
		return nil, errors.New("mcp: Sample: nil request")
	}
	var res SamplingResult
	if err := input(ctx, "sampling/createMessage", req, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Elicit asks the user for information through the client, from a tool,
// resource or prompt handler of a Server. A request without a mode is a form
// request. URL mode needs the client's elicitation.url capability; in
// sessions of revision 2025-11-25 a URL request without an ElicitationID
// gets a random one, and with ProtocolVersion, which has none, the
// ElicitationID is dropped.
func Elicit(ctx context.Context, req *ElicitRequest) (*ElicitResult, error) {
	if req == nil {
		return nil, errors.New("mcp: Elicit: nil request")
	}
	r := *req
	if r.Mode == "" {
		r.Mode = ElicitationForm
	}
	if r.Mode == ElicitationURL {
		if inv := invocationFrom(ctx); inv != nil {
			if c := inv.current(); c != nil && c.modern {
				r.ElicitationID = ""
			} else if r.ElicitationID == "" {
				r.ElicitationID = uuid.New().String()
			}
		}
	}
	var res ElicitResult
	if err := input(ctx, "elicitation/create", &r, &res); err != nil {
		return nil, err
	}
	switch res.Action {
	case ElicitAccept, ElicitDecline, ElicitCancel:
	default:
		return nil, fmt.Errorf("mcp: invalid elicitation action %q", res.Action)
	}
	return &res, nil
}

// ListRoots asks the client for its roots, from a handler of a Server. Roots
// are deprecated as of ProtocolVersion but still part of it.
func ListRoots(ctx context.Context) ([]Root, error) {
	var res struct {
		Roots []Root `json:"roots"`
	}
	if err := input(ctx, "roots/list", struct{}{}, &res); err != nil {
		return nil, err
	}
	return res.Roots, nil
}

// Log sends a log message (notifications/message) about the current
// request, if the client asked for messages of that level: with
// ProtocolVersion in the request's _meta, otherwise with logging/setLevel.
// Messages of other levels are dropped without an error. data must encode
// as JSON. Logging is deprecated as of ProtocolVersion but still part of it.
func Log(ctx context.Context, level LogLevel, logger string, data any) error {
	if err := validLogLevel(level); err != nil {
		return err
	}
	inv := invocationFrom(ctx)
	if inv == nil {
		return ErrNoRequest
	}
	c := inv.current()
	if c == nil {
		return nil
	}
	floor := c.level
	if c.sess != nil {
		floor = c.sess.logLevel()
	}
	if floor == "" || slices.Index(logLevels, level) < slices.Index(logLevels, floor) {
		return nil
	}
	params := map[string]any{"level": level, "data": data}
	if logger != "" {
		params["logger"] = logger
	}
	return c.send(notification("notifications/message", params))
}

// Progress reports the progress of the current request
// (notifications/progress), if the client asked for it with a progress
// token. progress must increase with every call; total and message are
// optional and omitted when zero.
func Progress(ctx context.Context, progress, total float64, message string) error {
	inv := invocationFrom(ctx)
	if inv == nil {
		return ErrNoRequest
	}
	c := inv.current()
	if c == nil || len(c.progress) == 0 {
		return nil
	}
	params := map[string]any{"progressToken": c.progress, "progress": progress}
	if total != 0 {
		params["total"] = total
	}
	if message != "" {
		params["message"] = message
	}
	return c.send(notification("notifications/progress", params))
}

// RequestInfo describes the request a handler serves.
type RequestInfo struct {
	Method string
	// ProtocolVersion is ProtocolVersion, or the revision of the session.
	ProtocolVersion string
	// Client is what the client reports about itself; it is not verified.
	Client Implementation
	// SessionID is the Streamable HTTP session of an initialization-based
	// revision, if any.
	SessionID string
	// Token is the verified access token, when the HTTP handler checks
	// tokens.
	Token *TokenInfo
}

// CurrentRequest returns information about the request a handler serves.
func CurrentRequest(ctx context.Context) (*RequestInfo, bool) {
	inv := invocationFrom(ctx)
	if inv == nil {
		return nil, false
	}
	inv.mu.Lock()
	c := cmp.Or(inv.cur, inv.origin)
	inv.mu.Unlock()
	if c == nil {
		return nil, false
	}
	info := &RequestInfo{Method: c.method, ProtocolVersion: c.version(), Client: c.info, Token: c.token}
	if c.sess != nil {
		info.SessionID = c.sess.id
	}
	return info, true
}

// CompleteElicitation tells the client of a session of revision 2025-11-25
// that the URL mode elicitation with the given ID is complete
// (notifications/elicitation/complete). ProtocolVersion has no such
// notification, since the client learns the outcome by retrying the
// request, so CompleteElicitation then does nothing.
func CompleteElicitation(ctx context.Context, elicitationID string) error {
	inv := invocationFrom(ctx)
	if inv == nil {
		return ErrNoRequest
	}
	inv.mu.Lock()
	c := inv.cur
	inv.mu.Unlock()
	if c == nil || c.sess == nil {
		return nil
	}
	return c.sess.notify(notification("notifications/elicitation/complete", map[string]any{"elicitationId": elicitationID}))
}

// URLElicitationRequired returns the error with which a handler in a
// session of revision 2025-11-25 tells the client that the user must
// complete URL mode elicitations first (code -32042). ProtocolVersion has
// no such error: handlers call Elicit with URL mode instead.
func URLElicitationRequired(message string, elicitations ...*ElicitRequest) *Error {
	for _, e := range elicitations {
		e.Mode = ElicitationURL
	}
	data, _ := json.Marshal(map[string]any{"elicitations": elicitations})
	return &Error{Code: CodeURLElicitationRequired, Message: message, Data: data}
}
