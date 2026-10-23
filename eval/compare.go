package eval

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
)

// Delta is a value in a baseline and a candidate report.
type Delta struct {
	Baseline  float64 `json:"baseline"`
	Candidate float64 `json:"candidate"`
}

// Change returns the candidate's value minus the baseline's.
func (d Delta) Change() float64 { return d.Candidate - d.Baseline }

// DurationDelta is a duration in a baseline and a candidate report.
type DurationDelta struct {
	Baseline  time.Duration `json:"baseline"`
	Candidate time.Duration `json:"candidate"`
}

// CaseChange classifies how a case changed between two reports.
type CaseChange string

const (
	// CaseRegressed means the candidate passed a smaller share of the
	// case's runs.
	CaseRegressed CaseChange = "regressed"
	// CaseImproved means the candidate passed a larger share.
	CaseImproved  CaseChange = "improved"
	CaseUnchanged CaseChange = "unchanged"
	// CaseAdded and CaseRemoved mark cases only in the candidate or only
	// in the baseline.
	CaseAdded   CaseChange = "added"
	CaseRemoved CaseChange = "removed"
)

// CaseDiff compares one case between two reports.
type CaseDiff struct {
	Name string `json:"name"`
	// Baseline and Candidate are nil for a case missing from the report.
	Baseline  *CaseSummary `json:"baseline,omitempty"`
	Candidate *CaseSummary `json:"candidate,omitempty"`
	Change    CaseChange   `json:"change"`
}

// Comparison compares a candidate report with a baseline, for example a
// new prompt or model with the current one.
type Comparison struct {
	Baseline  string `json:"baseline"`
	Candidate string `json:"candidate"`
	PassRate  Delta  `json:"pass_rate"`
	// Assertions and Scores compare the assertion pass rates and the mean
	// scores, by name, of those in either report; one missing from a
	// report is zero there.
	Assertions   map[string]Delta `json:"assertions,omitempty"`
	Scores       map[string]Delta `json:"scores,omitempty"`
	TotalTokens  Delta            `json:"total_tokens"`
	MeanTokens   Delta            `json:"mean_tokens"`
	MeanDuration DurationDelta    `json:"mean_duration"`
	// Cases lists the cases of the candidate in order, then those only in
	// the baseline.
	Cases []CaseDiff `json:"cases"`
}

// Compare compares candidate with baseline. Cases are matched by name.
func Compare(baseline, candidate *Report) *Comparison {
	bs, cs := summarize(baseline.Cases), summarize(candidate.Cases)
	c := &Comparison{
		Baseline:     reportName(baseline, "baseline"),
		Candidate:    reportName(candidate, "candidate"),
		PassRate:     Delta{bs.PassRate, cs.PassRate},
		Assertions:   deltas(bs.Assertions, cs.Assertions),
		Scores:       deltas(bs.Scores, cs.Scores),
		TotalTokens:  Delta{float64(bs.Usage.TotalTokens()), float64(cs.Usage.TotalTokens())},
		MeanTokens:   Delta{bs.MeanTokens(), cs.MeanTokens()},
		MeanDuration: DurationDelta{bs.MeanDuration, cs.MeanDuration},
	}
	base := map[string]*CaseSummary{}
	bcases := baseline.ByCase()
	for i := range bcases {
		base[bcases[i].Name] = &bcases[i]
	}
	seen := map[string]bool{}
	ccases := candidate.ByCase()
	for i := range ccases {
		cand := &ccases[i]
		seen[cand.Name] = true
		d := CaseDiff{Name: cand.Name, Baseline: base[cand.Name], Candidate: cand, Change: CaseAdded}
		if b := d.Baseline; b != nil {
			switch {
			case cand.PassRate < b.PassRate:
				d.Change = CaseRegressed
			case cand.PassRate > b.PassRate:
				d.Change = CaseImproved
			default:
				d.Change = CaseUnchanged
			}
		}
		c.Cases = append(c.Cases, d)
	}
	for i := range bcases {
		if !seen[bcases[i].Name] {
			c.Cases = append(c.Cases, CaseDiff{Name: bcases[i].Name, Baseline: &bcases[i], Change: CaseRemoved})
		}
	}
	return c
}

func reportName(r *Report, fallback string) string {
	if r.Name != "" {
		return r.Name
	}
	return fallback
}

func deltas(b, c map[string]float64) map[string]Delta {
	if len(b) == 0 && len(c) == 0 {
		return nil
	}
	out := map[string]Delta{}
	for k, v := range b {
		out[k] = Delta{Baseline: v, Candidate: c[k]}
	}
	for k, v := range c {
		d := out[k]
		d.Candidate = v
		out[k] = d
	}
	return out
}

// Regressions returns the cases that regressed.
func (c *Comparison) Regressions() []CaseDiff {
	var out []CaseDiff
	for _, d := range c.Cases {
		if d.Change == CaseRegressed {
			out = append(out, d)
		}
	}
	return out
}

// WriteText prints the comparison: the aggregate changes followed by a
// table of the cases.
func (c *Comparison) WriteText(w io.Writer) error {
	var b bytes.Buffer
	fmt.Fprintf(&b, "Comparison of %s (baseline) and %s (candidate)\n\n", c.Baseline, c.Candidate)
	fmt.Fprintf(&b, "Pass rate: %s -> %s (%+.1f)\n", percent(c.PassRate.Baseline), percent(c.PassRate.Candidate), c.PassRate.Change()*100)
	for _, k := range slices.Sorted(maps.Keys(c.Assertions)) {
		d := c.Assertions[k]
		fmt.Fprintf(&b, "Assertion %s: %s -> %s (%+.1f)\n", k, percent(d.Baseline), percent(d.Candidate), d.Change()*100)
	}
	for _, k := range slices.Sorted(maps.Keys(c.Scores)) {
		d := c.Scores[k]
		fmt.Fprintf(&b, "Score %s: %.3f -> %.3f (%+.3f)\n", k, d.Baseline, d.Candidate, d.Change())
	}
	fmt.Fprintf(&b, "Tokens per run: %.0f -> %.0f (%+.0f)\n", c.MeanTokens.Baseline, c.MeanTokens.Candidate, c.MeanTokens.Change())
	fmt.Fprintf(&b, "Mean duration: %s -> %s\n\n", fmtDuration(c.MeanDuration.Baseline), fmtDuration(c.MeanDuration.Candidate))

	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "CASE\tBASELINE\tCANDIDATE\tCHANGE")
	cell := func(s *CaseSummary) string {
		if s == nil {
			return "-"
		}
		return fmt.Sprintf("%d/%d", s.Passed, s.Runs)
	}
	for _, d := range c.Cases {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", d.Name, cell(d.Baseline), cell(d.Candidate), d.Change)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := w.Write(b.Bytes())
	return err
}

// String returns the comparison as WriteText prints it.
func (c *Comparison) String() string {
	var b strings.Builder
	_ = c.WriteText(&b)
	return b.String()
}
