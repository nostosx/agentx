package agent_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/guard"
	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/tool"
)

// cardGuard trips on answers that contain a card number.
var cardGuard = guard.OutputGuardrail{Name: "pii", Check: func(_ context.Context, _ *loop.Run, out *model.Message) (guard.Result, error) {
	return guard.Result{Tripwire: strings.Contains(out.Text(), "4242"), Message: "card number"}, nil
}}

// contains reports whether any message holds text.
func contains(msgs []*model.Message, text string) bool {
	for _, m := range msgs {
		for _, p := range m.Parts {
			if strings.Contains(p.Text, text) {
				return true
			}
			if r := p.ToolResult; r != nil && strings.Contains(r.Content, text) {
				return true
			}
		}
	}
	return false
}

// A blocked answer is withheld from the result, the checkpoint and the
// session: a placeholder takes its place.
func TestOutputGuardrailWithholdsAnswer(t *testing.T) {
	t.Parallel()
	cp := loop.NewMemoryCheckpointer()
	m := modeltest.New(modeltest.Reply("card 4242"), modeltest.Reply("fine"))
	a := agent.MustNew("assistant", agent.WithModel(m), agent.WithOutputGuardrail(cardGuard), agent.WithCheckpointer(cp))
	r := agent.NewRunner(a, nil)
	res, err := r.RunText(t.Context(), "s", "card?")
	if err != nil || res.Status != loop.StatusBlocked || res.Output != "" {
		t.Fatalf("run = %+v, %v", res, err)
	}
	last := res.Messages[len(res.Messages)-1]
	if contains(res.Messages, "4242") || contains(res.NewMessages, "4242") || last.Role != model.RoleAssistant ||
		last.Text() != "[response withheld by guardrail pii]" || len(res.NewMessages) != 2 {
		t.Fatalf("messages = %+v", res.Messages)
	}
	if _, err := cp.Load(t.Context(), res.RunID); !errors.Is(err, loop.ErrCheckpointNotFound) {
		t.Fatalf("checkpoint = %v", err)
	}
	sess, err := r.Store().Get(t.Context(), "s")
	if err != nil || contains(sess.Messages(), "4242") || len(sess.Messages()) != 2 {
		t.Fatalf("session = %+v, %v", sess.Messages(), err)
	}
	// The conversation stays valid: the next run answers after it.
	if res, err := r.RunText(t.Context(), "s", "again"); err != nil || res.Output != "fine" {
		t.Fatalf("next run = %+v, %v", res, err)
	}
	if req := m.Requests()[1]; contains(req.Messages, "4242") || len(req.Messages) != 3 {
		t.Fatalf("next request = %+v", req.Messages)
	}
}

// A tool output trip withholds the result's text, media and metadata
// from the conversation, and PostToolUse hooks never see it.
func TestToolOutputGuardrailWithholdsResult(t *testing.T) {
	t.Parallel()
	read := tool.Func("read", "", func(context.Context, struct{}) (*tool.Result, error) {
		res := tool.Media("the secret", model.ImagePart("image/png", []byte("secret png")))
		res.Metadata = map[string]any{"secret": true}
		return res, nil
	})
	var hooked []string
	g := guard.ToolGuardrail{Name: "dlp", After: func(_ context.Context, _ *loop.Run, _ *model.ToolCall, res *model.ToolResult) (guard.Result, error) {
		return guard.Result{Tripwire: strings.Contains(res.Content, "secret") && len(res.Parts) == 1, Message: "withheld"}, nil
	}}
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c", Name: "read"}))
	a := agent.MustNew("assistant", agent.WithModel(m), agent.WithTools(read), agent.WithToolGuardrail(g),
		agent.WithHook(hook.PostToolUse, "", hook.Func(func(_ context.Context, ev *hook.Event) (*hook.Result, error) {
			hooked = append(hooked, ev.ToolResult.Content)
			return nil, nil
		})))
	r := agent.NewRunner(a, nil)
	var events []*loop.Event
	res, err := loop.Collect(r.Run(t.Context(), "s", model.UserText("read")), func(ev *loop.Event) { events = append(events, ev) })
	if err != nil || res.Status != loop.StatusBlocked || res.Guardrail.Kind != guard.KindToolOutput {
		t.Fatalf("run = %+v, %v", res, err)
	}
	results := res.Messages[len(res.Messages)-1].ToolResults()
	if len(results) != 1 || results[0].Content != "withheld" || results[0].Parts != nil || results[0].Metadata != nil || len(hooked) != 0 {
		t.Fatalf("results = %+v; hooks saw %v", results, hooked)
	}
	for _, ev := range events {
		if ev.ToolResult != nil && strings.Contains(ev.ToolResult.Content, "secret") {
			t.Fatalf("event %s leaks the result", ev.Type)
		}
	}
	sess, _ := r.Store().Get(t.Context(), "s")
	if contains(sess.Messages(), "secret") {
		t.Fatalf("session = %+v", sess.Messages())
	}
}

// With HoldOutput a streamed answer is sent only once the guardrails
// passed: a blocked answer never reaches the consumer, while responses
// that call tools stream as usual.
func TestHoldOutputStreaming(t *testing.T) {
	t.Parallel()
	echo := tool.Func("echo", "", func(context.Context, struct{}) (string, error) { return "echoed", nil })
	run := func(hold bool, steps ...modeltest.Step) (*loop.Result, []*loop.Event) {
		var opts []guard.OutputOption
		if hold {
			opts = append(opts, guard.HoldOutput())
		}
		a := agent.MustNew("assistant", agent.WithModel(modeltest.New(steps...)), agent.WithStreaming(), agent.WithTools(echo),
			agent.WithOutputGuardrail(cardGuard, opts...))
		var events []*loop.Event
		res, err := loop.Collect(a.Run(t.Context(), agent.Text("go")), func(ev *loop.Event) { events = append(events, ev) })
		if err != nil {
			t.Fatal(err)
		}
		return res, events
	}
	sent := func(events []*loop.Event, text string) bool {
		for _, ev := range events {
			if strings.Contains(ev.Text(), text) {
				return true
			}
		}
		return false
	}
	types := func(events []*loop.Event) string {
		var b strings.Builder
		for _, ev := range events {
			b.WriteString(string(ev.Type) + " ")
		}
		return b.String()
	}

	// Without HoldOutput the blocked answer streams, but is not kept.
	res, events := run(false, modeltest.Reply("card 4242"))
	if res.Status != loop.StatusBlocked || !sent(events, "42") || contains(res.Messages, "4242") {
		t.Fatalf("unheld run = %+v; events %s", res, types(events))
	}
	// With it, nothing of the blocked answer is sent.
	res, events = run(true, modeltest.Reply("card 4242"))
	if res.Status != loop.StatusBlocked || sent(events, "card") || sent(events, "42") {
		t.Fatalf("held blocked run = %+v; events %s", res, types(events))
	}
	if got := types(events); got != "run_start turn_start run_end " {
		t.Fatalf("held blocked events = %s", got)
	}
	// An answer that passes arrives, deltas then message, before the end;
	// a response with tool calls streams before its calls run.
	res, events = run(true, modeltest.CallTools(modeltest.Call{ID: "e", Name: "echo"}), modeltest.Reply("all fine"))
	if res.Status != loop.StatusCompleted || res.Output != "all fine" {
		t.Fatalf("held run = %+v", res)
	}
	if got := types(events); got != "run_start turn_start delta message tool_call tool_result turn_start delta delta message run_end " {
		t.Fatalf("held events = %s", got)
	}
}

// A held invalid answer stays checkpointed for recovery, but only an
// accepted answer becomes output when the failed run resumes.
func TestHoldOutputResumeUncheckedAnswer(t *testing.T) {
	t.Parallel()
	type verdict struct {
		OK bool `json:"ok"`
	}
	cp := loop.NewMemoryCheckpointer()
	m := modeltest.New(modeltest.Reply("card 4242"), modeltest.Reply("still card 4242"))
	a := agent.MustNew("assistant", agent.WithModel(m), agent.WithStreaming(), agent.WithCheckpointer(cp),
		agent.WithOutput[verdict](agent.OutputRetries(1)), agent.WithOutputGuardrail(cardGuard, guard.HoldOutput()))
	runner := agent.NewRunner(a, nil)
	res, err := runner.RunText(t.Context(), "s", "go")
	if oe, ok := errors.AsType[*loop.OutputError](err); !ok || oe.Output != "" || res.Output != "" {
		t.Fatalf("failed run = %+v, %v", res, err)
	}
	st, err := cp.Load(t.Context(), res.RunID)
	if err != nil || !contains(st.Messages, "4242") {
		t.Fatalf("checkpoint = %+v, %v", st, err)
	}
	m.Append(modeltest.Reply(`{"ok":true}`))
	res, err = loop.Collect(runner.Resume(t.Context(), "s", nil), nil)
	if err != nil || res.Status != loop.StatusCompleted || res.Output != `{"ok":true}` || len(m.Requests()) != 3 {
		t.Fatalf("resumed run = %+v, %v", res, err)
	}
	msgs := m.Requests()[2].Messages
	if !contains(msgs, "still card 4242") || !strings.Contains(msgs[len(msgs)-1].Text(), "not valid output") {
		t.Fatal("resume did not send the draft and its validation feedback back to the model")
	}
}

// With HoldOutput an answer that does not end the run is never sent: a
// Stop hook that continues the run, a provider pause and an answer that
// fails the output schema all end before the output guardrails see it.
func TestHoldOutputUncheckedAnswers(t *testing.T) {
	t.Parallel()
	type verdict struct {
		OK bool `json:"ok"`
	}
	noExtraProperties := func(o *loop.Output) {
		allow := false
		o.Schema.AdditionalProperties = &allow
	}
	run := func(t *testing.T, steps []modeltest.Step, opts ...agent.Option) (*loop.Result, []*loop.Event) {
		t.Helper()
		opts = append(opts, agent.WithModel(modeltest.New(steps...)), agent.WithStreaming(),
			agent.WithOutputGuardrail(cardGuard, guard.HoldOutput()))
		a := agent.MustNew("assistant", opts...)
		var events []*loop.Event
		res, _ := loop.Collect(a.Run(t.Context(), agent.Text("go")), func(ev *loop.Event) { events = append(events, ev) })
		for _, ev := range events {
			if strings.Contains(ev.Text(), "4242") || ev.OutputRetry != nil &&
				strings.Contains(ev.OutputRetry.Output+strings.Join(ev.OutputRetry.Problems, " "), "4242") {
				t.Errorf("event %s sent the unchecked answer: %+v", ev.Type, ev)
			}
		}
		if strings.Contains(res.Output+res.Reason, "4242") || res.Status != loop.StatusCompleted && res.Output != "" {
			t.Errorf("result sent the unchecked answer: output %q, reason %q", res.Output, res.Reason)
		}
		if res.Err != nil && strings.Contains(res.Err.Error(), "4242") {
			t.Errorf("error sent the unchecked answer: %v", res.Err)
		}
		if oe, ok := errors.AsType[*loop.OutputError](res.Err); ok &&
			(oe.Output != "" || strings.Contains(strings.Join(oe.Problems, " "), "4242")) {
			t.Errorf("output error sent the unchecked answer: %+v", oe)
		}
		// Holding events does not remove drafts from the conversation used
		// to continue or resume the run.
		if !contains(res.Messages, "4242") || !contains(res.NewMessages, "4242") {
			t.Error("the unchecked draft was removed from the conversation")
		}
		return res, events
	}

	t.Run("stop hook continues", func(t *testing.T) {
		t.Parallel()
		stops := 0
		res, _ := run(t, []modeltest.Step{modeltest.Reply("draft 4242"), modeltest.Reply("all fine")},
			agent.WithHook(hook.Stop, "", hook.Func(func(context.Context, *hook.Event) (*hook.Result, error) {
				if stops++; stops == 1 {
					return &hook.Result{Decision: hook.Deny, Reason: "check again"}, nil
				}
				return nil, nil
			})))
		if res.Status != loop.StatusCompleted || res.Output != "all fine" {
			t.Fatalf("run = %+v", res)
		}
	})
	t.Run("pause", func(t *testing.T) {
		t.Parallel()
		paused := &model.Response{Message: model.AssistantText("part 4242"), FinishReason: model.FinishPause}
		res, _ := run(t, []modeltest.Step{modeltest.Respond(paused), modeltest.Reply("all fine")})
		if res.Status != loop.StatusCompleted || res.Output != "all fine" {
			t.Fatalf("run = %+v", res)
		}
	})
	t.Run("output retry", func(t *testing.T) {
		t.Parallel()
		res, events := run(t, []modeltest.Step{modeltest.Reply("card 4242"), modeltest.Reply(`{"ok":true}`)},
			agent.WithOutput[verdict](agent.OutputRetries(1)))
		if res.Status != loop.StatusCompleted {
			t.Fatalf("run = %+v", res)
		}
		retried := false
		for _, ev := range events {
			retried = retried || ev.Type == loop.EventOutputRetry && len(ev.OutputRetry.Problems) > 0
		}
		if !retried {
			t.Fatal("no output_retry event")
		}
	})
	t.Run("output retries exhausted", func(t *testing.T) {
		t.Parallel()
		res, _ := run(t, []modeltest.Step{modeltest.Reply("card 4242")}, agent.WithOutput[verdict](agent.OutputRetries(-1)))
		if oe, ok := errors.AsType[*loop.OutputError](res.Err); !ok || oe.Attempts != 1 || res.Status != loop.StatusFailed {
			t.Fatalf("run = %+v", res)
		}
	})
	t.Run("output diagnostics", func(t *testing.T) {
		t.Parallel()
		res, _ := run(t, []modeltest.Step{modeltest.Reply(`{"ok":true,"card 4242":true}`)},
			agent.WithOutput[verdict](agent.OutputRetries(-1), noExtraProperties))
		if res.Status != loop.StatusFailed {
			t.Fatalf("run = %+v", res)
		}
	})
	t.Run("output retry diagnostics", func(t *testing.T) {
		t.Parallel()
		res, _ := run(t, []modeltest.Step{modeltest.Reply(`{"ok":true,"card 4242":true}`), modeltest.Reply(`{"ok":true}`)},
			agent.WithOutput[verdict](agent.OutputRetries(1), noExtraProperties))
		if res.Status != loop.StatusCompleted || res.Output != `{"ok":true}` {
			t.Fatalf("run = %+v", res)
		}
	})
	t.Run("pause at turn limit", func(t *testing.T) {
		t.Parallel()
		res, _ := run(t, []modeltest.Step{modeltest.Respond(&model.Response{
			Message: model.AssistantText("part 4242"), FinishReason: model.FinishPause,
		})}, agent.WithMaxTurns(1))
		if res.Status != loop.StatusMaxTurns {
			t.Fatalf("run = %+v", res)
		}
	})
	t.Run("model failure after pause", func(t *testing.T) {
		t.Parallel()
		res, _ := run(t, []modeltest.Step{modeltest.Respond(&model.Response{
			Message: model.AssistantText("part 4242"), FinishReason: model.FinishPause,
		}), modeltest.Fail(errors.New("unavailable"))})
		if res.Status != loop.StatusFailed {
			t.Fatalf("run = %+v", res)
		}
	})
	t.Run("halt before guardrail", func(t *testing.T) {
		t.Parallel()
		res, _ := run(t, []modeltest.Step{modeltest.Reply("card 4242")},
			agent.WithHook(hook.Stop, "", hook.Func(func(context.Context, *hook.Event) (*hook.Result, error) {
				return &hook.Result{Halt: true, HaltReason: "stop"}, nil
			})))
		if res.Status != loop.StatusStopped {
			t.Fatalf("run = %+v", res)
		}
	})
}
