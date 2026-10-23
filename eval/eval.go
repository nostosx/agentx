// Package eval evaluates agents and other LLM tasks against datasets of
// cases.
//
// A Dataset holds Cases, each an input with an optional expected output
// and metadata. Dataset.Evaluate runs a Task on every case, with bounded
// concurrency, repetitions and timeouts, and scores each output with
// Evaluators. A failing or panicking task or evaluator affects only its
// own case. The resulting Report aggregates pass rates, scores, token
// usage and durations, prints as a table or JSON, and compares with a
// baseline report. Run does the same inside a Go test and fails it below a
// threshold.
//
// The model follows pydantic-evals (https://pydantic.dev/docs/ai/evals/):
// evaluators return assertions (pass or fail), scores and labels, each
// with an optional reason. Tool trajectories are matched as in Google ADK
// (https://adk.dev/evaluate/criteria/), and SimulateUser drives
// multi-turn conversations with a simulated user as ADK's user simulation
// does (https://adk.dev/evaluate/user-sim/). Spans recorded with Tracer
// support span-based evaluation.
//
// Package eval/replay records and replays HTTP interactions, so provider
// tests and evaluations run deterministically without network access.
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nostosx/agentx/loop"
)

// Case is one scenario of a dataset.
type Case[In, Out any] struct {
	// Name identifies the case in reports. Names are unique within a
	// dataset; Evaluate names unnamed cases "case N", counting from 1.
	Name  string `json:"name,omitempty"`
	Input In     `json:"input"`
	// Expected is the expected output, or nil if the case has none.
	// Evaluators that need it, such as EqualsExpected, skip cases without
	// one.
	Expected *Out `json:"expected,omitempty"`
	// Metadata is free-form data for evaluators, such as a difficulty or
	// a category.
	Metadata map[string]any `json:"metadata,omitempty"`
	// Evaluators run on this case in addition to the dataset's.
	Evaluators []Evaluator `json:"-"`
}

// Dataset is a named collection of cases for one task.
type Dataset[In, Out any] struct {
	Name  string          `json:"name,omitempty"`
	Cases []Case[In, Out] `json:"cases"`
	// Evaluators run on every case.
	Evaluators []Evaluator `json:"-"`
}

// Task produces the output for one input. Tasks that run an agent also
// return its run result, which evaluators such as ToolTrajectory and
// MaxTokens inspect; other tasks return a nil result. A non-nil error
// fails the case without running its evaluators.
type Task[In, Out any] func(ctx context.Context, in In) (Out, *loop.Result, error)

// Kind is the kind of an Evaluation.
type Kind string

const (
	// KindAssertion is a check that passes or fails.
	KindAssertion Kind = "assertion"
	// KindScore is a number, by convention between 0 and 1.
	KindScore Kind = "score"
	// KindLabel is a category, such as an error class.
	KindLabel Kind = "label"
)

// Evaluation is one result of an evaluator. Assertions decide whether a
// case passes; scores and labels are aggregated in reports.
type Evaluation struct {
	Name string `json:"name"`
	Kind Kind   `json:"kind"`
	// Pass is the outcome of an assertion.
	Pass bool `json:"pass"`
	// Score is the value of a score.
	Score float64 `json:"score"`
	// Label is the value of a label.
	Label string `json:"label"`
	// Reason explains the result.
	Reason string `json:"reason,omitempty"`
	// Evaluator is the name of the evaluator that returned it.
	Evaluator string `json:"evaluator,omitempty"`
}

// MarshalJSON encodes only the value field of the evaluation's kind.
func (e Evaluation) MarshalJSON() ([]byte, error) {
	out := struct {
		Name      string   `json:"name"`
		Kind      Kind     `json:"kind"`
		Pass      *bool    `json:"pass,omitempty"`
		Score     *float64 `json:"score,omitempty"`
		Label     *string  `json:"label,omitempty"`
		Reason    string   `json:"reason,omitempty"`
		Evaluator string   `json:"evaluator,omitempty"`
	}{Name: e.Name, Kind: e.Kind, Reason: e.Reason, Evaluator: e.Evaluator}
	switch e.Kind {
	case KindAssertion:
		out.Pass = &e.Pass
	case KindScore:
		out.Score = &e.Score
	case KindLabel:
		out.Label = &e.Label
	}
	return json.Marshal(out)
}

// Assertion returns an assertion.
func Assertion(name string, pass bool, reason string) Evaluation {
	return Evaluation{Name: name, Kind: KindAssertion, Pass: pass, Reason: reason}
}

// Score returns a score.
func Score(name string, value float64, reason string) Evaluation {
	return Evaluation{Name: name, Kind: KindScore, Score: value, Reason: reason}
}

// Label returns a label.
func Label(name, value, reason string) Evaluation {
	return Evaluation{Name: name, Kind: KindLabel, Label: value, Reason: reason}
}

// String formats the evaluation's value.
func (e Evaluation) String() string {
	switch e.Kind {
	case KindAssertion:
		if e.Pass {
			return e.Name + " ✔"
		}
		return e.Name + " ✗"
	case KindScore:
		return fmt.Sprintf("%s=%.3g", e.Name, e.Score)
	}
	return e.Name + "=" + e.Label
}

// EvaluatorContext is what an evaluator sees of one run of a case.
type EvaluatorContext struct {
	// Name is the case name.
	Name string
	// Repeat numbers the run of the case, from 0; see Repeat.
	Repeat int
	Input  any
	Output any
	// Expected is the expected output, or nil if the case has none.
	Expected any
	Metadata map[string]any
	// Result is the run result returned by the task, or nil.
	Result *loop.Result
	// Duration is how long the task took.
	Duration time.Duration
	// Spans are the root spans recorded by Tracer during the task.
	Spans Spans
}

// Evaluator scores the output of a case.
//
// Evaluate returns any number of evaluations; none means the evaluator
// does not apply to the case. An error, or a panic, is reported as an
// evaluator failure of the case, which then does not pass. Evaluators
// must be safe for concurrent use.
type Evaluator interface {
	Name() string
	Evaluate(ctx context.Context, ec *EvaluatorContext) ([]Evaluation, error)
}

type funcEvaluator struct {
	name string
	fn   func(context.Context, *EvaluatorContext) ([]Evaluation, error)
}

func (f funcEvaluator) Name() string { return f.name }

func (f funcEvaluator) Evaluate(ctx context.Context, ec *EvaluatorContext) ([]Evaluation, error) {
	return f.fn(ctx, ec)
}

// Func returns an evaluator that calls fn.
func Func(name string, fn func(ctx context.Context, ec *EvaluatorContext) ([]Evaluation, error)) Evaluator {
	return funcEvaluator{name, fn}
}

// Check returns an evaluator with a single assertion named name, which
// passes when fn returns true. The string is the reason.
func Check(name string, fn func(ec *EvaluatorContext) (bool, string)) Evaluator {
	return Func(name, func(_ context.Context, ec *EvaluatorContext) ([]Evaluation, error) {
		pass, reason := fn(ec)
		return []Evaluation{Assertion(name, pass, reason)}, nil
	})
}

// Typed is an EvaluatorContext with the input, output and expected output
// of a Dataset[In, Out] typed. Its fields shadow the untyped ones of the
// embedded context.
type Typed[In, Out any] struct {
	*EvaluatorContext
	Input    In
	Output   Out
	Expected *Out
}

// typed returns the typed view of ec.
func typed[In, Out any](ec *EvaluatorContext) (Typed[In, Out], error) {
	t := Typed[In, Out]{EvaluatorContext: ec}
	var ok bool
	if t.Input, ok = ec.Input.(In); !ok && ec.Input != nil {
		return t, fmt.Errorf("eval: input is %T, not %T", ec.Input, t.Input)
	}
	if t.Output, ok = ec.Output.(Out); !ok && ec.Output != nil {
		return t, fmt.Errorf("eval: output is %T, not %T", ec.Output, t.Output)
	}
	if ec.Expected != nil {
		exp, ok := ec.Expected.(Out)
		if !ok {
			return t, fmt.Errorf("eval: expected output is %T, not %T", ec.Expected, exp)
		}
		t.Expected = &exp
	}
	return t, nil
}

// TypedFunc returns an evaluator that calls fn with the typed view of the
// context. A case whose values have other types fails with an evaluator
// error.
func TypedFunc[In, Out any](name string, fn func(ctx context.Context, t Typed[In, Out]) ([]Evaluation, error)) Evaluator {
	return Func(name, func(ctx context.Context, ec *EvaluatorContext) ([]Evaluation, error) {
		t, err := typed[In, Out](ec)
		if err != nil {
			return nil, err
		}
		return fn(ctx, t)
	})
}

// TypedCheck is Check with the typed view of the context.
func TypedCheck[In, Out any](name string, fn func(t Typed[In, Out]) (bool, string)) Evaluator {
	return TypedFunc(name, func(_ context.Context, t Typed[In, Out]) ([]Evaluation, error) {
		pass, reason := fn(t)
		return []Evaluation{Assertion(name, pass, reason)}, nil
	})
}

// Named renames the evaluations of e: a single evaluation is named name,
// and several are prefixed with name and an underscore. The evaluator
// itself is named name too.
func Named(name string, e Evaluator) Evaluator {
	return Func(name, func(ctx context.Context, ec *EvaluatorContext) ([]Evaluation, error) {
		evs, err := e.Evaluate(ctx, ec)
		if len(evs) == 1 {
			evs[0].Name = name
		} else {
			for i := range evs {
				evs[i].Name = name + "_" + evs[i].Name
			}
		}
		return evs, err
	})
}
