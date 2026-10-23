package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/nostosx/agentx/model"
)

// TrajectoryMatch selects how ToolTrajectory compares the tool calls of a
// run with the expected ones.
type TrajectoryMatch string

const (
	// MatchExact requires exactly the expected calls, in order. It is ADK's
	// EXACT.
	MatchExact TrajectoryMatch = "exact"
	// MatchInOrder requires the expected calls in order; other calls may
	// come between them. It is ADK's IN_ORDER.
	MatchInOrder TrajectoryMatch = "in_order"
	// MatchAnyOrder requires exactly the expected calls, in any order.
	MatchAnyOrder TrajectoryMatch = "any_order"
	// MatchSubset requires the expected calls in any order; other calls may
	// come between them. It is ADK's ANY_ORDER.
	MatchSubset TrajectoryMatch = "subset"
)

// MetadataToolCalls is the metadata key from which ToolTrajectory reads
// the expected calls of a case when it is given none, so datasets loaded
// from files can hold them: a list of objects with "name" and optional
// "args", like ToolCall in JSON.
const MetadataToolCalls = "tool_calls"

// ToolCall is an expected tool call.
type ToolCall struct {
	Name string `json:"name"`
	// Args are the expected arguments, compared as JSON. Nil matches any
	// arguments.
	Args map[string]any `json:"args,omitempty"`
}

func (c ToolCall) String() string {
	if c.Args == nil {
		return c.Name
	}
	b, _ := json.Marshal(c.Args)
	return c.Name + string(b)
}

// TrajectoryOption configures ToolTrajectory.
type TrajectoryOption func(*trajectory)

// PartialArgs matches calls whose arguments include the expected ones:
// other arguments, and other fields of nested objects, are ignored.
// Without it, the arguments must equal the expected ones.
func PartialArgs() TrajectoryOption { return func(t *trajectory) { t.partial = true } }

// IgnoreArgs matches calls by tool name only.
func IgnoreArgs() TrajectoryOption { return func(t *trajectory) { t.ignoreArgs = true } }

type trajectory struct {
	match      TrajectoryMatch
	expected   []ToolCall
	partial    bool
	ignoreArgs bool
}

// ToolTrajectory returns an evaluator with an assertion, named
// "ToolTrajectory", on the tool calls the run made, as Google ADK's
// tool_trajectory_avg_score checks them
// (https://adk.dev/evaluate/criteria/). The calls are those of the
// assistant messages the run added, in order; calls of sub-agents are not
// included. Arguments are compared as JSON values, so 1 and 1.0 are equal.
//
// If expected is nil, the expected calls are read from the case metadata
// under MetadataToolCalls, and cases without them are skipped. Use an
// empty, non-nil slice to expect no calls.
func ToolTrajectory(match TrajectoryMatch, expected []ToolCall, opts ...TrajectoryOption) Evaluator {
	t := &trajectory{match: match, expected: expected}
	for _, opt := range opts {
		opt(t)
	}
	return Func("ToolTrajectory", t.evaluate)
}

func (t *trajectory) evaluate(_ context.Context, ec *EvaluatorContext) ([]Evaluation, error) {
	expected := t.expected
	if expected == nil {
		raw, ok := ec.Metadata[MetadataToolCalls]
		if !ok {
			return nil, nil
		}
		b, err := json.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("eval: metadata %s: %w", MetadataToolCalls, err)
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&expected); err != nil {
			return nil, fmt.Errorf("eval: metadata %s: %w", MetadataToolCalls, err)
		}
		if expected == nil {
			expected = []ToolCall{}
		}
	}
	if ec.Result == nil {
		return nil, errNoResult
	}
	var actual []*model.ToolCall
	for _, msg := range ec.Result.NewMessages {
		if msg != nil && msg.Role == model.RoleAssistant {
			actual = append(actual, msg.ToolCalls()...)
		}
	}
	want := make([]expectedCall, len(expected))
	for i, c := range expected {
		want[i].name = c.Name
		if c.Args != nil && !t.ignoreArgs {
			args, err := jsonValue(c.Args)
			if err != nil {
				return nil, fmt.Errorf("eval: expected arguments of %s: %w", c.Name, err)
			}
			want[i].args = args
		}
	}
	got := make([]actualCall, len(actual))
	for i, c := range actual {
		got[i].name = c.Name
		if len(bytes.TrimSpace(c.Arguments)) == 0 {
			got[i].args = map[string]any{}
		} else if err := json.Unmarshal(c.Arguments, &got[i].args); err != nil {
			got[i].invalid = true
		}
	}
	matches := func(i, j int) bool {
		w, g := want[i], got[j]
		if w.name != g.name {
			return false
		}
		if w.args == nil {
			return true
		}
		if g.invalid {
			return false
		}
		if t.partial {
			return partialEqual(w.args, g.args)
		}
		return reflect.DeepEqual(w.args, g.args)
	}

	var pass bool
	switch t.match {
	case MatchExact:
		pass = len(want) == len(got)
		for i := 0; pass && i < len(want); i++ {
			pass = matches(i, i)
		}
	case MatchInOrder:
		// Matching each expected call to the earliest possible call finds a
		// match whenever one exists.
		i := 0
		for j := 0; j < len(got) && i < len(want); j++ {
			if matches(i, j) {
				i++
			}
		}
		pass = i == len(want)
	case MatchAnyOrder:
		pass = len(want) == len(got) && maxMatching(len(want), len(got), matches) == len(want)
	case MatchSubset:
		pass = maxMatching(len(want), len(got), matches) == len(want)
	default:
		return nil, fmt.Errorf("eval: unknown trajectory match %q", t.match)
	}
	reason := fmt.Sprintf("expected %s (%s), got %s", formatCalls(expected), t.match, formatActual(actual))
	return []Evaluation{Assertion("ToolTrajectory", pass, reason)}, nil
}

type expectedCall struct {
	name string
	args any
}

type actualCall struct {
	name    string
	args    any
	invalid bool
}

// maxMatching returns the size of a maximum matching between n expected
// and m actual calls, with augmenting paths.
func maxMatching(n, m int, matches func(i, j int) bool) int {
	owner := make([]int, m)
	for j := range owner {
		owner[j] = -1
	}
	var try func(i int, seen []bool) bool
	try = func(i int, seen []bool) bool {
		for j := range m {
			if seen[j] || !matches(i, j) {
				continue
			}
			seen[j] = true
			if owner[j] < 0 || try(owner[j], seen) {
				owner[j] = i
				return true
			}
		}
		return false
	}
	size := 0
	for i := range n {
		if try(i, make([]bool, m)) {
			size++
		}
	}
	return size
}

// jsonValue returns v as encoding/json decodes its JSON encoding.
func jsonValue(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	err = json.Unmarshal(b, &out)
	return out, err
}

// partialEqual reports whether got includes want: objects may have other
// fields, arrays must have the same length with elements that include
// those of want, and other values must be equal.
func partialEqual(want, got any) bool {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return false
		}
		for k, wv := range w {
			gv, ok := g[k]
			if !ok || !partialEqual(wv, gv) {
				return false
			}
		}
		return true
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range w {
			if !partialEqual(w[i], g[i]) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(want, got)
}

func formatCalls(calls []ToolCall) string {
	parts := make([]string, len(calls))
	for i, c := range calls {
		parts[i] = c.String()
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func formatActual(calls []*model.ToolCall) string {
	parts := make([]string, len(calls))
	for i, c := range calls {
		var b bytes.Buffer
		if json.Compact(&b, c.Arguments) != nil {
			b.Reset()
			b.Write(c.Arguments)
		}
		parts[i] = c.Name + b.String()
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
