package eval_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/eval"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/middleware"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/telemetry"
	"github.com/nostosx/agentx/tool"
)

// weatherStep answers "weather in <city>" by calling the weather tool,
// then reports the tool's result, whatever the order of concurrent cases.
func weatherStep(req *model.Request) (*model.Response, error) {
	last := req.Messages[len(req.Messages)-1]
	if last.Role == model.RoleTool {
		resp, err := modeltest.Reply("Forecast: " + last.ToolResults()[0].Content)(req)
		return resp, err
	}
	text := last.Text()
	_, city, _ := strings.CutLast(text, " ")
	return modeltest.CallTools(modeltest.Call{ID: "call-" + city, Name: "weather", Args: map[string]any{"city": city}})(req)
}

type cityArgs struct {
	City string `json:"city"`
}

func weatherAgent(t *testing.T, steps int) *agent.LLMAgent {
	t.Helper()
	m := modeltest.New()
	for range steps {
		m.Append(weatherStep)
	}
	weather := tool.Func("weather", "Weather of a city", func(_ context.Context, in cityArgs) (string, error) {
		if in.City == "Atlantis" {
			return "", errors.New("unknown city")
		}
		return "sunny in " + in.City, nil
	})
	return agent.MustNew("forecaster", agent.WithModel(m), agent.WithTools(weather),
		agent.WithMiddleware(middleware.Tracing(eval.Tracer())))
}

func TestAgentTask(t *testing.T) {
	t.Parallel()
	ds := &eval.Dataset[string, string]{
		Name: "weather",
		Cases: []eval.Case[string, string]{
			{Name: "kyoto", Input: "weather in Kyoto", Expected: new("Forecast: sunny in Kyoto")},
			{Name: "oslo", Input: "weather in Oslo", Expected: new("Forecast: sunny in Oslo")},
			{Name: "atlantis", Input: "weather in Atlantis"},
		},
	}
	for i := range ds.Cases {
		city := ds.Cases[i].Input[len("weather in "):]
		ds.Cases[i].Evaluators = []eval.Evaluator{eval.ToolTrajectory(eval.MatchExact, []eval.ToolCall{{Name: "weather", Args: map[string]any{"city": city}}})}
	}
	var mu sync.Mutex
	spans := map[string]eval.Spans{}
	ds.Evaluators = []eval.Evaluator{
		eval.EqualsExpected(), eval.Completed(), eval.MaxTurns(2), eval.MaxTokens(30), eval.MaxToolCalls(1),
		eval.HasSpan(eval.SpanQuery{
			NamePattern: `^invoke_agent forecaster$`,
			Attributes:  map[string]any{"gen_ai.agent.name": "forecaster"},
			Child:       &eval.SpanQuery{Name: "execute_tool weather", Attributes: map[string]any{"agentx.turn": 1.0}},
		}),
		eval.NoSpan(eval.SpanQuery{Name: "execute_tool delete"}),
		eval.Func("spans", func(_ context.Context, ec *eval.EvaluatorContext) ([]eval.Evaluation, error) {
			mu.Lock()
			defer mu.Unlock()
			spans[ec.Name] = ec.Spans
			return nil, nil
		}),
	}
	rep, err := ds.Evaluate(t.Context(), eval.AgentTask(weatherAgent(t, 6), agent.RunMaxTurns(4)), eval.Concurrency(3))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range rep.Cases[:2] {
		if !c.Passed() || c.Status != loop.StatusCompleted || c.Turns != 2 || c.Usage.TotalTokens() != 30 || c.Usage.ToolCalls != 1 {
			t.Errorf("%s = %+v\n%s", c.Name, c, strings.Join(c.Failures(), "\n"))
		}
	}
	// The tool failed, so the forecast is an error; the trajectory still
	// matches and the error span is recorded.
	if c := rep.Cases[2]; !c.Passed() || !strings.Contains(c.Output.(string), "unknown city") {
		t.Errorf("atlantis = %+v %v", c, c.Failures())
	}
	if rep.Summary.Usage.TotalTokens() != 90 || rep.Summary.Assertions["HasSpan"] != 1 {
		t.Errorf("summary = %+v", rep.Summary)
	}

	roots := spans["kyoto"]
	if len(roots) != 1 || roots[0].Parent != nil || roots[0].Duration <= 0 || roots[0].Attributes["agentx.run.status"] != "completed" {
		t.Fatalf("roots = %+v", roots)
	}
	var names []string
	for sp := range roots.All() {
		names = append(names, sp.Name)
		if sp != roots[0] && sp.Parent != roots[0] {
			t.Errorf("span %s has parent %v", sp.Name, sp.Parent)
		}
	}
	if strings.Join(names, ",") != "invoke_agent forecaster,chat,execute_tool weather,chat" {
		t.Errorf("span names = %v", names)
	}
	errSpans := spans["atlantis"].Find(eval.SpanQuery{Error: new(true)})
	if len(errSpans) != 1 || errSpans[0].Name != "execute_tool weather" || errSpans[0].Attributes["error.type"] != "_OTHER" {
		t.Errorf("error spans = %+v", errSpans)
	}
	ok := spans["kyoto"].Find(eval.SpanQuery{NameContains: "chat", Ancestor: &eval.SpanQuery{NameContains: "invoke_agent"}, Error: new(false)})
	if len(ok) != 2 {
		t.Errorf("chat spans = %+v", ok)
	}
}

func TestSpanQuery(t *testing.T) {
	t.Parallel()
	root := &eval.Span{Name: "root", Duration: 100, Attributes: map[string]any{"n": 1, "s": "x", "list": []string{"a"}}}
	child := &eval.Span{Name: "child", Duration: 10, Parent: root, Error: "boom"}
	grand := &eval.Span{Name: "grandchild", Parent: child}
	child.Children = eval.Spans{grand}
	root.Children = eval.Spans{child}
	spans := eval.Spans{root}
	for i, tc := range []struct {
		q    eval.SpanQuery
		want int
	}{
		{eval.SpanQuery{}, 3},
		{eval.SpanQuery{Name: "child"}, 1},
		{eval.SpanQuery{NameContains: "child"}, 2},
		{eval.SpanQuery{NamePattern: "^gr"}, 1},
		{eval.SpanQuery{NamePattern: "("}, 0},
		{eval.SpanQuery{Attributes: map[string]any{"n": 1.0, "s": "x"}}, 1},
		{eval.SpanQuery{Attributes: map[string]any{"list": []any{"a"}}}, 1},
		{eval.SpanQuery{Attributes: map[string]any{"n": "1"}}, 0},
		{eval.SpanQuery{Attributes: map[string]any{"missing": 1}}, 0},
		{eval.SpanQuery{Error: new(true)}, 1},
		{eval.SpanQuery{MinDuration: 50}, 1},
		{eval.SpanQuery{MaxDuration: 50}, 2},
		{eval.SpanQuery{Child: &eval.SpanQuery{Name: "grandchild"}}, 1},
		{eval.SpanQuery{Descendant: &eval.SpanQuery{Name: "grandchild"}}, 2},
		{eval.SpanQuery{Ancestor: &eval.SpanQuery{Name: "root"}}, 2},
		{eval.SpanQuery{Not: &eval.SpanQuery{NameContains: "child"}}, 1},
	} {
		if got := len(spans.Find(tc.q)); got != tc.want {
			t.Errorf("%d: Find(%+v) = %d spans, want %d", i, tc.q, got, tc.want)
		}
	}
	if spans.Any(eval.SpanQuery{Name: "nope"}) || !spans.Any(eval.SpanQuery{Name: "grandchild"}) {
		t.Error("Any")
	}
	for range spans.All() {
		break // stopping early must not panic
	}
	got := check(t, eval.HasSpan(eval.SpanQuery{Name: "nope"}), &eval.EvaluatorContext{Spans: spans})
	if got.Pass || got.Reason != "no matching span among root, child, grandchild" {
		t.Errorf("HasSpan = %+v", got)
	}
	got = check(t, eval.HasSpan(eval.SpanQuery{Name: "nope"}), &eval.EvaluatorContext{})
	if got.Pass || !strings.Contains(got.Reason, "trace the agent with eval.Tracer") {
		t.Errorf("HasSpan without spans = %+v", got)
	}
	got = check(t, eval.NoSpan(eval.SpanQuery{NameContains: "child"}), &eval.EvaluatorContext{Spans: spans})
	if got.Pass || got.Reason != `2 matching spans, the first "child"` {
		t.Errorf("NoSpan = %+v", got)
	}

	// Outside an evaluation the tracer discards spans.
	ctx, sp := eval.Tracer().Start(t.Context(), "x", telemetry.SpanKindInternal)
	sp.SetAttributes(telemetry.Attr{Key: "k", Value: 1})
	sp.RecordError(errors.New("e"))
	sp.End()
	if ctx != t.Context() {
		t.Error("the tracer changed the context outside an evaluation")
	}
}

func TestTracerEvents(t *testing.T) {
	t.Parallel()
	ds := &eval.Dataset[string, string]{Cases: []eval.Case[string, string]{{Input: "x"}}}
	var got eval.Spans
	task := func(ctx context.Context, _ string) (string, *loop.Result, error) {
		tr := eval.Tracer()
		ctx, parent := tr.Start(ctx, "parent", telemetry.SpanKindInternal, telemetry.Attr{Key: "a", Value: 1})
		_, child := tr.Start(ctx, "child", telemetry.SpanKindClient)
		child.(telemetry.EventSpan).AddEvent("agentx.retry", telemetry.Attr{Key: "attempt", Value: 2})
		child.(telemetry.EventSpan).AddEvent("plain")
		child.RecordError(nil)
		child.End()
		parent.SetAttributes(telemetry.Attr{Key: "b", Value: "two"})
		// parent never ends.
		return "", nil, nil
	}
	_, err := ds.Evaluate(t.Context(), task, eval.WithEvaluators(eval.Func("grab", func(_ context.Context, ec *eval.EvaluatorContext) ([]eval.Evaluation, error) {
		got = ec.Spans
		return nil, nil
	})))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Duration != 0 || got[0].Attributes["a"] != 1 || got[0].Attributes["b"] != "two" || len(got[0].Children) != 1 {
		t.Fatalf("parent = %+v", got)
	}
	c := got[0].Children[0]
	if c.Kind != telemetry.SpanKindClient || c.Parent != got[0] || c.Duration <= 0 || c.Error != "" || len(c.Events) != 2 ||
		c.Events[0].Attributes["attempt"] != 2 || c.Events[1].Attributes != nil {
		t.Fatalf("child = %+v", c)
	}
	b, err := json.Marshal(got)
	if err != nil || !strings.Contains(string(b), `"name":"child"`) {
		t.Fatalf("spans JSON = %s, %v", b, err)
	}
}

func TestTypedAgentTask(t *testing.T) {
	t.Parallel()
	m := modeltest.New(
		modeltest.Reply(`{"city":"Kyoto","count":2}`),
		modeltest.Reply("not json"), modeltest.Reply("still not"),
	)
	a := agent.MustNew("typed", agent.WithModel(m), agent.WithOutput[answer](agent.OutputRetries(1)))
	ds := &eval.Dataset[string, answer]{
		Cases: []eval.Case[string, answer]{
			{Name: "good", Input: "q", Expected: &answer{"Kyoto", 2}},
			{Name: "bad", Input: "q"},
		},
		Evaluators: []eval.Evaluator{eval.EqualsExpected(), eval.IsType[answer](), eval.JSONSchema(tool.SchemaOf[answer]())},
	}
	rep, err := ds.Evaluate(t.Context(), eval.TypedAgentTask[answer](a), eval.Concurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	if c := rep.Cases[0]; !c.Passed() || c.Output != (answer{"Kyoto", 2}) || len(c.Evaluations) != 3 {
		t.Errorf("good = %+v", c)
	}
	if c := rep.Cases[1]; c.Passed() || !strings.Contains(c.Error, "does not match the output schema") || c.Usage.TotalTokens() != 30 {
		t.Errorf("bad = %+v", c)
	}
}

func TestAgentTaskFailure(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("provider down")
	a := agent.MustNew("broken", agent.WithModel(modeltest.New(modeltest.Fail(sentinel))))
	task := eval.AgentTaskFunc(a, func(n int) *agent.Input { return agent.Text(strings.Repeat("x", n)) },
		func(res *loop.Result) (int, error) { return len(res.Output), nil })
	out, res, err := task(t.Context(), 3)
	if !errors.Is(err, sentinel) || out != 0 || res == nil || res.Status != loop.StatusFailed {
		t.Fatalf("task = %v, %+v, %v", out, res, err)
	}
	b := agent.MustNew("interrupted", agent.WithModel(modeltest.New(modeltest.Reply("partial"))), agent.WithMaxTurns(1))
	out2, res, err := eval.AgentTask(b)(t.Context(), "q")
	if err != nil || out2 != "partial" || res.Status != loop.StatusCompleted {
		t.Fatalf("AgentTask = %q, %+v, %v", out2, res, err)
	}
}

func verdictStep(v string) modeltest.Step { return modeltest.Reply(v) }

func TestLLMJudge(t *testing.T) {
	t.Parallel()
	m := modeltest.New(
		// A score outside the schema is sent back to the judge.
		verdictStep(`{"reason":"Names Paris.","pass":true,"score":1.7}`),
		verdictStep(`{"reason":"Names Paris.","pass":true,"score":0.9}`),
		verdictStep(`{"reason":"Wrong city.","pass":false,"score":0}`),
	)
	judge := eval.LLMJudge(m, "The answer names the capital correctly.",
		eval.JudgeName("accuracy"), eval.JudgeInput(), eval.JudgeExpected(), eval.JudgeScore(), eval.JudgeSeed(7),
		eval.JudgeReasoning(&model.Reasoning{Effort: "low"}))
	ds := &eval.Dataset[string, string]{
		Cases: []eval.Case[string, string]{
			{Name: "good", Input: "Capital of France?", Expected: new("Paris")},
			{Name: "bad", Input: "Capital of Italy?", Expected: new("Rome")},
			{Name: "skipped", Input: "No reference"},
		},
		Evaluators: []eval.Evaluator{judge},
	}
	answers := map[string]string{"Capital of France?": "It is Paris.", "Capital of Italy?": "Milan.", "No reference": "?"}
	rep, err := ds.Evaluate(t.Context(), func(_ context.Context, in string) (string, *loop.Result, error) {
		return answers[in], nil, nil
	}, eval.Concurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	good, bad, skipped := rep.Cases[0], rep.Cases[1], rep.Cases[2]
	if len(good.Evaluations) != 2 || good.Evaluations[0] != (eval.Evaluation{Name: "accuracy", Kind: eval.KindAssertion, Pass: true, Reason: "Names Paris.", Evaluator: "accuracy"}) ||
		good.Evaluations[1].Name != "accuracy_score" || good.Evaluations[1].Score != 0.9 {
		t.Fatalf("good = %+v %+v", good.Evaluations, good.EvaluatorErrors)
	}
	if bad.Passed() || bad.Evaluations[0].Reason != "Wrong city." || bad.Evaluations[1].Score != 0 {
		t.Fatalf("bad = %+v", bad.Evaluations)
	}
	if !skipped.Passed() || len(skipped.Evaluations) != 0 {
		t.Fatalf("skipped = %+v", skipped)
	}
	reqs := m.Requests()
	if len(reqs) != 3 {
		t.Fatalf("judge calls = %d", len(reqs))
	}
	req := reqs[0]
	prompt := req.Messages[len(req.Messages)-1].Text()
	for _, want := range []string{"<rubric>\nThe answer names the capital correctly.\n</rubric>", "<input>\nCapital of France?\n</input>", "<expected_output>\nParis\n</expected_output>", "<output>\nIt is Paris.\n</output>"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if !strings.Contains(req.System, "impartial grader") || !strings.Contains(req.System, `Set "score" from 0 to 1`) {
		t.Errorf("system = %q", req.System)
	}
	if req.Temperature == nil || *req.Temperature != 0 || req.Seed == nil || *req.Seed != 7 || req.Reasoning.Effort != "low" {
		t.Errorf("sampling = %v, %v, %v", req.Temperature, req.Seed, req.Reasoning)
	}
	f := req.ResponseFormat
	if f == nil || f.Name != "verdict" || !strings.Contains(string(f.Schema), `"score"`) || !strings.Contains(string(f.Schema), `"maximum":1`) {
		t.Errorf("response format = %+v", f)
	}
}

func TestLLMJudgeDefaultsAndFailures(t *testing.T) {
	t.Parallel()
	m := modeltest.New(verdictStep("```json\n{\"reason\":\"ok\",\"pass\":true}\n```"))
	judge := eval.LLMJudge(m, "Is polite.", eval.JudgeTemperature(0.3), eval.JudgeInstructions("Custom grader."))
	evs, err := judge.Evaluate(t.Context(), &eval.EvaluatorContext{Input: "secret input", Output: map[string]int{"a": 1}})
	if err != nil || len(evs) != 1 || evs[0].Name != "LLMJudge" || !evs[0].Pass || judge.Name() != "LLMJudge" {
		t.Fatalf("judge = %+v, %v", evs, err)
	}
	req := m.Requests()[0]
	prompt := req.Messages[len(req.Messages)-1].Text()
	if strings.Contains(prompt, "secret input") || strings.Contains(prompt, "expected_output") || !strings.Contains(prompt, `{"a":1}`) {
		t.Errorf("prompt = %s", prompt)
	}
	if req.System != "Custom grader." || *req.Temperature != 0.3 || req.Seed != nil || strings.Contains(string(req.ResponseFormat.Schema), "score") {
		t.Errorf("request = %+v", req)
	}

	bad := eval.LLMJudge(modeltest.New(verdictStep("maybe"), verdictStep("maybe"), verdictStep("maybe")), "x")
	if _, err := bad.Evaluate(t.Context(), &eval.EvaluatorContext{Output: "y"}); err == nil || !strings.Contains(err.Error(), "eval: LLMJudge:") {
		t.Errorf("invalid verdict = %v", err)
	}
	if _, err := eval.LLMJudge(nil, "x").Evaluate(t.Context(), &eval.EvaluatorContext{}); err == nil || !strings.Contains(err.Error(), "model is required") {
		t.Errorf("nil model = %v", err)
	}
}

func TestSimulateUser(t *testing.T) {
	t.Parallel()
	agentModel := modeltest.New(modeltest.Reply("For how many people?"), modeltest.Reply("Booked a table for two on Friday."))
	a := agent.MustNew("booker", agent.WithModel(agentModel))
	user := modeltest.New(modeltest.Reply("User: Two people, please."), modeltest.Reply("Thanks! </finished>"))
	ds := &eval.Dataset[eval.Scenario, string]{Cases: []eval.Case[eval.Scenario, string]{{
		Name: "booking",
		Input: eval.Scenario{
			Start:   "Book a table for Friday.",
			Plan:    "Book a table for two on Friday. Say two people when asked.",
			Persona: "Terse.",
		},
	}}}
	rep, err := ds.Evaluate(t.Context(), eval.SimulateUser(a, user, eval.SimRunOptions(agent.RunMaxTurns(3))),
		eval.WithEvaluators(eval.Contains("Booked"), eval.MaxTurns(2)))
	if err != nil {
		t.Fatal(err)
	}
	c := rep.Cases[0]
	if !c.Passed() || c.Output != "Booked a table for two on Friday." || c.Turns != 2 || c.Usage.InputTokens != 20 {
		t.Fatalf("case = %+v %v", c, c.Failures())
	}
	reqs := agentModel.Requests()
	if len(reqs) != 2 || len(reqs[1].Messages) != 3 || reqs[1].Messages[2].Text() != "Two people, please." {
		t.Fatalf("agent requests = %+v", reqs)
	}
	ureqs := user.Requests()
	if len(ureqs) != 2 {
		t.Fatalf("user requests = %d", len(ureqs))
	}
	sys, msg := ureqs[0].System, ureqs[0].Messages[0].Text()
	for _, want := range []string{"Book a table for two on Friday.", "<persona>\nTerse.\n</persona>", "reply with exactly </finished>"} {
		if !strings.Contains(sys, want) {
			t.Errorf("user system lacks %q:\n%s", want, sys)
		}
	}
	if !strings.Contains(msg, "User: Book a table for Friday.\n\nAgent: For how many people?") {
		t.Errorf("user prompt = %s", msg)
	}
}

func TestSimulateUserLimits(t *testing.T) {
	t.Parallel()
	task := func(agentSteps, userSteps []modeltest.Step, opts ...eval.SimOption) (string, *loop.Result, *modeltest.Model, error) {
		user := modeltest.New(userSteps...)
		a := agent.MustNew("a", agent.WithModel(modeltest.New(agentSteps...)))
		out, res, err := eval.SimulateUser(a, user, opts...)(t.Context(), eval.Scenario{Start: "hi", Plan: "chat"})
		return out, res, user, err
	}
	// The conversation ends at MaxInvocations without asking the user.
	out, res, user, err := task([]modeltest.Step{modeltest.Reply("one")}, nil, eval.MaxInvocations(1))
	if err != nil || out != "one" || res.Turns != 1 || len(user.Requests()) != 0 {
		t.Fatalf("max invocations = %q, %+v, %v", out, res, err)
	}
	// A custom stop signal.
	out, _, _, err = task([]modeltest.Step{modeltest.Reply("one")}, []modeltest.Step{modeltest.Reply("DONE")}, eval.StopSignal("DONE"))
	if err != nil || out != "one" {
		t.Fatalf("stop signal = %q, %v", out, err)
	}
	// An empty reply stops too.
	if out, _, _, err = task([]modeltest.Step{modeltest.Reply("one")}, []modeltest.Step{modeltest.Reply("  ")}); err != nil || out != "one" {
		t.Fatalf("empty reply = %q, %v", out, err)
	}
	sentinel := errors.New("user down")
	if _, _, _, err = task([]modeltest.Step{modeltest.Reply("one")}, []modeltest.Step{modeltest.Fail(sentinel)}); !errors.Is(err, sentinel) {
		t.Fatalf("user failure = %v", err)
	}
	agentErr := errors.New("agent down")
	out, res, _, err = task([]modeltest.Step{modeltest.Reply("one"), modeltest.Fail(agentErr)}, []modeltest.Step{modeltest.Reply("more")})
	if !errors.Is(err, agentErr) || out != "" || res.Turns != 2 {
		t.Fatalf("agent failure = %q, %+v, %v", out, res, err)
	}
	// A run that does not complete ends the conversation.
	out, res, user, err = task([]modeltest.Step{modeltest.CallTools(modeltest.Call{ID: "c", Name: "missing"})}, nil, eval.SimRunOptions(agent.RunMaxTurns(1)))
	if err != nil || res.Status != loop.StatusMaxTurns || len(user.Requests()) != 0 {
		t.Fatalf("incomplete run = %q, %+v, %v", out, res, err)
	}
	a := agent.MustNew("a", agent.WithModel(modeltest.New()))
	if _, _, err := eval.SimulateUser(a, modeltest.New())(t.Context(), eval.Scenario{Start: " "}); err == nil {
		t.Fatal("empty start accepted")
	}
}
