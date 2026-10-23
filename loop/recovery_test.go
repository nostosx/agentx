package loop_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/tool"
)

// requireAnswered fails unless every tool call in msgs has a result.
func requireAnswered(t *testing.T, msgs []*model.Message) {
	t.Helper()
	answered := map[string]bool{}
	for _, m := range msgs {
		for _, r := range m.ToolResults() {
			answered[r.CallID] = true
		}
	}
	for _, m := range msgs {
		for _, c := range m.ToolCalls() {
			if !answered[c.ID] {
				t.Fatalf("tool call %s has no result in %d messages", c.ID, len(msgs))
			}
		}
	}
}

func TestRunEndAnswersUnrunToolCalls(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mw     func(cancel context.CancelFunc) loop.Middleware
		status loop.Status
	}{
		{name: "cancelled before tools", status: loop.StatusFailed, mw: func(cancel context.CancelFunc) loop.Middleware {
			return &loop.Funcs{MiddlewareName: "cancel", BeforeToolFunc: func(context.Context, *loop.Run, *loop.ToolPlan) error { cancel(); return nil }}
		}},
		{name: "after model error", status: loop.StatusFailed, mw: func(context.CancelFunc) loop.Middleware {
			return &loop.Funcs{MiddlewareName: "fail", AfterModelFunc: func(context.Context, *loop.Run, *model.Request, *model.Response) error {
				return errors.New("broken middleware")
			}}
		}},
		{name: "stopped before tools", status: loop.StatusStopped, mw: func(context.CancelFunc) loop.Middleware {
			return &loop.Funcs{MiddlewareName: "stop", BeforeToolFunc: func(context.Context, *loop.Run, *loop.ToolPlan) error { return loop.Stop("policy") }}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			echo := tool.Func("echo", "echo", func(_ context.Context, in echoArgs) (string, error) { calls++; return in.Text, nil })
			m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "a", Name: "echo", Args: echoArgs{Text: "x"}}, modeltest.Call{ID: "b", Name: "echo", Args: echoArgs{Text: "y"}}))
			r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, echo), Middlewares: []loop.Middleware{tc.mw(cancel)}})
			res, _ := loop.Collect(r.Run(ctx, &loop.Request{Input: model.UserText("q")}), nil)
			if res.Status != tc.status || calls != 0 {
				t.Fatalf("result = %+v; calls = %d", res, calls)
			}
			requireAnswered(t, res.Messages)
			if last := res.Messages[len(res.Messages)-1].ToolResults(); len(last) != 2 || !last[0].IsError {
				t.Fatalf("closing results = %+v", last)
			}
		})
	}
}

func TestCloseToolCalls(t *testing.T) {
	t.Parallel()
	call := func(id string) model.Part {
		return model.Part{Type: model.PartToolCall, ToolCall: &model.ToolCall{ID: id, Name: "t"}}
	}
	msgs := []*model.Message{
		model.UserText("q"),
		model.NewMessage(model.RoleAssistant, call("a"), call("b")),
		model.ToolResults(&model.ToolResult{CallID: "a", Content: "done"}),
	}
	msg := loop.CloseToolCalls(msgs, "not run")
	if res := msg.ToolResults(); len(res) != 1 || res[0].CallID != "b" || res[0].Content != "not run" || !res[0].IsError {
		t.Fatalf("closing message = %+v", msg)
	}
	if loop.CloseToolCalls(append(msgs, msg), "x") != nil || loop.CloseToolCalls([]*model.Message{model.UserText("q")}, "x") != nil {
		t.Fatal("closed conversation got another message")
	}
}

func TestResumeAfterCrashDoesNotRepeatWork(t *testing.T) {
	t.Parallel()
	cp := loop.NewMemoryCheckpointer()
	var reads, writes atomic.Int32
	// crashed is the checkpoint as it was while the write ran, as if the
	// process had died then.
	var crashed *loop.State
	read := tool.Func("read", "read", func(context.Context, struct{}) (string, error) { reads.Add(1); return "contents", nil }, tool.ReadOnly())
	write := tool.Func("write", "write", func(ctx context.Context, _ struct{}) (string, error) {
		writes.Add(1)
		st, err := cp.Load(ctx, loop.CallingRun(ctx).ID())
		crashed = st
		return "written", err
	})
	m := modeltest.New(
		modeltest.CallTools(modeltest.Call{ID: "r", Name: "read"}, modeltest.Call{ID: "w", Name: "write"}),
		modeltest.Reply("done"),
		modeltest.Reply("done"),
	)
	r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, read, write), Checkpointer: cp})
	if _, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil); err != nil {
		t.Fatal(err)
	}
	st := crashed
	if st == nil || st.Status != loop.StatusRunning || st.Version != loop.StateVersion || len(st.Pending) != 2 || st.Pending[0].Result == nil || !st.Pending[1].Started {
		t.Fatalf("checkpoint = %+v", st)
	}
	if err := cp.Save(t.Context(), st); err != nil {
		t.Fatal(err)
	}

	res, err := loop.Collect(r.Resume(t.Context(), st.RunID, nil), nil)
	if err != nil || res.Status != loop.StatusCompleted || res.Output != "done" {
		t.Fatalf("Resume = %+v, %v", res, err)
	}
	if reads.Load() != 1 || writes.Load() != 1 {
		t.Fatalf("reads = %d, writes = %d", reads.Load(), writes.Load())
	}
	results := m.Requests()[2].Messages[2].ToolResults()
	if len(results) != 2 || results[0].Content != "contents" || !results[1].IsError || !strings.Contains(results[1].Content, "may or may not have taken effect") {
		t.Fatalf("results sent to the model = %+v", results)
	}
	if _, err := cp.Load(t.Context(), st.RunID); !errors.Is(err, loop.ErrCheckpointNotFound) {
		t.Fatalf("checkpoint after completion: %v", err)
	}
}

// Stopping at a tool_call event stops the run before the call runs, so a
// resumed run runs it instead of reporting it as cut off.
func TestStopAtToolCallDoesNotStartIt(t *testing.T) {
	t.Parallel()
	var writes atomic.Int32
	write := tool.Func("write", "write", func(context.Context, struct{}) (string, error) { writes.Add(1); return "written", nil })
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "a", Name: "write"}, modeltest.Call{ID: "b", Name: "write"}), modeltest.Reply("done"))
	cp := loop.NewMemoryCheckpointer()
	var ended *loop.Result
	afterRun := &loop.Funcs{MiddlewareName: "after", AfterRunFunc: func(_ context.Context, _ *loop.Run, res *loop.Result) { ended = res }}
	r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, write), Checkpointer: cp, Middlewares: []loop.Middleware{afterRun}})
	var runID string
	for ev, err := range r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}) {
		if err != nil {
			t.Fatal(err)
		}
		runID = ev.RunID
		if ev.Type == loop.EventToolCall && ev.ToolCall.ID == "b" {
			break
		}
	}
	if writes.Load() != 1 || ended == nil || ended.Status != loop.StatusRunning {
		t.Fatalf("writes = %d; AfterRun saw %+v", writes.Load(), ended)
	}
	st, err := cp.Load(t.Context(), runID)
	if err != nil || st.Status != loop.StatusRunning || st.Pending[0].Result == nil || st.Pending[1].Started {
		t.Fatalf("checkpoint = %+v, %v", st, err)
	}
	res, err := loop.Collect(r.Resume(t.Context(), runID, nil), nil)
	if err != nil || res.Status != loop.StatusCompleted || writes.Load() != 2 {
		t.Fatalf("Resume = %+v, %v; writes = %d", res, err, writes.Load())
	}
	if results := m.Requests()[1].Messages[2].ToolResults(); len(results) != 2 || results[1].Content != "written" {
		t.Fatalf("results sent to the model = %+v", results)
	}
}

// Stopping at the message event leaves the calls unplanned; a resumed run
// plans and runs them instead of sending them to the model unanswered.
func TestStopAtMessageResumesItsCalls(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	echo := tool.Func("echo", "echo", func(_ context.Context, in echoArgs) (string, error) { calls.Add(1); return in.Text, nil })
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "a", Name: "echo", Args: echoArgs{Text: "x"}}), modeltest.Reply("done"))
	cp := loop.NewMemoryCheckpointer()
	r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, echo), Checkpointer: cp})
	var runID string
	for ev, err := range r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}) {
		if err != nil {
			t.Fatal(err)
		}
		runID = ev.RunID
		if ev.Type == loop.EventMessage {
			break
		}
	}
	res, err := loop.Collect(r.Resume(t.Context(), runID, nil), nil)
	if err != nil || res.Status != loop.StatusCompleted || calls.Load() != 1 {
		t.Fatalf("Resume = %+v, %v; calls = %d", res, err, calls.Load())
	}
	requireAnswered(t, m.Requests()[1].Messages)
	if results := m.Requests()[1].Messages[2].ToolResults(); len(results) != 1 || results[0].Content != "x" {
		t.Fatalf("results sent to the model = %+v", results)
	}
}

func TestResumeRepeatsCutOffReadOnlyCalls(t *testing.T) {
	t.Parallel()
	var reads atomic.Int32
	read := tool.Func("read", "read", func(context.Context, struct{}) (string, error) { reads.Add(1); return "contents", nil }, tool.ReadOnly())
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "r", Name: "read"}), modeltest.Reply("done"))
	cp := loop.NewMemoryCheckpointer()
	r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, read), Checkpointer: cp})
	var runID string
	for ev, err := range r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}) {
		if err != nil {
			t.Fatal(err)
		}
		runID = ev.RunID
		if ev.Type == loop.EventToolCall {
			break
		}
	}
	res, err := loop.Collect(r.Resume(t.Context(), runID, nil), nil)
	if err != nil || res.Status != loop.StatusCompleted || reads.Load() != 1 {
		t.Fatalf("Resume = %+v, %v; reads = %d", res, err, reads.Load())
	}
}

func TestResumeFailedRun(t *testing.T) {
	t.Parallel()
	m := modeltest.New(modeltest.Fail(errors.New("overloaded")), modeltest.Reply("recovered"))
	cp := loop.NewMemoryCheckpointer()
	r := newRuntime(t, loop.Config{Model: m, Checkpointer: cp})
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil)
	if err == nil || res.Status != loop.StatusFailed {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	res, err = loop.Collect(r.Resume(t.Context(), res.RunID, nil), nil)
	if err != nil || res.Status != loop.StatusCompleted || res.Output != "recovered" || len(res.NewMessages) != 2 {
		t.Fatalf("Resume = %+v, %v", res, err)
	}
}

func TestResumeRefusals(t *testing.T) {
	t.Parallel()
	r := newRuntime(t, loop.Config{Model: modeltest.New()})
	input := []*model.Message{model.UserText("q")}
	for name, st := range map[string]*loop.State{
		"newer version":    {RunID: "r", Version: loop.StateVersion + 1, Status: loop.StatusInterrupted, Messages: input},
		"blocked":          {RunID: "r", Status: loop.StatusBlocked, Messages: input},
		"failed, no input": {RunID: "r", Status: loop.StatusFailed},
	} {
		if _, err := loop.Collect(r.ResumeState(t.Context(), st, nil), nil); err == nil {
			t.Errorf("%s: resumed", name)
		}
	}
	for _, s := range []loop.Status{loop.StatusInterrupted, loop.StatusRunning, loop.StatusFailed} {
		if !s.Resumable() {
			t.Errorf("%s is not resumable", s)
		}
	}
	if loop.StatusCompleted.Resumable() || loop.StatusIncomplete.Resumable() {
		t.Error("final status is resumable")
	}
}

func TestIncompleteAnswers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		reason model.FinishReason
		status loop.Status
	}{
		{model.FinishLength, loop.StatusIncomplete},
		{model.FinishContentFilter, loop.StatusIncomplete},
		{model.FinishRefusal, loop.StatusCompleted},
		{model.FinishStop, loop.StatusCompleted},
	} {
		t.Run(string(tc.reason), func(t *testing.T) {
			t.Parallel()
			m := modeltest.New(modeltest.Respond(&model.Response{Message: model.AssistantText("partial"), FinishReason: tc.reason}))
			res, err := loop.Collect(newRuntime(t, loop.Config{Model: m}).Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil)
			if err != nil || res.Status != tc.status || res.FinishReason != tc.reason || res.Output != "partial" {
				t.Fatalf("result = %+v, %v", res, err)
			}
			if tc.status == loop.StatusIncomplete && res.Reason == "" {
				t.Fatal("incomplete run has no reason")
			}
		})
	}
}

func TestChangedArgumentsAreReported(t *testing.T) {
	t.Parallel()
	var got string
	echo := tool.Func("echo", "echo", func(_ context.Context, in echoArgs) (string, error) { got = in.Text; return in.Text, nil }, tool.RequireApproval())
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c", Name: "echo", Args: echoArgs{Text: "rm -rf /"}}), modeltest.Reply("done"))
	approver := loop.ApproverFunc(func(context.Context, *loop.ApprovalRequest) (loop.Approval, error) {
		return loop.Approval{Approved: true, UpdatedInput: json.RawMessage(`{"text":"ls"}`)}, nil
	})
	r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, echo), Approver: approver})
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil)
	if err != nil || res.Status != loop.StatusCompleted || got != "ls" {
		t.Fatalf("Run = %+v, %v; tool got %q", res, err, got)
	}
	msgs := m.Requests()[1].Messages
	if args := string(msgs[1].ToolCalls()[0].Arguments); args != `{"text":"rm -rf /"}` {
		t.Fatalf("the model's call was rewritten: %s", args)
	}
	if out := msgs[2].ToolResults()[0].Content; !strings.Contains(out, `changed before execution: {"text":"ls"}`) || !strings.HasSuffix(out, "\nls") {
		t.Fatalf("result = %q", out)
	}
	// Approving the arguments unchanged adds no note.
	m = modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c", Name: "echo", Args: echoArgs{Text: "ls"}}), modeltest.Reply("done"))
	r = newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, echo), Approver: approver})
	if _, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil); err != nil {
		t.Fatal(err)
	}
	if out := m.Requests()[1].Messages[2].ToolResults()[0].Content; out != "ls" {
		t.Fatalf("unchanged arguments: %q", out)
	}
}

func TestCallScopeOutsideRuns(t *testing.T) {
	t.Parallel()
	loop.ReportUsage(t.Context(), model.Usage{InputTokens: 1})
	if loop.InterruptCall(t.Context(), &loop.Interrupt{Requests: []*loop.ApprovalRequest{{Call: &model.ToolCall{ID: "x"}}}}) {
		t.Fatal("InterruptCall succeeded outside a run")
	}
	if _, _, ok := loop.ResumedCall(t.Context()); ok {
		t.Fatal("ResumedCall reported a resume outside a run")
	}
}

func TestInterruptCarriesState(t *testing.T) {
	t.Parallel()
	echo := tool.Func("echo", "echo", func(_ context.Context, in echoArgs) (string, error) { return in.Text, nil }, tool.RequireApproval())
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c", Name: "echo", Args: echoArgs{Text: "hi"}}), modeltest.Reply("done"))
	r := newRuntime(t, loop.Config{Name: "ops", Model: m, Tools: toolRegistry(t, echo)})
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil)
	if err != nil || res.Status != loop.StatusInterrupted || res.Interrupt.State == nil || res.Interrupt.Requests[0].Agent != "ops" {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	// Without a checkpointer the run resumes from the interrupt's state.
	res, err = loop.Collect(r.ResumeState(t.Context(), res.Interrupt.State, map[string]loop.Approval{"c": loop.Approve}), nil)
	if err != nil || res.Status != loop.StatusCompleted || res.Output != "done" {
		t.Fatalf("ResumeState = %+v, %v", res, err)
	}
}

func TestContextWrapper(t *testing.T) {
	t.Parallel()
	type key struct{}
	var order []string
	wrapper := func(name string) loop.Middleware {
		return &loop.Funcs{MiddlewareName: name, WrapContextFunc: func(ctx context.Context, run *loop.Run) (context.Context, func()) {
			order = append(order, "wrap "+name)
			return context.WithValue(ctx, key{}, name), func() { order = append(order, "release "+name) }
		}}
	}
	var seen any
	m := model.Func(func(ctx context.Context, _ *model.Request) (*model.Response, error) {
		seen = ctx.Value(key{})
		return &model.Response{Message: model.AssistantText("ok"), FinishReason: model.FinishStop}, nil
	})
	r := newRuntime(t, loop.Config{Model: m, Middlewares: []loop.Middleware{wrapper("outer"), wrapper("inner")}})
	// Release runs even when the consumer stops early.
	for _, err := range r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}) {
		if err != nil {
			t.Fatal(err)
		}
		break
	}
	if got := strings.Join(order, ","); got != "wrap outer,wrap inner,release inner,release outer" {
		t.Fatalf("order = %s", got)
	}
	order = nil
	if _, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil); err != nil || seen != "inner" {
		t.Fatalf("model saw %v, %v", seen, err)
	}
}

// A resumed run finishes only its own turn: an unanswered call in the
// history before its input is not executed.
func TestResumeIgnoresUnansweredHistoryCalls(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	echo := tool.Func("echo", "echo", func(_ context.Context, in echoArgs) (string, error) { calls.Add(1); return in.Text, nil })
	m := modeltest.New(modeltest.Reply("done"))
	cp := loop.NewMemoryCheckpointer()
	r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, echo), Checkpointer: cp})
	history := []*model.Message{
		model.UserText("old"),
		model.NewMessage(model.RoleAssistant, model.Part{Type: model.PartToolCall, ToolCall: &model.ToolCall{ID: "old", Name: "echo", Arguments: json.RawMessage(`{"text":"x"}`)}}),
	}
	var runID string
	for ev, err := range r.Run(t.Context(), &loop.Request{History: history, Input: model.UserText("q")}) {
		if err != nil {
			t.Fatal(err)
		}
		runID = ev.RunID
		if ev.Type == loop.EventTurnStart {
			break
		}
	}
	res, err := loop.Collect(r.Resume(t.Context(), runID, nil), nil)
	if err != nil || res.Status != loop.StatusCompleted || calls.Load() != 0 || len(res.NewMessages) != 2 {
		t.Fatalf("Resume = %+v, %v; calls = %d", res, err, calls.Load())
	}
}

// Stopping at the message event of a final answer leaves the answer
// unhandled; a resumed run handles it, with its finish reason, instead of
// asking the model again.
func TestStopAtFinalMessageResumesTheAnswer(t *testing.T) {
	t.Parallel()
	m := modeltest.New(modeltest.Respond(&model.Response{Message: model.AssistantText("first"), FinishReason: model.FinishLength}), modeltest.Reply("second"))
	var finishes atomic.Int32
	finish := &loop.Funcs{MiddlewareName: "finish", BeforeFinishFunc: func(context.Context, *loop.Run, *model.Message) (bool, error) {
		finishes.Add(1)
		return false, nil
	}}
	cp := loop.NewMemoryCheckpointer()
	r := newRuntime(t, loop.Config{Model: m, Checkpointer: cp, Middlewares: []loop.Middleware{finish}})
	var runID string
	for ev, err := range r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}) {
		if err != nil {
			t.Fatal(err)
		}
		runID = ev.RunID
		if ev.Type == loop.EventMessage {
			break
		}
	}
	res, err := loop.Collect(r.Resume(t.Context(), runID, nil), nil)
	if err != nil || res.Status != loop.StatusIncomplete || res.FinishReason != model.FinishLength || res.Output != "first" || len(res.NewMessages) != 2 {
		t.Fatalf("Resume = %+v, %v", res, err)
	}
	if len(m.Requests()) != 1 || finishes.Load() != 1 {
		t.Fatalf("model calls = %d; BeforeFinish calls = %d", len(m.Requests()), finishes.Load())
	}
}

// Stopping at EventRunStart runs nothing, not even AfterRun, and leaves
// the checkpoint of a resumed run as it was.
func TestStopAtRunStart(t *testing.T) {
	t.Parallel()
	echo := tool.Func("echo", "echo", func(_ context.Context, in echoArgs) (string, error) { return in.Text, nil }, tool.RequireApproval())
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c", Name: "echo", Args: echoArgs{Text: "x"}}), modeltest.Reply("done"))
	var afterRuns atomic.Int32
	afterRun := &loop.Funcs{MiddlewareName: "after", AfterRunFunc: func(context.Context, *loop.Run, *loop.Result) { afterRuns.Add(1) }}
	cp := loop.NewMemoryCheckpointer()
	r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, echo), Checkpointer: cp, Middlewares: []loop.Middleware{afterRun}})
	stopAtStart := func(events func(func(*loop.Event, error) bool)) {
		for _, err := range events {
			if err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	stopAtStart(r.Run(t.Context(), &loop.Request{RunID: "early", Input: model.UserText("q")}))
	if afterRuns.Load() != 0 {
		t.Fatal("AfterRun ran for a run that took no step")
	}
	if _, err := cp.Load(t.Context(), "early"); !errors.Is(err, loop.ErrCheckpointNotFound) {
		t.Fatalf("checkpoint of a run that took no step: %v", err)
	}

	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil)
	if err != nil || res.Status != loop.StatusInterrupted {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	afterRuns.Store(0)
	stopAtStart(r.Resume(t.Context(), res.RunID, nil))
	if st, err := cp.Load(t.Context(), res.RunID); err != nil || st.Status != loop.StatusInterrupted || afterRuns.Load() != 0 {
		t.Fatalf("checkpoint = %+v, %v; AfterRun calls = %d", st, err, afterRuns.Load())
	}
	res, err = loop.Collect(r.Resume(t.Context(), res.RunID, map[string]loop.Approval{"c": loop.Approve}), nil)
	if err != nil || res.Status != loop.StatusCompleted || res.Output != "done" {
		t.Fatalf("Resume = %+v, %v", res, err)
	}
}

// The reason of an abandoned run says whether it can be resumed.
func TestAbandonedReason(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		cp        loop.Checkpointer
		stopAt    loop.EventType
		resumable bool
	}{
		{name: "checkpointed", cp: loop.NewMemoryCheckpointer(), stopAt: loop.EventMessage, resumable: true},
		{name: "no checkpointer", stopAt: loop.EventMessage},
		{name: "before the input", cp: loop.NewMemoryCheckpointer(), stopAt: "note"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var later atomic.Int32
			var ended *loop.Result
			mws := []loop.Middleware{
				&loop.Funcs{MiddlewareName: "note", BeforeRunFunc: func(_ context.Context, run *loop.Run, _ *loop.RunInput) error {
					run.Emit(&loop.Event{Type: "note"})
					return nil
				}},
				&loop.Funcs{MiddlewareName: "later", BeforeRunFunc: func(context.Context, *loop.Run, *loop.RunInput) error { later.Add(1); return nil }},
				&loop.Funcs{MiddlewareName: "after", AfterRunFunc: func(_ context.Context, _ *loop.Run, res *loop.Result) { ended = res }},
			}
			m := modeltest.New(modeltest.Reply("done"))
			r := newRuntime(t, loop.Config{Model: m, Checkpointer: tc.cp, Middlewares: mws})
			for ev, err := range r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}) {
				if err != nil {
					t.Fatal(err)
				}
				if ev.Type == tc.stopAt {
					break
				}
			}
			if ended == nil || ended.Status != loop.StatusRunning || strings.Contains(ended.Reason, "can be resumed") != tc.resumable {
				t.Fatalf("AfterRun saw %+v", ended)
			}
			if tc.stopAt == "note" && (later.Load() != 0 || len(ended.NewMessages) != 0 || len(m.Requests()) != 0) {
				t.Fatalf("the run continued after the consumer stopped: later BeforeRun calls = %d; result = %+v", later.Load(), ended)
			}
		})
	}
}

// A middleware event that the consumer refuses in AfterModel stops the
// run before its calls are planned, so no approval is asked.
func TestStopInAfterModelSkipsApproval(t *testing.T) {
	t.Parallel()
	var runs, asked atomic.Int32
	echo := tool.Func("echo", "echo", func(_ context.Context, in echoArgs) (string, error) { runs.Add(1); return in.Text, nil }, tool.RequireApproval())
	approver := loop.ApproverFunc(func(context.Context, *loop.ApprovalRequest) (loop.Approval, error) {
		asked.Add(1)
		return loop.Approve, nil
	})
	note := &loop.Funcs{MiddlewareName: "note", AfterModelFunc: func(_ context.Context, run *loop.Run, _ *model.Request, _ *model.Response) error {
		run.Emit(&loop.Event{Type: "note"})
		return nil
	}}
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c", Name: "echo", Args: echoArgs{Text: "x"}}), modeltest.Reply("done"))
	cp := loop.NewMemoryCheckpointer()
	r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, echo), Checkpointer: cp, Approver: approver, Middlewares: []loop.Middleware{note}})
	var runID string
	for ev, err := range r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}) {
		if err != nil {
			t.Fatal(err)
		}
		runID = ev.RunID
		if ev.Type == "note" {
			break
		}
	}
	if asked.Load() != 0 {
		t.Fatalf("approver asked %d times after the consumer stopped", asked.Load())
	}
	res, err := loop.Collect(r.Resume(t.Context(), runID, nil), nil)
	if err != nil || res.Status != loop.StatusCompleted || asked.Load() != 1 || runs.Load() != 1 {
		t.Fatalf("Resume = %+v, %v; asked = %d; runs = %d", res, err, asked.Load(), runs.Load())
	}
}

// Nested requests raised in a batch the consumer stopped in were never
// shown; resuming the run interrupts with them instead of denying them.
func TestResumeAsksForUnseenNestedRequests(t *testing.T) {
	t.Parallel()
	var effects atomic.Int32
	sub := tool.Func("sub", "sub", func(ctx context.Context, _ struct{}) (string, error) {
		if _, approvals, ok := loop.ResumedCall(ctx); ok {
			if !approvals["inner"].Approved {
				return "denied", nil
			}
			effects.Add(1)
			return "sub done", nil
		}
		loop.InterruptCall(ctx, &loop.Interrupt{RunID: "inner_run", Requests: []*loop.ApprovalRequest{{Call: &model.ToolCall{ID: "inner", Name: "x"}}}})
		return "", nil
	}, tool.ReadOnly())
	read := tool.Func("read", "read", func(context.Context, struct{}) (string, error) { return "contents", nil }, tool.ReadOnly())
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "s", Name: "sub"}, modeltest.Call{ID: "r", Name: "read"}), modeltest.Reply("done"))
	cp := loop.NewMemoryCheckpointer()
	r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, sub, read), Checkpointer: cp})
	var runID string
	for ev, err := range r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}) {
		if err != nil {
			t.Fatal(err)
		}
		runID = ev.RunID
		if ev.Type == loop.EventToolResult {
			break
		}
	}
	res, err := loop.Collect(r.Resume(t.Context(), runID, nil), nil)
	if err != nil || res.Status != loop.StatusInterrupted || len(res.Interrupt.Requests) != 1 || res.Interrupt.Requests[0].Call.ID != "inner" {
		t.Fatalf("Resume = %+v, %v", res, err)
	}
	res, err = loop.Collect(r.Resume(t.Context(), runID, map[string]loop.Approval{"inner": loop.Approve}), nil)
	if err != nil || res.Status != loop.StatusCompleted || effects.Load() != 1 {
		t.Fatalf("Resume with approval = %+v, %v; effects = %d", res, err, effects.Load())
	}
}

// A run that fails before its input is added leaves no checkpoint, since
// it cannot be resumed.
func TestFailureBeforeInputDeletesCheckpoint(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cp := loop.NewMemoryCheckpointer()
	cancelRun := &loop.Funcs{MiddlewareName: "cancel", BeforeRunFunc: func(context.Context, *loop.Run, *loop.RunInput) error { cancel(); return nil }}
	r := newRuntime(t, loop.Config{Model: modeltest.New(), Checkpointer: cp, Middlewares: []loop.Middleware{cancelRun}})
	res, err := loop.Collect(r.Run(ctx, &loop.Request{Input: model.UserText("q")}), nil)
	if !errors.Is(err, context.Canceled) || res.Status != loop.StatusFailed {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if _, err := cp.Load(t.Context(), res.RunID); !errors.Is(err, loop.ErrCheckpointNotFound) {
		t.Fatalf("checkpoint of a run that failed before its input: %v", err)
	}
}
