package agent_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/session"
	"github.com/nostosx/agentx/tool"
)

type user struct{ Name string }

func TestInstructionsFunc(t *testing.T) {
	t.Parallel()
	perRun, perTurn := 0, 0
	cp := loop.NewMemoryCheckpointer()
	write := tool.Func("write", "", func(context.Context, struct{}) (string, error) { return "w", nil }, tool.RequireApproval())
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c", Name: "write"}), modeltest.Reply("done"))
	a := agent.MustNew("assistant", agent.WithModel(m), agent.WithTools(write), agent.WithCheckpointer(cp),
		agent.WithInstructions("Be brief."),
		agent.WithInstructionsFunc(func(ctx context.Context, run *loop.Run) (string, error) {
			perRun++
			u, _ := loop.Deps[user](ctx)
			return "The user is " + u.Name + ".", nil
		}),
		agent.WithInstructionsFunc(func(context.Context, *loop.Run) (string, error) { return "  ", nil }),
		agent.WithInstructionsFunc(func(_ context.Context, run *loop.Run) (string, error) {
			perTurn++
			return fmt.Sprintf("Turn %d.", run.Turn()), nil
		}, agent.EveryTurn()),
		agent.WithDeps(user{Name: "Ada"}))
	res, err := loop.Collect(a.Run(t.Context(), agent.Text("q")), nil)
	if err != nil || res.Status != loop.StatusInterrupted {
		t.Fatalf("run = %+v, %v", res, err)
	}
	// The resumed run keeps the system prompt of the run; only the
	// per-turn text is evaluated again. Dependencies are passed again.
	res, err = loop.Collect(a.Resume(loop.WithDeps(t.Context(), user{Name: "Bob"}), res.RunID, map[string]loop.Approval{"c": loop.Approve}), nil)
	if err != nil || res.Output != "done" || perRun != 1 || perTurn != 2 {
		t.Fatalf("resume = %+v, %v; per run %d, per turn %d", res, err, perRun, perTurn)
	}
	reqs := m.Requests()
	if reqs[0].System != "Be brief.\n\nThe user is Ada.\n\nTurn 1." || reqs[1].System != "Be brief.\n\nThe user is Ada.\n\nTurn 2." {
		t.Fatalf("system prompts = %q, %q", reqs[0].System, reqs[1].System)
	}

	failing := agent.MustNew("assistant", agent.WithModel(modeltest.New()), agent.WithInstructionsFunc(func(context.Context, *loop.Run) (string, error) {
		return "", errors.New("profile service down")
	}))
	if res, err := loop.Collect(failing.Run(t.Context(), agent.Text("q")), nil); err == nil || res.Status != loop.StatusFailed || !strings.Contains(err.Error(), "profile service down") {
		t.Fatalf("failing instructions = %+v, %v", res, err)
	}
}

func TestRenderTemplate(t *testing.T) {
	t.Parallel()
	st := &session.State{}
	st.Set("name", "Ada")
	st.Set("prefs", map[string]any{"lang": "go"})
	st.Set("app:tier", 2)
	ctx := agent.ContextWithSessionState(t.Context(), st)
	for _, tt := range []struct{ in, want, err string }{
		{in: "plain", want: "plain"},
		{in: "Hi {name}, {prefs} tier {app:tier}.", want: `Hi Ada, {"lang":"go"} tier 2.`},
		{in: "{missing?}|{{name}}|{ name }|{\"k\": 1}|}}|{", want: `|{name}|{ name }|{"k": 1}|}|{`},
		{in: "{1x} {a-b?}", want: "{1x} "},
		{in: "Hi {missing}", err: "no value for {missing}"},
	} {
		got, err := agent.RenderTemplate(ctx, tt.in)
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("RenderTemplate(%q) error = %v", tt.in, err)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("RenderTemplate(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
	if _, err := agent.RenderTemplate(t.Context(), "{name}"); err == nil {
		t.Error("RenderTemplate without a session state filled a required key")
	}
}

func TestInstructionTemplateAndSessionState(t *testing.T) {
	t.Parallel()
	type profile struct {
		Lang  string   `json:"lang"`
		Langs []string `json:"langs"`
	}
	remember := tool.Func("remember", "", func(ctx context.Context, in struct {
		Lang string `json:"lang"`
	}) (string, error) {
		st, ok := agent.SessionState(ctx)
		if !ok {
			return "", errors.New("no session")
		}
		p, _, err := st.Value[profile]("profile")
		if err != nil {
			return "", err
		}
		p.Lang = in.Lang
		p.Langs = append(p.Langs, in.Lang)
		st.Set("profile", p)
		st.Set("lang", in.Lang)
		return "saved", nil
	})
	store := session.NewMemoryStore()
	m := modeltest.New(
		modeltest.CallTools(modeltest.Call{ID: "c", Name: "remember", Args: map[string]string{"lang": "go"}}),
		modeltest.Reply("noted"),
		modeltest.Reply("hi"),
	)
	a := agent.MustNew("assistant", agent.WithModel(m), agent.WithTools(remember), agent.WithInstructionTemplate(),
		agent.WithInstructions("Answer in {lang?}. JSON looks like {\"a\": 1}."))
	runner := agent.NewRunner(a, store)
	if res, err := runner.RunText(t.Context(), "s", "I like Go"); err != nil || res.Output != "noted" {
		t.Fatalf("first run = %+v, %v", res, err)
	}
	if res, err := runner.RunText(t.Context(), "s", "hello"); err != nil || res.Output != "hi" {
		t.Fatalf("second run = %+v, %v", res, err)
	}
	reqs := m.Requests()
	if reqs[0].System != `Answer in . JSON looks like {"a": 1}.` || reqs[2].System != `Answer in go. JSON looks like {"a": 1}.` {
		t.Fatalf("system prompts = %q, %q", reqs[0].System, reqs[2].System)
	}
	saved, err := store.Get(t.Context(), "s")
	if err != nil {
		t.Fatal(err)
	}
	p, ok, err := saved.State().Value[profile]("profile")
	if err != nil || !ok || p.Lang != "go" || len(p.Langs) != 1 {
		t.Fatalf("saved profile = %+v, %v, %v", p, ok, err)
	}

	// Outside a Runner tools see no session state, and a required
	// placeholder fails the run.
	m.Append(modeltest.CallTools(modeltest.Call{ID: "c", Name: "remember", Args: map[string]string{"lang": "go"}}), modeltest.Reply("x"))
	res, err := loop.Collect(a.Run(t.Context(), agent.Text("q")), nil)
	if err != nil || !strings.Contains(res.Messages[2].ToolResults()[0].Content, "no session") {
		t.Fatalf("run without a session = %+v, %v", res, err)
	}
	strict := agent.MustNew("assistant", agent.WithModel(m), agent.WithInstructionTemplate(), agent.WithInstructions("Hi {name}."))
	if res, err := loop.Collect(strict.Run(t.Context(), agent.Text("q")), nil); err == nil || res.Status != loop.StatusFailed {
		t.Fatalf("missing key = %+v, %v", res, err)
	}
	st := &session.State{}
	st.Set("name", "Ada")
	m.Append(modeltest.Reply("y"))
	if res, err := loop.Collect(strict.Run(agent.ContextWithSessionState(t.Context(), st), agent.Text("q")), nil); err != nil || res.Output != "y" ||
		m.Requests()[len(m.Requests())-1].System != "Hi Ada." {
		t.Fatalf("explicit state = %+v, %v", res, err)
	}
}

func TestSessionStateSavedWhenInterrupted(t *testing.T) {
	t.Parallel()
	// The calls run concurrently; the lock makes their read-modify-write
	// of the state atomic.
	var mu sync.Mutex
	mark := tool.Func("mark", "", func(ctx context.Context, _ struct{}) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		st, _ := agent.SessionState(ctx)
		n, _, _ := st.Value[int]("count")
		st.Set("count", n+1)
		return "marked", nil
	}, tool.ReadOnly())
	write := tool.Func("write", "", func(context.Context, struct{}) (string, error) { return "w", nil }, tool.RequireApproval())
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "a", Name: "mark"}, modeltest.Call{ID: "b", Name: "mark"}), modeltest.CallTools(modeltest.Call{ID: "c", Name: "write"}), modeltest.Reply("done"))
	runner := agent.NewRunner(agent.MustNew("assistant", agent.WithModel(m), agent.WithTools(mark, write)), nil)
	res, err := runner.RunText(t.Context(), "s", "q")
	if err != nil || res.Status != loop.StatusInterrupted {
		t.Fatalf("run = %+v, %v", res, err)
	}
	saved, _ := runner.Store().Get(t.Context(), "s")
	if n, _, _ := saved.State().Value[int]("count"); n != 2 {
		t.Fatalf("count = %d", n)
	}
}

func TestDependencies(t *testing.T) {
	t.Parallel()
	type db struct{ name string }
	var seen []string
	query := tool.Func("query", "", func(ctx context.Context, _ struct{}) (string, error) {
		d, ok := loop.Deps[*db](ctx)
		if !ok {
			return "", errors.New("no db")
		}
		seen = append(seen, d.name)
		return d.name, nil
	}, tool.RequireApproval())
	hooked := ""
	leaf := agent.MustNew("leaf", agent.WithModel(modeltest.New(modeltest.CallTools(modeltest.Call{ID: "l", Name: "query"}), modeltest.Reply("leaf done"))), agent.WithTools(query),
		agent.WithApprover(loop.ApproverFunc(func(context.Context, *loop.ApprovalRequest) (loop.Approval, error) { return loop.Approve, nil })))
	m := modeltest.New(
		modeltest.CallTools(modeltest.Call{ID: "c", Name: "query"}), modeltest.Reply("one"),
		modeltest.CallTools(modeltest.Call{ID: "c", Name: "query"}), modeltest.Reply("two"),
		modeltest.CallTools(delegate("d", "leaf", "go")), modeltest.Reply("three"),
	)
	a := agent.MustNew("assistant", agent.WithModel(m), agent.WithTools(query), agent.WithSubAgents(leaf),
		agent.WithDeps(&db{name: "default"}),
		agent.WithHook(hook.PreToolUse, "query", hook.Func(func(ctx context.Context, _ *hook.Event) (*hook.Result, error) {
			d, _ := loop.Deps[*db](ctx)
			hooked = d.name
			return nil, nil
		})))
	runner := agent.NewRunner(a, nil)
	res, err := runner.RunText(t.Context(), "s", "q", agent.RunDeps(&db{name: "tenant"}))
	if err != nil || res.Status != loop.StatusInterrupted || hooked != "tenant" {
		t.Fatalf("run = %+v, %v; hook saw %q", res, err, hooked)
	}
	// Dependencies are not saved: the resumed run gets them again.
	res, err = loop.Collect(runner.Resume(t.Context(), "s", map[string]loop.Approval{"c": loop.Approve}, agent.RunDeps(&db{name: "resumed"})), nil)
	if err != nil || res.Output != "one" {
		t.Fatalf("resume = %+v, %v", res, err)
	}
	// Without run dependencies the agent's default applies; one in the
	// context takes precedence over it.
	if _, err := runner.RunText(t.Context(), "s", "q"); err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Collect(runner.Resume(t.Context(), "s", map[string]loop.Approval{"c": loop.Approve}), nil); err != nil {
		t.Fatal(err)
	}
	// A sub-agent sees the dependencies of the run that called it.
	if res, err := runner.RunText(loop.WithDeps(t.Context(), &db{name: "ctx"}), "s", "q"); err != nil || res.Output != "three" {
		t.Fatalf("sub-agent run = %+v, %v", res, err)
	}
	if strings.Join(seen, ",") != "resumed,default,ctx" {
		t.Fatalf("tools saw %v", seen)
	}
}
