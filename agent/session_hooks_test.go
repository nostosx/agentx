package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/session"
	"github.com/nostosx/agentx/tool"
)

// sessionLog records session events.
type sessionLog struct {
	mu     sync.Mutex
	events []hook.Event
}

func (l *sessionLog) hook(ctx string) hook.Func {
	return func(_ context.Context, ev *hook.Event) (*hook.Result, error) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.events = append(l.events, *ev)
		if ctx == "" {
			return nil, nil
		}
		return &hook.Result{AdditionalContext: ctx + " " + ev.Reason}, nil
	}
}

func (l *sessionLog) got() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, ev := range l.events {
		out = append(out, string(ev.Type)+":"+ev.Reason+":"+ev.SessionID)
	}
	return out
}

func TestRunnerSessionStartSources(t *testing.T) {
	t.Parallel()
	store := session.NewMemoryStore()
	log := &sessionLog{}
	steps := make([]modeltest.Step, 8)
	for i := range steps {
		steps[i] = modeltest.Reply("ok")
	}
	m := modeltest.New(steps...)
	newAgent := func() *agent.LLMAgent {
		return agent.MustNew("assistant", agent.WithModel(m),
			agent.WithHook(hook.SessionStart, "", log.hook("context")),
			agent.WithHook(hook.SessionEnd, "", log.hook("")))
	}
	first := agent.NewRunner(newAgent(), store)
	if _, err := first.RunText(t.Context(), "s", "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := first.RunText(t.Context(), "s", "two"); err != nil {
		t.Fatal(err)
	}
	// A new Runner, like a new process, resumes the stored session once.
	second := agent.NewRunner(newAgent(), store)
	for _, text := range []string{"three", "four"} {
		if _, err := second.RunText(t.Context(), "s", text); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"SessionStart:startup:s", "SessionStart:resume:s"}
	if got := log.got(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", got, want)
	}
	// The resume context reaches the next run's input, once.
	reqs := m.Requests()
	if in := reqs[2].Messages[len(reqs[2].Messages)-1].Text(); !strings.Contains(in, "context resume") || !strings.Contains(in, "three") {
		t.Fatalf("resumed input = %q", in)
	}
	if in := reqs[3].Messages[len(reqs[3].Messages)-1].Text(); strings.Contains(in, "context") {
		t.Fatalf("context repeated: %q", in)
	}

	// EndSession fires SessionEnd; the next run starts the session again.
	if err := second.EndSession(t.Context(), "s", "logout"); err != nil {
		t.Fatal(err)
	}
	if _, err := second.RunText(t.Context(), "s", "five"); err != nil {
		t.Fatal(err)
	}
	cleared, err := second.Clear(t.Context(), "s")
	if err != nil || cleared == "s" {
		t.Fatalf("Clear = %q, %v", cleared, err)
	}
	if sess, err := store.Get(t.Context(), cleared); err != nil || len(sess.Messages()) != 0 {
		t.Fatalf("cleared session = %v, %v", sess, err)
	}
	if _, err := second.RunText(t.Context(), cleared, "six"); err != nil {
		t.Fatal(err)
	}
	forked, err := second.Fork(t.Context(), cleared)
	if err != nil {
		t.Fatal(err)
	}
	if sess, err := store.Get(t.Context(), forked); err != nil || len(sess.Messages()) != 2 {
		t.Fatalf("forked session = %v, %v", sess, err)
	}
	if _, err := second.RunText(t.Context(), forked, "seven"); err != nil {
		t.Fatal(err)
	}
	want = append(want,
		"SessionEnd:logout:s", "SessionStart:resume:s",
		"SessionEnd:clear:s", "SessionStart:clear:"+cleared,
		"SessionStart:fork:"+forked)
	if got := log.got(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", got, want)
	}
	log.mu.Lock()
	end := log.events[2]
	log.mu.Unlock()
	if len(end.Messages) != 8 || end.Agent != "assistant" {
		t.Fatalf("SessionEnd event = %+v", end)
	}
}

func TestRunnerResumeFiresSessionStart(t *testing.T) {
	t.Parallel()
	store := session.NewMemoryStore()
	log := &sessionLog{}
	deploy := tool.Func("deploy", "deploy", func(context.Context, struct{}) (string, error) { return "deployed", nil }, tool.RequireApproval())
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c", Name: "deploy"}), modeltest.Reply("done"))
	newAgent := func() *agent.LLMAgent {
		return agent.MustNew("ops", agent.WithModel(m), agent.WithTools(deploy), agent.WithHook(hook.SessionStart, "", log.hook("context")))
	}
	first := agent.NewRunner(newAgent(), store)
	if res, err := first.RunText(t.Context(), "s", "deploy"); err != nil || res.Status != loop.StatusInterrupted {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	// The interrupted state lives in the first Runner; resume there after
	// it ended the session, as a new process would after a restart.
	if err := first.EndSession(t.Context(), "s", ""); err != nil {
		t.Fatal(err)
	}
	res, err := loop.Collect(first.Resume(t.Context(), "s", map[string]loop.Approval{"c": loop.Approve}), nil)
	if err != nil || res.Status != loop.StatusCompleted {
		t.Fatalf("Resume = %+v, %v", res, err)
	}
	if got := log.got(); strings.Join(got, ",") != "SessionStart:startup:s,SessionStart:resume:s" {
		t.Fatalf("events = %v", got)
	}
	// The resume context reaches the resumed run with the tool result.
	if out := m.Requests()[1].Messages[2].ToolResults()[0].Content; !strings.Contains(out, "deployed") || !strings.Contains(out, "context resume") {
		t.Fatalf("tool result = %q", out)
	}
}

func TestRunnerEndSessionBudget(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		slow := hook.Func(func(ctx context.Context, _ *hook.Event) (*hook.Result, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
		a := agent.MustNew("assistant", agent.WithModel(modeltest.New()),
			agent.WithHooks(hook.NewManager(hook.SessionEndTimeout(50*time.Millisecond))),
			agent.WithHook(hook.SessionEnd, "other", slow))
		start := time.Now()
		err := agent.NewRunner(a, nil).EndSession(t.Context(), "missing", "")
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 50*time.Millisecond {
			t.Fatalf("EndSession = %v after %v", err, time.Since(start))
		}
		// Without SessionEnd hooks there is nothing to do.
		if err := agent.NewRunner(agent.MustNew("plain", agent.WithModel(modeltest.New())), nil).EndSession(t.Context(), "x", "other"); err != nil {
			t.Fatal(err)
		}
	})
}

func TestHookVerifier(t *testing.T) {
	t.Parallel()
	m := modeltest.New(modeltest.Reply(`{"ok": false, "reason": "tests fail"}`))
	h := &hook.Agent{Verifier: agent.HookVerifier(m), Prompt: "Check $ARGUMENTS", Model: "small-model"}
	res, err := h.Handle(t.Context(), &hook.Event{Type: hook.Stop, Message: "done"})
	if err != nil || res.Decision != hook.Deny || res.Reason != "tests fail" {
		t.Fatalf("Handle = %+v, %v", res, err)
	}
	req := m.Requests()[0]
	if req.Model != "small-model" || !strings.Contains(req.Messages[0].Text(), `"last_assistant_message":"done"`) || !strings.Contains(req.Messages[0].Text(), `{"ok": true}`) {
		t.Fatalf("verifier request = %+v", req)
	}

	sub := agent.MustNew("checker", agent.WithModel(modeltest.New(modeltest.Reply(`{"ok": true}`))))
	if res, err := (&hook.Agent{Verifier: agent.AgentVerifier(sub), Prompt: "x"}).Handle(t.Context(), &hook.Event{Type: hook.PreToolUse, ToolCall: &model.ToolCall{Name: "t", Arguments: json.RawMessage(`{}`)}}); err != nil || res != nil {
		t.Fatalf("AgentVerifier = %+v, %v", res, err)
	}
}
