package eval

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/nostosx/agentx/loop"
)

// DefaultConcurrency bounds the case runs in progress when Concurrency is
// not set.
const DefaultConcurrency = 8

// Option configures Dataset.Evaluate and Run.
type Option func(*config)

type config struct {
	experiment  string
	concurrency int
	repeat      int
	timeout     time.Duration
	evalTimeout time.Duration
	evaluators  []Evaluator
	onResult    func(*CaseResult)

	// Used by Run only.
	threshold float64
	minScores map[string]float64
	baseline  *Report
}

func newConfig(opts []Option) *config {
	c := &config{concurrency: DefaultConcurrency, repeat: 1, threshold: 1}
	for _, opt := range opts {
		opt(c)
	}
	c.concurrency = max(c.concurrency, 1)
	c.repeat = max(c.repeat, 1)
	return c
}

// Experiment names the evaluation in its report, for example after the
// model or prompt version under test.
func Experiment(name string) Option { return func(c *config) { c.experiment = name } }

// Concurrency bounds the case runs in progress; it defaults to
// DefaultConcurrency. Use 1 to run cases one after another, for example
// against rate-limited APIs.
func Concurrency(n int) Option { return func(c *config) { c.concurrency = n } }

// Repeat runs every case n times, to measure flaky behavior. Reports keep
// each run and aggregate them per case; see Report.ByCase.
func Repeat(n int) Option { return func(c *config) { c.repeat = n } }

// Timeout bounds each run of the task. A task that does not return when
// its context ends is abandoned: its case fails with a timeout error and
// the evaluation goes on without waiting for it.
func Timeout(d time.Duration) Option { return func(c *config) { c.timeout = d } }

// EvaluatorTimeout bounds each evaluator call, like Timeout bounds tasks.
func EvaluatorTimeout(d time.Duration) Option { return func(c *config) { c.evalTimeout = d } }

// WithEvaluators adds evaluators to those of the dataset.
func WithEvaluators(es ...Evaluator) Option {
	return func(c *config) { c.evaluators = append(c.evaluators, es...) }
}

// OnResult calls fn with each case run as it finishes, one at a time, for
// example to report progress.
func OnResult(fn func(*CaseResult)) Option { return func(c *config) { c.onResult = fn } }

// Evaluate runs task on every case of the dataset and evaluates the
// outputs.
//
// Cases run concurrently, up to Concurrency at a time; the report lists
// them in dataset order, with the repetitions of a case together. A task
// or evaluator that fails or panics fails only its case. Evaluate returns
// an error, along with the report of the runs that finished, only if the
// dataset is invalid or ctx ends; runs that did not start then fail with
// the context's error.
func (d *Dataset[In, Out]) Evaluate(ctx context.Context, task Task[In, Out], opts ...Option) (*Report, error) {
	if task == nil {
		return nil, errors.New("eval: task is nil")
	}
	cfg := newConfig(opts)
	cases := slices.Clone(d.Cases)
	seen := make(map[string]bool, len(cases))
	for i := range cases {
		if cases[i].Name == "" {
			cases[i].Name = "case " + strconv.Itoa(i+1)
		}
		if seen[cases[i].Name] {
			return nil, fmt.Errorf("eval: duplicate case name %q", cases[i].Name)
		}
		seen[cases[i].Name] = true
	}

	report := &Report{
		Name:    cfg.experiment,
		Dataset: d.Name,
		Repeat:  cfg.repeat,
		Started: time.Now(),
		Cases:   make([]*CaseResult, len(cases)*cfg.repeat),
	}
	sem := make(chan struct{}, cfg.concurrency)
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	finish := func(i int, cr *CaseResult) {
		report.Cases[i] = cr
		if cfg.onResult != nil {
			mu.Lock()
			defer mu.Unlock()
			cfg.onResult(cr)
		}
	}
	for ci := range cases {
		for rep := range cfg.repeat {
			i := ci*cfg.repeat + rep
			c := &cases[ci]
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				cr := newCaseResult(c, rep)
				cr.Error = ctx.Err().Error()
				finish(i, cr)
				continue
			}
			wg.Go(func() {
				defer func() { <-sem }()
				finish(i, runCase(ctx, d.Evaluators, c, rep, task, cfg))
			})
		}
	}
	wg.Wait()
	report.Duration = time.Since(report.Started)
	report.Summary = summarize(report.Cases)
	return report, ctx.Err()
}

func newCaseResult[In, Out any](c *Case[In, Out], rep int) *CaseResult {
	cr := &CaseResult{Name: c.Name, Repeat: rep, Input: c.Input, Metadata: c.Metadata}
	if c.Expected != nil {
		cr.Expected = *c.Expected
	}
	return cr
}

// runCase runs one repetition of a case and its evaluators.
func runCase[In, Out any](ctx context.Context, shared []Evaluator, c *Case[In, Out], rep int, task Task[In, Out], cfg *config) *CaseResult {
	cr := newCaseResult(c, rep)
	col := &collector{}
	tctx := withCollector(ctx, col)
	if cfg.timeout > 0 {
		var cancel context.CancelFunc
		tctx, cancel = context.WithTimeoutCause(tctx, cfg.timeout, fmt.Errorf("eval: the task timed out after %s: %w", cfg.timeout, context.DeadlineExceeded))
		defer cancel()
	}
	start := time.Now()
	type taskOut struct {
		out Out
		res *loop.Result
	}
	r, err := guard(tctx, "task", func(ctx context.Context) (taskOut, error) {
		out, res, err := task(ctx, c.Input)
		return taskOut{out, res}, err
	})
	cr.Duration = time.Since(start)
	if res := r.res; res != nil {
		cr.Status = res.Status
		cr.Turns = res.Turns
		cr.Usage = res.Usage
	}
	if err != nil {
		cr.Error = err.Error()
		return cr
	}
	cr.Output = r.out

	ec := &EvaluatorContext{
		Name:     c.Name,
		Repeat:   rep,
		Input:    c.Input,
		Output:   r.out,
		Expected: cr.Expected,
		Metadata: c.Metadata,
		Result:   r.res,
		Duration: cr.Duration,
		Spans:    col.roots(),
	}
	for _, e := range slices.Concat(shared, c.Evaluators, cfg.evaluators) {
		ectx, cancel := ctx, context.CancelFunc(func() {})
		if cfg.evalTimeout > 0 {
			ectx, cancel = context.WithTimeoutCause(ctx, cfg.evalTimeout, fmt.Errorf("eval: the evaluator timed out after %s: %w", cfg.evalTimeout, context.DeadlineExceeded))
		}
		evs, err := guard(ectx, "evaluator", func(ctx context.Context) ([]Evaluation, error) { return e.Evaluate(ctx, ec) })
		cancel()
		if err != nil {
			cr.EvaluatorErrors = append(cr.EvaluatorErrors, EvaluatorError{Evaluator: e.Name(), Error: err.Error()})
			continue
		}
		for _, ev := range evs {
			ev.Evaluator = e.Name()
			cr.Evaluations = append(cr.Evaluations, ev)
		}
	}
	return cr
}

// guard calls fn in its own goroutine, turning a panic into an error, and
// returns early with the cause of ctx if ctx ends first.
func guard[T any](ctx context.Context, what string, fn func(context.Context) (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				var zero T
				done <- result{zero, fmt.Errorf("eval: %s panicked: %v\n%s", what, p, debug.Stack())}
			}
		}()
		v, err := fn(ctx)
		done <- result{v, err}
	}()
	select {
	case r := <-done:
		return r.v, withCause(ctx, r.err)
	case <-ctx.Done():
		// Prefer a result that arrived at the same time.
		select {
		case r := <-done:
			return r.v, withCause(ctx, r.err)
		default:
		}
		var zero T
		return zero, context.Cause(ctx)
	}
}

// withCause replaces the error of an ended ctx, such as
// context.DeadlineExceeded, with its cause, which explains a timeout.
func withCause(ctx context.Context, err error) error {
	if err != nil && ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return context.Cause(ctx)
	}
	return err
}
