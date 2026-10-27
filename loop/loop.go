package loop

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/tool"
)

// execution holds the state of one Run or Resume call.
//
// A run ends exactly once, in end, which finish, fail, interrupt and
// abandon call, or without ending if it took no step; see abandon. No step
// runs after the run ended or after the consumer stopped iterating; see
// active.
type execution struct {
	r      *Runtime
	st     *State
	handle *Run
	yield  func(*Event, error) bool
	// stopped is set when the consumer stops iterating.
	stopped bool
	// began is set once the run takes its first step: BeforeRun for a new
	// run, the pending work for a resumed one.
	began bool
	// ended is set once end has run.
	ended bool
	// halt is set when middleware asks to stop after the current step.
	halt       bool
	haltReason string
	// output, if set, replaces the text of the final assistant message as
	// the run's output, as when the run stops at a tool.
	output *string
	// guardrail is set when a guardrail's tripwire ended the run.
	guardrail *Guardrail
	// holding is set while the deltas of a model response are held, and
	// held holds the events of a response not sent yet; see AnswerHolder.
	holding bool
	held    []*Event
	// acceptedAnswer is the answer every BeforeFinisher accepted. When
	// holding answers, only this answer may become Result.Output.
	acceptedAnswer *model.Message
	// relay is set while the calls of a batch run concurrently; their
	// events go through it to the goroutine iterating the run. See
	// runConcurrently.
	relay chan *relayed
	// caller is the tool call that started the run, if a tool started it,
	// as for a sub-agent; see Run.Caller.
	caller *callScope
}

func (r *Runtime) newExecution(st *State, yield func(*Event, error) bool) *execution {
	l := &execution{r: r, st: st, yield: yield}
	l.handle = &Run{l: l}
	return l
}

// wrapContext applies the ContextWrapper middleware, the first outermost.
// The returned function releases them in reverse order.
func (l *execution) wrapContext(ctx context.Context) (context.Context, func()) {
	var releases []func()
	for _, w := range l.r.stages.wrapContext {
		var release func()
		ctx, release = w.WrapContext(ctx, l.handle)
		if release != nil {
			releases = append(releases, release)
		}
	}
	return ctx, func() {
		for _, release := range slices.Backward(releases) {
			release()
		}
	}
}

func (l *execution) emit(ev *Event) bool {
	if l.relay != nil {
		return l.send(ev, true)
	}
	return l.deliver(ev, true)
}

// deliver yields an event to the consumer, stamped with the run's identity
// unless it was forwarded from a nested run.
func (l *execution) deliver(ev *Event, stamp bool) bool {
	if l.stopped {
		return false
	}
	if stamp {
		ev.RunID, ev.SessionID, ev.Agent, ev.Turn = l.st.RunID, l.st.SessionID, l.st.Agent, l.st.Turn
	}
	if !l.yield(ev, nil) {
		l.stopped = true
		return false
	}
	return true
}

// active reports whether the run may take its next step.
func (l *execution) active() bool { return !l.ended && !l.stopped }

func (l *execution) haltText() string { return cmp.Or(l.haltReason, "halted by middleware") }

// check ends the run if a middleware returned an error. It reports whether
// the run may continue.
func (l *execution) check(ctx context.Context, err error) bool {
	if err == nil {
		return true
	}
	if se, ok := errors.AsType[*stopError](err); ok {
		l.finish(ctx, se.status, se.reason)
	} else if ge, ok := errors.AsType[*GuardrailError](err); ok {
		l.st.Usage.Add(ge.Usage)
		g := ge.Guardrail
		l.guardrail = &g
		l.finish(ctx, StatusBlocked, ge.Error())
	} else {
		l.fail(ctx, err)
	}
	return false
}

// start runs BeforeRun middleware and appends the input.
func (l *execution) start(ctx context.Context, req *Request) error {
	input := req.Input.Clone()
	if input.Role == "" {
		input.Role = model.RoleUser
	}
	in := &RunInput{
		NewSession: req.NewSession,
		Query:      req.Input.Text(),
		System:     strings.TrimSpace(l.r.cfg.Instructions),
		Message:    input,
	}
	l.began = true
	for _, m := range l.r.stages.beforeRun {
		if !l.check(ctx, m.BeforeRun(ctx, l.handle, in)) || l.stopped {
			return nil
		}
		if l.halt {
			l.finish(ctx, StatusStopped, l.haltText())
			return nil
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	l.st.System = in.System
	l.st.Messages = append(l.st.Messages, in.Message)
	l.r.checkpoint(ctx, l.st)
	return nil
}

// run drives turns until the run ends. A resumed run first finishes the
// turn it stopped in; approvals then holds the decisions for the interrupt
// it resumes, if any.
func (l *execution) run(ctx context.Context, resumed bool, approvals map[string]Approval) {
	if resumed && !l.resumeTurn(ctx, approvals) {
		return
	}
	for l.active() {
		l.turn(ctx)
	}
}

// resumeTurn finishes the turn a resumed run stopped in: its tool calls,
// or the model's final answer. It considers only the messages of this run,
// not the history before its input. It reports whether the run continues
// with the next turn.
func (l *execution) resumeTurn(ctx context.Context, approvals map[string]Approval) bool {
	st := l.st
	own := st.Messages[min(st.Start, len(st.Messages)):]
	switch calls := unansweredCalls(own); {
	case len(st.Pending) > 0:
		if !l.runTools(ctx, approvals) {
			return false
		}
	case len(calls) > 0:
		// The run stopped after the model's message, before its calls were
		// planned; plan them now. Approvals answer an interrupt, which these
		// calls have not raised.
		pending, err := l.plan(ctx, calls)
		if !l.check(ctx, err) || l.stopped {
			return false
		}
		st.Pending = pending
		if !l.runTools(ctx, nil) {
			return false
		}
	default:
		if n := len(own); n > 0 && own[n-1].Role == model.RoleAssistant {
			// The run stopped after the model's final answer, before it was
			// handled; handle it now instead of asking the model again.
			l.answer(ctx, &model.Response{Message: own[n-1], FinishReason: st.FinishReason})
			return l.active()
		}
		return true
	}
	if l.halt {
		l.finish(ctx, StatusStopped, l.haltText())
		return false
	}
	l.r.checkpoint(ctx, st)
	return true
}

func (l *execution) turn(ctx context.Context) {
	cfg := &l.r.cfg
	st := l.st
	if err := ctx.Err(); err != nil {
		l.fail(ctx, err)
		return
	}
	if maxTurns := cmp.Or(st.MaxTurns, cfg.MaxTurns); st.Turn >= maxTurns {
		l.finish(ctx, StatusMaxTurns, fmt.Sprintf("reached the limit of %d turns", maxTurns))
		return
	}
	if reason := l.modelLimit(); reason != "" {
		l.exceed(ctx, reason)
		return
	}
	// Events are held only until the response they belong to is handled;
	// whatever is left was not accepted and is dropped.
	l.drop()
	st.Turn++
	if !l.emit(&Event{Type: EventTurnStart}) {
		return
	}
	for _, m := range l.r.stages.beforeTurn {
		if !l.check(ctx, m.BeforeTurn(ctx, l.handle)) || l.stopped {
			return
		}
	}

	req := l.request()
	for _, m := range l.r.stages.beforeModel {
		if !l.check(ctx, m.BeforeModel(ctx, l.handle, req)) || l.stopped {
			return
		}
	}
	if l.halt {
		l.finish(ctx, StatusStopped, l.haltText())
		return
	}

	l.holding = l.r.stages.holdAnswer
	resp, err := l.call(ctx, req)
	l.holding = false
	if err != nil {
		if _, ok := errors.AsType[*GuardrailError](err); ok {
			// The response a guardrail cut short is not sent.
			l.held = nil
			l.check(ctx, err)
		} else if !errors.Is(err, errYieldStopped) {
			l.fail(ctx, err)
		}
		return
	}
	msg := resp.Message
	if msg == nil {
		msg = &model.Message{}
		resp.Message = msg
	}
	msg.Role = model.RoleAssistant
	st.FinishReason = resp.FinishReason
	resp.Usage.Requests = max(resp.Usage.Requests, 1)
	st.Usage.Add(resp.Usage)
	st.Messages = append(st.Messages, msg)
	msgEv := &Event{Type: EventMessage, Message: msg, Usage: resp.Usage}
	if l.r.stages.holdAnswer && len(msg.ToolCalls()) == 0 {
		l.held = append(l.held, msgEv)
	} else if !l.release() || !l.emit(msgEv) {
		return
	}
	for _, m := range slices.Backward(l.r.stages.afterModel) {
		if !l.check(ctx, m.AfterModel(ctx, l.handle, req, resp)) || l.stopped {
			return
		}
	}

	calls := msg.ToolCalls()
	if len(calls) == 0 {
		l.answer(ctx, resp)
		return
	}
	if !l.release() {
		return
	}
	if l.halt {
		l.finish(ctx, StatusStopped, l.haltText())
		return
	}
	pending, err := l.plan(ctx, calls)
	if !l.check(ctx, err) || l.stopped {
		return
	}
	st.Pending = pending
	if !l.runTools(ctx, nil) {
		return
	}
	if l.halt {
		l.finish(ctx, StatusStopped, l.haltText())
		return
	}
	l.r.checkpoint(ctx, st)
}

// request builds the model request of a turn. It copies the configured
// options, so middleware that modifies the request changes neither the
// configuration nor concurrent runs. The messages are shared with the run.
func (l *execution) request() *model.Request {
	cfg := &l.r.cfg
	req := &model.Request{
		System:          l.st.System,
		Messages:        slices.Clone(l.st.Messages),
		Tools:           cfg.Tools.Specs(),
		ToolChoice:      clonePtr(cfg.ToolChoice),
		MaxTokens:       cfg.MaxTokens,
		Temperature:     clonePtr(cfg.Temperature),
		Seed:            clonePtr(cfg.Seed),
		TopP:            clonePtr(cfg.TopP),
		Reasoning:       clonePtr(cfg.Reasoning),
		ResponseFormat:  clonePtr(cfg.ResponseFormat),
		ProviderOptions: model.CloneProviderOptions(cfg.ProviderOptions),
	}
	if req.ResponseFormat != nil {
		req.ResponseFormat.Schema = slices.Clone(req.ResponseFormat.Schema)
	}
	return req
}

// clonePtr returns a pointer to a shallow copy of *p, or nil.
func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	c := *p
	return &c
}

// answer handles a model reply without tool calls.
func (l *execution) answer(ctx context.Context, resp *model.Response) {
	if l.halt {
		l.drop()
		l.finish(ctx, StatusStopped, l.haltText())
		return
	}
	if resp.FinishReason == model.FinishPause {
		// The provider paused a long turn; continue it. The part written so
		// far stays in the conversation, but no BeforeFinisher checked it.
		l.drop()
		l.r.checkpoint(ctx, l.st)
		return
	}
	if !l.checkOutput(ctx, resp) {
		return
	}
	for _, m := range l.r.stages.beforeFinish {
		cont, err := m.BeforeFinish(ctx, l.handle, resp.Message)
		if ge, ok := errors.AsType[*GuardrailError](err); ok {
			l.withhold(resp.Message, ge.Guardrail.Name)
		}
		if !l.check(ctx, err) {
			return
		}
		if l.halt {
			l.drop()
			l.finish(ctx, StatusStopped, l.haltText())
			return
		}
		if cont {
			// The answer did not end the run, and the BeforeFinishers after
			// this one, such as output guardrails, never saw it.
			l.drop()
			l.r.checkpoint(ctx, l.st)
			return
		}
		if l.stopped {
			return
		}
	}
	l.acceptedAnswer = resp.Message
	if !l.release() {
		return
	}
	switch resp.FinishReason {
	case model.FinishLength:
		l.finish(ctx, StatusIncomplete, "the answer was cut off by the output token limit")
	case model.FinishContentFilter:
		l.finish(ctx, StatusIncomplete, "the answer was withheld by a content filter")
	default:
		l.finish(ctx, StatusCompleted, "")
	}
}

// call invokes the model through the ModelWrapper middleware, streaming
// when configured.
func (l *execution) call(ctx context.Context, req *model.Request) (*model.Response, error) {
	m := l.r.cfg.Model
	for _, w := range slices.Backward(l.r.stages.wrapModel) {
		m = w.WrapModel(l.handle, m)
	}
	if !l.r.cfg.Stream {
		resp, err := m.Generate(ctx, req)
		if err == nil && resp == nil {
			err = errors.New("loop: model returned no response")
		}
		return resp, err
	}
	var acc model.Accumulator
	var final *model.Response
	for ev, err := range m.Stream(ctx, req) {
		if err != nil {
			return nil, err
		}
		if ev == nil {
			continue
		}
		switch ev.Type {
		case model.StreamDelta:
			acc.Add(ev)
			dev := &Event{Type: EventDelta, Delta: ev}
			if l.holding && (ev.Delta == nil || ev.Delta.Type != model.PartToolCall) {
				l.held = append(l.held, dev)
				continue
			}
			// A response that calls tools streams as usual.
			l.holding = false
			if !l.release() || !l.emit(dev) {
				return nil, errYieldStopped
			}
		case model.StreamDone:
			final = ev.Response
		}
	}
	if final == nil {
		return nil, errors.New("loop: stream ended without a done event")
	}
	if final.Message == nil {
		final.Message = acc.Message()
	}
	return final, nil
}

// release sends the held events of a response; see AnswerHolder. It
// reports false if the consumer stopped iterating.
func (l *execution) release() bool {
	held := l.held
	l.held = nil
	for _, ev := range held {
		if !l.emit(ev) {
			return false
		}
	}
	return true
}

// drop discards the held events of a response that was not accepted as
// the run's answer, so no BeforeFinisher checked it: the consumer must
// not see it; see AnswerHolder. The response stays in the conversation.
func (l *execution) drop() { l.held = nil }

// withhold replaces an answer that a guardrail withheld in the
// conversation, and drops its held events.
func (l *execution) withhold(msg *model.Message, guardrail string) {
	l.drop()
	for i, m := range slices.Backward(l.st.Messages) {
		if m == msg {
			l.st.Messages[i] = WithheldAnswer(guardrail)
			return
		}
	}
}

// toolChain returns the ToolWrapper middleware as tool middleware.
func (l *execution) toolChain() []tool.Middleware {
	ws := l.r.stages.wrapTool
	if len(ws) == 0 {
		return nil
	}
	out := make([]tool.Middleware, len(ws))
	for i, w := range ws {
		out[i] = func(next tool.Invoker) tool.Invoker { return w.WrapTool(l.handle, next) }
	}
	return out
}

// Results given to tool calls that a run ends without running.
const (
	stoppedResult = "The run was stopped before this tool call ran."
	failedResult  = "The run failed before this tool call ran."
)

// closeCalls answers the tool calls that will not run when the run ends,
// keeping the results of calls that finished, so the conversation stays
// valid for providers that require a result for every call.
func (l *execution) closeCalls(content string) {
	st := l.st
	if len(st.Pending) > 0 {
		results := make([]*model.ToolResult, len(st.Pending))
		for i, p := range st.Pending {
			results[i] = p.Result
			if results[i] == nil {
				results[i] = &model.ToolResult{CallID: p.Call.ID, Name: p.Call.Name, Content: content, IsError: true}
			}
		}
		st.Pending = nil
		st.Messages = append(st.Messages, model.ToolResults(results...))
	}
	if msg := CloseToolCalls(st.Messages, content); msg != nil {
		st.Messages = append(st.Messages, msg)
	}
}

// CloseToolCalls returns a message that answers, with an error result
// holding content, the tool calls of the last assistant message in msgs
// that have no result, or nil if all are answered. Append it to end a
// conversation without leaving calls unanswered, which providers reject.
func CloseToolCalls(msgs []*model.Message, content string) *model.Message {
	var results []*model.ToolResult
	for _, c := range unansweredCalls(msgs) {
		results = append(results, &model.ToolResult{CallID: c.ID, Name: c.Name, Content: content, IsError: true})
	}
	if len(results) == 0 {
		return nil
	}
	return model.ToolResults(results...)
}

// unansweredCalls returns the tool calls of the last assistant message in
// msgs that have no result.
func unansweredCalls(msgs []*model.Message) []*model.ToolCall {
	last := -1
	for i, msg := range slices.Backward(msgs) {
		if msg != nil && msg.Role == model.RoleAssistant {
			last = i
			break
		}
	}
	if last < 0 {
		return nil
	}
	answered := map[string]bool{}
	for _, m := range msgs[last+1:] {
		for _, r := range m.ToolResults() {
			answered[r.CallID] = true
		}
	}
	var calls []*model.ToolCall
	for _, c := range msgs[last].ToolCalls() {
		if !answered[c.ID] {
			calls = append(calls, c)
		}
	}
	return calls
}

// Reasons of a run whose consumer stopped iterating.
const (
	abandonedReason        = "the consumer stopped iterating; the run can be resumed"
	abandonedUnsavedReason = "the consumer stopped iterating; the run has no checkpointer, so it cannot be resumed"
	abandonedEarlyReason   = "the consumer stopped iterating before the run's input was added; the run cannot be resumed"
)

// abandon ends a run whose consumer stopped iterating; it does nothing if
// the run ended or the consumer did not stop. The state stays running and
// is checkpointed, so the run can be resumed from where it stopped.
//
// A run that took no step, because the consumer stopped at its
// EventRunStart, does not end: AfterRun does not run, as BeforeRun did not,
// and the checkpoint of a resumed run is left as it was. A new run whose
// input was not added is not checkpointed, as there is nothing to resume.
func (l *execution) abandon(ctx context.Context) {
	if l.ended || !l.stopped {
		return
	}
	if !l.began {
		l.ended = true
		return
	}
	switch {
	case len(l.st.Messages) <= l.st.Start:
		l.st.Reason = abandonedEarlyReason
	case l.r.cfg.Checkpointer == nil:
		l.st.Reason = abandonedUnsavedReason
	default:
		l.r.checkpoint(ctx, l.st)
		l.st.Reason = abandonedReason
	}
	l.end(ctx, nil, nil)
}

// finish ends the run with a final status, answering the calls that did not
// run, and deletes its checkpoint.
func (l *execution) finish(ctx context.Context, status Status, reason string) {
	l.closeCalls(stoppedResult)
	l.st.Status = status
	l.st.Reason = reason
	l.r.deleteCheckpoint(ctx, l.st.RunID)
	l.end(ctx, nil, nil)
}

// fail ends the run as failed. The checkpoint is kept, so the run can be
// resumed, unless the run failed before its input was added: there is
// nothing to resume then, so the checkpoint is deleted.
func (l *execution) fail(ctx context.Context, err error) {
	l.closeCalls(failedResult)
	l.st.Status = StatusFailed
	l.st.Reason = err.Error()
	if len(l.st.Messages) > l.st.Start {
		l.r.checkpoint(ctx, l.st)
	} else {
		l.r.deleteCheckpoint(ctx, l.st.RunID)
	}
	l.end(ctx, nil, err)
}

// interrupt pauses the run for approval of requests.
func (l *execution) interrupt(ctx context.Context, requests []*ApprovalRequest) {
	st := l.st
	st.Status = StatusInterrupted
	st.Reason = "awaiting tool approval"
	l.r.checkpoint(ctx, st)
	intr := &Interrupt{RunID: st.RunID, Requests: requests, State: st.Clone()}
	l.emit(&Event{Type: EventInterrupted, Interrupt: intr})
	l.end(ctx, intr, nil)
}

// end runs AfterRun middleware, emits the final event and, for failures,
// the error. AfterRun runs even if the consumer stopped iterating, when the
// events are no longer delivered.
func (l *execution) end(ctx context.Context, intr *Interrupt, err error) {
	if l.ended {
		return
	}
	// A run that ends with events still held ends before its answer was
	// accepted.
	l.drop()
	l.ended = true
	st := l.st
	res := &Result{
		RunID:        st.RunID,
		Status:       st.Status,
		Reason:       st.Reason,
		FinishReason: st.FinishReason,
		Messages:     st.Messages,
		NewMessages:  st.NewMessages(),
		Usage:        st.Usage,
		Turns:        st.Turn,
		Interrupt:    intr,
		Err:          err,
		Values:       maps.Clone(st.Values),
		Agent:        st.Agent,
		Guardrail:    l.guardrail,
	}
	for i := len(st.Messages) - 1; i >= st.Start && i >= 0; i-- {
		if msg := st.Messages[i]; msg.Role == model.RoleAssistant {
			if !l.r.stages.holdAnswer || msg == l.acceptedAnswer || len(msg.ToolCalls()) > 0 {
				res.Output = msg.Text()
			}
			break
		}
	}
	if l.output != nil {
		res.Output = *l.output
	}
	if l.guardrail != nil {
		// The answer may be what the guardrail withheld.
		res.Output = ""
	}
	// AfterRun observes the run even when it was cancelled.
	actx := context.WithoutCancel(ctx)
	for _, m := range slices.Backward(l.r.stages.afterRun) {
		m.AfterRun(actx, l.handle, res)
	}
	if !l.emit(&Event{Type: EventRunEnd, Result: res}) {
		return
	}
	if err != nil {
		l.yield(nil, err)
	}
}
