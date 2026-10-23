package eval_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/nostosx/agentx/eval"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/tool"
)

// check runs e on ec and returns its single assertion.
func check(t *testing.T, e eval.Evaluator, ec *eval.EvaluatorContext) eval.Evaluation {
	t.Helper()
	evs, err := e.Evaluate(t.Context(), ec)
	if err != nil {
		t.Fatalf("%s: %v", e.Name(), err)
	}
	if len(evs) != 1 {
		t.Fatalf("%s: got %d evaluations, want 1", e.Name(), len(evs))
	}
	return evs[0]
}

type stringer struct{}

func (stringer) String() string { return "from stringer" }

type answer struct {
	City  string `json:"city"`
	Count int    `json:"count"`
}

func TestComparisonEvaluators(t *testing.T) {
	t.Parallel()
	for i, tc := range []struct {
		e      eval.Evaluator
		out    any
		pass   bool
		reason string
	}{
		{eval.Equals("Paris"), "Paris", true, ""},
		{eval.Equals("Paris"), "paris", false, `got "paris", want "Paris"`},
		{eval.Equals("Paris", eval.IgnoreCase(), eval.TrimSpace()), "  PARIS \n", true, ""},
		{eval.Equals("a b", eval.TrimSpace()), "a \t b", true, ""},
		{eval.Equals(answer{"Kyoto", 2}), answer{"Kyoto", 2}, true, ""},
		{eval.Equals(2), 2.0, false, "got 2, want 2"},
		{eval.Contains("Par"), "Paris", true, ""},
		{eval.Contains("par"), "Paris", false, `"Paris" does not contain "par"`},
		{eval.Contains("PAR", eval.IgnoreCase()), "Paris", true, ""},
		{eval.Contains(2), []int{1, 2}, true, ""},
		{eval.Contains("b", eval.IgnoreCase()), []string{"A", "B"}, true, ""},
		{eval.Contains(3), []int{1, 2}, false, "no element of [1,2] equals 3"},
		{eval.Contains(map[string]any{"a": 1}), map[string]any{"a": 1, "b": 2}, true, ""},
		{eval.Contains(map[string]any{"a": 2}), map[string]any{"a": 1}, false, "key a is 1, want 2"},
		{eval.Contains(map[string]any{"c": 2}), map[string]any{"a": 1}, false, "key c is missing"},
		{eval.Contains(map[int]int{1: 1}), map[string]any{"a": 1}, false, "key 1 has the wrong type"},
		{eval.Contains("x"), map[string]any{"a": 1}, false, `the output is a map, and "x" is not`},
		{eval.Contains(`"city":"Kyoto"`), answer{"Kyoto", 1}, true, ""},
		{eval.Contains("stringer"), stringer{}, true, ""},
		{eval.Regex(`^\d{3}$`), "123", true, ""},
		{eval.Regex(`^\d{3}$`), "1234", false, `"1234" does not match ^\d{3}$`},
		{eval.Regex(`Kyoto`), answer{"Kyoto", 1}, true, ""},
		{eval.IsType[string](), "x", true, ""},
		{eval.IsType[fmt.Stringer](), stringer{}, true, ""},
		{eval.IsType[int](), "x", false, "the output is string, not int"},
		{eval.DecodesAs[answer](), "```json\n{\"city\":\"Kyoto\",\"count\":1}\n```", true, ""},
		{eval.DecodesAs[answer](), `{"city":"Kyoto","extra":1}`, false, `decode as eval_test.answer: json: unknown field "extra"`},
		{eval.DecodesAs[answer](), `{"city":"Kyoto"} {}`, false, "the output holds more than one JSON value"},
		{eval.DecodesAs[answer](), answer{"Kyoto", 1}, true, ""},
		{eval.DecodesAs[answer](), func() {}, false, "encode the output: json: unsupported type: func()"},
	} {
		got := check(t, tc.e, &eval.EvaluatorContext{Output: tc.out})
		if got.Kind != eval.KindAssertion || got.Pass != tc.pass || got.Reason != tc.reason || got.Name != tc.e.Name() {
			t.Errorf("%d: %s(%v) = %+v, want pass %v reason %q", i, tc.e.Name(), tc.out, got, tc.pass, tc.reason)
		}
	}
	long := strings.Repeat("x", 300)
	got := check(t, eval.Equals("y"), &eval.EvaluatorContext{Output: long})
	if !strings.Contains(got.Reason, "…") || len([]rune(got.Reason)) > 220 {
		t.Errorf("long reason = %q", got.Reason)
	}
	if _, err := eval.Regex("(").Evaluate(t.Context(), &eval.EvaluatorContext{Output: "x"}); err == nil || !strings.Contains(err.Error(), "missing closing )") {
		t.Errorf("invalid regex = %v", err)
	}
}

func TestEqualsExpected(t *testing.T) {
	t.Parallel()
	e := eval.EqualsExpected(eval.IgnoreCase())
	if evs, err := e.Evaluate(t.Context(), &eval.EvaluatorContext{Output: "x"}); err != nil || evs != nil {
		t.Fatalf("without expected = %v, %v", evs, err)
	}
	if got := check(t, e, &eval.EvaluatorContext{Output: "X", Expected: "x"}); !got.Pass {
		t.Fatalf("equal = %+v", got)
	}
	if got := check(t, e, &eval.EvaluatorContext{Output: "y", Expected: "x"}); got.Pass || got.Reason != `got "y", want "x"` {
		t.Fatalf("different = %+v", got)
	}
}

func TestJSONSchema(t *testing.T) {
	t.Parallel()
	e := eval.JSONSchema(tool.SchemaOf[answer]())
	for _, tc := range []struct {
		out    any
		pass   bool
		reason string
	}{
		{`{"city":"Kyoto","count":3}`, true, ""},
		{"```json\n{\"city\":\"Kyoto\",\"count\":3}\n```", true, ""},
		{answer{"Kyoto", 3}, true, ""},
		{`{"city":"Kyoto"}`, false, "$: missing required property \"count\""},
		{`{"city":1,"count":3}`, false, "$.city: expected string, got number"},
		{"not json", false, "arguments are not valid JSON"},
		{make(chan int), false, "encode the output"},
	} {
		got := check(t, e, &eval.EvaluatorContext{Output: tc.out})
		if got.Pass != tc.pass || !strings.Contains(got.Reason, tc.reason) {
			t.Errorf("JSONSchema(%v) = %+v, want %v %q", tc.out, got, tc.pass, tc.reason)
		}
	}
}

func TestResultEvaluators(t *testing.T) {
	t.Parallel()
	res := &loop.Result{Status: loop.StatusCompleted, Turns: 3, Usage: model.Usage{InputTokens: 100, OutputTokens: 20, ToolCalls: 2}}
	ec := &eval.EvaluatorContext{Result: res, Duration: 1500 * time.Millisecond}
	for _, tc := range []struct {
		e      eval.Evaluator
		pass   bool
		reason string
	}{
		{eval.Completed(), true, ""},
		{eval.MaxTurns(3), true, "3 turns, limit 3"},
		{eval.MaxTurns(2), false, "3 turns, limit 2"},
		{eval.MaxTokens(120), true, "120 tokens, limit 120"},
		{eval.MaxTokens(100), false, "120 tokens, limit 100"},
		{eval.MaxToolCalls(1), false, "2 tool calls, limit 1"},
		{eval.MaxDuration(2 * time.Second), true, "took 1.5s, limit 2s"},
		{eval.MaxDuration(time.Second), false, "took 1.5s, limit 1s"},
	} {
		if got := check(t, tc.e, ec); got.Pass != tc.pass || got.Reason != tc.reason {
			t.Errorf("%s = %+v, want %v %q", tc.e.Name(), got, tc.pass, tc.reason)
		}
	}
	for _, r := range []*loop.Result{
		{Status: loop.StatusMaxTurns, Reason: "reached the limit of 2 turns"},
		{Status: loop.StatusInterrupted},
	} {
		got := check(t, eval.Completed(), &eval.EvaluatorContext{Result: r})
		want := "status " + string(r.Status)
		if r.Reason != "" {
			want += ": " + r.Reason
		}
		if got.Pass || got.Reason != want {
			t.Errorf("Completed(%s) = %+v", r.Status, got)
		}
	}
	for _, e := range []eval.Evaluator{eval.Completed(), eval.MaxTurns(1), eval.MaxTokens(1), eval.MaxToolCalls(1), eval.ToolTrajectory(eval.MatchExact, nil, eval.IgnoreArgs())} {
		_, err := e.Evaluate(t.Context(), &eval.EvaluatorContext{Metadata: map[string]any{eval.MetadataToolCalls: []any{}}})
		if err == nil || !strings.Contains(err.Error(), "no run result") {
			t.Errorf("%s without result = %v", e.Name(), err)
		}
	}
}

func TestResponseMatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		cand, ref string
		want      float64
	}{
		{"The capital is Paris.", "the capital is paris", 1},
		{"Paris", "The capital is Paris", 2 * (1.0 * 0.25) / 1.25},
		{"", "", 1},
		{"x", "", 0},
		{"London", "Paris", 0},
		{"首都是东京", "东京是首都", 1},
		{"a a b", "a b b", 2.0 / 3},
	} {
		if got := eval.Rouge1(tc.cand, tc.ref); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("Rouge1(%q, %q) = %v, want %v", tc.cand, tc.ref, got, tc.want)
		}
	}
	e := eval.ResponseMatch(0.8)
	evs, err := e.Evaluate(t.Context(), &eval.EvaluatorContext{Output: "Paris", Expected: "The capital is Paris"})
	if err != nil || len(evs) != 2 || evs[0].Pass || evs[0].Reason != "ROUGE-1 0.400, minimum 0.8" ||
		evs[1].Kind != eval.KindScore || evs[1].Name != "ResponseMatch_score" || math.Abs(evs[1].Score-0.4) > 1e-9 {
		t.Fatalf("ResponseMatch = %+v, %v", evs, err)
	}
	if evs, err := e.Evaluate(t.Context(), &eval.EvaluatorContext{Output: "x"}); err != nil || evs != nil {
		t.Fatalf("without expected = %+v, %v", evs, err)
	}
}

func TestText(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		v    any
		want string
	}{
		{nil, ""},
		{"s", "s"},
		{[]byte("b"), "b"},
		{stringer{}, "from stringer"},
		{answer{"K", 1}, `{"city":"K","count":1}`},
	} {
		if got := eval.Text(tc.v); got != tc.want {
			t.Errorf("Text(%T) = %q, want %q", tc.v, got, tc.want)
		}
	}
	ch := make(chan int)
	if got, want := eval.Text(ch), fmt.Sprint(ch); got != want {
		t.Errorf("Text(channel) = %q, want %q", got, want)
	}
}

// trajectoryResult returns a run result whose assistant messages make the
// given calls, one message per group.
func trajectoryResult(groups ...[]*model.ToolCall) *loop.Result {
	res := &loop.Result{Status: loop.StatusCompleted, NewMessages: []*model.Message{model.UserText("go")}}
	for _, calls := range groups {
		msg := &model.Message{Role: model.RoleAssistant}
		for _, c := range calls {
			msg.Parts = append(msg.Parts, model.Part{Type: model.PartToolCall, ToolCall: c})
		}
		res.NewMessages = append(res.NewMessages, msg, model.ToolResults(&model.ToolResult{CallID: "x", Content: "ok"}))
	}
	res.NewMessages = append(res.NewMessages, model.AssistantText("done"))
	return res
}

func call(name, args string) *model.ToolCall {
	return &model.ToolCall{ID: name, Name: name, Arguments: json.RawMessage(args)}
}

func TestToolTrajectory(t *testing.T) {
	t.Parallel()
	res := trajectoryResult(
		[]*model.ToolCall{call("search", `{"q":"kyoto","limit":5}`), call("weather", `{"city":"Kyoto"}`)},
		[]*model.ToolCall{call("search", `{"q":"osaka","limit":5}`)},
	)
	search := func(q string) eval.ToolCall {
		return eval.ToolCall{Name: "search", Args: map[string]any{"q": q, "limit": 5}}
	}
	weather := eval.ToolCall{Name: "weather", Args: map[string]any{"city": "Kyoto"}}
	for i, tc := range []struct {
		match    eval.TrajectoryMatch
		expected []eval.ToolCall
		opts     []eval.TrajectoryOption
		pass     bool
	}{
		{eval.MatchExact, []eval.ToolCall{search("kyoto"), weather, search("osaka")}, nil, true},
		{eval.MatchExact, []eval.ToolCall{search("kyoto"), search("osaka"), weather}, nil, false},
		{eval.MatchExact, []eval.ToolCall{search("kyoto"), weather}, nil, false},
		{eval.MatchExact, []eval.ToolCall{{Name: "search"}, {Name: "weather"}, {Name: "search"}}, nil, true},
		{eval.MatchExact, []eval.ToolCall{search("KYOTO"), weather, search("osaka")}, nil, false},
		{eval.MatchInOrder, []eval.ToolCall{search("kyoto"), search("osaka")}, nil, true},
		{eval.MatchInOrder, []eval.ToolCall{search("osaka"), weather}, nil, false},
		{eval.MatchInOrder, []eval.ToolCall{}, nil, true},
		{eval.MatchAnyOrder, []eval.ToolCall{search("osaka"), weather, search("kyoto")}, nil, true},
		{eval.MatchAnyOrder, []eval.ToolCall{search("osaka"), weather}, nil, false},
		{eval.MatchSubset, []eval.ToolCall{search("osaka"), weather}, nil, true},
		{eval.MatchSubset, []eval.ToolCall{weather, weather}, nil, false},
		{eval.MatchSubset, []eval.ToolCall{{Name: "search", Args: map[string]any{"q": "osaka"}}}, nil, false},
		{eval.MatchSubset, []eval.ToolCall{{Name: "search", Args: map[string]any{"q": "osaka"}}}, []eval.TrajectoryOption{eval.PartialArgs()}, true},
		{eval.MatchSubset, []eval.ToolCall{{Name: "search", Args: map[string]any{"q": "paris"}}}, []eval.TrajectoryOption{eval.IgnoreArgs()}, true},
		{eval.MatchSubset, []eval.ToolCall{{Name: "lookup"}}, nil, false},
		// A greedy assignment would give the first call to the general
		// expectation and leave the specific one unmatched.
		{eval.MatchSubset, []eval.ToolCall{{Name: "search"}, search("kyoto")}, nil, true},
		{eval.MatchAnyOrder, []eval.ToolCall{{Name: "search"}, search("kyoto"), {Name: "weather"}}, nil, true},
	} {
		got := check(t, eval.ToolTrajectory(tc.match, tc.expected, tc.opts...), &eval.EvaluatorContext{Result: res})
		if got.Pass != tc.pass {
			t.Errorf("%d: %s %v = %+v, want %v", i, tc.match, tc.expected, got, tc.pass)
		}
	}
	got := check(t, eval.ToolTrajectory(eval.MatchExact, []eval.ToolCall{{Name: "a", Args: map[string]any{"x": 1}}}), &eval.EvaluatorContext{Result: res})
	want := `expected [a{"x":1}] (exact), got [search{"q":"kyoto","limit":5}, weather{"city":"Kyoto"}, search{"q":"osaka","limit":5}]`
	if got.Reason != want {
		t.Errorf("reason = %q\nwant     %q", got.Reason, want)
	}
}

func TestToolTrajectoryArgs(t *testing.T) {
	t.Parallel()
	res := trajectoryResult([]*model.ToolCall{
		call("edit", `{"path":"a.go","opts":{"dry":true,"n":2},"lines":[{"n":1,"text":"x"}]}`),
		call("noargs", ``),
		call("broken", `{not json`),
	})
	for i, tc := range []struct {
		expected []eval.ToolCall
		opts     []eval.TrajectoryOption
		pass     bool
	}{
		{[]eval.ToolCall{{Name: "edit", Args: map[string]any{"opts": map[string]any{"n": 2}}}}, []eval.TrajectoryOption{eval.PartialArgs()}, true},
		{[]eval.ToolCall{{Name: "edit", Args: map[string]any{"opts": map[string]any{"n": 3}}}}, []eval.TrajectoryOption{eval.PartialArgs()}, false},
		{[]eval.ToolCall{{Name: "edit", Args: map[string]any{"lines": []any{map[string]any{"n": 1}}}}}, []eval.TrajectoryOption{eval.PartialArgs()}, true},
		{[]eval.ToolCall{{Name: "edit", Args: map[string]any{"lines": []any{}}}}, []eval.TrajectoryOption{eval.PartialArgs()}, false},
		{[]eval.ToolCall{{Name: "edit", Args: map[string]any{"opts": "x"}}}, []eval.TrajectoryOption{eval.PartialArgs()}, false},
		{[]eval.ToolCall{{Name: "edit", Args: map[string]any{"lines": "x"}}}, []eval.TrajectoryOption{eval.PartialArgs()}, false},
		{[]eval.ToolCall{{Name: "noargs", Args: map[string]any{}}}, nil, true},
		{[]eval.ToolCall{{Name: "broken", Args: map[string]any{}}}, nil, false},
		{[]eval.ToolCall{{Name: "broken"}}, nil, true},
	} {
		got := check(t, eval.ToolTrajectory(eval.MatchSubset, tc.expected, tc.opts...), &eval.EvaluatorContext{Result: res})
		if got.Pass != tc.pass {
			t.Errorf("%d: %v = %+v, want %v", i, tc.expected, got, tc.pass)
		}
	}
	if _, err := eval.ToolTrajectory("sideways", nil).Evaluate(t.Context(), &eval.EvaluatorContext{Result: res, Metadata: map[string]any{eval.MetadataToolCalls: nil}}); err == nil {
		t.Error("unknown match accepted")
	}
	if _, err := eval.ToolTrajectory(eval.MatchExact, []eval.ToolCall{{Name: "x", Args: map[string]any{"c": make(chan int)}}}).Evaluate(t.Context(), &eval.EvaluatorContext{Result: res}); err == nil {
		t.Error("unencodable arguments accepted")
	}
}

func TestToolTrajectoryFromMetadata(t *testing.T) {
	t.Parallel()
	res := trajectoryResult([]*model.ToolCall{call("search", `{"q":"kyoto"}`)})
	e := eval.ToolTrajectory(eval.MatchExact, nil)
	if evs, err := e.Evaluate(t.Context(), &eval.EvaluatorContext{Result: res}); err != nil || evs != nil {
		t.Fatalf("without metadata = %v, %v", evs, err)
	}
	// Metadata decoded from JSON holds generic values.
	var meta map[string]any
	if err := json.Unmarshal([]byte(`{"tool_calls":[{"name":"search","args":{"q":"kyoto"}}]}`), &meta); err != nil {
		t.Fatal(err)
	}
	if got := check(t, e, &eval.EvaluatorContext{Result: res, Metadata: meta}); !got.Pass {
		t.Fatalf("from metadata = %+v", got)
	}
	if got := check(t, e, &eval.EvaluatorContext{Result: trajectoryResult(), Metadata: map[string]any{eval.MetadataToolCalls: nil}}); !got.Pass {
		t.Fatalf("no calls expected = %+v", got)
	}
	for _, bad := range []any{"x", []any{map[string]any{"name": "a", "argz": 1}}, []any{map[string]any{"name": make(chan int)}}} {
		_, err := e.Evaluate(t.Context(), &eval.EvaluatorContext{Result: res, Metadata: map[string]any{eval.MetadataToolCalls: bad}})
		if err == nil || !strings.Contains(err.Error(), "metadata tool_calls") {
			t.Errorf("bad metadata %v = %v", bad, err)
		}
	}
}

func TestFuncAndCheck(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("nope")
	e := eval.Func("f", func(context.Context, *eval.EvaluatorContext) ([]eval.Evaluation, error) { return nil, sentinel })
	if _, err := e.Evaluate(t.Context(), &eval.EvaluatorContext{}); !errors.Is(err, sentinel) || e.Name() != "f" {
		t.Fatalf("Func = %v", err)
	}
	c := eval.Check("c", func(ec *eval.EvaluatorContext) (bool, string) { return ec.Output == 1, "reason" })
	if got := check(t, c, &eval.EvaluatorContext{Output: 1}); !got.Pass || got.Name != "c" || got.Reason != "reason" {
		t.Fatalf("Check = %+v", got)
	}
	typed := eval.TypedCheck("t", func(t eval.Typed[int, string]) (bool, string) { return t.Expected == nil && t.Output == "", "" })
	if got := check(t, typed, &eval.EvaluatorContext{Input: 1}); !got.Pass {
		t.Fatalf("TypedCheck with nil output = %+v", got)
	}
	for _, ec := range []*eval.EvaluatorContext{{Output: 1}, {Expected: 1}} {
		if _, err := typed.Evaluate(t.Context(), ec); err == nil {
			t.Errorf("TypedCheck(%+v) accepted the wrong type", ec)
		}
	}
}
