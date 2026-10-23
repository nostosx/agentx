package eval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
)

// Report is the outcome of an evaluation.
type Report struct {
	// Name is the experiment name; see Experiment.
	Name string `json:"name,omitempty"`
	// Dataset is the name of the dataset.
	Dataset string `json:"dataset,omitempty"`
	// Repeat is the number of runs of each case.
	Repeat   int           `json:"repeat"`
	Started  time.Time     `json:"started"`
	Duration time.Duration `json:"duration"`
	Summary  Summary       `json:"summary"`
	// Cases holds every run, in dataset order, with the runs of a case
	// together.
	Cases []*CaseResult `json:"cases"`
}

// CaseResult is the outcome of one run of a case.
type CaseResult struct {
	Name string `json:"name"`
	// Repeat numbers the run of the case, from 0.
	Repeat   int            `json:"repeat,omitempty"`
	Input    any            `json:"input"`
	Expected any            `json:"expected,omitempty"`
	Output   any            `json:"output,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	// Evaluations are the results of the evaluators, in order.
	Evaluations []Evaluation `json:"evaluations,omitempty"`
	// EvaluatorErrors lists the evaluators that failed.
	EvaluatorErrors []EvaluatorError `json:"evaluator_errors,omitempty"`
	// Error is the error of the task; its evaluators did not run.
	Error string `json:"error,omitempty"`
	// Status, Turns and Usage come from the task's run result, if any.
	Status   loop.Status   `json:"status,omitempty"`
	Turns    int           `json:"turns,omitempty"`
	Usage    model.Usage   `json:"usage,omitzero"`
	Duration time.Duration `json:"duration"`
}

// EvaluatorError reports an evaluator that returned an error or panicked.
type EvaluatorError struct {
	Evaluator string `json:"evaluator"`
	Error     string `json:"error"`
}

// Passed reports whether the run passed: the task succeeded, every
// evaluator ran and every assertion passed.
func (c *CaseResult) Passed() bool {
	if c.Error != "" || len(c.EvaluatorErrors) > 0 {
		return false
	}
	for _, ev := range c.Evaluations {
		if ev.Kind == KindAssertion && !ev.Pass {
			return false
		}
	}
	return true
}

// Failures describes why the run did not pass.
func (c *CaseResult) Failures() []string {
	var out []string
	if c.Error != "" {
		out = append(out, "error: "+c.Error)
	}
	for _, e := range c.EvaluatorErrors {
		out = append(out, e.Evaluator+" failed: "+e.Error)
	}
	for _, ev := range c.Evaluations {
		if ev.Kind == KindAssertion && !ev.Pass {
			s := ev.Name
			if ev.Reason != "" {
				s += ": " + ev.Reason
			}
			out = append(out, s)
		}
	}
	return out
}

// label names the run in text output.
func (c *CaseResult) label(repeat int) string {
	if repeat > 1 {
		return c.Name + " #" + strconv.Itoa(c.Repeat+1)
	}
	return c.Name
}

// Summary aggregates the runs of a report.
type Summary struct {
	Runs   int `json:"runs"`
	Passed int `json:"passed"`
	// Errors counts the runs whose task failed.
	Errors   int     `json:"errors"`
	PassRate float64 `json:"pass_rate"`
	// Assertions is the pass rate of each assertion, by name, among the
	// runs that have it.
	Assertions map[string]float64 `json:"assertions,omitempty"`
	// Scores is the mean of each score, by name.
	Scores map[string]float64 `json:"scores,omitempty"`
	// Labels counts the values of each label, by name.
	Labels map[string]map[string]int `json:"labels,omitempty"`
	// Usage is the total usage of all runs.
	Usage        model.Usage   `json:"usage"`
	MeanDuration time.Duration `json:"mean_duration"`
	P50Duration  time.Duration `json:"p50_duration"`
	P95Duration  time.Duration `json:"p95_duration"`
	MaxDuration  time.Duration `json:"max_duration"`
}

// MeanTokens returns the mean total tokens of a run.
func (s Summary) MeanTokens() float64 {
	if s.Runs == 0 {
		return 0
	}
	return float64(s.Usage.TotalTokens()) / float64(s.Runs)
}

// summarize aggregates runs.
func summarize(runs []*CaseResult) Summary {
	s := Summary{Runs: len(runs)}
	type acc struct{ sum, n float64 }
	asserts, scores := map[string]*acc{}, map[string]*acc{}
	add := func(m map[string]*acc, name string, v float64) {
		a := m[name]
		if a == nil {
			a = &acc{}
			m[name] = a
		}
		a.sum += v
		a.n++
	}
	durations := make([]time.Duration, 0, len(runs))
	var total time.Duration
	for _, c := range runs {
		if c.Passed() {
			s.Passed++
		}
		if c.Error != "" {
			s.Errors++
		}
		s.Usage.Add(c.Usage)
		durations = append(durations, c.Duration)
		total += c.Duration
		for _, ev := range c.Evaluations {
			switch ev.Kind {
			case KindAssertion:
				v := 0.0
				if ev.Pass {
					v = 1
				}
				add(asserts, ev.Name, v)
			case KindScore:
				add(scores, ev.Name, ev.Score)
			case KindLabel:
				if s.Labels == nil {
					s.Labels = map[string]map[string]int{}
				}
				if s.Labels[ev.Name] == nil {
					s.Labels[ev.Name] = map[string]int{}
				}
				s.Labels[ev.Name][ev.Label]++
			}
		}
	}
	means := func(m map[string]*acc) map[string]float64 {
		if len(m) == 0 {
			return nil
		}
		out := make(map[string]float64, len(m))
		for k, a := range m {
			out[k] = a.sum / a.n
		}
		return out
	}
	s.Assertions, s.Scores = means(asserts), means(scores)
	if len(runs) > 0 {
		s.PassRate = float64(s.Passed) / float64(len(runs))
		slices.Sort(durations)
		s.MeanDuration = total / time.Duration(len(runs))
		s.P50Duration = percentile(durations, 50)
		s.P95Duration = percentile(durations, 95)
		s.MaxDuration = durations[len(durations)-1]
	}
	return s
}

// percentile returns the nearest-rank percentile of sorted values.
func percentile(sorted []time.Duration, p int) time.Duration {
	rank := (p*len(sorted) + 99) / 100
	return sorted[max(rank, 1)-1]
}

// CaseSummary aggregates the runs of one case.
type CaseSummary struct {
	Name     string  `json:"name"`
	Runs     int     `json:"runs"`
	Passed   int     `json:"passed"`
	PassRate float64 `json:"pass_rate"`
	// Scores is the mean of each score, by name.
	Scores       map[string]float64 `json:"scores,omitempty"`
	Usage        model.Usage        `json:"usage"`
	MeanDuration time.Duration      `json:"mean_duration"`
}

// ByCase aggregates the runs of each case, in dataset order. It shows
// which cases are flaky when cases are repeated.
func (r *Report) ByCase() []CaseSummary {
	var order []string
	groups := map[string][]*CaseResult{}
	for _, c := range r.Cases {
		if _, ok := groups[c.Name]; !ok {
			order = append(order, c.Name)
		}
		groups[c.Name] = append(groups[c.Name], c)
	}
	out := make([]CaseSummary, len(order))
	for i, name := range order {
		s := summarize(groups[name])
		out[i] = CaseSummary{
			Name: name, Runs: s.Runs, Passed: s.Passed, PassRate: s.PassRate,
			Scores: s.Scores, Usage: s.Usage, MeanDuration: s.MeanDuration,
		}
	}
	return out
}

// WriteText prints the report as a table of runs followed by the summary
// and the reasons of failed runs.
func (r *Report) WriteText(w io.Writer) error {
	var b bytes.Buffer
	title := "Evaluation"
	if r.Name != "" {
		title += " " + r.Name
	}
	if r.Dataset != "" {
		title += " of " + r.Dataset
	}
	fmt.Fprintf(&b, "%s: %d runs in %s\n\n", title, len(r.Cases), fmtDuration(r.Duration))

	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	head := "CASE\tRESULT\tASSERTIONS\tSCORES\tTOKENS\tDURATION"
	fmt.Fprintln(tw, head)
	for _, c := range r.Cases {
		result := "pass"
		switch {
		case c.Error != "":
			result = "ERROR"
		case !c.Passed():
			result = "FAIL"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n", c.label(r.Repeat), result,
			assertionCell(c), valueCell(c), c.Usage.TotalTokens(), fmtDuration(c.Duration))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	s := r.Summary
	fmt.Fprintf(&b, "\nPass rate: %s (%d/%d runs", percent(s.PassRate), s.Passed, s.Runs)
	if s.Errors > 0 {
		fmt.Fprintf(&b, ", %d errors", s.Errors)
	}
	b.WriteString(")\n")
	if len(s.Assertions) > 0 {
		b.WriteString("Assertions:")
		for _, k := range slices.Sorted(maps.Keys(s.Assertions)) {
			fmt.Fprintf(&b, " %s %s", k, percent(s.Assertions[k]))
		}
		b.WriteString("\n")
	}
	if len(s.Scores) > 0 {
		b.WriteString("Scores:")
		for _, k := range slices.Sorted(maps.Keys(s.Scores)) {
			fmt.Fprintf(&b, " %s %.3f", k, s.Scores[k])
		}
		b.WriteString("\n")
	}
	for _, k := range slices.Sorted(maps.Keys(s.Labels)) {
		fmt.Fprintf(&b, "Label %s:", k)
		for _, v := range slices.Sorted(maps.Keys(s.Labels[k])) {
			fmt.Fprintf(&b, " %s=%d", v, s.Labels[k][v])
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Tokens: %d total, %.0f per run (input %d, output %d)\n",
		s.Usage.TotalTokens(), s.MeanTokens(), s.Usage.PromptTokens(), s.Usage.OutputTokens)
	fmt.Fprintf(&b, "Duration: mean %s, p50 %s, p95 %s, max %s\n",
		fmtDuration(s.MeanDuration), fmtDuration(s.P50Duration), fmtDuration(s.P95Duration), fmtDuration(s.MaxDuration))

	var failures []string
	for _, c := range r.Cases {
		for _, f := range c.Failures() {
			failures = append(failures, "  "+c.label(r.Repeat)+": "+strings.ReplaceAll(f, "\n", "\n    "))
		}
	}
	if len(failures) > 0 {
		b.WriteString("\nFailures:\n" + strings.Join(failures, "\n") + "\n")
	}
	_, err := w.Write(b.Bytes())
	return err
}

// String returns the report as WriteText prints it.
func (r *Report) String() string {
	var b strings.Builder
	_ = r.WriteText(&b)
	return b.String()
}

func assertionCell(c *CaseResult) string {
	var n, passed int
	var failed []string
	for _, ev := range c.Evaluations {
		if ev.Kind != KindAssertion {
			continue
		}
		n++
		if ev.Pass {
			passed++
		} else {
			failed = append(failed, ev.Name)
		}
	}
	for _, e := range c.EvaluatorErrors {
		failed = append(failed, e.Evaluator+"!")
	}
	if n == 0 && len(failed) == 0 {
		return "-"
	}
	s := fmt.Sprintf("%d/%d", passed, n)
	if len(failed) > 0 {
		s += " ✗ " + strings.Join(failed, ", ")
	}
	return s
}

func valueCell(c *CaseResult) string {
	var parts []string
	for _, ev := range c.Evaluations {
		if ev.Kind != KindAssertion {
			parts = append(parts, ev.String())
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " ")
}

func percent(v float64) string { return strconv.FormatFloat(v*100, 'f', 1, 64) + "%" }

// fmtDuration rounds d for display.
func fmtDuration(d time.Duration) string {
	switch {
	case d >= time.Second:
		return d.Round(10 * time.Millisecond).String()
	case d >= time.Millisecond:
		return d.Round(time.Millisecond).String()
	}
	return d.Round(time.Microsecond).String()
}

// WriteJSON writes the report as indented JSON.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// Save writes the report to a JSON file, for example to compare later
// reports with it.
func (r *Report) Save(path string) error {
	var b bytes.Buffer
	if err := r.WriteJSON(&b); err != nil {
		return err
	}
	return os.WriteFile(path, b.Bytes(), 0o644)
}

// LoadReport reads a report saved with Report.Save. Inputs and outputs are
// decoded as generic JSON values.
func LoadReport(path string) (*Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Report
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("eval: report %s: %w", path, err)
	}
	return &r, nil
}
