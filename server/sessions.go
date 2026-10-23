package server

import (
	"cmp"
	"context"
	"errors"
	"iter"
	"net/http"
	"strings"
	"time"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/session"
)

var errNoResult = errors.New("server: run ended without a result")

// RunRequest is the body of POST /sessions/{id}/runs. Exactly one of
// Input and Message is set.
type RunRequest struct {
	// Input is the text of the user message.
	Input string `json:"input,omitempty"`
	// Message is the user message; its role is set to user.
	Message *model.Message `json:"message,omitempty"`
	// MaxTurns and Limits override the agent's limits for this run; see
	// AllowRunOptions.
	MaxTurns int          `json:"max_turns,omitempty"`
	Limits   *loop.Limits `json:"limits,omitempty"`
}

// ResumeRequest is the body of POST /sessions/{id}/resume.
type ResumeRequest struct {
	// Approvals answers the approval requests of an interrupted run, keyed
	// by tool call ID. A failed run is resumed without approvals.
	Approvals map[string]loop.Approval `json:"approvals,omitempty"`
}

// SessionSummary is an item of GET /sessions.
type SessionSummary struct {
	ID           string    `json:"id"`
	MessageCount int       `json:"message_count"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	// Busy reports a request in progress on this server.
	Busy bool `json:"busy,omitempty"`
}

// SessionInfo is the body of GET /sessions/{id}.
type SessionInfo struct {
	SessionSummary
	// Messages is the conversation.
	Messages []*model.Message `json:"messages"`
	// Pending is the run Resume continues, if any.
	Pending *Pending `json:"pending,omitempty"`
}

// sessionIDBody is the body of responses that create a session.
type sessionIDBody struct {
	ID string `json:"id"`
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	id := session.NewID()
	if e := s.authorize(r, id); e != nil {
		writeError(w, e)
		return
	}
	sess, _, err := session.GetOrCreate(r.Context(), s.runner.Store(), id)
	if err == nil {
		err = s.runner.Store().Save(r.Context(), sess)
	}
	if err != nil {
		writeError(w, classify(err))
		return
	}
	writeJSON(w, http.StatusCreated, sessionIDBody{ID: id})
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	infos, err := s.runner.Store().List(r.Context())
	if err != nil {
		writeError(w, classify(err))
		return
	}
	out := []SessionSummary{}
	for _, info := range infos {
		if s.cfg.access != nil && s.cfg.access(r, info.ID) != nil {
			continue
		}
		out = append(out, SessionSummary{ID: info.ID, MessageCount: info.Messages, CreatedAt: info.CreatedAt, UpdatedAt: info.UpdatedAt, Busy: s.isBusy(info.ID)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if e := s.authorize(r, id); e != nil {
		writeError(w, e)
		return
	}
	sess, err := s.runner.Store().Get(r.Context(), id)
	if err != nil {
		writeError(w, classify(err))
		return
	}
	msgs := sess.Messages()
	if msgs == nil {
		msgs = []*model.Message{}
	}
	writeJSON(w, http.StatusOK, SessionInfo{
		ID:           id,
		MessageCount: len(msgs),
		CreatedAt:    sess.CreatedAt(),
		UpdatedAt:    sess.UpdatedAt(),
		Busy:         s.isBusy(id),
		Messages:     msgs,
		Pending:      s.pendingRun(r.Context(), sess),
	})
}

// endSession ends a session with Runner.EndSession; ?reason= is passed to
// the SessionEnd hooks and ?purge=true also deletes it from the store.
func (s *Server) endSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if e := s.authorize(r, id); e != nil {
		writeError(w, e)
		return
	}
	if !s.acquire(id) {
		writeError(w, errBusy(id))
		return
	}
	defer s.release(id)
	ctx := r.Context()
	if _, err := s.runner.Store().Get(ctx, id); err != nil {
		writeError(w, classify(err))
		return
	}
	if err := s.runner.EndSession(ctx, id, r.URL.Query().Get("reason")); err != nil {
		s.cfg.log.Warn("session end hooks failed", "session_id", id, "error", err)
	}
	if r.URL.Query().Get("purge") == "true" {
		if err := s.runner.Store().Delete(ctx, id); err != nil && !errors.Is(err, session.ErrNotFound) {
			writeError(w, classify(err))
			return
		}
		s.forget(id)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if e := s.authorize(r, id); e != nil {
		writeError(w, e)
		return
	}
	var req RunRequest
	if e := s.decode(w, r, &req, true, false); e != nil {
		writeError(w, e)
		return
	}
	msg, e := req.message()
	if e != nil {
		writeError(w, e)
		return
	}
	opts, e := s.runOptions(r, req.MaxTurns, req.Limits)
	if e != nil {
		writeError(w, e)
		return
	}
	enc, e := s.encoderFor(r, "", nil)
	if e != nil {
		writeError(w, e)
		return
	}
	if !s.acquire(id) {
		writeError(w, errBusy(id))
		return
	}
	if e := s.checkRun(r.Context(), id); e != nil {
		s.release(id)
		writeErrorBody(w, e.err, e.pending)
		return
	}
	s.respond(w, r, id, s.runner.Run(r.Context(), id, msg, opts...), enc)
}

func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if e := s.authorize(r, id); e != nil {
		writeError(w, e)
		return
	}
	var req ResumeRequest
	if e := s.decode(w, r, &req, true, true); e != nil {
		writeError(w, e)
		return
	}
	enc, e := s.encoderFor(r, "", denials(req.Approvals))
	if e != nil {
		writeError(w, e)
		return
	}
	if !s.acquire(id) {
		writeError(w, errBusy(id))
		return
	}
	if e := s.checkResume(r.Context(), id); e != nil {
		s.release(id)
		writeError(w, e)
		return
	}
	s.respond(w, r, id, s.runner.Resume(r.Context(), id, req.Approvals), enc)
}

func (s *Server) discard(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if e := s.authorize(r, id); e != nil {
		writeError(w, e)
		return
	}
	if !s.acquire(id) {
		writeError(w, errBusy(id))
		return
	}
	defer s.release(id)
	if e := s.checkResume(r.Context(), id); e != nil {
		writeError(w, e)
		return
	}
	if err := s.runner.Discard(r.Context(), id); err != nil {
		writeError(w, classify(err))
		return
	}
	s.forget(id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) clear(w http.ResponseWriter, r *http.Request) {
	s.derive(w, r, s.runner.Clear)
}

func (s *Server) fork(w http.ResponseWriter, r *http.Request) {
	s.derive(w, r, s.runner.Fork)
}

// derive makes a new session from one with Runner.Clear or Runner.Fork.
func (s *Server) derive(w http.ResponseWriter, r *http.Request, fn func(context.Context, string) (string, error)) {
	id := r.PathValue("id")
	if e := s.authorize(r, id); e != nil {
		writeError(w, e)
		return
	}
	if !s.acquire(id) {
		writeError(w, errBusy(id))
		return
	}
	defer s.release(id)
	if _, err := s.runner.Store().Get(r.Context(), id); err != nil {
		writeError(w, classify(err))
		return
	}
	newID, err := fn(r.Context(), id)
	if newID == "" {
		writeError(w, classify(err))
		return
	}
	if err != nil {
		s.cfg.log.Warn("session end hooks failed", "session_id", id, "error", err)
	}
	writeJSON(w, http.StatusCreated, sessionIDBody{ID: newID})
}

// message returns the user message of a run request.
func (req *RunRequest) message() (*model.Message, *Error) {
	switch {
	case req.Message != nil && req.Input != "":
		return nil, errorf(http.StatusBadRequest, CodeBadRequest, "set either input or message, not both")
	case req.Message != nil:
		if len(req.Message.Parts) == 0 {
			return nil, errorf(http.StatusBadRequest, CodeBadRequest, "the message has no parts")
		}
		msg := req.Message.Clone()
		msg.Role = model.RoleUser
		return msg, nil
	case strings.TrimSpace(req.Input) != "":
		return model.UserText(req.Input), nil
	}
	return nil, errorf(http.StatusBadRequest, CodeBadRequest, "input or message is required")
}

// runOptions returns the options of a run requested with maxTurns and
// limits.
func (s *Server) runOptions(r *http.Request, maxTurns int, limits *loop.Limits) ([]agent.RunOption, *Error) {
	var opts []agent.RunOption
	if maxTurns != 0 || limits != nil {
		if !s.cfg.clientOpts {
			return nil, errorf(http.StatusBadRequest, CodeBadRequest, "max_turns and limits are not allowed by this server")
		}
		if maxTurns < 0 {
			return nil, errorf(http.StatusBadRequest, CodeBadRequest, "max_turns must not be negative")
		}
		if maxTurns > 0 {
			opts = append(opts, agent.RunMaxTurns(maxTurns))
		}
		if limits != nil {
			opts = append(opts, agent.RunLimits(*limits))
		}
	}
	if s.cfg.runOpts != nil {
		opts = append(opts, s.cfg.runOpts(r)...)
	}
	return opts, nil
}

// runCheckError is a rejected run.
type runCheckError struct {
	err     *Error
	pending *Pending
}

// checkRun rejects a run of a session with an interrupted run.
func (s *Server) checkRun(ctx context.Context, id string) *runCheckError {
	sess, err := s.runner.Store().Get(ctx, id)
	if errors.Is(err, session.ErrNotFound) {
		return nil
	}
	if err != nil {
		return &runCheckError{err: classify(err)}
	}
	if p := s.pendingRun(ctx, sess); p != nil && p.Kind == "interrupted" {
		return &runCheckError{
			err:     errorf(http.StatusConflict, CodeRunInterrupted, "session %s has an interrupted run %s; resume or discard it", id, p.RunID),
			pending: p,
		}
	}
	return nil
}

// checkResume rejects resuming or discarding a session without a run to
// resume.
func (s *Server) checkResume(ctx context.Context, id string) *Error {
	sess, err := s.runner.Store().Get(ctx, id)
	if err != nil {
		return classify(err)
	}
	if s.pendingRun(ctx, sess) == nil {
		return errorf(http.StatusConflict, CodeNothingToResume, "session %s has no interrupted run or failed run", id)
	}
	return nil
}

// denials returns the IDs of the calls an approval set denies.
func denials(approvals map[string]loop.Approval) map[string]bool {
	out := map[string]bool{}
	for id, a := range approvals {
		if !a.Approved {
			out[id] = true
		}
	}
	return out
}

// encoderFor returns the encoder of a streamed response to r, or nil for
// a JSON response with the final result. messageID and denied configure
// the Vercel encoder.
func (s *Server) encoderFor(r *http.Request, messageID string, denied map[string]bool) (encoder, *Error) {
	if accepts(r, "application/json") && !accepts(r, "text/event-stream") {
		return nil, nil
	}
	if accepts(r, "application/x-ndjson") || accepts(r, "application/jsonl") {
		return jsonlEncoder{}, nil
	}
	p := s.cfg.protocol
	if q := r.URL.Query().Get("protocol"); q != "" {
		p = Protocol(q)
	}
	switch p {
	case ProtocolAgentX, "":
		return &nativeEncoder{}, nil
	case ProtocolVercel:
		return newVercelEncoder(messageID, denied), nil
	case ProtocolAGUI:
		return newAGUIEncoder("", ""), nil
	}
	return nil, errorf(http.StatusBadRequest, CodeBadRequest, "unknown protocol %q", p)
}

// respond sends the events of a run of a session that the caller marked
// busy: streamed by enc or, if enc is nil, as the final result.
func (s *Server) respond(w http.ResponseWriter, r *http.Request, id string, seq iter.Seq2[*loop.Event, error], enc encoder) {
	if enc != nil {
		s.serveStream(w, r, id, seq, enc)
		return
	}
	defer s.release(id)
	var res *loop.Result
	var runErr error
	for ev, err := range seq {
		if err != nil {
			runErr = err
			break
		}
		s.observe(id, ev)
		if ev.Type == loop.EventRunEnd && ev.Depth == 0 {
			res = ev.Result
		}
	}
	switch {
	case res == nil:
		writeError(w, s.runError(r.Context(), id, cmp.Or(runErr, errNoResult)))
	case runErr != nil && errors.Is(runErr, session.ErrConflict):
		writeError(w, classify(runErr))
	default:
		out := wireResult(res)
		if out.Err == nil {
			out.Err = runErr
		}
		writeJSON(w, http.StatusOK, out)
	}
}
