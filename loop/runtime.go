// Package loop implements the agent loop: it calls the model, executes
// tool calls, asks for approvals and checkpoints state so interrupted runs
// can be resumed. Everything else, such as hooks and context engineering,
// plugs into the loop as Middleware.
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"uuid"

	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/tool"
)

// DefaultMaxTurns bounds the number of model calls in a run.
const DefaultMaxTurns = 50

// Config configures a Runtime.
type Config struct {
	Settings
	Tools *tool.Registry
	// Middlewares extend the loop in order. If any is a ToolProvider, its
	// tools are added to a copy of Tools.
	Middlewares []Middleware
}

// Settings are the run settings shared by Config and agent.Config: the
// model and its request options, approval and checkpointing, and the
// limits and stop conditions of runs.
type Settings struct {
	// Name identifies the agent in events and hooks.
	Name  string
	Model model.Model
	// Instructions is the base system prompt; middleware may extend it.
	Instructions string
	// ToolMiddlewares wrap every tool call inside the ToolWrapper
	// middleware; the first is outermost.
	ToolMiddlewares []tool.Middleware
	Checkpointer    Checkpointer
	// Approver decides calls that need approval. Without one, the run is
	// interrupted and must be resumed with approvals.
	Approver Approver

	MaxTurns       int
	MaxTokens      int
	Temperature    *float64
	TopP           *float64
	Reasoning      *model.Reasoning
	ToolChoice     *model.ToolChoice
	ResponseFormat *model.ResponseFormat
	// Seed is passed to the provider in model.Request.Seed.
	Seed *int
	// ProviderOptions is passed to the provider in
	// model.Request.ProviderOptions.
	ProviderOptions map[string]map[string]any
	// Stream calls Model.Stream and emits delta events.
	Stream bool
	// Parallelism bounds tool calls running concurrently, which are those of
	// read-only and concurrent tools. Defaults to 8.
	Parallelism int
	Logger      *slog.Logger

	// Output asks the model for JSON matching a schema and validates the
	// final answer; see Output.
	Output *Output
	// Limits bounds the usage of each run; Request.Limits overrides it.
	Limits Limits
	// Budget, if set, is called with the run's usage so far before each
	// model call and each batch of tool calls. An error ends the run with
	// StatusLimitExceeded and the error's text as the reason, for example
	// when the usage priced in a currency exceeds a budget.
	Budget func(model.Usage) error
	// StopAtTools ends the run as completed once a tool with one of these
	// names returns a result that is not an error. Result.Output is the
	// content of that result, and the model is not called again.
	StopAtTools []string
	// StopWhen ends the run as completed when any condition holds after a
	// turn's tool calls have run; see StopCondition.
	StopWhen []StopCondition
}

// Runtime runs the agent loop. It is safe for concurrent runs.
type Runtime struct {
	cfg    Config
	stages stages
	exec   *tool.Executor
	log    *slog.Logger
}

// New returns a Runtime.
func New(cfg Config) (*Runtime, error) {
	if cfg.Model == nil {
		return nil, errors.New("loop: model is required")
	}
	if cfg.Tools == nil {
		cfg.Tools = &tool.Registry{}
	}
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = DefaultMaxTurns
	}
	if cfg.Parallelism <= 0 {
		cfg.Parallelism = 8
	}
	s, reg, err := newStages(cfg.Middlewares, cfg.Tools)
	if err != nil {
		return nil, err
	}
	cfg.Tools = reg
	if err := configureStops(&cfg); err != nil {
		return nil, err
	}
	if err := configureOutput(&cfg); err != nil {
		return nil, err
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Runtime{cfg: cfg, stages: s, exec: tool.NewExecutor(cfg.Tools, cfg.ToolMiddlewares...), log: log}, nil
}

// Config returns the runtime configuration.
func (r *Runtime) Config() Config { return r.cfg }

// Request starts a run.
type Request struct {
	// RunID identifies the run; generated when empty.
	RunID     string
	SessionID string
	// History is the conversation before Input. It is not modified.
	History []*model.Message
	// Input is the new user message.
	Input *model.Message
	// NewSession marks the first run of a session, for example to fire the
	// SessionStart hook.
	NewSession bool
	// MaxTurns, if positive, overrides Config.MaxTurns for this run.
	MaxTurns int
	// Limits, if non-nil, replaces Config.Limits for this run.
	Limits *Limits
	// Values are the initial values of the run, as Run.SetValue stores
	// them, for example the session-scoped values of the session's earlier
	// runs; see SessionValuePrefix. Empty values are ignored.
	Values map[string]string
	// Usage and Turns are the usage and turns of earlier parts of the same
	// run, such as those of an agent that handed the run off to this one.
	// They count toward the limits and are included in the Result.
	Usage model.Usage
	Turns int
}

// SessionValuePrefix starts the keys of run values that belong to the
// session rather than the run. agent.Runner saves them with the session
// when a run ends and passes them to the session's next runs in
// Request.Values; callers of Runtime.Run carry them from Result.Values
// themselves.
const SessionValuePrefix = "session."

// Result summarizes a finished or interrupted run.
type Result struct {
	RunID  string `json:"run_id"`
	Status Status `json:"status"`
	// Reason explains a non-completed status.
	Reason string `json:"reason,omitempty"`
	// FinishReason is the finish reason of the last model response, for
	// example model.FinishRefusal when the model declined to answer.
	FinishReason model.FinishReason `json:"finish_reason,omitempty"`
	// Output is the text of the final assistant message, or the result of
	// the tool the run stopped at; see Config.StopAtTools. Decode parses it.
	Output string `json:"output"`
	// Messages is the whole conversation after the run, including history.
	// It may be shorter than history plus new messages after compaction.
	Messages []*model.Message `json:"messages,omitempty"`
	// NewMessages are the messages added by this run.
	NewMessages []*model.Message `json:"new_messages,omitempty"`
	Usage       model.Usage      `json:"usage"`
	Turns       int              `json:"turns"`
	// Interrupt lists the approvals needed to resume.
	Interrupt *Interrupt `json:"interrupt,omitempty"`
	// Err is the error of a failed run. In JSON it is the string "error".
	Err error `json:"-"`
	// Values are the run's values when it ended; see Run.SetValue and
	// SessionValuePrefix.
	Values map[string]string `json:"values,omitempty"`
	// Agent names the agent that ran last, which differs from the agent
	// started when the run was handed off; see EventHandoff.
	Agent string `json:"agent,omitempty"`
	// Guardrail describes the guardrail whose tripwire ended a run with
	// StatusBlocked; see GuardrailError.
	Guardrail *Guardrail `json:"guardrail,omitempty"`
}

// resultJSON is Result without its methods, for encoding.
type resultJSON Result

// MarshalJSON encodes the result with Err as the string "error".
func (r Result) MarshalJSON() ([]byte, error) {
	out := struct {
		resultJSON
		Error string `json:"error,omitempty"`
	}{resultJSON: resultJSON(r)}
	if r.Err != nil {
		out.Error = r.Err.Error()
	}
	return json.Marshal(out)
}

// UnmarshalJSON decodes a result encoded by MarshalJSON. Err, if set, only
// keeps the error's text.
func (r *Result) UnmarshalJSON(b []byte) error {
	var in struct {
		resultJSON
		Error string `json:"error"`
	}
	if err := json.Unmarshal(b, &in); err != nil {
		return err
	}
	*r = Result(in.resultJSON)
	if in.Error != "" {
		r.Err = errors.New(in.Error)
	}
	return nil
}

// errYieldStopped signals that the consumer stopped iterating.
var errYieldStopped = errors.New("loop: consumer stopped")

// ErrNoCheckpointer is returned by Resume when the runtime has no
// Checkpointer. Resume such runs from Interrupt.State with ResumeState.
var ErrNoCheckpointer = errors.New("loop: resume requires a checkpointer")

// Run starts a run and returns its events. The last event is EventRunEnd;
// a failed run then yields its error.
//
// If the consumer stops iterating early, the run stops before its next
// step: tool calls that have not started do not run, and AfterRun
// middleware sees StatusRunning. Once the input was added, the state is
// checkpointed as running, so with a Checkpointer the run can be resumed
// from where it stopped; Result.Reason says whether it can. If the
// consumer stops at EventRunStart, nothing runs, not even AfterRun, and
// there is nothing to resume.
func (r *Runtime) Run(ctx context.Context, req *Request) iter.Seq2[*Event, error] {
	return func(yield func(*Event, error) bool) {
		if req == nil || req.Input == nil {
			yield(nil, errors.New("loop: request has no input"))
			return
		}
		st := &State{
			RunID:     req.RunID,
			SessionID: req.SessionID,
			Agent:     r.cfg.Name,
			Messages:  model.CloneMessages(req.History),
			Start:     len(req.History),
			Status:    StatusRunning,
			MaxTurns:  max(req.MaxTurns, 0),
			Limits:    clonePtr(req.Limits),
			Usage:     req.Usage,
			Turn:      max(req.Turns, 0),
		}
		for k, v := range req.Values {
			st.Values = setValue(st.Values, k, v)
		}
		if st.RunID == "" {
			st.RunID = "run_" + uuid.NewV7().String()
		}
		lp := r.newExecution(st, yield)
		lp.caller = scopeOf(ctx)
		ctx, release := lp.wrapContext(ctx)
		defer release()
		defer lp.abandon(ctx)
		if !lp.emit(&Event{Type: EventRunStart}) {
			return
		}
		if err := lp.start(ctx, req); err != nil {
			lp.fail(ctx, err)
			return
		}
		if lp.active() {
			lp.run(ctx, false, nil)
		}
	}
}

// Resume continues a run from its checkpoint. It resumes runs that were
// interrupted for approval, that failed, or whose process stopped while they
// were running; see Status.Resumable.
//
// approvals maps tool call IDs to decisions for an interrupted run; calls
// without a decision are denied. Approvals are ignored for runs that were
// not interrupted: requests they raise go to the Approver or interrupt the
// run. Finished tool calls are not repeated, and calls with side effects
// that were cut off are reported to the model as possibly incomplete
// instead of being run again.
//
// A run that stopped after the model's final answer, before the answer was
// handled, handles it without calling the model again. If the consumer
// stops at EventRunStart, the checkpoint is left as it was.
func (r *Runtime) Resume(ctx context.Context, runID string, approvals map[string]Approval) iter.Seq2[*Event, error] {
	return func(yield func(*Event, error) bool) {
		if r.cfg.Checkpointer == nil {
			yield(nil, ErrNoCheckpointer)
			return
		}
		st, err := r.cfg.Checkpointer.Load(ctx, runID)
		if err != nil {
			yield(nil, err)
			return
		}
		r.resume(ctx, st, approvals, yield)
	}
}

// ResumeState is like Resume for a state obtained elsewhere, such as
// Interrupt.State of an earlier run, without a Checkpointer.
func (r *Runtime) ResumeState(ctx context.Context, st *State, approvals map[string]Approval) iter.Seq2[*Event, error] {
	return func(yield func(*Event, error) bool) {
		if st == nil {
			yield(nil, errors.New("loop: no state to resume"))
			return
		}
		r.resume(ctx, st.Clone(), approvals, yield)
	}
}

func (r *Runtime) resume(ctx context.Context, st *State, approvals map[string]Approval, yield func(*Event, error) bool) {
	switch {
	case st.Version > StateVersion:
		yield(nil, fmt.Errorf("loop: run %s has state version %d; this version supports up to %d", st.RunID, st.Version, StateVersion))
		return
	case !st.Status.Resumable():
		yield(nil, fmt.Errorf("loop: run %s is %s and cannot be resumed", st.RunID, st.Status))
		return
	case st.Status != StatusInterrupted && len(st.Messages) <= st.Start:
		yield(nil, fmt.Errorf("loop: run %s ended before its input was added; start a new run", st.RunID))
		return
	}
	// Approvals answer only the requests of an interrupt; a run that stopped
	// otherwise has raised none, even if a nested run's requests are pending.
	if st.Status != StatusInterrupted {
		approvals = nil
	} else if approvals == nil {
		approvals = map[string]Approval{}
	}
	st.Status = StatusRunning
	st.Reason = ""
	lp := r.newExecution(st, yield)
	lp.caller = scopeOf(ctx)
	ctx, release := lp.wrapContext(ctx)
	defer release()
	defer lp.abandon(ctx)
	if !lp.emit(&Event{Type: EventRunStart}) {
		return
	}
	lp.began = true
	lp.run(ctx, true, approvals)
}

// Discard deletes the checkpoint of a run that will not be resumed. A
// missing checkpoint is not an error.
func (r *Runtime) Discard(ctx context.Context, runID string) error {
	if r.cfg.Checkpointer == nil {
		return nil
	}
	if err := r.cfg.Checkpointer.Delete(ctx, runID); err != nil && !errors.Is(err, ErrCheckpointNotFound) {
		return err
	}
	return nil
}

// Collect drains events and returns the run result. onEvent, if non-nil,
// sees every event, including those forwarded from nested runs.
func Collect(events iter.Seq2[*Event, error], onEvent func(*Event)) (*Result, error) {
	var res *Result
	for ev, err := range events {
		if err != nil {
			if res == nil {
				res = &Result{Status: StatusFailed, Err: err}
			}
			return res, err
		}
		if onEvent != nil {
			onEvent(ev)
		}
		if ev.Type == EventRunEnd && ev.Depth == 0 {
			res = ev.Result
		}
	}
	if res == nil {
		return nil, errors.New("loop: run ended without a result")
	}
	return res, res.Err
}
