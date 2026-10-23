package permission_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/permission"
	"github.com/nostosx/agentx/session"
	"github.com/nostosx/agentx/tool"
	"github.com/nostosx/agentx/tool/builtin"
)

// fakeBash is a bash tool that records the commands it runs.
type fakeBash struct{ ran atomic.Int32 }

func (b *fakeBash) tool() tool.Tool {
	type in struct {
		Command string `json:"command"`
	}
	return tool.Func("bash", "run", func(_ context.Context, a in) (string, error) {
		b.ran.Add(1)
		return "ran " + a.Command, nil
	}, tool.RequireApproval())
}

func bashCall(id, cmd string) modeltest.Step {
	return modeltest.CallTools(modeltest.Call{ID: id, Name: "bash", Args: map[string]string{"command": cmd}})
}

func toolResults(msgs []*model.Message) []*model.ToolResult {
	var out []*model.ToolResult
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Type == model.PartToolResult {
				out = append(out, p.ToolResult)
			}
		}
	}
	return out
}

func TestMiddlewareDeniesAndAllows(t *testing.T) {
	t.Parallel()
	b := &fakeBash{}
	p := permission.MustNew(permission.Config{Dir: t.TempDir(), Rules: []permission.Rules{{
		Allow: []string{"Bash(npm test)"}, Deny: []string{"Bash(rm *)"},
	}}})
	m := modeltest.New(bashCall("c1", "rm -rf build"), bashCall("c2", "npm test"), modeltest.Reply("done"))
	a := agent.MustNew("a", agent.WithModel(m), agent.WithTools(b.tool()), agent.WithPermissions(p))
	res, err := loop.Collect(a.Run(t.Context(), agent.Text("go")), nil)
	if err != nil || res.Status != loop.StatusCompleted {
		t.Fatalf("run = %+v, %v", res, err)
	}
	results := toolResults(res.Messages)
	if len(results) != 2 || !results[0].IsError || !strings.Contains(results[0].Content, `"Bash(rm *)"`) || results[1].Content != "ran npm test" {
		t.Fatalf("results = %+v", results)
	}
	if b.ran.Load() != 1 {
		t.Fatalf("ran %d commands", b.ran.Load())
	}
}

func TestMiddlewareAsks(t *testing.T) {
	t.Parallel()
	b := &fakeBash{}
	p := permission.MustNew(permission.Config{Dir: t.TempDir(), Mode: permission.ModeBypassPermissions,
		Rules: []permission.Rules{{Ask: []string{"Bash(git push *)"}}}})
	var reasons []string
	approver := loop.ApproverFunc(func(_ context.Context, req *loop.ApprovalRequest) (loop.Approval, error) {
		reasons = append(reasons, req.Reason)
		return loop.Deny, nil
	})
	m := modeltest.New(bashCall("c1", "make"), bashCall("c2", "git push origin main"), modeltest.Reply("done"))
	a := agent.MustNew("a", agent.WithModel(m), agent.WithTools(b.tool()), agent.WithPermissions(p), agent.WithApprover(approver))
	if _, err := loop.Collect(a.Run(t.Context(), agent.Text("go")), nil); err != nil {
		t.Fatal(err)
	}
	if b.ran.Load() != 1 || len(reasons) != 1 || !strings.Contains(reasons[0], "git push") {
		t.Fatalf("ran %d, asked %q", b.ran.Load(), reasons)
	}
}

func TestRememberSession(t *testing.T) {
	t.Parallel()
	b := &fakeBash{}
	p := permission.MustNew(permission.Config{Dir: t.TempDir()})
	var asked atomic.Int32
	scope := loop.RememberSession
	approver := loop.ApproverFunc(func(context.Context, *loop.ApprovalRequest) (loop.Approval, error) {
		asked.Add(1)
		return loop.Approval{Approved: true, Remember: scope}, nil
	})
	m := modeltest.New(
		bashCall("c1", "npm test"), modeltest.Reply("one"),
		bashCall("c2", "npm test"), modeltest.Reply("two"),
		bashCall("c3", "npm test --watch"), modeltest.Reply("three"),
	)
	a := agent.MustNew("a", agent.WithModel(m), agent.WithTools(b.tool()), agent.WithPermissions(p), agent.WithApprover(approver))
	runner := agent.NewRunner(a, nil)
	for i, want := range []int32{1, 1, 2} {
		if _, err := runner.RunText(t.Context(), "s", "go"); err != nil {
			t.Fatal(err)
		}
		if asked.Load() != want {
			t.Fatalf("run %d: asked %d times, want %d", i, asked.Load(), want)
		}
	}
	sess, err := runner.Store().Get(t.Context(), "s")
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]string
	if ok, err := sess.State().Decode(agent.SessionValuesKey, &values); !ok || err != nil {
		t.Fatalf("session values: %v %v", ok, err)
	}
	var grants []string
	if err := json.Unmarshal([]byte(values[permission.SessionGrantsKey]), &grants); err != nil ||
		strings.Join(grants, "|") != "Bash(npm test)|Bash(npm test --watch)" {
		t.Fatalf("grants = %q, %v", grants, err)
	}
	// Another session does not share the grants.
	m.Append(bashCall("c4", "npm test"), modeltest.Reply("four"))
	if _, err := runner.RunText(t.Context(), "other", "go"); err != nil {
		t.Fatal(err)
	}
	if asked.Load() != 3 {
		t.Fatalf("other session: asked %d times", asked.Load())
	}
}

func TestRememberRunAndResume(t *testing.T) {
	t.Parallel()
	b := &fakeBash{}
	p := permission.MustNew(permission.Config{Dir: t.TempDir()})
	m := modeltest.New(
		bashCall("c1", "npm test"), bashCall("c2", "npm test"), modeltest.Reply("one"),
		bashCall("c3", "npm test"), modeltest.Reply("two"),
	)
	a := agent.MustNew("a", agent.WithModel(m), agent.WithTools(b.tool()), agent.WithPermissions(p),
		agent.WithCheckpointer(loop.NewMemoryCheckpointer()))
	runner := agent.NewRunner(a, nil)
	res, err := runner.RunText(t.Context(), "s", "go")
	if err != nil || res.Status != loop.StatusInterrupted {
		t.Fatalf("run = %+v, %v", res, err)
	}
	approve := loop.Approval{Approved: true, Remember: loop.RememberRun, RememberRules: []string{"Bash(npm *)"}}
	res, err = loop.Collect(runner.Resume(t.Context(), "s", map[string]loop.Approval{res.Interrupt.Requests[0].Call.ID: approve}), nil)
	// The second call of the run is approved by the remembered rule.
	if err != nil || res.Status != loop.StatusCompleted || b.ran.Load() != 2 {
		t.Fatalf("resume = %+v, %v; ran %d", res, err, b.ran.Load())
	}
	// A run grant does not outlive its run.
	res, err = runner.RunText(t.Context(), "s", "again")
	if err != nil || res.Status != loop.StatusInterrupted {
		t.Fatalf("next run = %+v, %v", res, err)
	}
}

func TestRunMode(t *testing.T) {
	t.Parallel()
	b := &fakeBash{}
	p := permission.MustNew(permission.Config{Dir: t.TempDir(), Rules: []permission.Rules{{Allow: []string{"Bash"}}}})
	m := modeltest.New(bashCall("c1", "npm test"), modeltest.Reply("one"), bashCall("c2", "npm test"), modeltest.Reply("two"))
	a := agent.MustNew("a", agent.WithModel(m), agent.WithTools(b.tool()), agent.WithPermissions(p))
	runner := agent.NewRunner(a, nil)
	res, err := runner.RunText(t.Context(), "s", "plan it", agent.RunValue(permission.ModeKey, string(permission.ModePlan)))
	if err != nil || b.ran.Load() != 0 || !strings.Contains(toolResults(res.Messages)[0].Content, "Plan mode") {
		t.Fatalf("plan run = %+v, %v; ran %d", res, err, b.ran.Load())
	}
	// The mode is per run.
	if _, err := runner.RunText(t.Context(), "s", "do it"); err != nil || b.ran.Load() != 1 {
		t.Fatalf("default run: %v; ran %d", err, b.ran.Load())
	}
}

func TestSetMode(t *testing.T) {
	t.Parallel()
	b := &fakeBash{}
	p := permission.MustNew(permission.Config{Dir: t.TempDir()})
	var bypassErr error
	switcher := &loop.Funcs{MiddlewareName: "switch", BeforeRunFunc: func(_ context.Context, run *loop.Run, _ *loop.RunInput) error {
		bypassErr = p.SetMode(run, permission.ModeBypassPermissions)
		return p.SetMode(run, permission.ModeAcceptEdits)
	}, AfterRunFunc: func(_ context.Context, run *loop.Run, _ *loop.Result) {
		if p.Mode(run) != permission.ModeAcceptEdits {
			t.Errorf("mode = %s", p.Mode(run))
		}
	}}
	m := modeltest.New(bashCall("c1", "mkdir -p out"), modeltest.Reply("done"))
	a := agent.MustNew("a", agent.WithModel(m), agent.WithTools(b.tool()), agent.WithMiddleware(switcher), agent.WithPermissions(p))
	res, err := loop.Collect(a.Run(t.Context(), agent.Text("go")), nil)
	if err != nil || res.Status != loop.StatusCompleted || b.ran.Load() != 1 {
		t.Fatalf("run = %+v, %v; ran %d", res, err, b.ran.Load())
	}
	if bypassErr == nil {
		t.Fatal("switching to bypassPermissions was allowed")
	}
	// An invalid mode in the run value falls back to the policy's mode.
	m.Append(bashCall("c2", "npm test"), modeltest.Reply("done"))
	a2 := agent.MustNew("a", agent.WithModel(m), agent.WithTools(b.tool()), agent.WithPermissions(p))
	res, err = loop.Collect(a2.Run(t.Context(), &agent.Input{Message: model.UserText("go"),
		Values: map[string]string{permission.ModeKey: string(permission.ModeBypassPermissions)}}), nil)
	if err != nil || res.Status != loop.StatusInterrupted {
		t.Fatalf("bypass via value = %+v, %v", res, err)
	}
}

func TestCheckRedirect(t *testing.T) {
	t.Parallel()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("secret page"))
	}))
	t.Cleanup(target.Close)
	other := strings.Replace(target.URL, "127.0.0.1", "localhost", 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/same":
			http.Redirect(w, r, "/final", http.StatusFound)
		case "/final":
			_, _ = w.Write([]byte("same host page"))
		default:
			http.Redirect(w, r, other+"/x", http.StatusFound)
		}
	}))
	t.Cleanup(origin.Close)

	fetch := func(p *permission.Policy, url string) *tool.Result {
		t.Helper()
		wf := builtin.WebFetch(builtin.Config{AllowPrivateNetwork: true, CheckRedirect: p.CheckRedirect})
		res, err := wf.Call(t.Context(), args("url", url))
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	deny := permission.MustNew(permission.Config{Dir: t.TempDir(), Rules: []permission.Rules{{Deny: []string{"WebFetch(domain:localhost)"}}}})
	if res := fetch(deny, origin.URL+"/away"); !res.IsError || !strings.Contains(res.Content, "refused") {
		t.Fatalf("denied redirect: %+v", res)
	}
	askRules := permission.MustNew(permission.Config{Dir: t.TempDir(), Rules: []permission.Rules{{Ask: []string{"WebFetch(domain:localhost)", "WebFetch(domain:127.0.0.1)"}}}})
	if res := fetch(askRules, origin.URL+"/away"); !res.IsError || !strings.Contains(res.Content, "needs approval") {
		t.Fatalf("asked redirect: %+v", res)
	}
	// Redirects within the host that was decided are followed.
	if res := fetch(askRules, origin.URL+"/same"); res.IsError || !strings.Contains(res.Content, "same host page") {
		t.Fatalf("same-host redirect: %+v", res)
	}
	open := permission.MustNew(permission.Config{Dir: t.TempDir()})
	if res := fetch(open, origin.URL+"/away"); res.IsError || !strings.Contains(res.Content, "secret page") {
		t.Fatalf("allowed redirect: %+v", res)
	}

	// Inside a run, the run's mode and grants apply.
	wf := builtin.WebFetch(builtin.Config{AllowPrivateNetwork: true, CheckRedirect: askRules.CheckRedirect})
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c", Name: "web_fetch", Args: map[string]string{"url": origin.URL + "/away"}}), modeltest.Reply("done"))
	a := agent.MustNew("a", agent.WithModel(m), agent.WithTools(wf), agent.WithPermissions(askRules), agent.WithApprover(loop.ApproverFunc(
		func(context.Context, *loop.ApprovalRequest) (loop.Approval, error) {
			return loop.Approval{Approved: true}, nil
		})))
	res, err := loop.Collect(a.Run(t.Context(), agent.Text("go")), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := toolResults(res.Messages)[0]; !r.IsError || !strings.Contains(r.Content, "needs approval") {
		t.Fatalf("redirect in run: %+v", r)
	}
}

func TestHooksSeePermissionMode(t *testing.T) {
	t.Parallel()
	var modes []string
	record := hook.Func(func(_ context.Context, ev *hook.Event) (*hook.Result, error) {
		modes = append(modes, ev.PermissionMode)
		return nil, nil
	})
	p := permission.MustNew(permission.Config{Mode: permission.ModeAcceptEdits, Dir: t.TempDir()})
	m := modeltest.New(bashCall("c1", "ls"), modeltest.Reply("done"), bashCall("c2", "ls"), modeltest.Reply("done"))
	a := agent.MustNew("a", agent.WithModel(m), agent.WithTools((&fakeBash{}).tool()),
		agent.WithHook(hook.PreToolUse, "", record), agent.WithPermissions(p))
	runner := agent.NewRunner(a, session.NewMemoryStore())
	if _, err := runner.RunText(t.Context(), "s", "go"); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.RunText(t.Context(), "s", "plan", agent.RunValue(permission.ModeKey, string(permission.ModePlan))); err != nil {
		t.Fatal(err)
	}
	if len(modes) != 2 || modes[0] != "acceptEdits" || modes[1] != "plan" {
		t.Fatalf("modes = %q", modes)
	}
}

// TestPlanModeHandoff hands a run in plan mode off: the handoff is
// allowed, and the agent handed off to plans too although its own policy
// defaults to another mode.
func TestPlanModeHandoff(t *testing.T) {
	t.Parallel()
	b := &fakeBash{}
	bp := permission.MustNew(permission.Config{Dir: t.TempDir(), Rules: []permission.Rules{{Allow: []string{"Bash"}}}})
	bm := modeltest.New(bashCall("c1", "npm test"), modeltest.Reply("planned"))
	builder := agent.MustNew("builder", agent.WithModel(bm), agent.WithTools(b.tool()), agent.WithPermissions(bp))
	tp := permission.MustNew(permission.Config{Dir: t.TempDir(), Mode: permission.ModePlan})
	tm := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "h", Name: agent.HandoffToolPrefix + "builder"}))
	triage := agent.MustNew("triage", agent.WithModel(tm), agent.WithHandoffs(builder), agent.WithPermissions(tp))
	res, err := loop.Collect(triage.Run(t.Context(), agent.Text("build it")), nil)
	if err != nil || res.Status != loop.StatusCompleted || res.Agent != "builder" || res.Output != "planned" {
		t.Fatalf("run = %+v, %v", res, err)
	}
	results := toolResults(res.Messages)
	if len(results) != 2 || results[0].IsError || results[1].Name != "bash" || !strings.Contains(results[1].Content, "Plan mode") || b.ran.Load() != 0 {
		t.Fatalf("results = %+v; ran %d", results, b.ran.Load())
	}
}
