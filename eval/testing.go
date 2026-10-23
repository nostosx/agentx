package eval

import (
	"maps"
	"slices"
	"testing"
)

// Threshold sets the pass rate Run requires, from 0 to 1; it defaults to 1,
// every run passing.
func Threshold(rate float64) Option { return func(c *config) { c.threshold = rate } }

// MinScore makes Run require the mean of the score name to be at least
// mean, like the per-criterion thresholds of Google ADK. The score must
// be present.
func MinScore(name string, mean float64) Option {
	return func(c *config) {
		if c.minScores == nil {
			c.minScores = map[string]float64{}
		}
		c.minScores[name] = mean
	}
}

// Baseline makes Run compare the report with baseline, log the comparison
// and fail the test if a case regressed. Load a saved baseline with
// LoadReport.
func Baseline(r *Report) Option { return func(c *config) { c.baseline = r } }

// Run evaluates the dataset in a test: it runs Dataset.Evaluate with
// t.Context(), logs the report and fails the test if the pass rate is
// below Threshold, a MinScore is not met, or a case regressed from the
// Baseline. It returns the report, or nil if the evaluation could not run.
//
//	func TestCapitals(t *testing.T) {
//		eval.Run(t, dataset, eval.AgentTask(a), eval.Threshold(0.9))
//	}
func Run[In, Out any](t testing.TB, d *Dataset[In, Out], task Task[In, Out], opts ...Option) *Report {
	t.Helper()
	cfg := newConfig(opts)
	report, err := d.Evaluate(t.Context(), task, opts...)
	if report == nil {
		t.Fatalf("eval: %v", err)
		return nil
	}
	t.Log("\n" + report.String())
	if err != nil {
		t.Errorf("eval: %v", err)
	}
	s := report.Summary
	if s.PassRate < cfg.threshold {
		t.Errorf("eval: pass rate %s is below the threshold %s", percent(s.PassRate), percent(cfg.threshold))
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.minScores)) {
		want := cfg.minScores[name]
		got, ok := s.Scores[name]
		switch {
		case !ok:
			t.Errorf("eval: no score %s", name)
		case got < want:
			t.Errorf("eval: mean score %s %.3f is below %.3f", name, got, want)
		}
	}
	if cfg.baseline != nil {
		cmp := Compare(cfg.baseline, report)
		t.Log("\n" + cmp.String())
		for _, d := range cmp.Regressions() {
			t.Errorf("eval: case %s regressed: %d/%d runs passed, baseline %d/%d",
				d.Name, d.Candidate.Passed, d.Candidate.Runs, d.Baseline.Passed, d.Baseline.Runs)
		}
	}
	return report
}
