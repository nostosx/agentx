package agent_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model/modeltest"
)

func TestRunnerSessionValues(t *testing.T) {
	t.Parallel()
	var seen []map[string]string
	mw := &loop.Funcs{MiddlewareName: "count", BeforeRunFunc: func(_ context.Context, run *loop.Run, _ *loop.RunInput) error {
		s, _ := run.Value("session.count")
		r, _ := run.Value("run.only")
		o, _ := run.Value("option")
		seen = append(seen, map[string]string{"session": s, "run": r, "option": o})
		run.SetValue("session.count", s+"x")
		run.SetValue("run.only", "set")
		return nil
	}}
	m := modeltest.New(modeltest.Reply("1"), modeltest.Reply("2"), modeltest.Reply("3"))
	runner := agent.NewRunner(agent.MustNew("a", agent.WithModel(m), agent.WithMiddleware(mw)), nil)
	for _, opts := range [][]agent.RunOption{nil, {agent.RunValue("option", "o")}} {
		if _, err := runner.RunText(t.Context(), "s", "go", opts...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runner.RunText(t.Context(), "other", "go"); err != nil {
		t.Fatal(err)
	}
	want := []map[string]string{
		{"session": "", "run": "", "option": ""},
		{"session": "x", "run": "", "option": "o"},
		{"session": "", "run": "", "option": ""},
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("run values = %v, want %v", seen, want)
	}
	sess, err := runner.Store().Get(t.Context(), "s")
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]string
	if ok, err := sess.State().Decode(agent.SessionValuesKey, &values); !ok || err != nil || len(values) != 1 || values["session.count"] != "xx" {
		t.Fatalf("saved values %v, %v, %v", values, ok, err)
	}
}
