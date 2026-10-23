package loop_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/tool"
)

type forecast struct {
	City  string   `json:"city"`
	TempC int      `json:"temp_c"`
	Notes []string `json:"notes,omitempty"`
}

func forecastOutput(retries int) *loop.Output {
	return &loop.Output{Name: "forecast", Schema: tool.SchemaOf[forecast](), Retries: retries}
}

func TestOutputRequestsSchemaAndDecodes(t *testing.T) {
	t.Parallel()
	for _, answer := range []string{
		`{"city":"Kyoto","temp_c":21}`,
		"```json\n{\"city\":\"Kyoto\",\"temp_c\":21,\"notes\":null}\n```",
		"\n```\n{\"city\":\"Kyoto\",\"temp_c\":21}\n```\n",
	} {
		m := modeltest.New(modeltest.Reply(answer))
		r := newRuntime(t, loop.Config{Model: m, Output: forecastOutput(0)})
		res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil)
		if err != nil || res.Status != loop.StatusCompleted || res.Output != answer {
			t.Fatalf("Run(%q) = %+v, %v", answer, res, err)
		}
		var got forecast
		if err := res.Decode(&got); err != nil || got.City != "Kyoto" || got.TempC != 21 {
			t.Fatalf("Decode(%q) = %+v, %v", answer, got, err)
		}
		f := m.Requests()[0].ResponseFormat
		if f == nil || f.Name != "forecast" || !f.Strict || !strings.Contains(string(f.Schema), `"additionalProperties":false`) {
			t.Fatalf("response format = %+v", f)
		}
	}
	if f := newRuntime(t, loop.Config{Model: modeltest.New(), Output: &loop.Output{Schema: tool.SchemaOf[forecast]()}}).Config().ResponseFormat; f.Name != "output" {
		t.Fatalf("default name = %q", f.Name)
	}
	// An explicit response format is sent as configured.
	own := &model.ResponseFormat{Name: "own", Schema: json.RawMessage(`{"type":"object"}`)}
	m := modeltest.New(modeltest.Reply(`{"city":"Kyoto","temp_c":21}`))
	r := newRuntime(t, loop.Config{Model: m, Output: forecastOutput(0), ResponseFormat: own})
	if _, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil); err != nil {
		t.Fatal(err)
	}
	if f := m.Requests()[0].ResponseFormat; f.Name != "own" || f.Strict {
		t.Fatalf("response format = %+v", f)
	}
}

func TestOutputRetriesInvalidAnswers(t *testing.T) {
	t.Parallel()
	var finishes []string
	stopHook := &loop.Funcs{MiddlewareName: "stop", BeforeFinishFunc: func(_ context.Context, _ *loop.Run, msg *model.Message) (bool, error) {
		finishes = append(finishes, msg.Text())
		return false, nil
	}}
	m := modeltest.New(
		modeltest.Reply("It is sunny."),
		modeltest.Reply(`{"city":"Kyoto"}`),
		modeltest.Reply(`{"city":"Kyoto","temp_c":21}`),
	)
	r := newRuntime(t, loop.Config{Model: m, Output: forecastOutput(2), Middlewares: []loop.Middleware{stopHook}})
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil)
	if err != nil || res.Status != loop.StatusCompleted || res.Turns != 3 {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	// Invalid answers do not reach BeforeFinish middleware.
	if len(finishes) != 1 || finishes[0] != `{"city":"Kyoto","temp_c":21}` {
		t.Fatalf("BeforeFinish saw %q", finishes)
	}
	reqs := m.Requests()
	first := reqs[1].Messages[len(reqs[1].Messages)-1]
	if first.Role != model.RoleUser || !strings.Contains(first.Text(), "not a single JSON value") || !strings.Contains(first.Text(), `"temp_c"`) {
		t.Fatalf("first reminder = %q", first.Text())
	}
	if second := reqs[2].Messages[len(reqs[2].Messages)-1].Text(); !strings.Contains(second, `missing required property "temp_c"`) {
		t.Fatalf("second reminder = %q", second)
	}
}

func TestOutputFailsAfterRetriesAndResumes(t *testing.T) {
	t.Parallel()
	cp := loop.NewMemoryCheckpointer()
	m := modeltest.New(modeltest.Reply("no"), modeltest.Reply("still no"))
	r := newRuntime(t, loop.Config{Model: m, Output: forecastOutput(1), Checkpointer: cp})
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{RunID: "out", Input: model.UserText("q")}), nil)
	oe, ok := errors.AsType[*loop.OutputError](err)
	if !ok || res.Status != loop.StatusFailed || oe.Attempts != 2 || oe.Output != "still no" || len(oe.Problems) == 0 || !errors.Is(res.Err, oe) {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if !strings.Contains(err.Error(), "after 2 attempts") {
		t.Fatalf("error = %v", err)
	}
	if err := res.Decode(new(forecast)); !errors.Is(err, oe) {
		t.Fatalf("Decode = %v", err)
	}
	// Resuming the failed run gives the model its retries again: the last
	// answer is sent back without calling the model first.
	m.Append(modeltest.Reply(`{"city":"Kyoto","temp_c":21}`))
	res, err = loop.Collect(r.Resume(t.Context(), "out", nil), nil)
	if err != nil || res.Status != loop.StatusCompleted || len(m.Requests()) != 3 {
		t.Fatalf("Resume = %+v, %v; model calls = %d", res, err, len(m.Requests()))
	}
	if _, err := cp.Load(t.Context(), "out"); !errors.Is(err, loop.ErrCheckpointNotFound) {
		t.Fatalf("checkpoint of completed run: %v", err)
	}
}

func TestOutputWithoutRetries(t *testing.T) {
	t.Parallel()
	m := modeltest.New(modeltest.Reply("{}"))
	r := newRuntime(t, loop.Config{Model: m, Output: forecastOutput(-1)})
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil)
	if oe, ok := errors.AsType[*loop.OutputError](err); !ok || oe.Attempts != 1 || res.Status != loop.StatusFailed || len(m.Requests()) != 1 {
		t.Fatalf("Run = %+v, %v", res, err)
	}
}

func TestOutputSkipsIncompleteAndRefusedAnswers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		finish model.FinishReason
		status loop.Status
	}{
		{model.FinishLength, loop.StatusIncomplete},
		{model.FinishContentFilter, loop.StatusIncomplete},
		{model.FinishRefusal, loop.StatusCompleted},
	} {
		m := modeltest.New(modeltest.Respond(&model.Response{Message: model.AssistantText(`{"city":`), FinishReason: tc.finish}))
		r := newRuntime(t, loop.Config{Model: m, Output: forecastOutput(0)})
		res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil)
		if err != nil || res.Status != tc.status || len(m.Requests()) != 1 {
			t.Fatalf("%s: Run = %+v, %v", tc.finish, res, err)
		}
		if err := res.Decode(new(forecast)); err == nil {
			t.Fatalf("%s: Decode accepted the answer", tc.finish)
		}
	}
}

func TestOutputConfigErrors(t *testing.T) {
	t.Parallel()
	for name, o := range map[string]*loop.Output{
		"no schema":      {},
		"not an object":  {Schema: tool.SchemaOf[[]string]()},
		"no properties":  {Schema: tool.SchemaOf[map[string]int]()},
		"invalid name":   {Name: "my output", Schema: tool.SchemaOf[forecast]()},
		"name too long":  {Name: strings.Repeat("a", 65), Schema: tool.SchemaOf[forecast]()},
		"empty struct":   {Schema: tool.SchemaOf[struct{}]()},
		"string as root": {Schema: tool.String("")},
	} {
		if _, err := loop.New(loop.Config{Model: modeltest.New(), Output: o}); err == nil {
			t.Errorf("%s: New accepted the output", name)
		}
	}
}

func TestResultDecode(t *testing.T) {
	t.Parallel()
	var v map[string]any
	for name, res := range map[string]*loop.Result{
		"nil":         nil,
		"failed":      {Status: loop.StatusFailed, Err: errors.New("boom")},
		"max turns":   {Status: loop.StatusMaxTurns, Reason: "limit"},
		"refused":     {Status: loop.StatusCompleted, FinishReason: model.FinishRefusal, Output: "I can't"},
		"not JSON":    {Status: loop.StatusCompleted, Output: "hello"},
		"two blocks":  {Status: loop.StatusCompleted, Output: "```json\n{}\n```\n```json\n{}\n```"},
		"inline code": {Status: loop.StatusCompleted, Output: "```{}```"},
	} {
		if err := res.Decode(&v); err == nil {
			t.Errorf("%s: Decode succeeded", name)
		}
		if _, err := res.DecodeAs[map[string]any](); err == nil {
			t.Errorf("%s: DecodeAs succeeded", name)
		}
	}
	res := &loop.Result{Status: loop.StatusCompleted, Output: "```JSON\n{\"a\":1}\n```"}
	if err := res.Decode(&v); err != nil || v["a"] != float64(1) {
		t.Fatalf("Decode = %v, %v", v, err)
	}
	if got, err := res.DecodeAs[struct{ A int }](); err != nil || got.A != 1 {
		t.Fatalf("DecodeAs = %+v, %v", got, err)
	}
}

func TestOutputRetryEvents(t *testing.T) {
	t.Parallel()
	m := modeltest.New(
		modeltest.Reply("It is sunny."),
		modeltest.Reply(`{"city":"Kyoto"}`),
		modeltest.Reply(`{"city":"Kyoto","temp_c":21}`),
	)
	r := newRuntime(t, loop.Config{Model: m, Output: forecastOutput(0)})
	var retries []*loop.OutputRetry
	var types []loop.EventType
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), func(ev *loop.Event) {
		types = append(types, ev.Type)
		if ev.Type == loop.EventOutputRetry {
			if ev.Turn == 0 || ev.RunID == "" {
				t.Errorf("event not stamped: %+v", ev)
			}
			retries = append(retries, ev.OutputRetry)
		}
	})
	if err != nil || res.Status != loop.StatusCompleted {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if len(retries) != 2 {
		t.Fatalf("output retries = %+v", retries)
	}
	for i, want := range []struct {
		output, problem string
	}{{"It is sunny.", "not a single JSON value"}, {`{"city":"Kyoto"}`, `missing required property "temp_c"`}} {
		got := retries[i]
		if got.Attempt != i+1 || got.Retries != loop.DefaultOutputRetries || got.Output != want.output || len(got.Problems) == 0 || !strings.Contains(got.Problems[0], want.problem) {
			t.Errorf("retry %d = %+v", i, got)
		}
	}
	// Each retry follows the rejected message and precedes the next turn.
	for i, typ := range types {
		if typ == loop.EventOutputRetry && (types[i-1] != loop.EventMessage || types[i+1] != loop.EventTurnStart) {
			t.Fatalf("events = %v", types)
		}
	}
}

// A consumer that stops at an output retry leaves a checkpoint from which
// the run continues with the answer already sent back.
func TestOutputRetryEventAndResume(t *testing.T) {
	t.Parallel()
	cp := loop.NewMemoryCheckpointer()
	m := modeltest.New(modeltest.Reply("no"))
	r := newRuntime(t, loop.Config{Model: m, Output: forecastOutput(0), Checkpointer: cp})
	stopped := false
	for ev, err := range r.Run(t.Context(), &loop.Request{RunID: "retry", Input: model.UserText("q")}) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == loop.EventOutputRetry {
			stopped = true
			break
		}
	}
	if !stopped {
		t.Fatal("no output retry event")
	}
	m.Append(modeltest.Reply(`{"city":"Kyoto","temp_c":21}`))
	var retries int
	res, err := loop.Collect(r.Resume(t.Context(), "retry", nil), func(ev *loop.Event) {
		if ev.Type == loop.EventOutputRetry {
			retries++
		}
	})
	if err != nil || res.Status != loop.StatusCompleted || retries != 0 || len(m.Requests()) != 2 {
		t.Fatalf("Resume = %+v, %v; retries = %d, model calls = %d", res, err, retries, len(m.Requests()))
	}
	last := m.Requests()[1].Messages
	if reminder := last[len(last)-1].Text(); !strings.Contains(reminder, "not a single JSON value") {
		t.Fatalf("reminder = %q", reminder)
	}
}
