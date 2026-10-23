package server

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/nostosx/agentx"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/session"
)

// This file implements the server side of the A2A protocol 1.0 over its
// JSON-RPC binding: https://a2a-protocol.org/latest/specification/

// A2AVersion is the A2A protocol version the server implements.
const A2AVersion = "1.0"

// DefaultA2AMaxTasks bounds the A2A tasks the server remembers unless
// A2AConfig.MaxTasks changes it.
const DefaultA2AMaxTasks = 1000

// A2AConfig describes the agent in its A2A agent card.
type A2AConfig struct {
	// Name, Description and Version identify the agent. They default to
	// "agentx", "An agentx agent." and the framework version.
	Name        string
	Description string
	Version     string
	// URL is the URL of the JSON-RPC endpoint, POST /a2a. By default it is
	// derived from the request for the card, which is wrong when the
	// handler is mounted under a prefix.
	URL              string
	Provider         *A2AProvider
	DocumentationURL string
	IconURL          string
	// Skills default to one "chat" skill with the agent's description.
	Skills []A2ASkill
	// SecuritySchemes and SecurityRequirements are copied into the card
	// as they are, to describe the authentication WithAuth checks.
	SecuritySchemes      map[string]any
	SecurityRequirements []map[string]any
	// MaxTasks bounds the tasks the server remembers; the oldest finished
	// tasks are forgotten first. It defaults to DefaultA2AMaxTasks.
	MaxTasks int
}

// A2AProvider is the provider of an agent in its card.
type A2AProvider struct {
	Organization string `json:"organization"`
	URL          string `json:"url"`
}

// A2ASkill is a skill of an agent in its card.
type A2ASkill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Examples    []string `json:"examples,omitempty"`
	InputModes  []string `json:"inputModes,omitempty"`
	OutputModes []string `json:"outputModes,omitempty"`
}

// A2A task states.
const (
	stateSubmitted     = "TASK_STATE_SUBMITTED"
	stateWorking       = "TASK_STATE_WORKING"
	stateCompleted     = "TASK_STATE_COMPLETED"
	stateFailed        = "TASK_STATE_FAILED"
	stateCanceled      = "TASK_STATE_CANCELED"
	stateInputRequired = "TASK_STATE_INPUT_REQUIRED"
	stateRejected      = "TASK_STATE_REJECTED"
)

func terminal(state string) bool {
	switch state {
	case stateCompleted, stateFailed, stateCanceled, stateRejected:
		return true
	}
	return false
}

// A2A and JSON-RPC error codes.
const (
	rpcParseError          = -32700
	rpcInvalidRequest      = -32600
	rpcMethodNotFound      = -32601
	rpcInvalidParams       = -32602
	rpcInternal            = -32603
	a2aTaskNotFound        = -32001
	a2aTaskNotCancelable   = -32002
	a2aPushNotSupported    = -32003
	a2aUnsupportedOp       = -32004
	a2aContentTypeNotSupp  = -32005
	a2aExtendedCardMissing = -32007
	a2aVersionNotSupported = -32009
)

// inputModes are the media types of message parts the server accepts:
// text and data parts, and files of acceptedMedia.
var inputModes = append([]string{"text/plain", "application/json"}, acceptedMedia...)

type a2aPart struct {
	Text      *string         `json:"text,omitempty"`
	Raw       []byte          `json:"raw,omitempty"`
	URL       string          `json:"url,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Metadata  map[string]any  `json:"metadata,omitempty"`
	Filename  string          `json:"filename,omitempty"`
	MediaType string          `json:"mediaType,omitempty"`
}

func textPart(s string) a2aPart { return a2aPart{Text: &s} }

type a2aMessage struct {
	MessageID        string         `json:"messageId"`
	ContextID        string         `json:"contextId,omitempty"`
	TaskID           string         `json:"taskId,omitempty"`
	Role             string         `json:"role"`
	Parts            []a2aPart      `json:"parts"`
	Metadata         map[string]any `json:"metadata,omitempty"`
	Extensions       []string       `json:"extensions,omitempty"`
	ReferenceTaskIDs []string       `json:"referenceTaskIds,omitempty"`
}

type a2aStatus struct {
	State     string      `json:"state"`
	Message   *a2aMessage `json:"message,omitempty"`
	Timestamp string      `json:"timestamp,omitempty"`
}

type a2aArtifact struct {
	ArtifactID string    `json:"artifactId"`
	Name       string    `json:"name,omitempty"`
	Parts      []a2aPart `json:"parts"`
}

type a2aTask struct {
	ID        string         `json:"id"`
	ContextID string         `json:"contextId"`
	Status    a2aStatus      `json:"status"`
	Artifacts []a2aArtifact  `json:"artifacts,omitempty"`
	History   []*a2aMessage  `json:"history,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

type a2aStatusUpdate struct {
	TaskID    string    `json:"taskId"`
	ContextID string    `json:"contextId"`
	Status    a2aStatus `json:"status"`
}

type a2aArtifactUpdate struct {
	TaskID    string      `json:"taskId"`
	ContextID string      `json:"contextId"`
	Artifact  a2aArtifact `json:"artifact"`
	LastChunk bool        `json:"lastChunk,omitempty"`
}

// a2aStreamResponse holds exactly one of its fields.
type a2aStreamResponse struct {
	Task           *a2aTask           `json:"task,omitempty"`
	Message        *a2aMessage        `json:"message,omitempty"`
	StatusUpdate   *a2aStatusUpdate   `json:"statusUpdate,omitempty"`
	ArtifactUpdate *a2aArtifactUpdate `json:"artifactUpdate,omitempty"`
}

// taskRecord is an A2A task: the runs of one user message in a session,
// including the runs that resume it after input was required.
type taskRecord struct {
	mu        sync.Mutex
	id        string
	contextID string
	state     string
	message   *a2aMessage
	updated   time.Time
	artifacts []a2aArtifact
	history   []*a2aMessage
	metadata  map[string]any
	// interrupt is the interrupt an input-required task waits on.
	interrupt *loop.Interrupt
	running   bool
	canceled  bool
	cancel    context.CancelFunc
	// idle is closed when the running segment of the task ends.
	idle chan struct{}
	subs map[chan a2aStreamResponse]bool
}

// snapshot returns the task with at most historyLength messages of
// history, all if nil.
func (t *taskRecord) snapshot(historyLength *int, artifacts bool) *a2aTask {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := &a2aTask{ID: t.id, ContextID: t.contextID, Status: t.status(), Metadata: t.metadata}
	if artifacts {
		out.Artifacts = slices.Clone(t.artifacts)
	}
	h := t.history
	if historyLength != nil {
		h = h[len(h)-min(max(*historyLength, 0), len(h)):]
	}
	out.History = slices.Clone(h)
	return out
}

func (t *taskRecord) status() a2aStatus {
	return a2aStatus{State: t.state, Message: t.message, Timestamp: t.updated.Format(time.RFC3339Nano)}
}

// setStatus changes the task's status and broadcasts it. t.mu is held.
func (t *taskRecord) setStatus(state string, msg *a2aMessage) {
	t.state, t.message, t.updated = state, msg, time.Now().UTC()
	if msg != nil {
		t.history = append(t.history, msg)
	}
	t.broadcast(a2aStreamResponse{StatusUpdate: &a2aStatusUpdate{TaskID: t.id, ContextID: t.contextID, Status: t.status()}})
}

// broadcast sends an update to the subscribers. A subscriber that does
// not keep up is dropped; it can subscribe again. t.mu is held.
func (t *taskRecord) broadcast(resp a2aStreamResponse) {
	for ch := range t.subs {
		select {
		case ch <- resp:
		default:
			delete(t.subs, ch)
			close(ch)
		}
	}
}

// subscribe returns a channel of the task's updates, closed when its
// running segment ends, and the task as it is now.
func (t *taskRecord) subscribe() (chan a2aStreamResponse, *a2aTask) {
	snap := t.snapshot(nil, true)
	t.mu.Lock()
	defer t.mu.Unlock()
	ch := make(chan a2aStreamResponse, 256)
	if !t.running {
		close(ch)
		return ch, snap
	}
	t.subs[ch] = true
	return ch, snap
}

func (t *taskRecord) unsubscribe(ch chan a2aStreamResponse) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.subs[ch] {
		delete(t.subs, ch)
		close(ch)
	}
}

// a2aTasks holds the tasks the server remembers.
type a2aTasks struct {
	mu    sync.Mutex
	max   int
	tasks map[string]*taskRecord
	order []string
}

func newA2ATasks(maxTasks int) *a2aTasks {
	if maxTasks <= 0 {
		maxTasks = DefaultA2AMaxTasks
	}
	return &a2aTasks{max: maxTasks, tasks: map[string]*taskRecord{}}
}

func (ts *a2aTasks) get(id string) *taskRecord {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.tasks[id]
}

// add records a task and forgets the oldest finished tasks over the limit.
func (ts *a2aTasks) add(t *taskRecord) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.tasks[t.id] = t
	ts.order = append(ts.order, t.id)
	for i := 0; len(ts.tasks) > ts.max && i < len(ts.order); {
		old := ts.tasks[ts.order[i]]
		old.mu.Lock()
		done := terminal(old.state)
		old.mu.Unlock()
		if !done {
			i++
			continue
		}
		delete(ts.tasks, old.id)
		ts.order = slices.Delete(ts.order, i, i+1)
	}
}

func (ts *a2aTasks) all() []*taskRecord {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	out := make([]*taskRecord, 0, len(ts.order))
	for _, id := range ts.order {
		out = append(out, ts.tasks[id])
	}
	return out
}

// pendingTask returns the input-required task of a context, if known.
func (ts *a2aTasks) pendingTask(contextID string) string {
	for _, t := range ts.all() {
		t.mu.Lock()
		match := t.contextID == contextID && t.state == stateInputRequired
		t.mu.Unlock()
		if match {
			return t.id
		}
	}
	return ""
}

// agentCard serves the A2A agent card.
func (s *Server) agentCard(w http.ResponseWriter, r *http.Request) {
	c := s.cfg.a2a
	url := c.URL
	if url == "" {
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		url = scheme + "://" + r.Host + "/a2a"
	}
	desc := cmp.Or(c.Description, "An agentx agent.")
	skills := c.Skills
	if len(skills) == 0 {
		skills = []A2ASkill{{ID: "chat", Name: "Chat", Description: desc, Tags: []string{"chat"}}}
	}
	card := struct {
		Name                 string              `json:"name"`
		Description          string              `json:"description"`
		SupportedInterfaces  []map[string]string `json:"supportedInterfaces"`
		Provider             *A2AProvider        `json:"provider,omitempty"`
		Version              string              `json:"version"`
		DocumentationURL     string              `json:"documentationUrl,omitempty"`
		Capabilities         map[string]bool     `json:"capabilities"`
		SecuritySchemes      map[string]any      `json:"securitySchemes,omitempty"`
		SecurityRequirements []map[string]any    `json:"securityRequirements,omitempty"`
		DefaultInputModes    []string            `json:"defaultInputModes"`
		DefaultOutputModes   []string            `json:"defaultOutputModes"`
		Skills               []A2ASkill          `json:"skills"`
		IconURL              string              `json:"iconUrl,omitempty"`
	}{
		Name:                 cmp.Or(c.Name, "agentx"),
		Description:          desc,
		SupportedInterfaces:  []map[string]string{{"url": url, "protocolBinding": "JSONRPC", "protocolVersion": A2AVersion}},
		Provider:             c.Provider,
		Version:              cmp.Or(c.Version, agentx.Version),
		DocumentationURL:     c.DocumentationURL,
		Capabilities:         map[string]bool{"streaming": true, "pushNotifications": false, "extendedAgentCard": false},
		SecuritySchemes:      c.SecuritySchemes,
		SecurityRequirements: c.SecurityRequirements,
		DefaultInputModes:    inputModes,
		DefaultOutputModes:   []string{"text/plain"},
		Skills:               skills,
		IconURL:              c.IconURL,
	}
	w.Header().Set("Cache-Control", "max-age=300")
	writeJSON(w, http.StatusOK, card)
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

func rpcErr(code int, format string, args ...any) *rpcError {
	return &rpcError{Code: code, Message: fmt.Sprintf(format, args...)}
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, e *rpcError) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: id, Result: result, Error: e})
}

// a2aRPC serves the JSON-RPC binding.
func (s *Server) a2aRPC(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.maxBody)
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeError(w, errorf(http.StatusUnsupportedMediaType, CodeUnsupportedMedia, "the request body must be application/json"))
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, classify(err))
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPC(w, nil, nil, rpcErr(rpcParseError, "Invalid JSON payload: %v", err))
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" || len(req.ID) == 0 {
		writeRPC(w, req.ID, nil, rpcErr(rpcInvalidRequest, "Request payload validation error: jsonrpc must be \"2.0\" and method and id are required"))
		return
	}
	if v := a2aVersion(r); v != A2AVersion {
		writeRPC(w, req.ID, nil, rpcErr(a2aVersionNotSupported, "A2A version %q is not supported; send A2A-Version: %s", v, A2AVersion))
		return
	}
	switch req.Method {
	case "SendMessage", "SendStreamingMessage":
		s.a2aSend(w, r, &req)
	case "GetTask":
		s.a2aGetTask(w, r, &req)
	case "ListTasks":
		s.a2aListTasks(w, r, &req)
	case "CancelTask":
		s.a2aCancelTask(w, r, &req)
	case "SubscribeToTask":
		s.a2aSubscribe(w, r, &req)
	case "GetExtendedAgentCard":
		writeRPC(w, req.ID, nil, rpcErr(a2aExtendedCardMissing, "the agent has no extended agent card"))
	case "CreateTaskPushNotificationConfig", "GetTaskPushNotificationConfig",
		"ListTaskPushNotificationConfigs", "DeleteTaskPushNotificationConfig":
		writeRPC(w, req.ID, nil, rpcErr(a2aPushNotSupported, "push notifications are not supported"))
	default:
		writeRPC(w, req.ID, nil, rpcErr(rpcMethodNotFound, "Method not found: %s", req.Method))
	}
}

// a2aVersion returns the Major.Minor protocol version of a request; an
// empty one means 0.3.
func a2aVersion(r *http.Request) string {
	v := strings.TrimSpace(cmp.Or(r.Header.Get("A2A-Version"), r.URL.Query().Get("A2A-Version")))
	if v == "" {
		return "0.3"
	}
	if major, rest, ok := strings.Cut(v, "."); ok {
		minor, _, _ := strings.Cut(rest, ".")
		return major + "." + minor
	}
	return v
}

// decodeParams decodes the params of a request.
func decodeParams(req *rpcRequest, v any) *rpcError {
	if len(req.Params) == 0 {
		return rpcErr(rpcInvalidParams, "Invalid parameters: params are required")
	}
	if err := json.Unmarshal(req.Params, v); err != nil {
		return rpcErr(rpcInvalidParams, "Invalid parameters: %v", err)
	}
	return nil
}

// task returns a task the request may access.
func (s *Server) task(r *http.Request, id string) (*taskRecord, *rpcError) {
	t := s.a2a.get(id)
	if t != nil && s.cfg.access != nil && s.cfg.access(r, t.contextID) != nil {
		t = nil
	}
	if t == nil {
		return nil, rpcErr(a2aTaskNotFound, "task %s not found", id)
	}
	return t, nil
}

type sendParams struct {
	Message       *a2aMessage `json:"message"`
	Configuration *struct {
		AcceptedOutputModes []string `json:"acceptedOutputModes"`
		HistoryLength       *int     `json:"historyLength"`
		ReturnImmediately   bool     `json:"returnImmediately"`
	} `json:"configuration"`
}

// a2aSend serves SendMessage and SendStreamingMessage. A message without
// a task ID starts a task with a run in the session named by its context
// ID; a message to an input-required task answers its approvals and
// resumes it.
func (s *Server) a2aSend(w http.ResponseWriter, r *http.Request, req *rpcRequest) {
	var p sendParams
	if e := decodeParams(req, &p); e != nil {
		writeRPC(w, req.ID, nil, e)
		return
	}
	msg := p.Message
	switch {
	case msg == nil:
		writeRPC(w, req.ID, nil, rpcErr(rpcInvalidParams, "Invalid parameters: message is required"))
		return
	case msg.MessageID == "" || len(msg.Parts) == 0:
		writeRPC(w, req.ID, nil, rpcErr(rpcInvalidParams, "Invalid parameters: message.messageId and message.parts are required"))
		return
	case msg.Role != "ROLE_USER":
		writeRPC(w, req.ID, nil, rpcErr(rpcInvalidParams, "Invalid parameters: message.role must be ROLE_USER"))
		return
	}
	var history *int
	if c := p.Configuration; c != nil {
		history = c.HistoryLength
		if len(c.AcceptedOutputModes) > 0 && !slices.ContainsFunc(c.AcceptedOutputModes, acceptsText) {
			writeRPC(w, req.ID, nil, rpcErr(a2aContentTypeNotSupp, "the agent only outputs text/plain"))
			return
		}
	}
	var t *taskRecord
	var seq iter.Seq2[*loop.Event, error]
	ctx, cancel := context.WithCancel(s.base)
	if msg.TaskID != "" {
		var e *rpcError
		t, seq, e = s.a2aContinue(ctx, cancel, r, msg)
		if e != nil {
			cancel()
			writeRPC(w, req.ID, nil, e)
			return
		}
	} else {
		contextID := msg.ContextID
		if contextID == "" {
			contextID = session.NewID()
		}
		if !validID(contextID) {
			cancel()
			writeRPC(w, req.ID, nil, rpcErr(rpcInvalidParams, "Invalid parameters: invalid contextId %q", contextID))
			return
		}
		if e := s.authorize(r, contextID); e != nil {
			cancel()
			writeError(w, e)
			return
		}
		in, e := a2aInput(msg.Parts)
		if e != nil {
			cancel()
			writeRPC(w, req.ID, nil, e)
			return
		}
		if !s.acquire(contextID) {
			cancel()
			writeRPC(w, req.ID, nil, rpcErr(a2aUnsupportedOp, "context %s has a task in progress", contextID))
			return
		}
		if ce := s.checkRun(r.Context(), contextID); ce != nil {
			s.release(contextID)
			cancel()
			e := rpcErr(rpcInternal, "%s", ce.err.Message)
			if ce.err.Code == CodeRunInterrupted {
				e = rpcErr(a2aUnsupportedOp, "context %s has a task awaiting input; reply to it with its taskId %s", contextID, s.a2a.pendingTask(contextID))
			}
			writeRPC(w, req.ID, nil, e)
			return
		}
		t = &taskRecord{
			id: "task_" + uuid.NewV7().String(), contextID: contextID, state: stateSubmitted, updated: time.Now().UTC(),
			subs: map[chan a2aStreamResponse]bool{}, running: true, cancel: cancel, idle: make(chan struct{}),
		}
		m := *msg
		m.TaskID, m.ContextID = t.id, contextID
		t.history = []*a2aMessage{&m}
		s.a2a.add(t)
		opts, _ := s.runOptions(r, 0, nil)
		seq = s.runner.Run(ctx, contextID, in, opts...)
	}
	t.mu.Lock()
	idle := t.idle
	t.mu.Unlock()
	var sub chan a2aStreamResponse
	var first *a2aTask
	if req.Method == "SendStreamingMessage" {
		sub, first = t.subscribe()
	}
	s.wg.Go(func() { s.a2aRun(t, seq) })
	if sub != nil {
		s.a2aStream(w, r, req.ID, t, sub, first)
		return
	}
	if p.Configuration == nil || !p.Configuration.ReturnImmediately {
		select {
		case <-idle:
		case <-r.Context().Done():
			return
		}
	}
	writeRPC(w, req.ID, map[string]any{"task": t.snapshot(history, true)}, nil)
}

// a2aContinue answers the approvals of an input-required task and
// returns the events of the resumed run.
func (s *Server) a2aContinue(ctx context.Context, cancel context.CancelFunc, r *http.Request, msg *a2aMessage) (*taskRecord, iter.Seq2[*loop.Event, error], *rpcError) {
	t, e := s.task(r, msg.TaskID)
	if e != nil {
		return nil, nil, e
	}
	t.mu.Lock()
	state, contextID, intr := t.state, t.contextID, t.interrupt
	t.mu.Unlock()
	switch {
	case msg.ContextID != "" && msg.ContextID != contextID:
		return nil, nil, rpcErr(rpcInvalidParams, "Invalid parameters: contextId %s does not match task %s", msg.ContextID, t.id)
	case terminal(state):
		return nil, nil, rpcErr(a2aUnsupportedOp, "task %s is in terminal state %s", t.id, state)
	case state != stateInputRequired:
		return nil, nil, rpcErr(a2aUnsupportedOp, "task %s is still working", t.id)
	}
	approvals, e := a2aApprovals(msg.Parts, intr)
	if e != nil {
		return nil, nil, e
	}
	if !s.acquire(contextID) {
		return nil, nil, rpcErr(a2aUnsupportedOp, "context %s has a task in progress", contextID)
	}
	if err := s.checkResume(r.Context(), contextID); err != nil {
		s.release(contextID)
		return nil, nil, rpcErr(a2aUnsupportedOp, "task %s cannot continue: %s", t.id, err.Message)
	}
	m := *msg
	m.ContextID = contextID
	t.mu.Lock()
	t.history = append(t.history, &m)
	t.running, t.canceled, t.cancel, t.idle = true, false, cancel, make(chan struct{})
	t.mu.Unlock()
	return t, s.runner.Resume(ctx, contextID, approvals), nil
}

// a2aRun runs a segment of a task in the background and records its
// progress. The task's session is released when the run has ended.
func (s *Server) a2aRun(t *taskRecord, seq iter.Seq2[*loop.Event, error]) {
	var res *loop.Result
	var runErr error
	for ev, err := range seq {
		if err != nil {
			runErr = err
			break
		}
		s.observe(t.contextID, ev)
		if ev.Depth != 0 {
			continue
		}
		switch ev.Type {
		case loop.EventRunStart:
			t.mu.Lock()
			t.setStatus(stateWorking, nil)
			t.mu.Unlock()
		case loop.EventMessage:
			if calls := ev.Message.ToolCalls(); len(calls) > 0 {
				t.mu.Lock()
				t.setStatus(stateWorking, t.agentMessage(ev.Message.Text(), map[string]any{"tool_calls": calls}))
				t.mu.Unlock()
			}
		case loop.EventHandoff:
			if h := ev.Handoff; h != nil {
				t.mu.Lock()
				t.setStatus(stateWorking, t.agentMessage(fmt.Sprintf("The %s agent handed the task off to the %s agent.", h.From, h.To), map[string]any{"handoff": h}))
				t.mu.Unlock()
			}
		case loop.EventRunEnd:
			res = ev.Result
		}
	}
	s.a2aFinish(t, res, runErr)
}

// agentMessage returns a message of the agent with text and data parts.
// t.mu is held.
func (t *taskRecord) agentMessage(text string, data any) *a2aMessage {
	m := &a2aMessage{MessageID: "msg_" + uuid.NewV7().String(), ContextID: t.contextID, TaskID: t.id, Role: "ROLE_AGENT"}
	if text != "" {
		m.Parts = append(m.Parts, textPart(text))
	}
	if data != nil {
		b, _ := json.Marshal(data)
		m.Parts = append(m.Parts, a2aPart{Data: b, MediaType: "application/json"})
	}
	return m
}

// a2aFinish records the end of a task's running segment.
func (s *Server) a2aFinish(t *taskRecord, res *loop.Result, runErr error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Release the session before publishing completion or waking a caller that
	// may immediately start another run. Keep the task locked through the update
	// so a continuation cannot observe the previous segment's state.
	s.release(t.contextID)
	t.interrupt = nil
	state, text := stateFailed, ""
	var data any
	switch {
	case t.canceled:
		state, text = stateCanceled, "The task was canceled."
	case res == nil:
		text = cmp.Or(runErr, errNoResult).Error()
	case res.Status == loop.StatusCompleted || res.Status == loop.StatusIncomplete:
		state = stateCompleted
		if res.Status == loop.StatusIncomplete {
			state, text = stateFailed, runMessage(res)
		}
		if res.Output != "" {
			art := a2aArtifact{ArtifactID: "output-" + res.RunID, Name: "output", Parts: []a2aPart{textPart(res.Output)}}
			t.artifacts = append(t.artifacts, art)
			t.broadcast(a2aStreamResponse{ArtifactUpdate: &a2aArtifactUpdate{TaskID: t.id, ContextID: t.contextID, Artifact: art, LastChunk: true}})
		}
	case res.Status == loop.StatusInterrupted && res.Interrupt != nil:
		state = stateInputRequired
		t.interrupt = wireInterrupt(res.Interrupt)
		var names []string
		for _, req := range res.Interrupt.Requests {
			names = append(names, req.Call.Name)
		}
		text = "Approval required for tool calls: " + strings.Join(names, ", ") + `. Reply with a data part {"approved": true} or {"approvals": {"<call_id>": {"approved": true}}}.`
		data = map[string]any{"approval_requests": t.interrupt.Requests}
	case res.Status == loop.StatusBlocked:
		state, text = stateRejected, runMessage(res)
	default:
		text = runMessage(res)
	}
	if res != nil {
		meta := map[string]any{"run_id": res.RunID, "status": res.Status, "reason": res.Reason, "usage": res.Usage}
		if res.Agent != "" {
			meta["agent"] = res.Agent
		}
		if res.Guardrail != nil {
			meta["guardrail"] = res.Guardrail
		}
		t.metadata = map[string]any{"agentx": meta}
	}
	var msg *a2aMessage
	if text != "" || data != nil {
		msg = t.agentMessage(text, data)
	}
	t.setStatus(state, msg)
	t.running = false
	for ch := range t.subs {
		delete(t.subs, ch)
		close(ch)
	}
	close(t.idle)
}

// a2aStream streams a task's updates as JSON-RPC responses in
// Server-Sent Events, starting with the task. A client that goes away
// does not stop the task.
func (s *Server) a2aStream(w http.ResponseWriter, r *http.Request, id json.RawMessage, t *taskRecord, sub chan a2aStreamResponse, first *a2aTask) {
	defer t.unsubscribe(sub)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	sw := newSSEWriter(w)
	send := func(resp a2aStreamResponse) {
		sw.event("", "", rpcResponse{JSONRPC: "2.0", ID: id, Result: resp})
	}
	send(a2aStreamResponse{Task: first})
	if sw.flush() != nil {
		return
	}
	var tick <-chan time.Time
	if s.cfg.heartbeat > 0 {
		tk := time.NewTicker(s.cfg.heartbeat)
		defer tk.Stop()
		tick = tk.C
	}
	for {
		select {
		case resp, ok := <-sub:
			if !ok {
				return
			}
			send(resp)
		case <-tick:
			sw.comment("ping")
		case <-r.Context().Done():
			return
		}
		if sw.flush() != nil {
			return
		}
	}
}

func (s *Server) a2aGetTask(w http.ResponseWriter, r *http.Request, req *rpcRequest) {
	var p struct {
		ID            string `json:"id"`
		HistoryLength *int   `json:"historyLength"`
	}
	if e := decodeParams(req, &p); e != nil {
		writeRPC(w, req.ID, nil, e)
		return
	}
	t, e := s.task(r, p.ID)
	if e != nil {
		writeRPC(w, req.ID, nil, e)
		return
	}
	writeRPC(w, req.ID, t.snapshot(p.HistoryLength, true), nil)
}

func (s *Server) a2aListTasks(w http.ResponseWriter, r *http.Request, req *rpcRequest) {
	var p struct {
		ContextID            string `json:"contextId"`
		Status               string `json:"status"`
		PageSize             *int   `json:"pageSize"`
		PageToken            string `json:"pageToken"`
		HistoryLength        *int   `json:"historyLength"`
		StatusTimestampAfter string `json:"statusTimestampAfter"`
		IncludeArtifacts     bool   `json:"includeArtifacts"`
	}
	if len(req.Params) > 0 {
		if e := decodeParams(req, &p); e != nil {
			writeRPC(w, req.ID, nil, e)
			return
		}
	}
	size := 50
	if p.PageSize != nil {
		if *p.PageSize < 1 || *p.PageSize > 100 {
			writeRPC(w, req.ID, nil, rpcErr(rpcInvalidParams, "Invalid parameters: pageSize must be between 1 and 100"))
			return
		}
		size = *p.PageSize
	}
	offset := 0
	if p.PageToken != "" {
		n, err := strconv.Atoi(p.PageToken)
		if err != nil || n < 0 {
			writeRPC(w, req.ID, nil, rpcErr(rpcInvalidParams, "Invalid parameters: invalid pageToken"))
			return
		}
		offset = n
	}
	var after time.Time
	if p.StatusTimestampAfter != "" {
		ts, err := time.Parse(time.RFC3339Nano, p.StatusTimestampAfter)
		if err != nil {
			writeRPC(w, req.ID, nil, rpcErr(rpcInvalidParams, "Invalid parameters: invalid statusTimestampAfter"))
			return
		}
		after = ts
	}
	type entry struct {
		t       *taskRecord
		updated time.Time
	}
	var matched []entry
	for _, t := range s.a2a.all() {
		t.mu.Lock()
		ctxID, state, updated := t.contextID, t.state, t.updated
		t.mu.Unlock()
		if (p.ContextID != "" && ctxID != p.ContextID) || (p.Status != "" && state != p.Status) || (!after.IsZero() && !updated.After(after)) {
			continue
		}
		if s.cfg.access != nil && s.cfg.access(r, ctxID) != nil {
			continue
		}
		matched = append(matched, entry{t, updated})
	}
	slices.SortStableFunc(matched, func(a, b entry) int { return b.updated.Compare(a.updated) })
	tasks := []*a2aTask{}
	for _, m := range matched[min(offset, len(matched)):min(offset+size, len(matched))] {
		tasks = append(tasks, m.t.snapshot(p.HistoryLength, p.IncludeArtifacts))
	}
	next := ""
	if offset+size < len(matched) {
		next = strconv.Itoa(offset + size)
	}
	writeRPC(w, req.ID, map[string]any{"tasks": tasks, "nextPageToken": next, "pageSize": size, "totalSize": len(matched)}, nil)
}

func (s *Server) a2aCancelTask(w http.ResponseWriter, r *http.Request, req *rpcRequest) {
	var p struct {
		ID string `json:"id"`
	}
	if e := decodeParams(req, &p); e != nil {
		writeRPC(w, req.ID, nil, e)
		return
	}
	t, e := s.task(r, p.ID)
	if e != nil {
		writeRPC(w, req.ID, nil, e)
		return
	}
	t.mu.Lock()
	state, running, idle, cancel := t.state, t.running, t.idle, t.cancel
	if running {
		t.canceled = true
	}
	t.mu.Unlock()
	switch {
	case running:
		cancel()
		select {
		case <-idle:
		case <-r.Context().Done():
			return
		}
	case state == stateInputRequired:
		if !s.acquire(t.contextID) {
			writeRPC(w, req.ID, nil, rpcErr(a2aTaskNotCancelable, "task %s cannot be canceled while its context is busy", t.id))
			return
		}
		err := s.runner.Discard(r.Context(), t.contextID)
		s.release(t.contextID)
		if err != nil {
			writeRPC(w, req.ID, nil, rpcErr(rpcInternal, "Internal error: %v", err))
			return
		}
		s.forget(t.contextID)
		t.mu.Lock()
		t.interrupt = nil
		t.setStatus(stateCanceled, nil)
		t.mu.Unlock()
	default:
		writeRPC(w, req.ID, nil, rpcErr(a2aTaskNotCancelable, "task %s is in state %s and cannot be canceled", t.id, state))
		return
	}
	writeRPC(w, req.ID, t.snapshot(nil, true), nil)
}

func (s *Server) a2aSubscribe(w http.ResponseWriter, r *http.Request, req *rpcRequest) {
	var p struct {
		ID string `json:"id"`
	}
	if e := decodeParams(req, &p); e != nil {
		writeRPC(w, req.ID, nil, e)
		return
	}
	t, e := s.task(r, p.ID)
	if e != nil {
		writeRPC(w, req.ID, nil, e)
		return
	}
	sub, first := t.subscribe()
	if terminal(first.Status.State) {
		t.unsubscribe(sub)
		writeRPC(w, req.ID, nil, rpcErr(a2aUnsupportedOp, "task %s is in terminal state %s", t.id, first.Status.State))
		return
	}
	s.a2aStream(w, r, req.ID, t, sub, first)
}

// acceptsText reports whether an accepted output mode covers text/plain.
func acceptsText(mode string) bool {
	return mode == "text/plain" || mode == "text/*" || mode == "*/*"
}

// a2aInput converts the parts of a user message.
func a2aInput(parts []a2aPart) (*model.Message, *rpcError) {
	msg := &model.Message{Role: model.RoleUser}
	for _, p := range parts {
		switch {
		case p.Text != nil:
			msg.Parts = append(msg.Parts, model.TextPart(*p.Text))
		case len(p.Data) > 0:
			msg.Parts = append(msg.Parts, model.TextPart(string(p.Data)))
		case len(p.Raw) > 0 && (p.MediaType == "text/plain" || p.MediaType == "application/json"):
			msg.Parts = append(msg.Parts, model.TextPart(string(p.Raw)))
		case len(p.Raw) > 0 || p.URL != "":
			part, e := filePart(p.MediaType, p.Raw, p.URL, p.Filename)
			switch {
			case e != nil && e.Status == http.StatusUnsupportedMediaType:
				return nil, rpcErr(a2aContentTypeNotSupp, "a part with media type %q is not supported; supported: %s", p.MediaType, strings.Join(inputModes, ", "))
			case e != nil:
				return nil, rpcErr(rpcInvalidParams, "%s", e.Message)
			}
			msg.Parts = append(msg.Parts, part)
		default:
			return nil, rpcErr(rpcInvalidParams, "a part has no text, data, raw content or URL")
		}
	}
	return msg, nil
}

// a2aApprovals reads the approvals of a reply to an input-required task:
// a data part {"approvals": {"<call_id>": {"approved": true}}}, or
// {"approved": bool, "reason": "..."} for every pending request.
func a2aApprovals(parts []a2aPart, intr *loop.Interrupt) (map[string]loop.Approval, *rpcError) {
	for _, p := range parts {
		if len(p.Data) == 0 {
			continue
		}
		var d struct {
			Approvals map[string]loop.Approval `json:"approvals"`
			Approved  *bool                    `json:"approved"`
			Reason    string                   `json:"reason"`
		}
		if json.Unmarshal(p.Data, &d) != nil {
			continue
		}
		switch {
		case d.Approvals != nil:
			for id, a := range d.Approvals {
				if !a.Approved && a.Reason == "" {
					a.Reason = loop.Deny.Reason
					d.Approvals[id] = a
				}
			}
			return d.Approvals, nil
		case d.Approved != nil:
			if intr == nil {
				return nil, rpcErr(rpcInvalidParams, "Invalid parameters: the pending approvals are unknown; answer them by call ID with {\"approvals\": {...}}")
			}
			a := loop.Approval{Approved: *d.Approved, Reason: d.Reason}
			if !a.Approved && a.Reason == "" {
				a.Reason = loop.Deny.Reason
			}
			out := map[string]loop.Approval{}
			for _, req := range intr.Requests {
				out[req.Call.ID] = a
			}
			return out, nil
		}
	}
	return nil, rpcErr(rpcInvalidParams, `Invalid parameters: answer the approvals with a data part {"approved": true} or {"approvals": {"<call_id>": {"approved": true}}}`)
}
