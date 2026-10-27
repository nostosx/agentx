package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/session"
)

// Protocol selects the encoding of streamed run events.
type Protocol string

const (
	// ProtocolAgentX streams runtime events as Server-Sent Events named
	// after the event type; see the package documentation.
	ProtocolAgentX Protocol = "agentx"
	// ProtocolVercel streams the Vercel AI SDK UI message stream, which the
	// AI SDK's useChat reads.
	ProtocolVercel Protocol = "vercel-ui"
	// ProtocolAGUI streams AG-UI events.
	ProtocolAGUI Protocol = "ag-ui"
)

// DefaultMaxBodyBytes bounds request bodies unless WithMaxBodyBytes
// changes it.
const DefaultMaxBodyBytes = 1 << 20

// DefaultHeartbeat is the interval of the comment lines that keep idle
// event streams open.
const DefaultHeartbeat = 15 * time.Second

// Error is an error with an HTTP status and a machine-readable code. Hooks
// such as WithAuth return one to choose the response, for example a 403.
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// errorf returns an *Error.
func errorf(status int, code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Error codes of the JSON error bodies.
const (
	CodeBadRequest       = "bad_request"
	CodeUnauthorized     = "unauthorized"
	CodeForbidden        = "forbidden"
	CodeNotFound         = "not_found"
	CodeSessionBusy      = "session_busy"
	CodeRunInterrupted   = "run_interrupted"
	CodeNothingToResume  = "nothing_to_resume"
	CodeConflict         = "conflict"
	CodeCannotResume     = "cannot_resume"
	CodeCannotRewind     = "cannot_rewind"
	CodePayloadTooLarge  = "payload_too_large"
	CodeUnsupportedMedia = "unsupported_media_type"
	CodeRunFailed        = "run_failed"
	CodeInternal         = "internal"
)

type config struct {
	auth       func(*http.Request) error
	access     func(*http.Request, string) error
	origins    []string
	maxBody    int64
	heartbeat  time.Duration
	protocol   Protocol
	clientOpts bool
	runOpts    func(*http.Request) []agent.RunOption
	cp         loop.Checkpointer
	a2a        A2AConfig
	log        *slog.Logger
}

// Option configures a Server.
type Option func(*config)

// WithAuth authenticates every request except CORS preflights. A non-nil
// error rejects the request with 401, or with the status of an *Error.
func WithAuth(fn func(r *http.Request) error) Option { return func(c *config) { c.auth = fn } }

// WithSessionAccess authorizes access to a session, whether its ID comes
// from the path, a chat or thread ID, or an A2A context ID. A non-nil
// error rejects the request with 403, or with the status of an *Error.
// GET /sessions lists only the sessions it allows.
func WithSessionAccess(fn func(r *http.Request, sessionID string) error) Option {
	return func(c *config) { c.access = fn }
}

// WithCORS allows cross-origin requests from origins; "*" allows any
// origin. Preflight requests are answered without authentication.
func WithCORS(origins ...string) Option { return func(c *config) { c.origins = origins } }

// WithMaxBodyBytes bounds request bodies; larger ones are rejected with
// 413. It defaults to DefaultMaxBodyBytes.
func WithMaxBodyBytes(n int64) Option { return func(c *config) { c.maxBody = n } }

// WithHeartbeat sets the interval of the comment lines sent on idle event
// streams. It defaults to DefaultHeartbeat; zero or less disables them.
func WithHeartbeat(d time.Duration) Option { return func(c *config) { c.heartbeat = d } }

// WithProtocol sets the encoding of event streams of the session
// endpoints. It defaults to ProtocolAgentX; the protocol query parameter
// overrides it per request.
func WithProtocol(p Protocol) Option { return func(c *config) { c.protocol = p } }

// AllowRunOptions lets clients set max_turns and limits of a run in the
// request body. Without it such requests are rejected, because they would
// let clients replace the agent's limits.
func AllowRunOptions() Option { return func(c *config) { c.clientOpts = true } }

// WithRunOptions adds options to every run and resumed run started by a
// request, for example limits chosen by the caller's plan. They apply
// after options from the request body.
func WithRunOptions(fn func(r *http.Request) []agent.RunOption) Option {
	return func(c *config) { c.runOpts = fn }
}

// WithCheckpointer gives the server the agent's checkpointer, so it can
// report the approvals an interrupted run waits for after a restart. The
// server otherwise remembers them only for runs it saw interrupted.
func WithCheckpointer(cp loop.Checkpointer) Option { return func(c *config) { c.cp = cp } }

// WithA2A configures the A2A agent card and task retention.
func WithA2A(cfg A2AConfig) Option { return func(c *config) { c.a2a = cfg } }

// WithLogger logs server errors.
func WithLogger(l *slog.Logger) Option { return func(c *config) { c.log = l } }

// Server serves an agent Runner over HTTP. See the package documentation
// for its endpoints and wire formats.
type Server struct {
	runner *agent.Runner
	cfg    config
	mux    *http.ServeMux

	mu sync.Mutex
	// busy holds the sessions with a request in progress.
	busy map[string]bool
	// interrupts holds, by session ID, the interrupt of the session's
	// interrupted run, without its state.
	interrupts map[string]*loop.Interrupt

	a2a *a2aTasks
	// base is cancelled by Shutdown; background A2A tasks run under it.
	base   context.Context
	cancel context.CancelFunc
	// closing is set, under mu, once Shutdown started; wg tracks the
	// background tasks started before.
	closing bool
	wg      sync.WaitGroup
}

// New returns a server for runner.
func New(runner *agent.Runner, opts ...Option) *Server {
	cfg := config{maxBody: DefaultMaxBodyBytes, heartbeat: DefaultHeartbeat, protocol: ProtocolAgentX}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.log == nil {
		cfg.log = slog.New(slog.DiscardHandler)
	}
	base, cancel := context.WithCancel(context.Background())
	s := &Server{
		runner:     runner,
		cfg:        cfg,
		busy:       map[string]bool{},
		interrupts: map[string]*loop.Interrupt{},
		a2a:        newA2ATasks(cfg.a2a.MaxTasks),
		base:       base,
		cancel:     cancel,
	}
	s.routes()
	return s
}

// Shutdown cancels the A2A tasks running in the background and waits for
// them to save their sessions until ctx ends, when it returns the cause of
// ctx. Tasks requested after Shutdown started see a canceled context and
// run in their request. Shutdown can be called again, for example with a
// longer deadline. It does not stop the HTTP server serving s; shut that
// down first.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	s.cancel()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// Close is Shutdown without a deadline.
func (s *Server) Close() error { return s.Shutdown(context.Background()) }

// background runs fn in a goroutine that Shutdown waits for. Once Shutdown
// started, it runs fn in the calling goroutine instead, where fn sees the
// canceled base context; starting a goroutine then could race with the
// wait.
func (s *Server) background(fn func()) {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		fn()
		return
	}
	s.wg.Go(fn)
	s.mu.Unlock()
}

func (s *Server) routes() {
	m := http.NewServeMux()
	m.HandleFunc("POST /sessions", s.guard(s.createSession))
	m.HandleFunc("GET /sessions", s.guard(s.listSessions))
	m.HandleFunc("GET /sessions/{id}", s.guard(s.getSession))
	m.HandleFunc("DELETE /sessions/{id}", s.guard(s.endSession))
	m.HandleFunc("POST /sessions/{id}/runs", s.guard(s.run))
	m.HandleFunc("POST /sessions/{id}/resume", s.guard(s.resume))
	m.HandleFunc("POST /sessions/{id}/discard", s.guard(s.discard))
	m.HandleFunc("POST /sessions/{id}/clear", s.guard(s.clear))
	m.HandleFunc("POST /sessions/{id}/fork", s.guard(s.fork))
	m.HandleFunc("GET /sessions/{id}/checkpoints", s.guard(s.checkpoints))
	m.HandleFunc("POST /sessions/{id}/rewind", s.guard(s.rewind))
	m.HandleFunc("POST /chat", s.guard(s.chat))
	m.HandleFunc("GET /chat/{id}/stream", s.guard(s.chatReconnect))
	m.HandleFunc("POST /agui", s.guard(s.agui))
	m.HandleFunc("GET /.well-known/agent-card.json", s.agentCard)
	m.HandleFunc("POST /a2a", s.guard(s.a2aRPC))
	s.mux = m
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.cors(w, r) {
		return
	}
	s.mux.ServeHTTP(w, r)
}

// cors sets the CORS headers of an allowed origin and answers preflight
// requests, reporting whether it did.
func (s *Server) cors(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || len(s.cfg.origins) == 0 {
		return false
	}
	if !slices.Contains(s.cfg.origins, "*") && !slices.Contains(s.cfg.origins, origin) {
		return false
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Add("Vary", "Origin")
	h.Set("Access-Control-Expose-Headers", "X-Vercel-AI-UI-Message-Stream")
	if r.Method != http.MethodOptions || r.Header.Get("Access-Control-Request-Method") == "" {
		return false
	}
	h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, Last-Event-ID, A2A-Version, A2A-Extensions")
	h.Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
	return true
}

// guard authenticates a request before its handler runs.
func (s *Server) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.auth != nil {
			if err := s.cfg.auth(r); err != nil {
				writeError(w, asError(err, http.StatusUnauthorized, CodeUnauthorized))
				return
			}
		}
		h(w, r)
	}
}

// authorize checks access to a session.
func (s *Server) authorize(r *http.Request, id string) *Error {
	if !validID(id) {
		return errorf(http.StatusBadRequest, CodeBadRequest, "invalid session ID %q", id)
	}
	if s.cfg.access != nil {
		if err := s.cfg.access(r, id); err != nil {
			return asError(err, http.StatusForbidden, CodeForbidden)
		}
	}
	return nil
}

// idPattern matches the session IDs the server accepts, which every
// built-in store accepts too.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]{0,127}$`)

func validID(id string) bool { return idPattern.MatchString(id) }

// acquire marks a session busy. It reports false if it already was.
func (s *Server) acquire(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy[id] {
		return false
	}
	s.busy[id] = true
	return true
}

func (s *Server) release(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.busy, id)
}

func (s *Server) isBusy(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy[id]
}

func errBusy(id string) *Error {
	return errorf(http.StatusConflict, CodeSessionBusy, "session %s has a request in progress", id)
}

// observe records the interrupt of a session's top-level run when it ends.
func (s *Server) observe(sessionID string, ev *loop.Event) {
	if ev.Type != loop.EventRunEnd || ev.Depth != 0 || ev.Result == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if res := ev.Result; res.Status == loop.StatusInterrupted && res.Interrupt != nil {
		s.interrupts[sessionID] = wireInterrupt(res.Interrupt)
		return
	}
	delete(s.interrupts, sessionID)
}

func (s *Server) forget(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.interrupts, sessionID)
}

// Pending describes a session's run that can be resumed.
type Pending struct {
	RunID string `json:"run_id"`
	// Kind is "interrupted" or "failed".
	Kind string `json:"kind"`
	// Interrupt lists the approvals an interrupted run waits for, when the
	// server knows them.
	Interrupt *loop.Interrupt `json:"interrupt,omitempty"`
}

// pendingRun returns the resumable run of a session, or nil.
func (s *Server) pendingRun(ctx context.Context, sess session.Session) *Pending {
	if runID, _, err := sess.State().Value[string](agent.PendingRunKey); err == nil && runID != "" {
		return &Pending{RunID: runID, Kind: "interrupted", Interrupt: s.interrupt(ctx, sess.ID(), runID)}
	}
	if runID, _, err := sess.State().Value[string](agent.FailedRunKey); err == nil && runID != "" {
		return &Pending{RunID: runID, Kind: "failed"}
	}
	return nil
}

// interrupt returns the interrupt of a session's interrupted run runID.
func (s *Server) interrupt(ctx context.Context, sessionID, runID string) *loop.Interrupt {
	s.mu.Lock()
	intr := s.interrupts[sessionID]
	s.mu.Unlock()
	if intr != nil && intr.RunID == runID {
		return intr
	}
	if s.cfg.cp == nil {
		return nil
	}
	st, err := s.cfg.cp.Load(ctx, runID)
	if err != nil {
		return nil
	}
	return interruptOf(st)
}

// interruptOf rebuilds the approval requests of an interrupted run from
// its state.
func interruptOf(st *loop.State) *loop.Interrupt {
	if st.Status != loop.StatusInterrupted {
		return nil
	}
	intr := &loop.Interrupt{RunID: st.RunID}
	for _, p := range st.Pending {
		switch {
		case p.NeedsApproval && !p.Denied && p.Result == nil:
			intr.Requests = append(intr.Requests, &loop.ApprovalRequest{Call: p.Call, Reason: p.Reason, Agent: st.Agent})
		case p.Nested != nil && p.NestedApprovals == nil && p.Result == nil:
			intr.Requests = append(intr.Requests, p.Nested.Requests...)
		}
	}
	if len(intr.Requests) == 0 {
		return nil
	}
	return intr
}

// asError converts err to an *Error, with status and code unless it is one.
func asError(err error, status int, code string) *Error {
	if e, ok := errors.AsType[*Error](err); ok {
		if e.Status == 0 {
			e = &Error{Status: status, Code: e.Code, Message: e.Message}
		}
		return e
	}
	return &Error{Status: status, Code: code, Message: err.Error()}
}

// classify maps an error of the session store or the runner to an *Error.
func classify(err error) *Error {
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &mbe):
		return errorf(http.StatusRequestEntityTooLarge, CodePayloadTooLarge, "request body exceeds %d bytes", mbe.Limit)
	case errors.Is(err, session.ErrNotFound):
		return errorf(http.StatusNotFound, CodeNotFound, "%v", err)
	case errors.Is(err, session.ErrConflict):
		return errorf(http.StatusConflict, CodeConflict, "%v", err)
	case errors.Is(err, loop.ErrCheckpointNotFound), errors.Is(err, loop.ErrNoCheckpointer):
		return errorf(http.StatusConflict, CodeCannotResume, "%v", err)
	}
	if e, ok := errors.AsType[*Error](err); ok {
		return e
	}
	return errorf(http.StatusInternalServerError, CodeInternal, "%v", err)
}

// errorBody is the JSON body of error responses.
type errorBody struct {
	Error *Error `json:"error"`
	// Pending is set for run_interrupted errors.
	Pending *Pending `json:"pending,omitempty"`
}

func writeError(w http.ResponseWriter, e *Error) { writeErrorBody(w, e, nil) }

func writeErrorBody(w http.ResponseWriter, e *Error, p *Pending) {
	writeJSON(w, e.Status, errorBody{Error: e, Pending: p})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// decode reads a JSON request body into v. An empty body leaves v as it
// is when optional.
func (s *Server) decode(w http.ResponseWriter, r *http.Request, v any, strict, optional bool) *Error {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.maxBody)
	if ct := r.Header.Get("Content-Type"); ct != "" || !optional {
		mt, _, err := mime.ParseMediaType(ct)
		if err != nil || mt != "application/json" {
			return errorf(http.StatusUnsupportedMediaType, CodeUnsupportedMedia, "the request body must be application/json")
		}
	}
	dec := json.NewDecoder(r.Body)
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) && optional {
			return nil
		}
		if e := classify(err); e.Status == http.StatusRequestEntityTooLarge {
			return e
		}
		return errorf(http.StatusBadRequest, CodeBadRequest, "invalid request body: %v", err)
	}
	if dec.More() {
		return errorf(http.StatusBadRequest, CodeBadRequest, "invalid request body: trailing data")
	}
	return nil
}

// accepts reports whether the Accept header of r lists mediaType.
func accepts(r *http.Request, mediaType string) bool {
	for v := range strings.SplitSeq(r.Header.Get("Accept"), ",") {
		mt, _, err := mime.ParseMediaType(strings.TrimSpace(v))
		if err == nil && mt == mediaType {
			return true
		}
	}
	return false
}
