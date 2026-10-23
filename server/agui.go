package server

import (
	"cmp"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"uuid"

	"github.com/nostosx/kitx/dataurl"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
)

// This file implements AG-UI 1.0 events over Server-Sent Events:
// https://docs.ag-ui.com/concepts/events and
// https://docs.ag-ui.com/concepts/interrupts

// aguiEvent is an AG-UI event. Only the fields of its type are set.
type aguiEvent struct {
	Type                string       `json:"type"`
	SubagentRunID       string       `json:"subagentRunId,omitempty"`
	ThreadID            string       `json:"threadId,omitempty"`
	RunID               string       `json:"runId,omitempty"`
	StepName            string       `json:"stepName,omitempty"`
	MessageID           string       `json:"messageId,omitempty"`
	Role                string       `json:"role,omitempty"`
	Delta               string       `json:"delta,omitempty"`
	ToolCallID          string       `json:"toolCallId,omitempty"`
	ToolCallName        string       `json:"toolCallName,omitempty"`
	Content             *string      `json:"content,omitempty"`
	Name                string       `json:"name,omitempty"`
	ParentSubagentRunID string       `json:"parentSubagentRunId,omitempty"`
	ParentToolCallID    string       `json:"parentToolCallId,omitempty"`
	Result              any          `json:"result,omitempty"`
	Outcome             *aguiOutcome `json:"outcome,omitempty"`
	Message             string       `json:"message,omitempty"`
	Code                string       `json:"code,omitempty"`
	Usage               []aguiUsage  `json:"usage,omitempty"`
	ProtocolVersion     string       `json:"protocolVersion,omitempty"`
	// Value is the payload of a CUSTOM event.
	Value any `json:"value,omitempty"`
}

type aguiOutcome struct {
	Type         string          `json:"type"`
	Interrupts   []aguiInterrupt `json:"interrupts,omitempty"`
	InterruptIDs []string        `json:"interruptIds,omitempty"`
}

type aguiInterrupt struct {
	ID             string         `json:"id"`
	Reason         string         `json:"reason"`
	Message        string         `json:"message,omitempty"`
	ToolCallID     string         `json:"toolCallId,omitempty"`
	SubagentRunID  string         `json:"subagentRunId,omitempty"`
	ResponseSchema map[string]any `json:"responseSchema,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
}

type aguiUsage struct {
	InputTokens           int `json:"inputTokens,omitempty"`
	OutputTokens          int `json:"outputTokens,omitempty"`
	TotalTokens           int `json:"totalTokens,omitempty"`
	ReasoningTokens       int `json:"reasoningTokens,omitempty"`
	CachedInputTokens     int `json:"cachedInputTokens,omitempty"`
	CacheWriteInputTokens int `json:"cacheWriteInputTokens,omitempty"`
}

// approvalSchema is the response schema of tool call interrupts: the
// payload of a resume entry.
var approvalSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"approved":   map[string]any{"type": "boolean"},
		"reason":     map[string]any{"type": "string"},
		"editedArgs": map[string]any{"type": "object"},
	},
	"required": []string{"approved"},
}

// aguiRun is the state of a run in an AG-UI stream: the top-level run or
// a nested run, reported as a subagent.
type aguiRun struct {
	// sub is the subagent run ID, empty for the top-level run.
	sub    string
	turn   int
	step   string
	blocks map[int]*uiBlock
}

// aguiEncoder encodes runtime events as AG-UI events.
type aguiEncoder struct {
	threadID, runID string
	started         bool
	finished        bool
	runs            map[string]*aguiRun
	// owner maps tool call IDs to the subagent run ID that made them.
	owner map[string]string
}

func newAGUIEncoder(threadID, runID string) *aguiEncoder {
	return &aguiEncoder{threadID: threadID, runID: runID, runs: map[string]*aguiRun{}, owner: map[string]string{}}
}

func (*aguiEncoder) header(h http.Header)   { h.Set("Content-Type", "text/event-stream") }
func (*aguiEncoder) heartbeat(w *sseWriter) { w.comment("ping") }
func (*aguiEncoder) inStream() bool         { return true }

func (e *aguiEncoder) send(w *sseWriter, ev aguiEvent) { w.event("", "", ev) }

func (e *aguiEncoder) run(ev *loop.Event) *aguiRun {
	r := e.runs[ev.RunID]
	if r == nil {
		r = &aguiRun{blocks: map[int]*uiBlock{}}
		if ev.Depth > 0 {
			r.sub = ev.RunID
		}
		e.runs[ev.RunID] = r
	}
	return r
}

func (e *aguiEncoder) event(w *sseWriter, ev *loop.Event) {
	if e.finished {
		return
	}
	if ev.Depth == 0 {
		e.start(w, ev)
	}
	r := e.run(ev)
	switch ev.Type {
	case loop.EventRunStart:
		if ev.Depth > 0 {
			e.send(w, aguiEvent{Type: "SUBAGENT_STARTED", SubagentRunID: r.sub, Name: ev.Agent,
				ParentToolCallID: ev.ParentCallID, ParentSubagentRunID: e.owner[ev.ParentCallID]})
		}
	case loop.EventTurnStart:
		e.closeStep(w, r)
		r.turn = ev.Turn
		r.step = fmt.Sprintf("turn %d", ev.Turn)
		e.send(w, aguiEvent{Type: "STEP_STARTED", SubagentRunID: r.sub, StepName: r.step})
	case loop.EventDelta:
		if ev.Delta != nil && ev.Delta.Delta != nil {
			e.delta(w, r, ev.RunID, ev.Delta.Index, ev.Delta.Delta)
		}
	case loop.EventMessage:
		if ev.Message != nil {
			e.message(w, r, ev.RunID, ev.Message)
		}
	case loop.EventHandoff:
		if ev.Handoff != nil {
			e.send(w, aguiEvent{Type: "CUSTOM", SubagentRunID: r.sub, Name: "handoff", Value: ev.Handoff})
		}
	case loop.EventOutputRetry:
		if ev.OutputRetry != nil {
			e.send(w, aguiEvent{Type: "CUSTOM", SubagentRunID: r.sub, Name: "output_retry", Value: ev.OutputRetry})
		}
	case loop.EventToolResult:
		if res := ev.ToolResult; res != nil {
			content := res.Content
			e.send(w, aguiEvent{Type: "TOOL_CALL_RESULT", SubagentRunID: r.sub, MessageID: "result-" + res.CallID,
				ToolCallID: res.CallID, Content: &content, Role: "tool"})
		}
	case loop.EventRunEnd:
		e.closeStep(w, r)
		if ev.Result != nil {
			e.endRun(w, r, ev.Result)
		}
	}
}

// start sends RUN_STARTED before the first event of the top-level run.
func (e *aguiEncoder) start(w *sseWriter, ev *loop.Event) {
	if e.started {
		return
	}
	e.started = true
	e.threadID = cmp.Or(e.threadID, ev.SessionID)
	e.runID = cmp.Or(e.runID, ev.RunID)
	e.send(w, aguiEvent{Type: "RUN_STARTED", ThreadID: e.threadID, RunID: e.runID, ProtocolVersion: "1.0"})
}

func (e *aguiEncoder) closeStep(w *sseWriter, r *aguiRun) {
	for _, idx := range slices.Sorted(maps.Keys(r.blocks)) {
		e.endBlock(w, r, r.blocks[idx])
	}
	r.blocks = map[int]*uiBlock{}
	if r.step != "" {
		e.send(w, aguiEvent{Type: "STEP_FINISHED", SubagentRunID: r.sub, StepName: r.step})
		r.step = ""
	}
}

func (e *aguiEncoder) endBlock(w *sseWriter, r *aguiRun, b *uiBlock) {
	if b.ended {
		return
	}
	b.ended = true
	switch b.kind {
	case model.PartText:
		e.send(w, aguiEvent{Type: "TEXT_MESSAGE_END", SubagentRunID: r.sub, MessageID: b.id})
	case model.PartReasoning:
		e.send(w, aguiEvent{Type: "REASONING_MESSAGE_END", SubagentRunID: r.sub, MessageID: b.id})
		e.send(w, aguiEvent{Type: "REASONING_END", SubagentRunID: r.sub, MessageID: b.id})
	case model.PartToolCall:
		if b.callID != "" {
			e.send(w, aguiEvent{Type: "TOOL_CALL_END", SubagentRunID: r.sub, ToolCallID: b.callID})
		}
	}
}

func (e *aguiEncoder) openBlock(w *sseWriter, r *aguiRun, runID string, idx int, kind model.PartType) *uiBlock {
	b := &uiBlock{kind: kind, id: fmt.Sprintf("%s-%d-%d", runID, r.turn, idx)}
	r.blocks[idx] = b
	switch kind {
	case model.PartText:
		e.send(w, aguiEvent{Type: "TEXT_MESSAGE_START", SubagentRunID: r.sub, MessageID: b.id, Role: "assistant"})
	case model.PartReasoning:
		e.send(w, aguiEvent{Type: "REASONING_START", SubagentRunID: r.sub, MessageID: b.id})
		e.send(w, aguiEvent{Type: "REASONING_MESSAGE_START", SubagentRunID: r.sub, MessageID: b.id, Role: "reasoning"})
	}
	return b
}

func (e *aguiEncoder) delta(w *sseWriter, r *aguiRun, runID string, idx int, d *model.Part) {
	b := r.blocks[idx]
	if b == nil {
		if d.Type != model.PartText && d.Type != model.PartReasoning && d.Type != model.PartToolCall {
			return
		}
		b = e.openBlock(w, r, runID, idx, d.Type)
	}
	switch b.kind {
	case model.PartText:
		if d.Text != "" {
			e.send(w, aguiEvent{Type: "TEXT_MESSAGE_CONTENT", SubagentRunID: r.sub, MessageID: b.id, Delta: d.Text})
		}
	case model.PartReasoning:
		if d.Text != "" {
			e.send(w, aguiEvent{Type: "REASONING_MESSAGE_CONTENT", SubagentRunID: r.sub, MessageID: b.id, Delta: d.Text})
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
			e.owner[b.callID] = r.sub
			e.send(w, aguiEvent{Type: "TOOL_CALL_START", SubagentRunID: r.sub, ToolCallID: b.callID, ToolCallName: b.name})
			args, b.args = b.args, ""
		}
		if args != "" {
			e.send(w, aguiEvent{Type: "TOOL_CALL_ARGS", SubagentRunID: r.sub, ToolCallID: b.callID, Delta: args})
		}
	}
}

func (e *aguiEncoder) message(w *sseWriter, r *aguiRun, runID string, msg *model.Message) {
	for i, p := range msg.Parts {
		if b := r.blocks[i]; b != nil {
			e.endBlock(w, r, b)
			continue
		}
		switch p.Type {
		case model.PartText, model.PartReasoning:
			if p.Text == "" || p.Redacted {
				continue
			}
			b := e.openBlock(w, r, runID, i, p.Type)
			typ := "TEXT_MESSAGE_CONTENT"
			if p.Type == model.PartReasoning {
				typ = "REASONING_MESSAGE_CONTENT"
			}
			e.send(w, aguiEvent{Type: typ, SubagentRunID: r.sub, MessageID: b.id, Delta: p.Text})
			e.endBlock(w, r, b)
		case model.PartToolCall:
			c := p.ToolCall
			if c == nil {
				continue
			}
			e.owner[c.ID] = r.sub
			e.send(w, aguiEvent{Type: "TOOL_CALL_START", SubagentRunID: r.sub, ToolCallID: c.ID, ToolCallName: c.Name})
			if len(c.Arguments) > 0 {
				e.send(w, aguiEvent{Type: "TOOL_CALL_ARGS", SubagentRunID: r.sub, ToolCallID: c.ID, Delta: string(c.Arguments)})
			}
			e.send(w, aguiEvent{Type: "TOOL_CALL_END", SubagentRunID: r.sub, ToolCallID: c.ID})
			r.blocks[i] = &uiBlock{kind: model.PartToolCall, callID: c.ID, ended: true}
		}
	}
}

// endRun reports the end of a run: RUN_FINISHED or RUN_ERROR for the
// top-level run, SUBAGENT_FINISHED or SUBAGENT_ERROR for a nested one.
func (e *aguiEncoder) endRun(w *sseWriter, r *aguiRun, res *loop.Result) {
	var result any
	if res.Output != "" {
		result = res.Output
	}
	ok := res.Status == loop.StatusCompleted || res.Status == loop.StatusInterrupted
	if r.sub != "" {
		switch {
		case res.Status == loop.StatusInterrupted:
			out := &aguiOutcome{Type: "suspended"}
			if res.Interrupt != nil {
				for _, req := range res.Interrupt.Requests {
					out.InterruptIDs = append(out.InterruptIDs, req.Call.ID)
				}
			}
			e.send(w, aguiEvent{Type: "SUBAGENT_FINISHED", SubagentRunID: r.sub, Outcome: out})
		case ok:
			e.send(w, aguiEvent{Type: "SUBAGENT_FINISHED", SubagentRunID: r.sub, Result: result, Outcome: &aguiOutcome{Type: "success"}})
		default:
			e.send(w, aguiEvent{Type: "SUBAGENT_ERROR", SubagentRunID: r.sub, Message: runMessage(res), Code: string(res.Status)})
		}
		return
	}
	e.finished = true
	usage := aguiUsageOf(res.Usage)
	if !ok {
		e.send(w, aguiEvent{Type: "RUN_ERROR", Message: runMessage(res), Code: string(res.Status), Usage: usage})
		return
	}
	out := &aguiOutcome{Type: "success"}
	if res.Status == loop.StatusInterrupted && res.Interrupt != nil {
		out = &aguiOutcome{Type: "interrupt"}
		for _, req := range res.Interrupt.Requests {
			out.Interrupts = append(out.Interrupts, e.interrupt(req))
		}
		result = nil
	}
	e.send(w, aguiEvent{Type: "RUN_FINISHED", ThreadID: e.threadID, RunID: e.runID, Result: result, Outcome: out, Usage: usage})
}

func (e *aguiEncoder) interrupt(req *loop.ApprovalRequest) aguiInterrupt {
	msg := req.Reason
	if msg == "" {
		msg = fmt.Sprintf("Approve the call of tool %s?", req.Call.Name)
	}
	return aguiInterrupt{
		ID:             req.Call.ID,
		Reason:         "tool_call",
		Message:        msg,
		ToolCallID:     req.Call.ID,
		SubagentRunID:  e.owner[req.Call.ID],
		ResponseSchema: approvalSchema,
		Metadata: map[string]any{"agentx": map[string]any{
			"tool": req.Call.Name, "arguments": jsonInput(req.Call.Arguments), "agent": req.Agent,
		}},
	}
}

func (e *aguiEncoder) fail(w *sseWriter, err error) {
	if e.finished {
		return
	}
	e.finished = true
	rec := newErrorRecord(err)
	e.send(w, aguiEvent{Type: "RUN_ERROR", Message: rec.Message, Code: rec.Code})
}

func (e *aguiEncoder) end(w *sseWriter) {
	if !e.finished {
		e.fail(w, errNoResult)
	}
}

func runMessage(res *loop.Result) string {
	switch {
	case res.Err != nil:
		return res.Err.Error()
	case res.Reason != "":
		return res.Reason
	}
	return "the run ended with status " + string(res.Status)
}

func aguiUsageOf(u model.Usage) []aguiUsage {
	if u == (model.Usage{}) {
		return nil
	}
	in := u.PromptTokens()
	return []aguiUsage{{
		InputTokens:           in,
		OutputTokens:          u.OutputTokens,
		TotalTokens:           in + u.OutputTokens,
		ReasoningTokens:       u.ReasoningTokens,
		CachedInputTokens:     u.CacheReadTokens,
		CacheWriteInputTokens: u.CacheWriteTokens,
	}}
}

// aguiInput is the RunAgentInput an AG-UI client posts.
type aguiInput struct {
	ThreadID string        `json:"threadId"`
	RunID    string        `json:"runId"`
	Messages []aguiMessage `json:"messages"`
	Resume   []aguiResume  `json:"resume"`
}

type aguiMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// aguiPart is a content part of a user message: text, or an image, audio,
// video or document whose source is data (base64), a URL or a file ID.
type aguiPart struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	Source *struct {
		Type     string `json:"type"`
		Value    string `json:"value"`
		MIMEType string `json:"mimeType"`
		Provider string `json:"provider"`
	} `json:"source"`
	Metadata map[string]any `json:"metadata"`
}

type aguiResume struct {
	InterruptID string `json:"interruptId"`
	Status      string `json:"status"`
	Payload     *struct {
		Approved   *bool           `json:"approved"`
		Reason     string          `json:"reason"`
		EditedArgs json.RawMessage `json:"editedArgs"`
	} `json:"payload"`
}

// agui serves an AG-UI agent: the thread ID is the session ID. A run
// without resume entries sends the last message, which must be a user
// message, since the session holds the conversation; resume entries
// answer the interrupt of the thread's run and resume it.
func (s *Server) agui(w http.ResponseWriter, r *http.Request) {
	var in aguiInput
	if e := s.decode(w, r, &in, false, false); e != nil {
		writeError(w, e)
		return
	}
	if e := s.authorize(r, in.ThreadID); e != nil {
		writeError(w, e)
		return
	}
	id := in.ThreadID
	enc := newAGUIEncoder(id, cmp.Or(in.RunID, "run_"+uuid.NewV7().String()))
	fail := func(code, format string, args ...any) {
		enc.header(w.Header())
		w.WriteHeader(http.StatusOK)
		sw := newSSEWriter(w)
		enc.send(sw, aguiEvent{Type: "RUN_ERROR", Message: fmt.Sprintf(format, args...), Code: code})
		_ = sw.flush()
	}
	if len(in.Resume) > 0 {
		approvals, err := aguiApprovals(in.Resume)
		if err != "" {
			fail("invalid_resume", "%s", err)
			return
		}
		if !s.acquire(id) {
			fail(CodeSessionBusy, "thread %s has a run in progress", id)
			return
		}
		if e := s.checkResume(r.Context(), id); e != nil {
			s.release(id)
			fail(e.Code, "%s", e.Message)
			return
		}
		if e := s.checkApprovals(r, id, approvals); e != "" {
			s.release(id)
			fail("invalid_resume", "%s", e)
			return
		}
		s.serveStream(w, r, id, s.runner.Resume(r.Context(), id, approvals), enc)
		return
	}
	if len(in.Messages) == 0 || in.Messages[len(in.Messages)-1].Role != "user" {
		fail(CodeBadRequest, "the last message must be a user message; frontend tools are not supported")
		return
	}
	msg, e := aguiUserMessage(in.Messages[len(in.Messages)-1])
	if e != nil {
		fail(e.Code, "%s", e.Message)
		return
	}
	opts, _ := s.runOptions(r, 0, nil)
	if !s.acquire(id) {
		fail(CodeSessionBusy, "thread %s has a run in progress", id)
		return
	}
	if e := s.checkRun(r.Context(), id); e != nil {
		s.release(id)
		fail("interrupt_pending", "%s", e.err.Message)
		return
	}
	s.serveStream(w, r, id, s.runner.Run(r.Context(), id, msg, opts...), enc)
}

// checkApprovals checks that resume entries answer every open interrupt
// of a session, when the server knows them.
func (s *Server) checkApprovals(r *http.Request, id string, approvals map[string]loop.Approval) string {
	sess, err := s.runner.Store().Get(r.Context(), id)
	if err != nil {
		return err.Error()
	}
	p := s.pendingRun(r.Context(), sess)
	if p == nil || p.Interrupt == nil {
		return ""
	}
	open := map[string]bool{}
	for _, req := range p.Interrupt.Requests {
		open[req.Call.ID] = true
		if _, ok := approvals[req.Call.ID]; !ok {
			return fmt.Sprintf("interrupt %s is not answered", req.Call.ID)
		}
	}
	for id := range approvals {
		if !open[id] {
			return fmt.Sprintf("interrupt %s is not open", id)
		}
	}
	return ""
}

// aguiApprovals converts resume entries to approvals.
func aguiApprovals(entries []aguiResume) (map[string]loop.Approval, string) {
	out := map[string]loop.Approval{}
	for _, en := range entries {
		if en.InterruptID == "" {
			return nil, "a resume entry has no interruptId"
		}
		switch en.Status {
		case "cancelled":
			out[en.InterruptID] = loop.Approval{Reason: "The user cancelled this tool call."}
		case "resolved":
			if en.Payload == nil || en.Payload.Approved == nil {
				return nil, fmt.Sprintf("the payload of interrupt %s must set approved", en.InterruptID)
			}
			a := loop.Approval{Approved: *en.Payload.Approved, Reason: en.Payload.Reason}
			if a.Approved && len(en.Payload.EditedArgs) > 0 && string(en.Payload.EditedArgs) != "null" {
				a.UpdatedInput = en.Payload.EditedArgs
			}
			if !a.Approved && a.Reason == "" {
				a.Reason = loop.Deny.Reason
			}
			out[en.InterruptID] = a
		default:
			return nil, fmt.Sprintf("invalid status %q of interrupt %s", en.Status, en.InterruptID)
		}
	}
	return out, ""
}

// aguiUserMessage converts an AG-UI user message.
func aguiUserMessage(m aguiMessage) (*model.Message, *Error) {
	var text string
	if json.Unmarshal(m.Content, &text) == nil {
		if text == "" {
			return nil, errorf(http.StatusBadRequest, CodeBadRequest, "the user message is empty")
		}
		return model.UserText(text), nil
	}
	var parts []aguiPart
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		return nil, errorf(http.StatusBadRequest, CodeBadRequest, "invalid user message content: %v", err)
	}
	msg := &model.Message{Role: model.RoleUser}
	for _, p := range parts {
		switch p.Type {
		case "text":
			msg.Parts = append(msg.Parts, model.TextPart(p.Text))
		case "image", "audio", "video", "document":
			part, e := aguiMediaPart(p)
			if e != nil {
				return nil, e
			}
			msg.Parts = append(msg.Parts, part)
		default:
			return nil, errorf(http.StatusUnsupportedMediaType, CodeUnsupportedMedia, "content part %q is not supported", p.Type)
		}
	}
	if len(msg.Parts) == 0 {
		return nil, errorf(http.StatusBadRequest, CodeBadRequest, "the user message is empty")
	}
	return msg, nil
}

// aguiMediaPart converts an image, audio, video or document part, whose
// type names the model part type. A "filename" in its metadata names the
// file.
func aguiMediaPart(p aguiPart) (model.Part, *Error) {
	src := p.Source
	if src == nil {
		return model.Part{}, errorf(http.StatusBadRequest, CodeBadRequest, "the %s part has no source", p.Type)
	}
	if src.MIMEType != "" && !accepted(src.MIMEType) {
		return model.Part{}, errUnsupportedMedia(src.MIMEType)
	}
	part := model.Part{Type: model.PartType(p.Type), MediaType: src.MIMEType}
	switch src.Type {
	case "data":
		b, err := base64.StdEncoding.DecodeString(src.Value)
		if err != nil {
			return model.Part{}, errorf(http.StatusBadRequest, CodeBadRequest, "invalid %s data: %v", p.Type, err)
		}
		part.Data = b
	case "url":
		part.URL = src.Value
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(src.Value)), "data:") {
			mt, b, err := dataurl.Decode(src.Value)
			if err != nil {
				return model.Part{}, errorf(http.StatusBadRequest, CodeBadRequest, "invalid data URL")
			}
			part.URL, part.Data, part.MediaType = "", b, cmp.Or(part.MediaType, mt)
		}
	case "file":
		part.FileID, part.Provider = src.Value, src.Provider
	default:
		return model.Part{}, errorf(http.StatusBadRequest, CodeBadRequest, "unknown source type %q of a %s part", src.Type, p.Type)
	}
	if part.MediaType != "" && !accepted(part.MediaType) {
		return model.Part{}, errUnsupportedMedia(part.MediaType)
	}
	if part.Data == nil && part.URL == "" && part.FileID == "" {
		return model.Part{}, errorf(http.StatusBadRequest, CodeBadRequest, "the %s part is empty", p.Type)
	}
	if name, ok := p.Metadata["filename"].(string); ok && part.Type != model.PartImage {
		part.Filename = name
	}
	return part, nil
}
