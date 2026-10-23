package agent_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/guard"
	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/tool"
)

// inputGuard trips on inputs containing word, after waiting for release
// if it is not nil.
func inputGuard(name, word string, release <-chan struct{}, calls *atomic.Int32) guard.InputGuardrail {
	return guard.InputGuardrail{Name: name, Check: func(ctx context.Context, _ *loop.Run, in *model.Message) (guard.Result, error) {
		calls.Add(1)
		if release != nil {
			select {
			case <-release:
			case <-ctx.Done():
				return guard.Result{}, ctx.Err()
			}
		}
		if strings.Contains(in.Text(), word) {
			return guard.Result{Tripwire: true, Message: "off topic", Info: map[string]string{"word": word}}, nil
		}
		return guard.Result{}, nil
	}}
}

func TestSequentialInputGuardrail(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	m := modeltest.New(modeltest.Reply("fine"))
	prompts := 0
	a := agent.MustNew("assistant", agent.WithModel(m),
		agent.WithInputGuardrail(inputGuard("topic", "homework", nil, &calls)),
		agent.WithHook(hook.UserPromptSubmit, "", hook.Func(func(context.Context, *hook.Event) (*hook.Result, error) { prompts++; return nil, nil })))
	res, err := loop.Collect(a.Run(t.Context(), agent.Text("do my homework")), nil)
	if err != nil || res.Status != loop.StatusBlocked || res.Guardrail == nil || res.Guardrail.Kind != guard.KindInput ||
		res.Guardrail.Message != "off topic" || res.Guardrail.Info.(map[string]string)["word"] != "homework" || len(m.Requests()) != 0 || prompts != 0 || len(res.NewMessages) != 0 {
		t.Fatalf("tripped run = %+v, %v; model calls %d, prompt hooks %d", res, err, len(m.Requests()), prompts)
	}
	res, err = loop.Collect(a.Run(t.Context(), agent.Text("hello")), nil)
	if err != nil || res.Output != "fine" || res.Guardrail != nil || prompts != 1 {
		t.Fatalf("passing run = %+v, %v", res, err)
	}

	failing := agent.MustNew("assistant", agent.WithModel(modeltest.New()), agent.WithInputGuardrail(guard.InputGuardrail{
		Name: "broken", Check: func(context.Context, *loop.Run, *model.Message) (guard.Result, error) {
			return guard.Result{}, errors.New("classifier down")
		},
	}))
	res, err = loop.Collect(failing.Run(t.Context(), agent.Text("hi")), nil)
	if err == nil || res.Status != loop.StatusFailed || !strings.Contains(err.Error(), "classifier down") {
		t.Fatalf("guardrail error = %+v, %v", res, err)
	}
}

// blockingModel answers after release is closed, or fails when its
// context is done.
func blockingModel(release <-chan struct{}, started chan<- struct{}, resp *model.Response) model.Model {
	return model.Func(func(ctx context.Context, _ *model.Request) (*model.Response, error) {
		if started != nil {
			close(started)
		}
		select {
		case <-release:
			return resp, nil
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	})
}

func TestParallelInputGuardrailCancelsModelCall(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{})
	never := make(chan struct{})
	m := blockingModel(never, started, &model.Response{Message: model.AssistantText("late")})
	a := agent.MustNew("assistant", agent.WithModel(m), agent.WithInputGuardrail(inputGuard("topic", "homework", release, &calls), guard.Parallel()))
	go func() {
		<-started
		close(release)
	}()
	res, err := loop.Collect(a.Run(t.Context(), agent.Text("homework")), nil)
	if err != nil || res.Status != loop.StatusBlocked || res.Guardrail.Name != "topic" || calls.Load() != 1 || res.Output != "" {
		t.Fatalf("run = %+v, %v", res, err)
	}
	// The input entered the conversation, as the run had started.
	if len(res.NewMessages) != 1 || res.NewMessages[0].Text() != "homework" {
		t.Fatalf("new messages = %+v", res.NewMessages)
	}
}

func TestParallelInputGuardrailHoldsFirstResponse(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		for _, stream := range []bool{false, true} {
			var calls atomic.Int32
			release := make(chan struct{})
			ran := 0
			write := tool.Func("write", "", func(context.Context, struct{}) (string, error) { ran++; return "w", nil })
			m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c", Name: "write"}))
			opts := []agent.Option{agent.WithModel(m), agent.WithTools(write), agent.WithInputGuardrail(inputGuard("topic", "homework", release, &calls), guard.Parallel())}
			if stream {
				opts = append(opts, agent.WithStreaming())
			}
			a := agent.MustNew("assistant", opts...)
			go func() {
				// The model answers at once; the guardrail decides later.
				for len(m.Requests()) == 0 {
					time.Sleep(time.Millisecond)
				}
				close(release)
			}()
			res, err := loop.Collect(a.Run(t.Context(), agent.Text("homework")), nil)
			if err != nil || res.Status != loop.StatusBlocked || ran != 0 || res.Usage.Requests != 1 || res.Usage.OutputTokens != 5 {
				t.Fatalf("stream=%v: run = %+v, %v; tool ran %d times", stream, res, err, ran)
			}

			// A passing guardrail lets the run continue.
			calls.Store(0)
			m.Append(modeltest.CallTools(modeltest.Call{ID: "c", Name: "write"}), modeltest.Reply("done"))
			pass := make(chan struct{})
			close(pass)
			a = agent.MustNew("assistant", append(slices.Clip(opts), agent.WithInputGuardrail(inputGuard("other", "never", pass, &calls), guard.Parallel()))...)
			res, err = loop.Collect(a.Run(t.Context(), agent.Text("hello")), nil)
			if err != nil || res.Output != "done" || ran != 1 || calls.Load() != 2 {
				t.Fatalf("stream=%v: passing run = %+v, %v; tool ran %d, guardrails %d", stream, res, err, ran, calls.Load())
			}
			if _, ok := res.Values["guard.input_pending"]; ok {
				t.Fatalf("pending mark kept: %v", res.Values)
			}
		}
	})
}

func TestParallelInputGuardrailRerunsOnResume(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	cp := loop.NewMemoryCheckpointer()
	m := modeltest.New(modeltest.Reply("answer"))
	never := make(chan struct{})
	a := agent.MustNew("assistant", agent.WithModel(m), agent.WithCheckpointer(cp),
		agent.WithInputGuardrail(inputGuard("topic", "homework", never, &calls), guard.Parallel()))
	var runID string
	// Stop at the first turn, as if the process had stopped while the
	// guardrail was still running.
	for ev, err := range a.Run(t.Context(), agent.Text("homework")) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == loop.EventTurnStart {
			runID = ev.RunID
			break
		}
	}
	if len(m.Requests()) != 0 || calls.Load() != 1 {
		t.Fatalf("model calls %d, guardrail calls %d", len(m.Requests()), calls.Load())
	}
	b := agent.MustNew("assistant", agent.WithModel(m), agent.WithCheckpointer(cp),
		agent.WithInputGuardrail(inputGuard("topic", "homework", nil, &calls), guard.Parallel()))
	res, err := loop.Collect(b.Resume(t.Context(), runID, nil), nil)
	if err != nil || res.Status != loop.StatusBlocked || calls.Load() != 2 || res.Guardrail.Name != "topic" {
		t.Fatalf("resumed run = %+v, %v; guardrail calls %d", res, err, calls.Load())
	}
}

func TestOutputGuardrail(t *testing.T) {
	t.Parallel()
	var seen []string
	g := guard.OutputGuardrail{Name: "pii", Check: func(_ context.Context, _ *loop.Run, out *model.Message) (guard.Result, error) {
		seen = append(seen, out.Text())
		return guard.Result{Tripwire: strings.Contains(out.Text(), "ssn"), Message: "contains PII"}, nil
	}}
	stops := 0
	m := modeltest.New(modeltest.Reply("draft"), modeltest.Reply("the ssn is 1"))
	a := agent.MustNew("assistant", agent.WithModel(m), agent.WithOutputGuardrail(g),
		// The Stop hook continues the run once; the draft is not final.
		agent.WithHook(hook.Stop, "", hook.Func(func(context.Context, *hook.Event) (*hook.Result, error) {
			stops++
			if stops == 1 {
				return &hook.Result{Decision: hook.Deny, Reason: "check again"}, nil
			}
			return nil, nil
		})))
	runner := agent.NewRunner(a, nil)
	res, err := runner.RunText(t.Context(), "s", "q")
	if err != nil || res.Status != loop.StatusBlocked || res.Output != "" || res.Guardrail.Kind != guard.KindOutput || len(seen) != 1 || seen[0] != "the ssn is 1" {
		t.Fatalf("run = %+v, %v; guardrail saw %q", res, err, seen)
	}
	// A blocked run does not block the session.
	m.Append(modeltest.Reply("ok"))
	if res, err := runner.RunText(t.Context(), "s", "again"); err != nil || res.Output != "ok" {
		t.Fatalf("next run = %+v, %v", res, err)
	}
}

func TestToolGuardrails(t *testing.T) {
	t.Parallel()
	ran := map[string]int{}
	newTool := func(name, out string) tool.Tool {
		return tool.Func(name, "", func(context.Context, struct{}) (string, error) { ran[name]++; return out, nil })
	}
	g := guard.ToolGuardrail{
		Name:  "shell",
		Tools: []string{"shell", "read"},
		Before: func(_ context.Context, _ *loop.Run, call *model.ToolCall) (guard.Result, error) {
			return guard.Result{Tripwire: call.Name == "shell", Message: "no shell"}, nil
		},
		After: func(_ context.Context, _ *loop.Run, _ *model.ToolCall, res *model.ToolResult) (guard.Result, error) {
			return guard.Result{Tripwire: strings.Contains(res.Content, "secret"), Message: "leak"}, nil
		},
	}
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "a", Name: "echo"}, modeltest.Call{ID: "b", Name: "shell"}))
	a := agent.MustNew("assistant", agent.WithModel(m), agent.WithToolGuardrail(g),
		agent.WithTools(newTool("echo", "x"), newTool("shell", "y"), newTool("read", "the secret")))
	res, err := loop.Collect(a.Run(t.Context(), agent.Text("q")), nil)
	if err != nil || res.Status != loop.StatusBlocked || res.Guardrail.Kind != guard.KindToolInput || len(ran) != 0 {
		t.Fatalf("tool input trip = %+v, %v; ran %v", res, err, ran)
	}

	m.Append(modeltest.CallTools(modeltest.Call{ID: "c", Name: "read"}))
	res, err = loop.Collect(a.Run(t.Context(), agent.Text("q")), nil)
	if err != nil || res.Status != loop.StatusBlocked || res.Guardrail.Kind != guard.KindToolOutput || ran["read"] != 1 {
		t.Fatalf("tool output trip = %+v, %v", res, err)
	}
	last := res.Messages[len(res.Messages)-1].ToolResults()
	if len(last) != 1 || last[0].Content != "leak" || !last[0].IsError {
		t.Fatalf("withheld result = %+v", last)
	}
	// Tools the guardrail does not name run unchecked.
	m.Append(modeltest.CallTools(modeltest.Call{ID: "d", Name: "echo"}), modeltest.Reply("done"))
	if res, err := loop.Collect(a.Run(t.Context(), agent.Text("q")), nil); err != nil || res.Output != "done" || ran["echo"] != 1 {
		t.Fatalf("unguarded tool = %+v, %v", res, err)
	}
}
