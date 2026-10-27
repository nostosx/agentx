package loop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/tool"
)

// Middleware extends the agent loop. Every middleware implements Name and
// any of the stage interfaces below; the runtime calls only the stages a
// middleware implements.
//
// Before stages run in registration order, After stages in reverse order,
// and for Wrap stages the first middleware is outermost.
//
// An error returned from a stage ends the run: Block and Stop end it with
// StatusBlocked and StatusStopped, any other error fails it. Middleware that
// should not stop the run on failure must handle its errors itself.
type Middleware interface {
	// Name identifies the middleware. Names must be unique in a Runtime.
	Name() string
}

// ContextWrapper derives the context of a run once when Run or Resume
// starts, for example to start a trace span that the run's model and tool
// calls become children of. The returned context must derive from ctx.
// release, if non-nil, is called when the Run or Resume call returns,
// however the run ended.
type ContextWrapper interface {
	WrapContext(ctx context.Context, run *Run) (_ context.Context, release func())
}

// BeforeRunner runs when a new run starts, before the input enters the
// conversation. It does not run when an interrupted run is resumed.
type BeforeRunner interface {
	BeforeRun(ctx context.Context, run *Run, in *RunInput) error
}

// BeforeTurner runs at the start of each turn, before the model request is
// built. It may replace the conversation, for example to compact it.
type BeforeTurner interface {
	BeforeTurn(ctx context.Context, run *Run) error
}

// BeforeModeler runs before each model call and may modify the request.
// The request's options are copies, but the values in ProviderOptions are
// shared with the configuration, and the messages are shared with the run:
// replace a value in ProviderOptions or a message in Request.Messages
// instead of modifying it.
type BeforeModeler interface {
	BeforeModel(ctx context.Context, run *Run, req *model.Request) error
}

// ModelWrapper wraps each model call. The wrapper may call next any number
// of times, including not at all.
type ModelWrapper interface {
	WrapModel(run *Run, next model.Model) model.Model
}

// AfterModeler runs after each model call, once the assistant message has
// been added to the conversation. req is the request that was sent.
type AfterModeler interface {
	AfterModel(ctx context.Context, run *Run, req *model.Request, resp *model.Response) error
}

// BeforeTooler runs for each tool call while the turn decides which calls
// need approval, before the run may be interrupted.
type BeforeTooler interface {
	BeforeTool(ctx context.Context, run *Run, plan *ToolPlan) error
}

// ToolRevalidator checks arguments changed by an approved UpdatedInput,
// after approval and before execution. Middleware that enforces argument
// policies in BeforeTool must also implement this stage; BeforeTool itself
// is not repeated, so hooks and other one-time operations do not run twice.
//
// RevalidateTool may Deny the call or return an error, as BeforeTool does.
// Ask is already satisfied by the explicit approval, including approval of
// the edited arguments, and does not prompt again. Call and Info describe
// those arguments, and Approval returns the decision. Revalidators must not
// rewrite Call; the runtime rejects a changed call rather than execute
// arguments an earlier revalidator did not check.
//
// Revalidation runs in middleware order, stopping at a denial or error.
// Unchanged and denied approvals do not run this stage.
type ToolRevalidator interface {
	RevalidateTool(ctx context.Context, run *Run, plan *ToolPlan) error
}

// ToolWrapper wraps each tool execution, after the arguments have been
// validated. Read-only and concurrent calls run concurrently, so the
// returned invoker must be safe for concurrent use and may only call the
// read methods of run and Emit.
type ToolWrapper interface {
	WrapTool(run *Run, next tool.Invoker) tool.Invoker
}

// AfterTooler runs after each tool call, including denied ones, and may
// modify the result.
type AfterTooler interface {
	AfterTool(ctx context.Context, run *Run, plan *ToolPlan, res *model.ToolResult) error
}

// BeforeFinisher runs when the model answers without tool calls. Returning
// true continues the run; the middleware must have appended the next input
// with Run.Append.
type BeforeFinisher interface {
	BeforeFinish(ctx context.Context, run *Run, msg *model.Message) (bool, error)
}

// AnswerHolder is implemented by BeforeFinisher middleware that must see
// the final answer before the consumer does, such as the guard package's
// output guardrails with guard.HoldOutput. If HoldAnswer reports true,
// the runtime holds the EventDelta and EventMessage events of each model
// response until it knows the response has tool calls, when they are
// sent, or until every BeforeFinisher accepted the answer. An answer that
// does not end the run that way is never sent: one a BeforeFinisher
// rejects with a *GuardrailError or continues the run past, one the
// provider paused (model.FinishPause), one that fails Config.Output (its
// OutputRetry has no Output then), and one of a run that stops or fails
// before the answer is accepted. It stays in the conversation, including
// Result.Messages and Result.NewMessages, but is not Result.Output or
// OutputError.Output. Schema diagnostics reported to the consumer then
// describe only a generic mismatch, since details may quote unchecked text.
// The deltas of a response therefore arrive at once, after the response
// ended, unless the response calls tools.
type AnswerHolder interface {
	HoldAnswer() bool
}

// AfterRunner runs when a run ends, including when it is interrupted. If
// the consumer stops iterating early, it runs with StatusRunning: the run
// stopped where it was, and Result.Reason says whether it can be resumed
// from a checkpoint. It does not run if the consumer stops at
// EventRunStart, before the run took a step.
type AfterRunner interface {
	AfterRun(ctx context.Context, run *Run, res *Result)
}

// ToolProvider contributes tools to the runtime's registry.
type ToolProvider interface {
	Tools() []tool.Tool
}

// RunInput is the input of a new run, as seen by BeforeRun.
type RunInput struct {
	NewSession bool
	// Query is the text of the input message before any middleware ran.
	Query string
	// System is the system prompt. It starts as Config.Instructions.
	System string
	// Message is the input message. Middleware may modify it, for example
	// by inserting parts in front of the user's text.
	Message *model.Message
}

// Run gives middleware access to the state of a run. The methods are not
// safe for concurrent use, except the read methods and Emit while tools
// execute.
type Run struct {
	l *execution
}

// ID returns the run ID.
func (r *Run) ID() string { return r.l.st.RunID }

// SessionID returns the session ID.
func (r *Run) SessionID() string { return r.l.st.SessionID }

// Agent returns the agent name.
func (r *Run) Agent() string { return r.l.st.Agent }

// Turn returns the current turn, zero before the first model call.
func (r *Run) Turn() int { return r.l.st.Turn }

// System returns the system prompt of the run.
func (r *Run) System() string { return r.l.st.System }

// Messages returns the conversation, including history. It must not be
// modified.
func (r *Run) Messages() []*model.Message { return r.l.st.Messages }

// NewMessages returns the messages this run added to the conversation,
// starting with its input. It must not be modified.
func (r *Run) NewMessages() []*model.Message { return r.l.st.NewMessages() }

// Tools returns the specs of the available tools.
func (r *Run) Tools() []*model.ToolSpec { return r.l.r.cfg.Tools.Specs() }

// Append adds messages to the conversation.
func (r *Run) Append(msgs ...*model.Message) { r.l.st.Messages = append(r.l.st.Messages, msgs...) }

// ReplaceMessages replaces the conversation with a shorter one whose
// messages were removed from the front, as compaction does.
func (r *Run) ReplaceMessages(msgs []*model.Message) {
	st := r.l.st
	st.Start = max(st.Start-(len(st.Messages)-len(msgs)), 0)
	st.Messages = msgs
}

// Value returns a value stored with SetValue.
func (r *Run) Value(key string) (string, bool) {
	v, ok := r.l.st.Values[key]
	return v, ok
}

// SetValue stores a value in the run state, which is saved with
// checkpoints. An empty value deletes the key. Prefix keys with the
// middleware name.
func (r *Run) SetValue(key, value string) { r.l.st.Values = setValue(r.l.st.Values, key, value) }

// Emit sends an event to the consumer of the run. It reports false when the
// consumer stopped iterating; no event is delivered after that. Tool calls
// running concurrently may emit too: their events are delivered one at a
// time, from the goroutine iterating the run.
func (r *Run) Emit(ev *Event) bool { return r.l.emit(ev) }

// Halt stops the run with StatusStopped after the current step.
func (r *Run) Halt(reason string) {
	r.l.halt = true
	if r.l.haltReason == "" {
		r.l.haltReason = reason
	}
}

// Logger returns the runtime logger.
func (r *Run) Logger() *slog.Logger { return r.l.r.log }

// Decision is the verdict of BeforeTool middleware on a tool call.
type Decision int

const (
	// NoDecision leaves approval to the tool's RequiresApproval.
	NoDecision Decision = iota
	// Allowed skips approval.
	Allowed
	// Asked requires approval.
	Asked
	// Denied blocks the call.
	Denied
)

// ToolPlan is a tool call as seen by BeforeTool and AfterTool.
type ToolPlan struct {
	// Call is the tool call. BeforeTool may rewrite its arguments.
	Call *model.ToolCall
	// Info describes the tool; Known is false for unknown tools. Its
	// RequiresApproval is that of this call when the tool is a
	// tool.CallApprover.
	Info  tool.Info
	Known bool

	decision Decision
	reason   string
	values   map[string]string
	approval *Approval
}

// Approval returns the decision of the approval the call needed, without
// its UpdatedInput, and reports false if the call needed none or is not
// decided yet. AfterTool middleware uses it, for example, to remember an
// approval; see Approval.Remember.
func (p *ToolPlan) Approval() (Approval, bool) {
	if p.approval == nil {
		return Approval{}, false
	}
	a := *p.approval
	a.RememberRules = slices.Clone(a.RememberRules)
	return a, true
}

// Allow skips approval unless another middleware asks or denies.
func (p *ToolPlan) Allow() { p.decide(Allowed, "") }

// Ask requires approval; reason is shown to the approver.
func (p *ToolPlan) Ask(reason string) { p.decide(Asked, reason) }

// Deny blocks the call; reason is shown to the model.
func (p *ToolPlan) Deny(reason string) { p.decide(Denied, reason) }

// decide keeps the most restrictive decision.
func (p *ToolPlan) decide(d Decision, reason string) {
	switch {
	case d > p.decision:
		p.decision, p.reason = d, reason
	case d == p.decision && p.reason == "":
		p.reason = reason
	}
}

// Decision returns the decision so far and its reason.
func (p *ToolPlan) Decision() (Decision, string) { return p.decision, p.reason }

// Value returns a value stored with SetValue.
func (p *ToolPlan) Value(key string) (string, bool) {
	v, ok := p.values[key]
	return v, ok
}

// SetValue stores a value with the call. It is saved with checkpoints, so
// AfterTool sees it after an interrupted run resumes. An empty value
// deletes the key.
func (p *ToolPlan) SetValue(key, value string) { p.values = setValue(p.values, key, value) }

func setValue(m map[string]string, key, value string) map[string]string {
	if value == "" {
		delete(m, key)
		return m
	}
	if m == nil {
		m = map[string]string{}
	}
	m[key] = value
	return m
}

// stopError ends a run with a status other than failed.
type stopError struct {
	status Status
	reason string
}

func (e *stopError) Error() string { return fmt.Sprintf("run %s: %s", e.status, e.reason) }

// Block returns an error that ends the run with StatusBlocked. Use it in
// BeforeRun to reject the input.
func Block(reason string) error { return &stopError{status: StatusBlocked, reason: reason} }

// Stop returns an error that ends the run with StatusStopped.
func Stop(reason string) error { return &stopError{status: StatusStopped, reason: reason} }

// Funcs builds a middleware from functions. Nil functions are skipped.
type Funcs struct {
	MiddlewareName     string
	WrapContextFunc    func(ctx context.Context, run *Run) (context.Context, func())
	BeforeRunFunc      func(ctx context.Context, run *Run, in *RunInput) error
	BeforeTurnFunc     func(ctx context.Context, run *Run) error
	BeforeModelFunc    func(ctx context.Context, run *Run, req *model.Request) error
	WrapModelFunc      func(run *Run, next model.Model) model.Model
	AfterModelFunc     func(ctx context.Context, run *Run, req *model.Request, resp *model.Response) error
	BeforeToolFunc     func(ctx context.Context, run *Run, plan *ToolPlan) error
	RevalidateToolFunc func(ctx context.Context, run *Run, plan *ToolPlan) error
	WrapToolFunc       func(run *Run, next tool.Invoker) tool.Invoker
	AfterToolFunc      func(ctx context.Context, run *Run, plan *ToolPlan, res *model.ToolResult) error
	BeforeFinishFunc   func(ctx context.Context, run *Run, msg *model.Message) (bool, error)
	AfterRunFunc       func(ctx context.Context, run *Run, res *Result)
}

// Name implements Middleware.
func (f *Funcs) Name() string { return f.MiddlewareName }

// WrapContext implements ContextWrapper.
func (f *Funcs) WrapContext(ctx context.Context, run *Run) (context.Context, func()) {
	if f.WrapContextFunc == nil {
		return ctx, nil
	}
	return f.WrapContextFunc(ctx, run)
}

// BeforeRun implements BeforeRunner.
func (f *Funcs) BeforeRun(ctx context.Context, run *Run, in *RunInput) error {
	if f.BeforeRunFunc == nil {
		return nil
	}
	return f.BeforeRunFunc(ctx, run, in)
}

// BeforeTurn implements BeforeTurner.
func (f *Funcs) BeforeTurn(ctx context.Context, run *Run) error {
	if f.BeforeTurnFunc == nil {
		return nil
	}
	return f.BeforeTurnFunc(ctx, run)
}

// BeforeModel implements BeforeModeler.
func (f *Funcs) BeforeModel(ctx context.Context, run *Run, req *model.Request) error {
	if f.BeforeModelFunc == nil {
		return nil
	}
	return f.BeforeModelFunc(ctx, run, req)
}

// WrapModel implements ModelWrapper.
func (f *Funcs) WrapModel(run *Run, next model.Model) model.Model {
	if f.WrapModelFunc == nil {
		return next
	}
	return f.WrapModelFunc(run, next)
}

// AfterModel implements AfterModeler.
func (f *Funcs) AfterModel(ctx context.Context, run *Run, req *model.Request, resp *model.Response) error {
	if f.AfterModelFunc == nil {
		return nil
	}
	return f.AfterModelFunc(ctx, run, req, resp)
}

// BeforeTool implements BeforeTooler.
func (f *Funcs) BeforeTool(ctx context.Context, run *Run, plan *ToolPlan) error {
	if f.BeforeToolFunc == nil {
		return nil
	}
	return f.BeforeToolFunc(ctx, run, plan)
}

// RevalidateTool implements ToolRevalidator.
func (f *Funcs) RevalidateTool(ctx context.Context, run *Run, plan *ToolPlan) error {
	if f.RevalidateToolFunc == nil {
		return nil
	}
	return f.RevalidateToolFunc(ctx, run, plan)
}

// WrapTool implements ToolWrapper.
func (f *Funcs) WrapTool(run *Run, next tool.Invoker) tool.Invoker {
	if f.WrapToolFunc == nil {
		return next
	}
	return f.WrapToolFunc(run, next)
}

// AfterTool implements AfterTooler.
func (f *Funcs) AfterTool(ctx context.Context, run *Run, plan *ToolPlan, res *model.ToolResult) error {
	if f.AfterToolFunc == nil {
		return nil
	}
	return f.AfterToolFunc(ctx, run, plan, res)
}

// BeforeFinish implements BeforeFinisher.
func (f *Funcs) BeforeFinish(ctx context.Context, run *Run, msg *model.Message) (bool, error) {
	if f.BeforeFinishFunc == nil {
		return false, nil
	}
	return f.BeforeFinishFunc(ctx, run, msg)
}

// AfterRun implements AfterRunner.
func (f *Funcs) AfterRun(ctx context.Context, run *Run, res *Result) {
	if f.AfterRunFunc != nil {
		f.AfterRunFunc(ctx, run, res)
	}
}

// stages holds the middleware of each stage in call order.
type stages struct {
	wrapContext    []ContextWrapper
	beforeRun      []BeforeRunner
	beforeTurn     []BeforeTurner
	beforeModel    []BeforeModeler
	wrapModel      []ModelWrapper
	afterModel     []AfterModeler
	beforeTool     []BeforeTooler
	revalidateTool []ToolRevalidator
	wrapTool       []ToolWrapper
	afterTool      []AfterTooler
	beforeFinish   []BeforeFinisher
	afterRun       []AfterRunner
	// holdAnswer is set when an AnswerHolder asks to hold answers.
	holdAnswer bool
}

// newStages sorts middleware into stages. Tools of ToolProvider middleware
// are added to a copy of reg, which is returned.
func newStages(mws []Middleware, reg *tool.Registry) (stages, *tool.Registry, error) {
	var s stages
	seen := map[string]bool{}
	cloned := false
	for _, mw := range mws {
		if mw == nil {
			continue
		}
		name := mw.Name()
		if name == "" {
			return s, nil, errors.New("loop: middleware has no name")
		}
		if seen[name] {
			return s, nil, fmt.Errorf("loop: duplicate middleware %q", name)
		}
		seen[name] = true
		if p, ok := mw.(ToolProvider); ok {
			if !cloned {
				reg, cloned = reg.Clone(), true
			}
			if err := reg.Register(p.Tools()...); err != nil {
				return s, nil, fmt.Errorf("loop: middleware %s: %w", name, err)
			}
		}
		if m, ok := mw.(ContextWrapper); ok {
			s.wrapContext = append(s.wrapContext, m)
		}
		if m, ok := mw.(BeforeRunner); ok {
			s.beforeRun = append(s.beforeRun, m)
		}
		if m, ok := mw.(BeforeTurner); ok {
			s.beforeTurn = append(s.beforeTurn, m)
		}
		if m, ok := mw.(BeforeModeler); ok {
			s.beforeModel = append(s.beforeModel, m)
		}
		if m, ok := mw.(ModelWrapper); ok {
			s.wrapModel = append(s.wrapModel, m)
		}
		if m, ok := mw.(AfterModeler); ok {
			s.afterModel = append(s.afterModel, m)
		}
		if m, ok := mw.(BeforeTooler); ok {
			s.beforeTool = append(s.beforeTool, m)
		}
		if m, ok := mw.(ToolRevalidator); ok {
			s.revalidateTool = append(s.revalidateTool, m)
		}
		if m, ok := mw.(ToolWrapper); ok {
			s.wrapTool = append(s.wrapTool, m)
		}
		if m, ok := mw.(AfterTooler); ok {
			s.afterTool = append(s.afterTool, m)
		}
		if m, ok := mw.(BeforeFinisher); ok {
			s.beforeFinish = append(s.beforeFinish, m)
		}
		if m, ok := mw.(AfterRunner); ok {
			s.afterRun = append(s.afterRun, m)
		}
		if m, ok := mw.(AnswerHolder); ok && m.HoldAnswer() {
			s.holdAnswer = true
		}
	}
	return s, reg, nil
}
