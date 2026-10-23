package agent_test

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/session"
	"github.com/nostosx/agentx/tool"
)

func TestRunnerPersistsConversation(t *testing.T) {
	t.Parallel()
	m := modeltest.New(modeltest.Reply("first answer"), modeltest.Reply("second answer"))
	starts := 0
	a := agent.MustNew("assistant", agent.WithModel(m), agent.WithHook(hook.SessionStart, "", hook.Func(func(context.Context, *hook.Event) (*hook.Result, error) { starts++; return nil, nil })))
	runner := agent.NewRunner(a, nil)
	var id string
	res, err := loop.Collect(runner.Run(t.Context(), "", model.UserText("first")), func(ev *loop.Event) { id = ev.SessionID })
	if err != nil || res.Output != "first answer" || id == "" || starts != 1 {
		t.Fatalf("first Run = %+v, %v; session = %q; starts = %d", res, err, id, starts)
	}
	res, err = runner.RunText(t.Context(), id, "second")
	if err != nil || res.Output != "second answer" || starts != 1 {
		t.Fatalf("second Run = %+v, %v; starts = %d", res, err, starts)
	}
	requests := m.Requests()
	if len(requests) != 2 || len(requests[1].Messages) != 3 || requests[1].Messages[0].Text() != "first" || requests[1].Messages[1].Text() != "first answer" {
		t.Fatalf("second model request = %+v", requests)
	}
	saved, err := runner.Store().Get(t.Context(), id)
	if err != nil || len(saved.Messages()) != 4 || saved.Messages()[3].Text() != "second answer" {
		t.Fatalf("saved session = %+v, %v", saved, err)
	}
}

func TestRunnerInterruptedRunMustResume(t *testing.T) {
	t.Parallel()
	calls := 0
	write := tool.Func("write", "write", func(context.Context, struct{}) (string, error) { calls++; return "written", nil }, tool.RequireApproval())
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "call", Name: "write"}), modeltest.Reply("done"), modeltest.Reply("follow-up"))
	a := agent.MustNew("assistant", agent.WithModel(m), agent.WithTools(write), agent.WithCheckpointer(loop.NewMemoryCheckpointer()))
	runner := agent.NewRunner(a, nil)
	res, err := runner.RunText(t.Context(), "session", "write")
	if err != nil || res.Status != loop.StatusInterrupted || calls != 0 {
		t.Fatalf("Run = %+v, %v; calls = %d", res, err, calls)
	}
	saved, err := runner.Store().Get(t.Context(), "session")
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := saved.State().Get(agent.PendingRunKey); id != res.RunID || len(saved.Messages()) != 2 {
		t.Fatalf("pending state = %+v", session.TakeSnapshot(saved))
	}
	if _, err := runner.RunText(t.Context(), "session", "new input"); err == nil || !strings.Contains(err.Error(), "resume it with Runner.Resume") || len(m.Requests()) != 1 {
		t.Fatalf("Run while interrupted = %v; model calls = %d", err, len(m.Requests()))
	}
	res, err = loop.Collect(runner.Resume(t.Context(), "session", map[string]loop.Approval{"call": loop.Approve}), nil)
	if err != nil || res.Output != "done" || calls != 1 {
		t.Fatalf("Resume = %+v, %v; calls = %d", res, err, calls)
	}
	saved, err = runner.Store().Get(t.Context(), "session")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := saved.State().Get(agent.PendingRunKey); ok || len(saved.Messages()) != 4 {
		t.Fatalf("resumed session = %+v", session.TakeSnapshot(saved))
	}
	if _, err := runner.RunText(t.Context(), "session", "next"); err != nil {
		t.Fatalf("Run after resume = %v", err)
	}
}

func TestRunnerPersistsCancelledRun(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m := model.Func(func(context.Context, *model.Request) (*model.Response, error) {
		cancel()
		return nil, context.Canceled
	})
	store := &observingStore{Store: session.NewMemoryStore()}
	runner := agent.NewRunner(agent.MustNew("assistant", agent.WithModel(m)), store)
	res, err := runner.RunText(ctx, "session", "keep this message")
	if !errors.Is(err, context.Canceled) || res.Status != loop.StatusFailed || store.saveContextErr != nil {
		t.Fatalf("cancelled Run = %+v, %v; save context error = %v", res, err, store.saveContextErr)
	}
	saved, err := store.Get(t.Context(), "session")
	if err != nil || len(saved.Messages()) != 1 || saved.Messages()[0].Text() != "keep this message" {
		t.Fatalf("cancelled saved session = %+v, %v", saved, err)
	}
}

type observingStore struct {
	session.Store
	getErr         error
	saveErr        error
	saveContextErr error
}

func (s *observingStore) Get(ctx context.Context, id string) (session.Session, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.Store.Get(ctx, id)
}

func (s *observingStore) Save(ctx context.Context, sess session.Session) error {
	s.saveContextErr = ctx.Err()
	if s.saveErr != nil {
		return s.saveErr
	}
	return s.Store.Save(ctx, sess)
}

func TestRunnerStoreErrors(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("store unavailable")
	for _, tc := range []struct {
		name string
		get  bool
	}{
		{name: "load", get: true},
		{name: "save"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &observingStore{Store: session.NewMemoryStore()}
			if tc.get {
				store.getErr = wantErr
			} else {
				store.saveErr = wantErr
			}
			m := modeltest.New(modeltest.Reply("answer"))
			runner := agent.NewRunner(agent.MustNew("assistant", agent.WithModel(m)), store)
			if _, err := runner.RunText(t.Context(), "session", "question"); !errors.Is(err, wantErr) {
				t.Fatalf("Run = %v, want storage error", err)
			}
			if tc.get && len(m.Requests()) != 0 {
				t.Fatal("model ran despite load failure")
			}
		})
	}
}

type emptyAgent struct{}

func (emptyAgent) Name() string        { return "empty" }
func (emptyAgent) Description() string { return "" }
func (emptyAgent) Run(context.Context, *agent.Input) iter.Seq2[*loop.Event, error] {
	return func(func(*loop.Event, error) bool) {}
}

func TestRunnerMissingResultAndResumeErrors(t *testing.T) {
	t.Parallel()
	runner := agent.NewRunner(emptyAgent{}, nil)
	if _, err := runner.RunText(t.Context(), "session", "question"); err == nil || !strings.Contains(err.Error(), "without a result") {
		t.Fatalf("Run without result = %v", err)
	}
	if _, err := loop.Collect(runner.Resume(t.Context(), "session", nil), nil); err == nil || !strings.Contains(err.Error(), "cannot resume") {
		t.Fatalf("Resume unsupported agent = %v", err)
	}
	runner = agent.NewRunner(agent.MustNew("assistant", agent.WithModel(modeltest.New())), nil)
	if _, err := loop.Collect(runner.Resume(t.Context(), "missing", nil), nil); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("Resume missing session = %v", err)
	}
	if err := runner.Store().Save(t.Context(), session.New("session")); err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Collect(runner.Resume(t.Context(), "session", nil), nil); err == nil || !strings.Contains(err.Error(), "no interrupted run") {
		t.Fatalf("Resume session without pending run = %v", err)
	}
}

func TestRunnerSerializesSameSessionAndReloadsHistory(t *testing.T) {
	t.Parallel()
	for _, exists := range []bool{false, true} {
		t.Run(map[bool]string{false: "new session", true: "existing session"}[exists], func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			store := session.NewMemoryStore()
			if exists {
				if err := store.Save(ctx, session.New("session")); err != nil {
					t.Fatal(err)
				}
			}
			firstStarted := make(chan struct{})
			releaseFirst := make(chan struct{})
			m := model.Func(func(ctx context.Context, req *model.Request) (*model.Response, error) {
				last := req.Messages[len(req.Messages)-1].Text()
				if last == "first" {
					close(firstStarted)
					select {
					case <-releaseFirst:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				} else if len(req.Messages) != 3 || req.Messages[0].Text() != "first" || req.Messages[1].Text() != "first answer" {
					t.Errorf("second run used stale history: %+v", req.Messages)
				}
				return &model.Response{Message: model.AssistantText(last + " answer")}, nil
			})
			runner := agent.NewRunner(agent.MustNew("assistant", agent.WithModel(m)), store)
			done := make(chan error, 2)
			go func() { _, err := runner.RunText(ctx, "session", "first"); done <- err }()
			select {
			case <-firstStarted:
			case <-ctx.Done():
				t.Fatal("first run did not start")
			}
			secondStarted := make(chan struct{})
			go func() {
				close(secondStarted)
				_, err := runner.RunText(ctx, "session", "second")
				done <- err
			}()
			<-secondStarted
			// Allow the competing call to contend for the session lock before
			// releasing the first run. Mutex waits are not durable in synctest.
			time.Sleep(20 * time.Millisecond)
			close(releaseFirst)
			for range 2 {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			saved, err := store.Get(ctx, "session")
			if err != nil || len(saved.Messages()) != 4 || saved.Messages()[3].Text() != "second answer" {
				t.Fatalf("serialized conversation = %+v, %v", saved, err)
			}
		})
	}
}

func TestRunnerRunOptions(t *testing.T) {
	t.Parallel()
	echo := tool.Func("echo", "echo", func(context.Context, struct{}) (string, error) { return "ok", nil })
	calls := modeltest.CallTools(modeltest.Call{ID: "c", Name: "echo"})
	m := modeltest.New(calls, calls, calls, calls, modeltest.Reply("done"))
	a := agent.MustNew("assistant", agent.WithModel(m), agent.WithTools(echo), agent.WithMaxTurns(10))
	runner := agent.NewRunner(a, nil)
	res, err := runner.RunText(t.Context(), "s", "go", agent.RunMaxTurns(2))
	if err != nil || res.Status != loop.StatusMaxTurns || res.Turns != 2 {
		t.Fatalf("RunMaxTurns: %+v, %v", res, err)
	}
	res, err = loop.Collect(runner.Run(t.Context(), "s", model.UserText("again"), agent.RunLimits(loop.Limits{ToolCalls: 1})), nil)
	if err != nil || res.Status != loop.StatusLimitExceeded || !strings.Contains(res.Reason, "limit of 1 tool calls") {
		t.Fatalf("RunLimits: %+v, %v", res, err)
	}
	// Without options the agent's own limits apply again.
	res, err = runner.RunText(t.Context(), "s", "finish")
	if err != nil || res.Status != loop.StatusCompleted || res.Output != "done" {
		t.Fatalf("Run without options: %+v, %v", res, err)
	}
}
