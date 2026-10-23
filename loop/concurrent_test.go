package loop_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/tool"
)

// barrier blocks each caller until n callers arrived, or fails the test
// after a timeout.
type barrier struct {
	t       *testing.T
	n       int
	release chan struct{}
	once    sync.Once
	count   atomic.Int32
}

func newBarrier(t *testing.T, n int) *barrier {
	return &barrier{t: t, n: n, release: make(chan struct{})}
}

func (b *barrier) wait() {
	if int(b.count.Add(1)) == b.n {
		b.once.Do(func() { close(b.release) })
	}
	select {
	case <-b.release:
	case <-time.After(5 * time.Second):
		b.t.Error("calls did not run concurrently")
	}
}

func TestConcurrentToolsRunTogether(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := newBarrier(t, 3)
		var active, overlaps atomic.Int32
		work := tool.Func("work", "work", func(_ context.Context, in echoArgs) (string, error) {
			active.Add(1)
			defer active.Add(-1)
			b.wait()
			return in.Text, nil
		}, tool.Concurrent())
		read := tool.Func("read", "read", func(context.Context, struct{}) (string, error) { b.wait(); return "read", nil }, tool.ReadOnly())
		write := tool.Func("write", "write", func(context.Context, struct{}) (string, error) {
			if active.Load() != 0 {
				overlaps.Add(1)
			}
			return "written", nil
		})
		m := modeltest.New(modeltest.CallTools(
			modeltest.Call{ID: "a", Name: "work", Args: echoArgs{Text: "first"}},
			modeltest.Call{ID: "r", Name: "read"},
			modeltest.Call{ID: "b", Name: "work", Args: echoArgs{Text: "second"}},
			modeltest.Call{ID: "w", Name: "write"},
		), modeltest.Reply("done"))
		r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, work, read, write)})
		res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil)
		if err != nil || res.Status != loop.StatusCompleted || overlaps.Load() != 0 {
			t.Fatalf("Run = %+v, %v; overlaps = %d", res, err, overlaps.Load())
		}
		results := m.Requests()[1].Messages[2].ToolResults()
		for i, want := range []string{"first", "read", "second", "written"} {
			if len(results) != 4 || results[i].Content != want {
				t.Fatalf("results = %+v, want position %d = %s", results, i, want)
			}
		}
	})
}

// Concurrent calls with side effects that a crash cut off are reported as
// possibly incomplete, not repeated, like any call with side effects.
func TestCutOffConcurrentCallsAreNotRepeated(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cp := loop.NewMemoryCheckpointer()
		b := newBarrier(t, 2)
		var effects atomic.Int32
		var crashed atomic.Pointer[loop.State]
		work := tool.Func("work", "work", func(ctx context.Context, _ struct{}) (string, error) {
			effects.Add(1)
			b.wait()
			st, err := cp.Load(ctx, loop.CallingRun(ctx).ID())
			crashed.Store(st)
			return "done", err
		}, tool.Concurrent())
		m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "a", Name: "work"}, modeltest.Call{ID: "b", Name: "work"}), modeltest.Reply("done"), modeltest.Reply("done"))
		r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, work), Checkpointer: cp})
		if _, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil); err != nil {
			t.Fatal(err)
		}
		st := crashed.Load()
		if st == nil || len(st.Pending) != 2 || !st.Pending[0].Started || !st.Pending[1].Started || !st.Pending[0].Concurrent || st.Pending[0].ReadOnly {
			t.Fatalf("checkpoint = %+v", st)
		}
		if err := cp.Save(t.Context(), st); err != nil {
			t.Fatal(err)
		}
		res, err := loop.Collect(r.Resume(t.Context(), st.RunID, nil), nil)
		if err != nil || res.Status != loop.StatusCompleted || effects.Load() != 2 {
			t.Fatalf("Resume = %+v, %v; effects = %d", res, err, effects.Load())
		}
		for _, r := range m.Requests()[2].Messages[2].ToolResults() {
			if !r.IsError || !strings.Contains(r.Content, "may or may not have taken effect") {
				t.Fatalf("result after crash = %+v", r)
			}
		}
	})
}

// nestedTool returns a concurrent tool whose calls interrupt for a nested
// request named after their argument, and record the decisions they are
// resumed with.
func nestedTool(t *testing.T, resumed *barrier, decisions *sync.Map, cp loop.Checkpointer, crashed *atomic.Pointer[loop.State]) tool.Tool {
	t.Helper()
	return tool.Func("sub", "sub", func(ctx context.Context, in echoArgs) (string, error) {
		id := "inner_" + in.Text
		if intr, approvals, ok := loop.ResumedCall(ctx); ok {
			if intr.RunID != "run_"+in.Text {
				t.Errorf("call %s resumed with interrupt %s", in.Text, intr.RunID)
			}
			resumed.wait()
			decisions.Store(in.Text, approvals[id].Approved)
			if crashed != nil {
				st, err := cp.Load(ctx, loop.CallingRun(ctx).ID())
				if err != nil {
					return "", err
				}
				crashed.Store(st)
			}
			return in.Text + " resumed", nil
		}
		loop.InterruptCall(ctx, &loop.Interrupt{RunID: "run_" + in.Text, Requests: []*loop.ApprovalRequest{{Call: &model.ToolCall{ID: id, Name: "x"}, Agent: in.Text}}})
		return "", nil
	}, tool.Concurrent())
}

// Several concurrent calls that interrupt for nested approvals surface in
// one interrupt, resume together with their own decisions, and are not
// repeated after a crash while they resume.
func TestConcurrentNestedInterrupts(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cp := loop.NewMemoryCheckpointer()
		var decisions sync.Map
		var crashed atomic.Pointer[loop.State]
		sub := nestedTool(t, newBarrier(t, 2), &decisions, cp, &crashed)
		m := modeltest.New(
			modeltest.CallTools(modeltest.Call{ID: "a", Name: "sub", Args: echoArgs{Text: "a"}}, modeltest.Call{ID: "b", Name: "sub", Args: echoArgs{Text: "b"}}),
			modeltest.Reply("done"),
			modeltest.Reply("done"),
		)
		r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, sub), Checkpointer: cp})
		res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil)
		if err != nil || res.Status != loop.StatusInterrupted || len(res.Interrupt.Requests) != 2 ||
			res.Interrupt.Requests[0].Call.ID != "inner_a" || res.Interrupt.Requests[1].Call.ID != "inner_b" {
			t.Fatalf("Run = %+v, %v", res, err)
		}
		approvals := map[string]loop.Approval{"inner_a": loop.Approve, "inner_b": loop.Deny}
		res, err = loop.Collect(r.Resume(t.Context(), res.RunID, approvals), nil)
		if err != nil || res.Status != loop.StatusCompleted {
			t.Fatalf("Resume = %+v, %v", res, err)
		}
		a, _ := decisions.Load("a")
		b, _ := decisions.Load("b")
		if a != true || b != false {
			t.Fatalf("decisions = %v, %v", a, b)
		}
		if results := m.Requests()[1].Messages[2].ToolResults(); len(results) != 2 || results[0].Content != "a resumed" || results[1].Content != "b resumed" {
			t.Fatalf("results = %+v", results)
		}

		// The checkpoint saved while both calls resumed their nested runs marks
		// them so; resuming it does not repeat them.
		st := crashed.Load()
		if st == nil || !st.Pending[0].Resuming || !st.Pending[1].Resuming {
			t.Fatalf("checkpoint while resuming = %+v", st)
		}
		if err := cp.Save(t.Context(), st); err != nil {
			t.Fatal(err)
		}
		decisions.Clear()
		res, err = loop.Collect(r.Resume(t.Context(), st.RunID, nil), nil)
		if err != nil || res.Status != loop.StatusCompleted {
			t.Fatalf("Resume after crash = %+v, %v", res, err)
		}
		if _, ran := decisions.Load("a"); ran {
			t.Fatal("a resumed call was repeated after a crash")
		}
		for _, r := range m.Requests()[2].Messages[2].ToolResults() {
			if !strings.Contains(r.Content, "may or may not have taken effect") {
				t.Fatalf("result after crash = %+v", r)
			}
		}
	})
}

// An approver decides the requests of several nested runs at once, and
// the calls resume concurrently.
func TestApproverDecidesConcurrentNestedRequests(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var decisions sync.Map
		var asked atomic.Int32
		sub := nestedTool(t, newBarrier(t, 2), &decisions, nil, nil)
		approver := loop.ApproverFunc(func(_ context.Context, req *loop.ApprovalRequest) (loop.Approval, error) {
			asked.Add(1)
			return loop.Approval{Approved: req.Agent == "a"}, nil
		})
		m := modeltest.New(
			modeltest.CallTools(modeltest.Call{ID: "a", Name: "sub", Args: echoArgs{Text: "a"}}, modeltest.Call{ID: "b", Name: "sub", Args: echoArgs{Text: "b"}}),
			modeltest.Reply("done"),
		)
		r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, sub), Approver: approver})
		res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil)
		if err != nil || res.Status != loop.StatusCompleted || asked.Load() != 2 {
			t.Fatalf("Run = %+v, %v; asked = %d", res, err, asked.Load())
		}
		if a, _ := decisions.Load("a"); a != true {
			t.Fatalf("decision of a = %v", a)
		}
		if b, _ := decisions.Load("b"); b != false {
			t.Fatalf("decision of b = %v", b)
		}
	})
}

// nestedRun returns a tool that runs r as a nested run and forwards its
// events.
func nestedRun(name string, r *loop.Runtime, opts ...tool.Option) tool.Tool {
	return tool.Func(name, name, func(ctx context.Context, in echoArgs) (string, error) {
		res, err := loop.Collect(r.Run(ctx, &loop.Request{RunID: name + "_run", Input: model.UserText(in.Text)}), func(ev *loop.Event) {
			loop.ForwardEvent(ctx, ev)
		})
		if err != nil {
			return "", err
		}
		return res.Output, nil
	}, opts...)
}

func TestForwardEvent(t *testing.T) {
	t.Parallel()
	inner := newRuntime(t, loop.Config{Name: "inner", Model: modeltest.New(modeltest.Reply("inner answer"))})
	middle := newRuntime(t, loop.Config{
		Name:   "middle",
		Model:  modeltest.New(modeltest.CallTools(modeltest.Call{ID: "m1", Name: "inner", Args: echoArgs{Text: "go"}}), modeltest.Reply("middle answer")),
		Tools:  toolRegistry(t, nestedRun("inner", inner)),
		Stream: true,
	})
	var leaked context.Context
	leak := tool.Func("leak", "leak", func(ctx context.Context, _ struct{}) (string, error) { leaked = ctx; return "", nil })
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "p1", Name: "middle", Args: echoArgs{Text: "go"}}, modeltest.Call{ID: "p2", Name: "leak"}), modeltest.Reply("top answer"))
	r := newRuntime(t, loop.Config{Name: "top", Model: m, Tools: toolRegistry(t, nestedRun("middle", middle), leak)})

	var nested []*loop.Event
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), func(ev *loop.Event) {
		if ev.Depth > 0 {
			nested = append(nested, ev)
		} else if ev.Agent != "top" || ev.ParentCallID != "" {
			t.Errorf("own event = %+v", ev)
		}
	})
	if err != nil || res.Status != loop.StatusCompleted || res.Output != "top answer" {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	var deltas, ends int
	for _, ev := range nested {
		switch {
		case ev.Agent == "middle" && ev.Depth == 1 && ev.ParentCallID == "p1" && ev.RunID == "middle_run":
			if ev.Type == loop.EventDelta {
				deltas++
			}
		case ev.Agent == "inner" && ev.Depth == 2 && ev.ParentCallID == "m1" && ev.RunID == "inner_run":
		default:
			t.Fatalf("nested event = %+v", ev)
		}
		if ev.Type == loop.EventRunEnd {
			ends++
		}
	}
	if deltas == 0 || ends != 2 || nested[0].Type != loop.EventRunStart {
		t.Fatalf("nested events: %d deltas, %d ends, first %s", deltas, ends, nested[0].Type)
	}
	if loop.ForwardEvent(leaked, &loop.Event{Type: "late"}) || loop.ForwardEvent(t.Context(), &loop.Event{Type: "outside"}) {
		t.Fatal("ForwardEvent delivered an event outside a running call")
	}
}

// Events emitted or forwarded by calls running concurrently reach the
// consumer one at a time and stop once it stops; the calls still finish.
func TestEventsOfConcurrentCalls(t *testing.T) {
	t.Parallel()
	const n = 4
	var finished atomic.Int32
	noisy := tool.Func("noisy", "noisy", func(ctx context.Context, in echoArgs) (string, error) {
		defer finished.Add(1)
		for i := range 50 {
			loop.ForwardEvent(ctx, &loop.Event{Type: "progress", RunID: in.Text, Turn: i})
		}
		return in.Text, nil
	}, tool.Concurrent())
	emit := &loop.Funcs{MiddlewareName: "emit", WrapToolFunc: func(run *loop.Run, next tool.Invoker) tool.Invoker {
		return func(ctx context.Context, call *tool.Call) (*tool.Result, error) {
			run.Emit(&loop.Event{Type: "wrapped"})
			return next(ctx, call)
		}
	}}
	calls := make([]modeltest.Call, n)
	for i := range calls {
		calls[i] = modeltest.Call{ID: string(rune('a' + i)), Name: "noisy", Args: echoArgs{Text: string(rune('a' + i))}}
	}
	m := modeltest.New(modeltest.CallTools(calls...), modeltest.Reply("done"), modeltest.CallTools(calls...), modeltest.Reply("done"))
	cp := loop.NewMemoryCheckpointer()
	r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, noisy), Middlewares: []loop.Middleware{emit}, Checkpointer: cp, Parallelism: 2})

	var progress, wrapped int
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), func(ev *loop.Event) {
		switch ev.Type {
		case "progress":
			if ev.Depth != 1 || ev.ParentCallID != ev.RunID {
				t.Errorf("forwarded event = %+v", ev)
			}
			progress++
		case "wrapped":
			if ev.Depth != 0 || ev.Turn != 1 {
				t.Errorf("emitted event = %+v", ev)
			}
			wrapped++
		}
	})
	if err != nil || res.Status != loop.StatusCompleted || progress != n*50 || wrapped != n {
		t.Fatalf("Run = %+v, %v; progress = %d, wrapped = %d", res, err, progress, wrapped)
	}

	// Stopping at the first forwarded event: no event follows, and the
	// calls run to the end and keep their results.
	finished.Store(0)
	var runID string
	for ev, err := range r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == "progress" {
			break
		}
		runID = ev.RunID
	}
	st, err := cp.Load(t.Context(), runID)
	if err != nil || finished.Load() != n || len(st.Pending) != n {
		t.Fatalf("checkpoint = %+v, %v; finished = %d", st, err, finished.Load())
	}
	for _, p := range st.Pending {
		if p.Result == nil || p.Result.Content != p.Call.ID {
			t.Fatalf("pending call = %+v", p)
		}
	}
}
