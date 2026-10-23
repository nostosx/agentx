package eval_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nostosx/agentx/eval"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
)

func upper(_ context.Context, in string) (string, *loop.Result, error) {
	return strings.ToUpper(in), nil, nil
}

func TestEvaluate(t *testing.T) {
	t.Parallel()
	ds := &eval.Dataset[string, string]{
		Name: "upper",
		Cases: []eval.Case[string, string]{
			{Name: "a", Input: "abc", Expected: new("ABC"), Metadata: map[string]any{"kind": "easy"}},
			{Name: "b", Input: "xyz", Expected: new("XYZ!")},
			{Input: "none"},
		},
		Evaluators: []eval.Evaluator{eval.EqualsExpected()},
	}
	ds.Cases[0].Evaluators = []eval.Evaluator{eval.Contains("B")}
	var seen []string
	var mu sync.Mutex
	rep, err := ds.Evaluate(t.Context(), upper,
		eval.Experiment("v1"),
		eval.WithEvaluators(eval.Func("label", func(_ context.Context, ec *eval.EvaluatorContext) ([]eval.Evaluation, error) {
			return []eval.Evaluation{eval.Label("len", fmt.Sprint(len(ec.Output.(string))), ""), eval.Score("ratio", 0.5, "")}, nil
		})),
		eval.OnResult(func(cr *eval.CaseResult) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, cr.Name)
		}))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Name != "v1" || rep.Dataset != "upper" || rep.Repeat != 1 || len(rep.Cases) != 3 || len(seen) != 3 {
		t.Fatalf("report = %+v, seen %v", rep, seen)
	}
	a, b, c := rep.Cases[0], rep.Cases[1], rep.Cases[2]
	if a.Name != "a" || !a.Passed() || a.Output != "ABC" || a.Expected != "ABC" || len(a.Evaluations) != 4 {
		t.Fatalf("case a = %+v", a)
	}
	if a.Evaluations[0].Name != "EqualsExpected" || a.Evaluations[1].Name != "Contains" || a.Evaluations[2].Evaluator != "label" {
		t.Fatalf("evaluator order = %+v", a.Evaluations)
	}
	if b.Passed() || len(b.Failures()) != 1 || !strings.Contains(b.Failures()[0], `want "XYZ!"`) {
		t.Fatalf("case b = %+v", b)
	}
	// Unnamed cases are numbered; evaluators needing an expected output skip.
	if c.Name != "case 3" || !c.Passed() || c.Expected != nil || len(c.Evaluations) != 2 {
		t.Fatalf("case c = %+v", c)
	}
	s := rep.Summary
	if s.Runs != 3 || s.Passed != 2 || s.Errors != 0 || s.PassRate != 2.0/3 ||
		s.Assertions["EqualsExpected"] != 0.5 || s.Assertions["Contains"] != 1 ||
		s.Scores["ratio"] != 0.5 || s.Labels["len"]["3"] != 2 || s.Labels["len"]["4"] != 1 {
		t.Fatalf("summary = %+v", s)
	}
}

func TestEvaluateInvalid(t *testing.T) {
	t.Parallel()
	ds := &eval.Dataset[string, string]{Cases: []eval.Case[string, string]{{Name: "x"}, {Name: "x"}}}
	if _, err := ds.Evaluate(t.Context(), upper); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicates: %v", err)
	}
	if _, err := ds.Evaluate(t.Context(), nil); err == nil {
		t.Fatal("nil task accepted")
	}
	// The second case takes the automatic name of the first.
	ds.Cases = []eval.Case[string, string]{{}, {Name: "case 1"}}
	if _, err := ds.Evaluate(t.Context(), upper); err == nil {
		t.Fatal("clashing automatic name accepted")
	}
	empty := &eval.Dataset[string, string]{}
	rep, err := empty.Evaluate(t.Context(), upper)
	if err != nil || rep.Summary.Runs != 0 || rep.Summary.PassRate != 0 || !strings.Contains(rep.String(), "0 runs") {
		t.Fatalf("empty = %+v, %v", rep, err)
	}
}

func TestConcurrencyAndRepeat(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := &eval.Dataset[int, int]{}
		for i := range 6 {
			ds.Cases = append(ds.Cases, eval.Case[int, int]{Name: fmt.Sprint(i), Input: i})
		}
		var running, peak atomic.Int32
		var calls [6]atomic.Int32
		task := func(_ context.Context, in int) (int, *loop.Result, error) {
			n := running.Add(1)
			defer running.Add(-1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			// Odd cases pass every other run.
			c := calls[in].Add(1)
			res := &loop.Result{Status: loop.StatusCompleted, Turns: 1, Usage: model.Usage{InputTokens: 10, OutputTokens: 2}}
			if in%2 == 1 && c%2 == 0 {
				return in, res, errors.New("flaky")
			}
			return in, res, nil
		}
		rep, err := ds.Evaluate(t.Context(), task, eval.Concurrency(2), eval.Repeat(4))
		if err != nil {
			t.Fatal(err)
		}
		if p := peak.Load(); p != 2 {
			t.Fatalf("peak concurrency %d", p)
		}
		if len(rep.Cases) != 24 || rep.Repeat != 4 {
			t.Fatalf("runs = %d", len(rep.Cases))
		}
		for i, c := range rep.Cases {
			if c.Name != fmt.Sprint(i/4) || c.Repeat != i%4 || c.Turns != 1 || c.Usage.TotalTokens() != 12 {
				t.Fatalf("run %d = %+v", i, c)
			}
		}
		if rep.Summary.Usage.TotalTokens() != 24*12 || rep.Summary.MeanTokens() != 12 {
			t.Fatalf("usage = %+v", rep.Summary.Usage)
		}
		byCase := rep.ByCase()
		if len(byCase) != 6 {
			t.Fatalf("by case = %+v", byCase)
		}
		for i, c := range byCase {
			wantPassed := 4
			if i%2 == 1 {
				wantPassed = 2
			}
			if calls[i].Load() != 4 || c.Runs != 4 || c.Passed != wantPassed || c.PassRate != float64(wantPassed)/4 || c.Usage.TotalTokens() != 48 {
				t.Fatalf("case %d = %+v, calls = %d", i, c, calls[i].Load())
			}
		}
		if rep.Summary.Errors != 6 || rep.Summary.Passed != 18 || rep.Summary.PassRate != 0.75 {
			t.Fatalf("summary = %+v", rep.Summary)
		}
		if !strings.Contains(rep.String(), "0 #1") {
			t.Fatalf("repeated runs are not numbered:\n%s", rep)
		}
	})
}

func TestFailureIsolation(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := &eval.Dataset[string, string]{Cases: []eval.Case[string, string]{
			{Name: "ok", Input: "ok"},
			{Name: "error", Input: "error"},
			{Name: "panic", Input: "panic"},
			{Name: "hang", Input: "hang"},
			{Name: "slow-evaluator", Input: "slow"},
		}}
		release := make(chan struct{})
		defer close(release)
		task := func(ctx context.Context, in string) (string, *loop.Result, error) {
			switch in {
			case "error":
				return "", &loop.Result{Status: loop.StatusFailed, Usage: model.Usage{InputTokens: 3}}, errors.New("boom")
			case "panic":
				panic("kaput")
			case "hang":
				<-release // ignores ctx
			}
			return in, nil, nil
		}
		evals := []eval.Evaluator{
			eval.Func("panicky", func(_ context.Context, ec *eval.EvaluatorContext) ([]eval.Evaluation, error) {
				if ec.Output == "ok" {
					panic("evaluator kaput")
				}
				return nil, nil
			}),
			eval.Func("slow", func(ctx context.Context, ec *eval.EvaluatorContext) ([]eval.Evaluation, error) {
				if ec.Output == "slow" {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return []eval.Evaluation{eval.Assertion("fine", true, "")}, nil
			}),
		}
		rep, err := ds.Evaluate(t.Context(), task, eval.WithEvaluators(evals...),
			eval.Timeout(50*time.Millisecond), eval.EvaluatorTimeout(50*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		byName := map[string]*eval.CaseResult{}
		for _, c := range rep.Cases {
			byName[c.Name] = c
		}
		if c := byName["ok"]; c.Passed() || len(c.EvaluatorErrors) != 1 || !strings.Contains(c.EvaluatorErrors[0].Error, "evaluator panicked: evaluator kaput") || len(c.Evaluations) != 1 {
			t.Fatalf("ok = %+v", c)
		}
		if c := byName["error"]; c.Error != "boom" || c.Status != loop.StatusFailed || c.Usage.InputTokens != 3 || len(c.Evaluations) != 0 {
			t.Fatalf("error = %+v", c)
		}
		if c := byName["panic"]; !strings.Contains(c.Error, "task panicked: kaput") {
			t.Fatalf("panic = %+v", c)
		}
		if c := byName["hang"]; !strings.Contains(c.Error, "timed out after 50ms") {
			t.Fatalf("hang = %+v", c)
		}
		if c := byName["slow-evaluator"]; len(c.EvaluatorErrors) != 1 || !strings.Contains(c.EvaluatorErrors[0].Error, "evaluator timed out") {
			t.Fatalf("slow = %+v", c)
		}
		if rep.Summary.Errors != 3 || rep.Summary.Passed != 0 {
			t.Fatalf("summary = %+v", rep.Summary)
		}
		text := rep.String()
		for _, want := range []string{"ERROR", "FAIL", "Failures:", "panicky failed:", "error: boom", "panicky!"} {
			if !strings.Contains(text, want) {
				t.Errorf("report lacks %q:\n%s", want, text)
			}
		}
	})
}

func TestEvaluateCanceled(t *testing.T) {
	t.Parallel()
	ds := &eval.Dataset[int, int]{}
	for i := range 5 {
		ds.Cases = append(ds.Cases, eval.Case[int, int]{Input: i})
	}
	ctx, cancel := context.WithCancel(t.Context())
	task := func(ctx context.Context, in int) (int, *loop.Result, error) {
		if in == 0 {
			cancel()
		}
		<-ctx.Done()
		return 0, nil, ctx.Err()
	}
	rep, err := ds.Evaluate(ctx, task, eval.Concurrency(1))
	if !errors.Is(err, context.Canceled) || rep == nil || len(rep.Cases) != 5 {
		t.Fatalf("Evaluate = %+v, %v", rep, err)
	}
	for _, c := range rep.Cases {
		if c == nil || !strings.Contains(c.Error, "canceled") {
			t.Fatalf("case = %+v", c)
		}
	}
}

func TestTypedAndNamed(t *testing.T) {
	t.Parallel()
	type in struct{ N int }
	type out struct{ Double int }
	ds := &eval.Dataset[in, out]{Cases: []eval.Case[in, out]{{Input: in{2}, Expected: &out{4}}}}
	double := eval.TypedCheck("double", func(t eval.Typed[in, out]) (bool, string) {
		return t.Output.Double == t.Input.N*2 && t.Expected.Double == 4 && t.Name == "case 1", ""
	})
	scores := eval.Named("quality", eval.Func("two", func(context.Context, *eval.EvaluatorContext) ([]eval.Evaluation, error) {
		return []eval.Evaluation{eval.Score("a", 1, ""), eval.Score("b", 0, "")}, nil
	}))
	wrong := eval.TypedCheck("wrong", func(eval.Typed[string, string]) (bool, string) { return true, "" })
	rep, err := ds.Evaluate(t.Context(), func(_ context.Context, v in) (out, *loop.Result, error) {
		return out{v.N * 2}, nil, nil
	}, eval.WithEvaluators(double, eval.Named("same", eval.EqualsExpected()), scores, wrong))
	if err != nil {
		t.Fatal(err)
	}
	c := rep.Cases[0]
	names := []string{}
	for _, ev := range c.Evaluations {
		names = append(names, ev.Name)
	}
	if strings.Join(names, ",") != "double,same,quality_a,quality_b" || !c.Evaluations[0].Pass || !c.Evaluations[1].Pass {
		t.Fatalf("evaluations = %+v", c.Evaluations)
	}
	if len(c.EvaluatorErrors) != 1 || !strings.Contains(c.EvaluatorErrors[0].Error, "input is eval_test.in, not string") {
		t.Fatalf("type mismatch = %+v", c.EvaluatorErrors)
	}
}

func TestEvaluationJSON(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		ev   eval.Evaluation
		want string
	}{
		{eval.Assertion("a", false, "why"), `{"name":"a","kind":"assertion","pass":false,"reason":"why"}`},
		{eval.Score("s", 0, ""), `{"name":"s","kind":"score","score":0}`},
		{eval.Label("l", "x", ""), `{"name":"l","kind":"label","label":"x"}`},
	} {
		b, err := json.Marshal(tc.ev)
		if err != nil || string(b) != tc.want {
			t.Errorf("Marshal(%v) = %s, %v", tc.ev, b, err)
		}
		var back eval.Evaluation
		if err := json.Unmarshal(b, &back); err != nil || back != tc.ev {
			t.Errorf("round trip = %+v, %v", back, err)
		}
	}
	for ev, want := range map[eval.Evaluation]string{
		eval.Score("s", 0.25, ""):      "s=0.25",
		eval.Assertion("a", true, ""):  "a ✔",
		eval.Assertion("a", false, ""): "a ✗",
		eval.Label("l", "x", ""):       "l=x",
	} {
		if got := ev.String(); got != want {
			t.Errorf("String = %q, want %q", got, want)
		}
	}
}

func TestReportTextJSONAndCompare(t *testing.T) {
	t.Parallel()
	ds := &eval.Dataset[string, string]{
		Name: "words",
		Cases: []eval.Case[string, string]{
			{Name: "keep", Input: "a", Expected: new("A")},
			{Name: "break", Input: "b", Expected: new("B")},
			{Name: "fix", Input: "c", Expected: new("C")},
			{Name: "old", Input: "d", Expected: new("D")},
		},
		Evaluators: []eval.Evaluator{eval.EqualsExpected(), eval.Func("score", func(_ context.Context, ec *eval.EvaluatorContext) ([]eval.Evaluation, error) {
			return []eval.Evaluation{eval.Score("quality", float64(len(ec.Output.(string))), "")}, nil
		})},
	}
	baseline, err := ds.Evaluate(t.Context(), func(_ context.Context, in string) (string, *loop.Result, error) {
		if in == "c" {
			return "wrong", nil, nil
		}
		return strings.ToUpper(in), &loop.Result{Usage: model.Usage{InputTokens: 4}}, nil
	}, eval.Experiment("base"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "base.json")
	if err := baseline.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := eval.LoadReport(path)
	if err != nil || loaded.Name != "base" || len(loaded.Cases) != 4 || loaded.Summary.Passed != 3 ||
		loaded.Cases[0].Evaluations[0].Kind != eval.KindAssertion || !loaded.Cases[0].Evaluations[0].Pass {
		t.Fatalf("LoadReport = %+v, %v", loaded, err)
	}
	if _, err := eval.LoadReport(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing report loaded")
	}

	ds.Cases = append(ds.Cases[:3], eval.Case[string, string]{Name: "new", Input: "e"})
	candidate, err := ds.Evaluate(t.Context(), func(_ context.Context, in string) (string, *loop.Result, error) {
		if in == "b" {
			return "nope", nil, nil
		}
		return strings.ToUpper(in), nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cmp := eval.Compare(loaded, candidate)
	changes := map[string]eval.CaseChange{}
	for _, d := range cmp.Cases {
		changes[d.Name] = d.Change
	}
	want := map[string]eval.CaseChange{"keep": eval.CaseUnchanged, "break": eval.CaseRegressed, "fix": eval.CaseImproved, "new": eval.CaseAdded, "old": eval.CaseRemoved}
	for k, v := range want {
		if changes[k] != v {
			t.Errorf("%s: change %s, want %s", k, changes[k], v)
		}
	}
	if cmp.Cases[len(cmp.Cases)-1].Name != "old" || len(cmp.Regressions()) != 1 || cmp.Regressions()[0].Name != "break" {
		t.Fatalf("cases = %+v", cmp.Cases)
	}
	if cmp.Baseline != "base" || cmp.Candidate != "candidate" || cmp.PassRate.Baseline != 0.75 || cmp.PassRate.Candidate != 0.75 ||
		cmp.Assertions["EqualsExpected"].Candidate != 2.0/3 || cmp.TotalTokens.Baseline != 12 || cmp.TotalTokens.Candidate != 0 {
		t.Fatalf("comparison = %+v", cmp)
	}
	if d := cmp.Scores["quality"]; d.Baseline != 2 || d.Candidate != 1.75 {
		t.Fatalf("score delta = %+v", d)
	}
	text := cmp.String()
	for _, w := range []string{"Comparison of base (baseline) and candidate (candidate)", "Pass rate: 75.0% -> 75.0% (+0.0)", "Score quality: 2.000 -> 1.750 (-0.250)", "regressed", "removed"} {
		if !strings.Contains(text, w) {
			t.Errorf("comparison lacks %q:\n%s", w, text)
		}
	}
	rtext := candidate.String()
	for _, w := range []string{"Evaluation of words: 4 runs", "CASE", "break", "1/1", "0/1 ✗ EqualsExpected", "quality=4", "Pass rate: 75.0% (3/4 runs)", "Assertions: EqualsExpected 66.7%", "Scores: quality 1.750", "Tokens: 0 total", "Duration: mean", `break: EqualsExpected: got "nope", want "B"`} {
		if !strings.Contains(rtext, w) {
			t.Errorf("report lacks %q:\n%s", w, rtext)
		}
	}
}

// fakeTB records the failures of Run.
type fakeTB struct {
	testing.TB
	ctx    context.Context
	mu     sync.Mutex
	errors []string
	logs   []string
	fatal  bool
}

func (f *fakeTB) Helper()                  {}
func (f *fakeTB) Context() context.Context { return f.ctx }
func (f *fakeTB) Log(args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, fmt.Sprint(args...))
}

func (f *fakeTB) Errorf(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errors = append(f.errors, fmt.Sprintf(format, args...))
}

func (f *fakeTB) Fatalf(format string, args ...any) {
	f.Errorf(format, args...)
	f.fatal = true
}

func TestRun(t *testing.T) {
	t.Parallel()
	ds := &eval.Dataset[string, string]{
		Cases: []eval.Case[string, string]{
			{Name: "a", Input: "a", Expected: new("A")},
			{Name: "b", Input: "b", Expected: new("x")},
		},
		Evaluators: []eval.Evaluator{eval.EqualsExpected(), eval.ResponseMatch(0.5)},
	}
	tb := &fakeTB{ctx: t.Context()}
	rep := eval.Run(tb, ds, upper, eval.Threshold(0.5), eval.MinScore("ResponseMatch_score", 0.5))
	if rep == nil || len(tb.errors) != 0 || len(tb.logs) != 1 || !strings.Contains(tb.logs[0], "Pass rate: 50.0%") {
		t.Fatalf("Run passing = %v, logs %v", tb.errors, tb.logs)
	}

	tb = &fakeTB{ctx: t.Context()}
	eval.Run(tb, ds, upper, eval.MinScore("ResponseMatch_score", 0.9), eval.MinScore("missing", 1))
	if len(tb.errors) != 3 || !strings.Contains(tb.errors[0], "pass rate 50.0% is below the threshold 100.0%") ||
		!strings.Contains(tb.errors[1], "mean score ResponseMatch_score 0.500 is below 0.900") || !strings.Contains(tb.errors[2], "no score missing") {
		t.Fatalf("Run failing = %q", tb.errors)
	}

	baseline := &eval.Report{Cases: []*eval.CaseResult{{Name: "a"}, {Name: "b"}}}
	tb = &fakeTB{ctx: t.Context()}
	eval.Run(tb, ds, upper, eval.Threshold(0), eval.Baseline(baseline))
	if len(tb.errors) != 1 || !strings.Contains(tb.errors[0], "case b regressed: 0/1 runs passed, baseline 1/1") || len(tb.logs) != 2 {
		t.Fatalf("Run with baseline = %q, logs %q", tb.errors, tb.logs)
	}

	tb = &fakeTB{ctx: t.Context()}
	if eval.Run(tb, ds, nil) != nil || !tb.fatal {
		t.Fatalf("Run with a nil task = %q", tb.errors)
	}
}
