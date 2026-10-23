package server

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"

	"github.com/nostosx/kitx/dataurl"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
)

// This file implements the Vercel AI SDK UI message stream protocol, v1:
// https://ai-sdk.dev/docs/ai-sdk-ui/stream-protocol

// uiChunk is a chunk of the UI message stream. Only the fields of its type
// are set.
type uiChunk struct {
	Type            string          `json:"type"`
	ID              string          `json:"id,omitempty"`
	MessageID       string          `json:"messageId,omitempty"`
	Delta           string          `json:"delta,omitempty"`
	ToolCallID      string          `json:"toolCallId,omitempty"`
	ToolName        string          `json:"toolName,omitempty"`
	InputTextDelta  string          `json:"inputTextDelta,omitempty"`
	Input           json.RawMessage `json:"input,omitempty"`
	Output          any             `json:"output,omitempty"`
	ErrorText       string          `json:"errorText,omitempty"`
	ApprovalID      string          `json:"approvalId,omitempty"`
	Reason          string          `json:"reason,omitempty"`
	Dynamic         bool            `json:"dynamic,omitempty"`
	URL             string          `json:"url,omitempty"`
	MediaType       string          `json:"mediaType,omitempty"`
	FinishReason    string          `json:"finishReason,omitempty"`
	MessageMetadata any             `json:"messageMetadata,omitempty"`
	// Data is the payload of a data-* part.
	Data any `json:"data,omitempty"`
}

// uiBlock is a part of the current turn's message being streamed.
type uiBlock struct {
	kind model.PartType
	id   string
	// callID and name identify a tool call; args buffers its arguments
	// until its ID is known.
	callID, name string
	args         string
	ended        bool
}

// vercelEncoder encodes the top-level run of a stream as one assistant UI
// message. Events of nested runs, such as sub-agents', are left out.
type vercelEncoder struct {
	messageID string
	// denied holds the calls the approvals being resumed deny.
	denied   map[string]bool
	started  bool
	stepOpen bool
	turn     int
	blocks   map[int]*uiBlock
	// inputs holds the calls whose complete input was sent.
	inputs map[string]bool
	result *loop.Result
	failed bool
}

func newVercelEncoder(messageID string, denied map[string]bool) *vercelEncoder {
	return &vercelEncoder{messageID: messageID, denied: denied, blocks: map[int]*uiBlock{}, inputs: map[string]bool{}}
}

func (*vercelEncoder) header(h http.Header) {
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Vercel-AI-UI-Message-Stream", "v1")
}

func (*vercelEncoder) heartbeat(w *sseWriter) { w.comment("ping") }
func (*vercelEncoder) inStream() bool         { return false }

func (e *vercelEncoder) send(w *sseWriter, c uiChunk) { w.event("", "", c) }

func (e *vercelEncoder) event(w *sseWriter, ev *loop.Event) {
	if ev.Depth != 0 {
		return
	}
	switch ev.Type {
	case loop.EventRunStart:
		e.start(w, ev.RunID)
	case loop.EventTurnStart:
		e.start(w, ev.RunID)
		e.closeStep(w)
		e.send(w, uiChunk{Type: "start-step"})
		e.stepOpen, e.turn = true, ev.Turn
	case loop.EventDelta:
		if ev.Delta != nil && ev.Delta.Delta != nil {
			e.delta(w, ev.Delta.Index, ev.Delta.Delta)
		}
	case loop.EventMessage:
		if ev.Message != nil {
			e.message(w, ev.Message)
		}
	case loop.EventToolResult:
		if r := ev.ToolResult; r != nil {
			e.toolResult(w, r)
		}
	case loop.EventHandoff:
		if ev.Handoff != nil {
			e.start(w, ev.RunID)
			e.send(w, uiChunk{Type: "data-handoff", Data: ev.Handoff})
		}
	case loop.EventOutputRetry:
		if ev.OutputRetry != nil {
			e.start(w, ev.RunID)
			e.send(w, uiChunk{Type: "data-output-retry", Data: ev.OutputRetry})
		}
	case loop.EventInterrupted:
		if ev.Interrupt != nil {
			e.approvals(w, ev.Interrupt)
		}
	case loop.EventRunEnd:
		e.result = ev.Result
		e.closeStep(w)
	}
}

func (e *vercelEncoder) start(w *sseWriter, runID string) {
	if e.started {
		return
	}
	e.started = true
	if e.messageID == "" {
		e.messageID = "msg_" + runID
	}
	e.send(w, uiChunk{Type: "start", MessageID: e.messageID})
}

// closeStep ends the parts and the step in progress.
func (e *vercelEncoder) closeStep(w *sseWriter) {
	for _, idx := range slices.Sorted(maps.Keys(e.blocks)) {
		e.endBlock(w, e.blocks[idx])
	}
	e.blocks = map[int]*uiBlock{}
	if e.stepOpen {
		e.send(w, uiChunk{Type: "finish-step"})
		e.stepOpen = false
	}
}

func (e *vercelEncoder) endBlock(w *sseWriter, b *uiBlock) {
	if b.ended {
		return
	}
	b.ended = true
	switch b.kind {
	case model.PartText:
		e.send(w, uiChunk{Type: "text-end", ID: b.id})
	case model.PartReasoning:
		e.send(w, uiChunk{Type: "reasoning-end", ID: b.id})
	}
}

func (e *vercelEncoder) delta(w *sseWriter, idx int, d *model.Part) {
	b := e.blocks[idx]
	if b == nil {
		switch d.Type {
		case model.PartText, model.PartReasoning, model.PartToolCall:
		default:
			return
		}
		b = &uiBlock{kind: d.Type, id: fmt.Sprintf("%s-%d-%d", d.Type, e.turn, idx)}
		e.blocks[idx] = b
		switch d.Type {
		case model.PartText:
			e.send(w, uiChunk{Type: "text-start", ID: b.id})
		case model.PartReasoning:
			e.send(w, uiChunk{Type: "reasoning-start", ID: b.id})
		}
	}
	switch b.kind {
	case model.PartText:
		if d.Text != "" {
			e.send(w, uiChunk{Type: "text-delta", ID: b.id, Delta: d.Text})
		}
	case model.PartReasoning:
		if d.Text != "" {
			e.send(w, uiChunk{Type: "reasoning-delta", ID: b.id, Delta: d.Text})
		}
	case model.PartToolCall:
		if d.ToolCall == nil {
			return
		}
		args := string(d.ToolCall.Arguments)
		if b.callID == "" {
			b.name = cmp.Or(d.ToolCall.Name, b.name)
			b.args += args
			if d.ToolCall.ID == "" {
				return
			}
			b.callID = d.ToolCall.ID
			e.send(w, uiChunk{Type: "tool-input-start", ToolCallID: b.callID, ToolName: b.name})
			args, b.args = b.args, ""
		}
		if args != "" {
			e.send(w, uiChunk{Type: "tool-input-delta", ToolCallID: b.callID, InputTextDelta: args})
		}
	}
}

// message completes the parts of the turn's message, sending those that
// were not streamed whole.
func (e *vercelEncoder) message(w *sseWriter, msg *model.Message) {
	for i, p := range msg.Parts {
		b := e.blocks[i]
		switch p.Type {
		case model.PartText, model.PartReasoning:
			if b != nil {
				e.endBlock(w, b)
				continue
			}
			if p.Text == "" || p.Redacted {
				continue
			}
			kind := string(p.Type)
			id := fmt.Sprintf("%s-%d-%d", p.Type, e.turn, i)
			e.send(w, uiChunk{Type: kind + "-start", ID: id})
			e.send(w, uiChunk{Type: kind + "-delta", ID: id, Delta: p.Text})
			e.send(w, uiChunk{Type: kind + "-end", ID: id})
			e.blocks[i] = &uiBlock{kind: p.Type, id: id, ended: true}
		case model.PartToolCall:
			if p.ToolCall != nil {
				e.toolInput(w, p.ToolCall, false)
			}
		case model.PartImage, model.PartDocument, model.PartAudio, model.PartVideo:
			e.file(w, &p)
		}
	}
}

// file sends a media part as a file part, unless it is a provider file ID,
// which the client cannot fetch.
func (e *vercelEncoder) file(w *sseWriter, p *model.Part) {
	url, err := partURL(p)
	if err != nil {
		e.fail(w, fmt.Errorf("encode media part: %w", err))
		return
	}
	if url != "" {
		e.send(w, uiChunk{Type: "file", URL: url, MediaType: cmp.Or(p.MediaType, string(p.Type)+"/*")})
	}
}

func (e *vercelEncoder) toolInput(w *sseWriter, call *model.ToolCall, dynamic bool) {
	e.inputs[call.ID] = true
	e.send(w, uiChunk{Type: "tool-input-available", ToolCallID: call.ID, ToolName: call.Name, Input: jsonInput(call.Arguments), Dynamic: dynamic})
}

func (e *vercelEncoder) toolResult(w *sseWriter, r *model.ToolResult) {
	switch {
	case e.denied[r.CallID]:
		e.send(w, uiChunk{Type: "tool-output-denied", ToolCallID: r.CallID})
	case r.IsError:
		e.send(w, uiChunk{Type: "tool-output-error", ToolCallID: r.CallID, ErrorText: cmp.Or(r.Content, "error")})
	default:
		e.send(w, uiChunk{Type: "tool-output-available", ToolCallID: r.CallID, Output: r.Content})
	}
	// The media a tool returned follow its output as file parts.
	for i := range r.Parts {
		if r.Parts[i].IsMedia() {
			e.file(w, &r.Parts[i])
		}
	}
}

// approvals requests the approvals of an interrupt. Calls of nested runs,
// which the client has not seen, are sent as dynamic tool calls first.
func (e *vercelEncoder) approvals(w *sseWriter, intr *loop.Interrupt) {
	for _, req := range intr.Requests {
		if req.Call == nil {
			continue
		}
		if !e.inputs[req.Call.ID] {
			e.toolInput(w, req.Call, true)
		}
		e.send(w, uiChunk{Type: "tool-approval-request", ApprovalID: req.Call.ID, ToolCallID: req.Call.ID, Reason: req.Reason})
	}
}

func (e *vercelEncoder) fail(w *sseWriter, err error) {
	e.closeStep(w)
	e.failed = true
	e.send(w, uiChunk{Type: "error", ErrorText: err.Error()})
}

func (e *vercelEncoder) end(w *sseWriter) {
	e.closeStep(w)
	c := uiChunk{Type: "finish", FinishReason: e.finishReason()}
	if res := e.result; res != nil {
		c.MessageMetadata = map[string]any{"agentx": vercelMetadata{RunID: res.RunID, Status: res.Status, Reason: res.Reason, Usage: res.Usage,
			Agent: res.Agent, Guardrail: res.Guardrail}}
	}
	e.send(w, c)
	w.eventRaw("", "", "[DONE]")
}

// vercelMetadata is the message metadata of the finish chunk.
type vercelMetadata struct {
	RunID  string      `json:"run_id"`
	Status loop.Status `json:"status"`
	Reason string      `json:"reason,omitempty"`
	Usage  model.Usage `json:"usage"`
	// Agent names the agent that ran last, after handoffs; Guardrail the
	// guardrail that blocked the run.
	Agent     string          `json:"agent,omitempty"`
	Guardrail *loop.Guardrail `json:"guardrail,omitempty"`
}

// finishReason maps the run's end to a UI message stream finish reason.
func (e *vercelEncoder) finishReason() string {
	res := e.result
	switch {
	case e.failed || res == nil || res.Status == loop.StatusFailed:
		return "error"
	case res.Status == loop.StatusInterrupted:
		return "tool-calls"
	case res.Status == loop.StatusBlocked && res.Guardrail != nil:
		// A guardrail withheld the answer, like a content filter.
		return "content-filter"
	case res.Status == loop.StatusCompleted || res.Status == loop.StatusIncomplete:
		switch res.FinishReason {
		case model.FinishLength:
			return "length"
		case model.FinishContentFilter:
			return "content-filter"
		case model.FinishToolCalls:
			return "tool-calls"
		case model.FinishStop, "":
			return "stop"
		}
	case res.Status == loop.StatusMaxTurns || res.Status == loop.StatusLimitExceeded:
		return "length"
	}
	return "other"
}

// jsonInput returns tool call arguments as a JSON value: {} when empty and
// a string when they are not valid JSON.
func jsonInput(args json.RawMessage) json.RawMessage {
	switch {
	case len(args) == 0:
		return json.RawMessage("{}")
	case json.Valid(args):
		return args
	}
	b, _ := json.Marshal(string(args))
	return b
}

// partURL returns the URL of a media part, or a data URL of its data.
func partURL(p *model.Part) (string, error) {
	if p.URL != "" {
		return p.URL, nil
	}
	if len(p.Data) > 0 {
		return dataurl.Encode(cmp.Or(p.MediaType, "application/octet-stream"), p.Data)
	}
	return "", nil
}

// chatRequest is the body useChat's DefaultChatTransport posts.
type chatRequest struct {
	ID        string      `json:"id"`
	Messages  []uiMessage `json:"messages"`
	Trigger   string      `json:"trigger"`
	MessageID string      `json:"messageId"`
}

type uiMessage struct {
	ID    string   `json:"id"`
	Role  string   `json:"role"`
	Parts []uiPart `json:"parts"`
}

type uiPart struct {
	Type       string      `json:"type"`
	Text       string      `json:"text"`
	MediaType  string      `json:"mediaType"`
	URL        string      `json:"url"`
	Filename   string      `json:"filename"`
	ToolCallID string      `json:"toolCallId"`
	State      string      `json:"state"`
	Approval   *uiApproval `json:"approval"`
}

type uiApproval struct {
	ID       string `json:"id"`
	Approved *bool  `json:"approved"`
	Reason   string `json:"reason"`
}

// chat serves useChat: the chat ID is the session ID, and only the last
// message is used, since the session holds the conversation. A user
// message starts a run; an assistant message whose tool parts hold
// approval responses resumes the interrupted run.
func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if e := s.decode(w, r, &req, false, false); e != nil {
		writeError(w, e)
		return
	}
	if e := s.authorize(r, req.ID); e != nil {
		writeError(w, e)
		return
	}
	if req.Trigger != "" && req.Trigger != "submit-message" {
		writeError(w, errorf(http.StatusBadRequest, CodeBadRequest, "trigger %q is not supported", req.Trigger))
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, errorf(http.StatusBadRequest, CodeBadRequest, "messages is empty"))
		return
	}
	last := req.Messages[len(req.Messages)-1]
	id := req.ID
	switch last.Role {
	case "user":
		msg, e := uiUserMessage(last)
		if e != nil {
			writeError(w, e)
			return
		}
		opts, _ := s.runOptions(r, 0, nil)
		if !s.acquire(id) {
			writeError(w, errBusy(id))
			return
		}
		if e := s.checkRun(r.Context(), id); e != nil {
			s.release(id)
			writeErrorBody(w, e.err, e.pending)
			return
		}
		s.serveStream(w, r, id, s.runner.Run(r.Context(), id, msg, opts...), newVercelEncoder("", nil))
	case "assistant":
		approvals := uiApprovals(last)
		if len(approvals) == 0 {
			writeError(w, errorf(http.StatusBadRequest, CodeBadRequest, "the last message is an assistant message without approval responses"))
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
		s.serveStream(w, r, id, s.runner.Resume(r.Context(), id, approvals), newVercelEncoder(last.ID, denials(approvals)))
	default:
		writeError(w, errorf(http.StatusBadRequest, CodeBadRequest, "unsupported role %q of the last message", last.Role))
	}
}

// chatReconnect answers useChat's stream resumption: the server does not
// buffer streams, so there is never one to resume.
func (s *Server) chatReconnect(w http.ResponseWriter, r *http.Request) {
	if e := s.authorize(r, r.PathValue("id")); e != nil {
		writeError(w, e)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// uiUserMessage converts a user UI message.
func uiUserMessage(m uiMessage) (*model.Message, *Error) {
	msg := &model.Message{Role: model.RoleUser}
	for _, p := range m.Parts {
		switch p.Type {
		case "text":
			msg.Parts = append(msg.Parts, model.TextPart(p.Text))
		case "file":
			part, e := filePart(p.MediaType, nil, p.URL, p.Filename)
			if e != nil {
				return nil, e
			}
			msg.Parts = append(msg.Parts, part)
		}
	}
	if len(msg.Parts) == 0 {
		return nil, errorf(http.StatusBadRequest, CodeBadRequest, "the message has no text or file parts")
	}
	return msg, nil
}

// uiApprovals returns the approval responses of an assistant UI message.
// The server issues approval IDs equal to tool call IDs.
func uiApprovals(m uiMessage) map[string]loop.Approval {
	out := map[string]loop.Approval{}
	for _, p := range m.Parts {
		if p.State != "approval-responded" || p.Approval == nil || p.Approval.Approved == nil {
			continue
		}
		id := cmp.Or(p.ToolCallID, p.Approval.ID)
		a := loop.Approval{Approved: *p.Approval.Approved, Reason: p.Approval.Reason}
		if !a.Approved && a.Reason == "" {
			a.Reason = loop.Deny.Reason
		}
		out[id] = a
	}
	return out
}
