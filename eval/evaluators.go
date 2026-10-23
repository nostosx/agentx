package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/tool"
)

// errNoResult is the error of evaluators that need a run result when the
// task returned none.
var errNoResult = errors.New("eval: the task returned no run result")

// MatchOption configures how Equals, EqualsExpected and Contains compare
// strings.
type MatchOption func(*matchConfig)

type matchConfig struct{ fold, trim bool }

// IgnoreCase compares strings case-insensitively.
func IgnoreCase() MatchOption { return func(c *matchConfig) { c.fold = true } }

// TrimSpace ignores leading and trailing white space, and compares runs of
// white space inside strings as a single space.
func TrimSpace() MatchOption { return func(c *matchConfig) { c.trim = true } }

func newMatchConfig(opts []MatchOption) matchConfig {
	var c matchConfig
	for _, opt := range opts {
		opt(&c)
	}
	return c
}

func (c matchConfig) norm(s string) string {
	if c.trim {
		s = strings.Join(strings.Fields(s), " ")
	}
	if c.fold {
		s = strings.ToLower(s)
	}
	return s
}

// equal compares values deeply, and strings after normalization.
func (c matchConfig) equal(got, want any) bool {
	g, gok := got.(string)
	w, wok := want.(string)
	if gok && wok {
		return c.norm(g) == c.norm(w)
	}
	return reflect.DeepEqual(got, want)
}

// Equals returns an evaluator with an assertion, named "Equals", that
// passes when the output deeply equals value.
func Equals(value any, opts ...MatchOption) Evaluator {
	mc := newMatchConfig(opts)
	return Check("Equals", func(ec *EvaluatorContext) (bool, string) {
		if mc.equal(ec.Output, value) {
			return true, ""
		}
		return false, fmt.Sprintf("got %s, want %s", brief(ec.Output), brief(value))
	})
}

// EqualsExpected returns an evaluator with an assertion, named
// "EqualsExpected", that passes when the output deeply equals the case's
// expected output. Cases without one are skipped.
func EqualsExpected(opts ...MatchOption) Evaluator {
	mc := newMatchConfig(opts)
	return Func("EqualsExpected", func(_ context.Context, ec *EvaluatorContext) ([]Evaluation, error) {
		if ec.Expected == nil {
			return nil, nil
		}
		if mc.equal(ec.Output, ec.Expected) {
			return []Evaluation{Assertion("EqualsExpected", true, "")}, nil
		}
		return []Evaluation{Assertion("EqualsExpected", false,
			fmt.Sprintf("got %s, want %s", brief(ec.Output), brief(ec.Expected)))}, nil
	})
}

// Contains returns an evaluator with an assertion, named "Contains", as
// pydantic-evals defines it: a string output must contain value as a
// substring, a slice or array must have an element equal to value, and a
// map must hold every key of the map value with an equal value. Other
// outputs are compared as their JSON text, with value as a substring.
func Contains(value any, opts ...MatchOption) Evaluator {
	mc := newMatchConfig(opts)
	return Check("Contains", func(ec *EvaluatorContext) (bool, string) {
		return contains(mc, ec.Output, value)
	})
}

func contains(mc matchConfig, out, value any) (bool, string) {
	if s, ok := out.(string); ok {
		if strings.Contains(mc.norm(s), mc.norm(fmt.Sprint(value))) {
			return true, ""
		}
		return false, fmt.Sprintf("%s does not contain %s", brief(s), brief(value))
	}
	rv := reflect.ValueOf(out)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		for i := range rv.Len() {
			if mc.equal(rv.Index(i).Interface(), value) {
				return true, ""
			}
		}
		return false, fmt.Sprintf("no element of %s equals %s", brief(out), brief(value))
	case reflect.Map:
		want := reflect.ValueOf(value)
		if want.Kind() != reflect.Map {
			return false, fmt.Sprintf("the output is a map, and %s is not", brief(value))
		}
		for _, k := range want.MapKeys() {
			if !k.Type().AssignableTo(rv.Type().Key()) {
				return false, fmt.Sprintf("key %v has the wrong type", k)
			}
			got := rv.MapIndex(k)
			if !got.IsValid() {
				return false, fmt.Sprintf("key %v is missing", k)
			}
			if !mc.equal(got.Interface(), want.MapIndex(k).Interface()) {
				return false, fmt.Sprintf("key %v is %s, want %s", k, brief(got.Interface()), brief(want.MapIndex(k).Interface()))
			}
		}
		return true, ""
	}
	return contains(mc, Text(out), value)
}

// Regex returns an evaluator with an assertion, named "Regex", that passes
// when the output's text matches the regular expression pattern. An
// invalid pattern fails every case with an evaluator error.
func Regex(pattern string) Evaluator {
	re, err := regexp.Compile(pattern)
	return Func("Regex", func(_ context.Context, ec *EvaluatorContext) ([]Evaluation, error) {
		if err != nil {
			return nil, fmt.Errorf("eval: Regex: %w", err)
		}
		text := Text(ec.Output)
		if re.MatchString(text) {
			return []Evaluation{Assertion("Regex", true, "")}, nil
		}
		return []Evaluation{Assertion("Regex", false, fmt.Sprintf("%s does not match %s", brief(text), pattern))}, nil
	})
}

// JSONSchema returns an evaluator with an assertion, named "JSONSchema",
// that passes when the output is JSON matching schema. A string output is
// parsed as JSON, without the fence of a single Markdown code block, as
// loop.Output does; other outputs are encoded as JSON first.
func JSONSchema(schema *tool.Schema) Evaluator {
	return Check("JSONSchema", func(ec *EvaluatorContext) (bool, string) {
		raw, err := outputJSON(ec.Output)
		if err != nil {
			return false, err.Error()
		}
		if err := schema.Validate(raw); err != nil {
			if ve, ok := errors.AsType[*tool.ValidationError](err); ok {
				return false, strings.Join(ve.Problems, "; ")
			}
			return false, err.Error()
		}
		return true, ""
	})
}

// IsType returns an evaluator with an assertion, named "IsType", that
// passes when the output's dynamic type is T or, if T is an interface,
// implements it. It suits tasks whose output type is an interface.
func IsType[T any]() Evaluator {
	return Check("IsType", func(ec *EvaluatorContext) (bool, string) {
		if _, ok := ec.Output.(T); ok {
			return true, ""
		}
		return false, fmt.Sprintf("the output is %T, not %s", ec.Output, reflect.TypeFor[T]())
	})
}

// DecodesAs returns an evaluator with an assertion, named "DecodesAs", that
// passes when the output's text is a single JSON value that decodes as T
// without unknown fields. Markdown code fences are removed as JSONSchema
// does.
func DecodesAs[T any]() Evaluator {
	return Check("DecodesAs", func(ec *EvaluatorContext) (bool, string) {
		raw, err := outputJSON(ec.Output)
		if err != nil {
			return false, err.Error()
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		var v T
		if err := dec.Decode(&v); err != nil {
			return false, fmt.Sprintf("decode as %s: %v", reflect.TypeFor[T](), err)
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			return false, "the output holds more than one JSON value"
		}
		return true, ""
	})
}

// outputJSON returns the JSON of an output: a string without a single
// Markdown code fence, or another value encoded.
func outputJSON(out any) (json.RawMessage, error) {
	if s, ok := out.(string); ok {
		return json.RawMessage(unfence(s)), nil
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("encode the output: %w", err)
	}
	return raw, nil
}

// unfence returns text without surrounding white space or, if it is a
// single Markdown code block, the content of the block.
func unfence(text string) string {
	s := strings.TrimSpace(text)
	if len(s) < 6 || !strings.HasPrefix(s, "```") || !strings.HasSuffix(s, "```") {
		return s
	}
	info, body, ok := strings.Cut(s[3:len(s)-3], "\n")
	if !ok || strings.Contains(info, "`") {
		return s
	}
	return strings.TrimSpace(body)
}

// resultCheck returns an evaluator with an assertion on the run result.
func resultCheck(name string, fn func(res *loop.Result) (bool, string)) Evaluator {
	return Func(name, func(_ context.Context, ec *EvaluatorContext) ([]Evaluation, error) {
		if ec.Result == nil {
			return nil, errNoResult
		}
		pass, reason := fn(ec.Result)
		return []Evaluation{Assertion(name, pass, reason)}, nil
	})
}

// Completed returns an evaluator with an assertion, named "Completed",
// that passes when the run completed, rather than ending at a limit, being
// interrupted for approval or failing.
func Completed() Evaluator {
	return resultCheck("Completed", func(res *loop.Result) (bool, string) {
		if res.Status == loop.StatusCompleted {
			return true, ""
		}
		return false, strings.TrimSuffix(fmt.Sprintf("status %s: %s", res.Status, res.Reason), ": ")
	})
}

// MaxTurns returns an evaluator with an assertion, named "MaxTurns", that
// passes when the run took at most n turns (model calls).
func MaxTurns(n int) Evaluator {
	return resultCheck("MaxTurns", func(res *loop.Result) (bool, string) {
		return res.Turns <= n, fmt.Sprintf("%d turns, limit %d", res.Turns, n)
	})
}

// MaxTokens returns an evaluator with an assertion, named "MaxTokens", that
// passes when the run used at most n tokens in total, as
// model.Usage.TotalTokens counts them.
func MaxTokens(n int) Evaluator {
	return resultCheck("MaxTokens", func(res *loop.Result) (bool, string) {
		total := res.Usage.TotalTokens()
		return total <= n, fmt.Sprintf("%d tokens, limit %d", total, n)
	})
}

// MaxToolCalls returns an evaluator with an assertion, named
// "MaxToolCalls", that passes when the run executed at most n tool calls.
func MaxToolCalls(n int) Evaluator {
	return resultCheck("MaxToolCalls", func(res *loop.Result) (bool, string) {
		return res.Usage.ToolCalls <= n, fmt.Sprintf("%d tool calls, limit %d", res.Usage.ToolCalls, n)
	})
}

// MaxDuration returns an evaluator with an assertion, named "MaxDuration",
// that passes when the task took at most d.
func MaxDuration(d time.Duration) Evaluator {
	return Check("MaxDuration", func(ec *EvaluatorContext) (bool, string) {
		return ec.Duration <= d, fmt.Sprintf("took %s, limit %s", ec.Duration.Round(time.Millisecond), d)
	})
}

// ResponseMatch returns an evaluator that compares the output's text with
// the expected output's by ROUGE-1, the F1 measure of the words they share,
// as Google ADK's response_match_score does
// (https://adk.dev/evaluate/criteria/). It records the score as
// "ResponseMatch_score" and an assertion, "ResponseMatch", that passes
// when the score is at least minScore; ADK's default is 0.8. Words are compared
// case-insensitively, and each CJK character counts as a word. Cases
// without an expected output are skipped.
func ResponseMatch(minScore float64) Evaluator {
	return Func("ResponseMatch", func(_ context.Context, ec *EvaluatorContext) ([]Evaluation, error) {
		if ec.Expected == nil {
			return nil, nil
		}
		score := Rouge1(Text(ec.Output), Text(ec.Expected))
		reason := fmt.Sprintf("ROUGE-1 %.3f, minimum %.3g", score, minScore)
		return []Evaluation{
			Assertion("ResponseMatch", score >= minScore, reason),
			Score("ResponseMatch_score", score, ""),
		}, nil
	})
}

// Rouge1 returns the ROUGE-1 F1 measure of candidate against reference.
func Rouge1(candidate, reference string) float64 {
	c, r := words(candidate), words(reference)
	if len(c) == 0 || len(r) == 0 {
		if len(c) == len(r) {
			return 1
		}
		return 0
	}
	counts := map[string]int{}
	for _, w := range r {
		counts[w]++
	}
	overlap := 0
	for _, w := range c {
		if counts[w] > 0 {
			counts[w]--
			overlap++
		}
	}
	if overlap == 0 {
		return 0
	}
	p := float64(overlap) / float64(len(c))
	rec := float64(overlap) / float64(len(r))
	return 2 * p * rec / (p + rec)
}

// words splits text into lower-case words of letters and digits; each
// CJK character is a word of its own.
func words(text string) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}
	for _, r := range strings.ToLower(text) {
		switch {
		case unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul):
			flush()
			out = append(out, string(r))
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// Text returns the text of an output for evaluators that compare text: a
// string as is, a byte slice as a string, a fmt.Stringer formatted, and
// other values as JSON.
func Text(v any) string {
	switch v := v.(type) {
	case nil:
		return ""
	case string:
		return v
	case []byte:
		return string(v)
	case fmt.Stringer:
		return v.String()
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// brief formats a value for a reason, shortening long text.
func brief(v any) string {
	s := Text(v)
	if _, ok := v.(string); ok || v == nil {
		s = fmt.Sprintf("%q", s)
	}
	const limit = 200
	if r := []rune(s); len(r) > limit {
		s = string(r[:limit]) + "…"
	}
	return s
}
