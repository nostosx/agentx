package guard

import (
	"cmp"
	"context"
	"errors"
	"iter"
	"slices"
	"sync"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/tool"
)

// pendingKey is the run value set while the parallel input guardrails of
// a run have not passed, so a resumed run runs them again.
const pendingKey = "guard.input_pending"

// withheldResult replaces a tool result that a guardrail tripped on.
const withheldResult = "This tool result was withheld by a guardrail."

// Middleware returns the middleware that runs the guardrails of cfg.
// Register first before all other middleware, so input guardrails see
// the input as the user sent it, and last after all others, so tool input
// guardrails see the arguments that will run, tool output guardrails the
// result as the tool returned it, before any other middleware sees it,
// and output guardrails only the answer the run finishes with. agent.New
// does this.
func Middleware(cfg Config) (first, last loop.Middleware) {
	return &inputMW{cfg: cfg}, &finalMW{cfg: cfg}
}

type inputMW struct{ cfg Config }

func (m *inputMW) Name() string { return "guard" }

type finalMW struct {
	cfg Config
	// trips holds the trips of tool output guardrails, by run and call,
	// from WrapTool until AfterTool ends the run with them.
	trips sync.Map
}

func (m *finalMW) Name() string { return "guard.final" }

// tripKey identifies a call of a run in finalMW.trips.
type tripKey struct {
	run  *loop.Run
	call string
}

// HoldAnswer implements loop.AnswerHolder: answers are held until the
// output guardrails pass if one of them asks for it; see HoldOutput.
func (m *finalMW) HoldAnswer() bool {
	return slices.ContainsFunc(m.cfg.Output, func(g OutputGuardrail) bool { return g.Hold })
}

// WrapTool implements loop.ToolWrapper with the tool output
// guardrails. Registered last, it is the innermost wrapper, so it checks
// the result before other middleware, such as tracing and PostToolUse
// hooks, sees it. A tripped guardrail replaces the result with its
// message, without the tool's parts and metadata, and AfterTool ends the
// run.
func (m *finalMW) WrapTool(run *loop.Run, next tool.Invoker) tool.Invoker {
	return func(ctx context.Context, call *tool.Call) (*tool.Result, error) {
		out, err := next(ctx, call)
		tc := &model.ToolCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments}
		res := &model.ToolResult{CallID: call.ID, Name: call.Name}
		switch {
		case err != nil:
			res.Content, res.IsError = "Error: "+err.Error(), true
		case out != nil:
			res.Content, res.Parts, res.IsError, res.Metadata = out.Content, out.Parts, out.IsError, out.Metadata
		}
		for _, g := range m.cfg.Tool {
			if g.After == nil || !g.applies(call.Name) {
				continue
			}
			r, gerr := g.After(ctx, run, tc, res)
			if gerr != nil {
				return nil, gerr
			}
			if r.Tripwire {
				m.trips.Store(tripKey{run, call.ID}, trip(g.Name, KindToolOutput, r))
				return &tool.Result{Content: cmp.Or(r.Message, withheldResult), IsError: true}, nil
			}
		}
		if err != nil {
			return out, err
		}
		return &tool.Result{Content: res.Content, Parts: res.Parts, IsError: res.IsError, Metadata: res.Metadata}, nil
	}
}

// AfterTool implements loop.AfterTooler: it ends the run of a call
// whose result a tool output guardrail withheld. Registered last, it runs
// first, so later AfterTool middleware, such as PostToolUse hooks, does
// not run for the call.
func (m *finalMW) AfterTool(_ context.Context, run *loop.Run, plan *loop.ToolPlan, _ *model.ToolResult) error {
	if ge, ok := m.trips.LoadAndDelete(tripKey{run, plan.Call.ID}); ok {
		return ge.(*loop.GuardrailError)
	}
	return nil
}

// AfterRun implements loop.AfterRunner. It forgets the trips of calls
// whose AfterTool did not run, such as later calls of a batch whose run a
// trip ended.
func (m *finalMW) AfterRun(_ context.Context, run *loop.Run, _ *loop.Result) {
	m.trips.Range(func(k, _ any) bool {
		if k.(tripKey).run == run {
			m.trips.Delete(k)
		}
		return true
	})
}

func (m *inputMW) parallel() []InputGuardrail {
	var out []InputGuardrail
	for _, g := range m.cfg.Input {
		if g.Parallel {
			out = append(out, g)
		}
	}
	return out
}

// runState holds the parallel input guardrails of one Run or Resume call.
type runState struct {
	cancel context.CancelCauseFunc
	wg     sync.WaitGroup
	// started is set once the guardrails were started, finished once
	// they passed. Both are used only by the goroutine of the run.
	started, finished bool

	mu  sync.Mutex
	hit *loop.GuardrailError
	err error
}

type stateKey struct{}

var errTripped = errors.New("guard: input guardrail tripped")

// WrapContext implements loop.ContextWrapper. It derives a context the
// parallel guardrails can cancel.
func (m *inputMW) WrapContext(ctx context.Context, run *loop.Run) (context.Context, func()) {
	if v, _ := run.Value(SkipInputKey); len(m.parallel()) == 0 || v != "" {
		return ctx, nil
	}
	ctx, cancel := context.WithCancelCause(ctx)
	st := &runState{cancel: cancel}
	return context.WithValue(ctx, stateKey{}, st), func() {
		cancel(nil)
		st.wg.Wait()
	}
}

// start runs the parallel guardrails on input.
func (m *inputMW) start(ctx context.Context, st *runState, run *loop.Run, input *model.Message) {
	st.started = true
	for _, g := range m.parallel() {
		st.wg.Go(func() {
			r, err := g.Check(ctx, run, input)
			st.mu.Lock()
			defer st.mu.Unlock()
			switch {
			case st.hit != nil || st.err != nil:
			case err != nil:
				st.err = err
				st.cancel(err)
			case r.Tripwire:
				st.hit = trip(g.Name, KindInput, r)
				st.cancel(errTripped)
			}
		})
	}
}

// wait waits for the parallel guardrails and returns the trip or error
// that ends the run, if any.
func (st *runState) wait() error {
	st.wg.Wait()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.hit != nil {
		return st.hit
	}
	return st.err
}

// BeforeRun implements loop.BeforeRunner. It runs the sequential input
// guardrails and starts the parallel ones.
func (m *inputMW) BeforeRun(ctx context.Context, run *loop.Run, in *loop.RunInput) error {
	if v, _ := run.Value(SkipInputKey); v != "" {
		return nil
	}
	for _, g := range m.cfg.Input {
		if g.Parallel {
			continue
		}
		r, err := g.Check(ctx, run, in.Message)
		if err != nil {
			return err
		}
		if r.Tripwire {
			return trip(g.Name, KindInput, r)
		}
	}
	if st, _ := ctx.Value(stateKey{}).(*runState); st != nil {
		// A copy, as later middleware may modify the input meanwhile.
		m.start(ctx, st, run, in.Message.Clone())
		run.SetValue(pendingKey, "true")
	}
	return nil
}

// WrapModel implements loop.ModelWrapper. The first model call of a
// run with parallel guardrails waits for them.
func (m *inputMW) WrapModel(run *loop.Run, next model.Model) model.Model {
	return &guardedModel{Model: next, mw: m, run: run}
}

type guardedModel struct {
	model.Model
	mw  *inputMW
	run *loop.Run
}

// state returns the run's guardrail state if the call must wait for the
// parallel guardrails, starting them for a resumed run that had not
// passed them.
func (g *guardedModel) state(ctx context.Context) *runState {
	st, _ := ctx.Value(stateKey{}).(*runState)
	if st == nil || st.finished {
		return nil
	}
	if !st.started {
		if v, _ := g.run.Value(pendingKey); v == "" {
			st.finished = true
			return nil
		}
		msgs := g.run.NewMessages()
		if len(msgs) == 0 {
			st.finished = true
			return nil
		}
		g.mw.start(ctx, st, g.run, msgs[0].Clone())
	}
	return st
}

// settle waits for the guardrails after the call and returns the error
// that ends the run, or callErr.
func (g *guardedModel) settle(st *runState, usage model.Usage, callErr error) error {
	err := st.wait()
	st.finished = true
	if ge, ok := errors.AsType[*loop.GuardrailError](err); ok {
		ge.Usage = usage
		return ge
	}
	if err != nil {
		return err
	}
	g.run.SetValue(pendingKey, "")
	return callErr
}

func (g *guardedModel) Generate(ctx context.Context, req *model.Request) (*model.Response, error) {
	st := g.state(ctx)
	if st == nil {
		return g.Model.Generate(ctx, req)
	}
	resp, err := g.Model.Generate(ctx, req)
	var usage model.Usage
	if err == nil && resp != nil {
		usage = completed(resp.Usage)
	}
	if err := g.settle(st, usage, err); err != nil {
		return nil, err
	}
	return resp, nil
}

func (g *guardedModel) Stream(ctx context.Context, req *model.Request) iter.Seq2[*model.StreamEvent, error] {
	st := g.state(ctx)
	if st == nil {
		return g.Model.Stream(ctx, req)
	}
	return func(yield func(*model.StreamEvent, error) bool) {
		var done *model.StreamEvent
		var callErr error
		for ev, err := range g.Model.Stream(ctx, req) {
			if err != nil {
				callErr = err
				break
			}
			if ev != nil && ev.Type == model.StreamDone {
				done = ev
				continue
			}
			if !yield(ev, nil) {
				return
			}
		}
		var usage model.Usage
		if done != nil && done.Response != nil {
			usage = completed(done.Response.Usage)
		}
		if err := g.settle(st, usage, callErr); err != nil {
			yield(nil, err)
			return
		}
		if done != nil {
			yield(done, nil)
		}
	}
}

// completed returns the usage of a model call that completed, which counts
// as a request.
func completed(u model.Usage) model.Usage {
	u.Requests = max(u.Requests, 1)
	return u
}

// BeforeTool implements loop.BeforeTooler with the tool input
// guardrails.
func (m *finalMW) BeforeTool(ctx context.Context, run *loop.Run, plan *loop.ToolPlan) error {
	if d, _ := plan.Decision(); d == loop.Denied {
		return nil
	}
	for _, g := range m.cfg.Tool {
		if g.Before == nil || !g.applies(plan.Call.Name) {
			continue
		}
		r, err := g.Before(ctx, run, plan.Call)
		if err != nil {
			return err
		}
		if r.Tripwire {
			return trip(g.Name, KindToolInput, r)
		}
	}
	return nil
}

// RevalidateTool checks tool input guardrails again when an approval edits
// the arguments, before the edited call can run.
func (m *finalMW) RevalidateTool(ctx context.Context, run *loop.Run, plan *loop.ToolPlan) error {
	return m.BeforeTool(ctx, run, plan)
}

// BeforeFinish implements loop.BeforeFinisher with the output
// guardrails.
func (m *finalMW) BeforeFinish(ctx context.Context, run *loop.Run, msg *model.Message) (bool, error) {
	for _, g := range m.cfg.Output {
		r, err := g.Check(ctx, run, msg)
		if err != nil {
			return false, err
		}
		if r.Tripwire {
			return false, trip(g.Name, KindOutput, r)
		}
	}
	return false, nil
}
