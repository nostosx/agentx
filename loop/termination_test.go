package loop_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/tool"
)

func TestCancellationStopsRemainingTools(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var later atomic.Int32
	first := tool.Func("first", "first", func(context.Context, struct{}) (string, error) { cancel(); return "ok", nil })
	second := tool.Func("second", "second", func(context.Context, struct{}) (string, error) { later.Add(1); return "ran", nil })
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "a", Name: "first"}, modeltest.Call{ID: "b", Name: "second"}))
	r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, first, second)})
	res, err := loop.Collect(r.Run(ctx, &loop.Request{Input: model.UserText("q")}), nil)
	if !errors.Is(err, context.Canceled) || res.Status != loop.StatusFailed || later.Load() != 0 {
		t.Fatalf("Run = %+v, %v; later calls = %d", res, err, later.Load())
	}
	requireAnswered(t, res.Messages)
	results := res.Messages[len(res.Messages)-1].ToolResults()
	if len(results) != 2 || results[0].Content != "ok" || !results[1].IsError {
		t.Fatalf("results = %+v", results)
	}
}

func TestAfterRunWhenConsumerStopsAtInterrupt(t *testing.T) {
	t.Parallel()
	echo := tool.Func("echo", "echo", func(_ context.Context, in echoArgs) (string, error) { return in.Text, nil }, tool.RequireApproval())
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c", Name: "echo", Args: echoArgs{Text: "hi"}}))
	var ended []loop.Status
	afterRun := &loop.Funcs{MiddlewareName: "after", AfterRunFunc: func(_ context.Context, _ *loop.Run, res *loop.Result) {
		ended = append(ended, res.Status)
	}}
	r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, echo), Middlewares: []loop.Middleware{afterRun}})
	for ev, err := range r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == loop.EventInterrupted {
			break
		}
	}
	if len(ended) != 1 || ended[0] != loop.StatusInterrupted {
		t.Fatalf("AfterRun saw %v", ended)
	}
}

func TestRequestDoesNotShareConfig(t *testing.T) {
	t.Parallel()
	echo := tool.Func("echo", "echo", func(_ context.Context, in echoArgs) (string, error) { return in.Text, nil }, tool.ReadOnly())
	mutate := &loop.Funcs{MiddlewareName: "mutate", BeforeModelFunc: func(_ context.Context, _ *loop.Run, req *model.Request) error {
		if req.ProviderOptions["openai"]["k"] != "v" || *req.Temperature != 0.5 || req.Reasoning.Effort != "low" || req.ToolChoice.Mode != model.ToolChoiceAuto || string(req.ResponseFormat.Schema) != "{}" {
			return errors.New("request does not hold the configured options")
		}
		req.ProviderOptions["openai"]["k"] = "changed"
		*req.Temperature = 1
		req.Reasoning.Effort = "high"
		req.ToolChoice.Mode = model.ToolChoiceNone
		req.ResponseFormat.Schema[0] = '['
		return nil
	}}
	cfg := loop.Config{
		Model: model.Func(func(_ context.Context, req *model.Request) (*model.Response, error) {
			if len(req.Messages) == 1 {
				return modeltest.CallTools(modeltest.Call{ID: "c", Name: "echo", Args: echoArgs{Text: "x"}})(req)
			}
			return modeltest.Reply("done")(req)
		}),
		Tools:           toolRegistry(t, echo),
		Middlewares:     []loop.Middleware{mutate},
		ProviderOptions: map[string]map[string]any{"openai": {"k": "v"}},
		Temperature:     new(0.5),
		Reasoning:       &model.Reasoning{Effort: "low"},
		ToolChoice:      &model.ToolChoice{Mode: model.ToolChoiceAuto},
		ResponseFormat:  &model.ResponseFormat{Name: "out", Schema: json.RawMessage("{}")},
	}
	r := newRuntime(t, cfg)
	// Concurrent runs, each with two model calls, must not see each
	// other's changes; the race detector reports shared options.
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil); err != nil || res.Status != loop.StatusCompleted {
				t.Errorf("Run = %+v, %v", res, err)
			}
		})
	}
	wg.Wait()
	if cfg.ProviderOptions["openai"]["k"] != "v" || *cfg.Temperature != 0.5 || cfg.Reasoning.Effort != "low" || cfg.ToolChoice.Mode != model.ToolChoiceAuto || string(cfg.ResponseFormat.Schema) != "{}" {
		t.Fatalf("configuration changed: %+v", cfg)
	}
}

// cancelCheckpointer fails Save and Delete calls made with a cancelled
// context, and Save calls once failSave is set.
type cancelCheckpointer struct {
	*loop.MemoryCheckpointer
	failSave atomic.Bool
}

func (c *cancelCheckpointer) Save(ctx context.Context, st *loop.State) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.failSave.Load() {
		return errors.New("disk full")
	}
	return c.MemoryCheckpointer.Save(ctx, st)
}

func (c *cancelCheckpointer) Delete(ctx context.Context, runID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.MemoryCheckpointer.Delete(ctx, runID)
}

func TestFinalStatusesDeleteCheckpoint(t *testing.T) {
	t.Parallel()
	echo := tool.Func("echo", "echo", func(_ context.Context, in echoArgs) (string, error) { return in.Text, nil })
	calls := modeltest.CallTools(modeltest.Call{ID: "c", Name: "echo", Args: echoArgs{Text: "x"}})
	for _, tc := range []struct {
		name   string
		status loop.Status
		cfg    func(cancel context.CancelFunc) loop.Config
	}{
		{name: "stopped", status: loop.StatusStopped, cfg: func(context.CancelFunc) loop.Config {
			return loop.Config{Model: modeltest.New(calls), Middlewares: []loop.Middleware{&loop.Funcs{
				MiddlewareName: "stop",
				AfterToolFunc: func(context.Context, *loop.Run, *loop.ToolPlan, *model.ToolResult) error {
					return loop.Stop("enough")
				},
			}}}
		}},
		{name: "max turns", status: loop.StatusMaxTurns, cfg: func(context.CancelFunc) loop.Config {
			return loop.Config{Model: modeltest.New(calls), MaxTurns: 1}
		}},
		{name: "incomplete", status: loop.StatusIncomplete, cfg: func(context.CancelFunc) loop.Config {
			return loop.Config{Model: modeltest.New(modeltest.Respond(&model.Response{Message: model.AssistantText("cut"), FinishReason: model.FinishLength}))}
		}},
		{name: "completed after cancel", status: loop.StatusCompleted, cfg: func(cancel context.CancelFunc) loop.Config {
			return loop.Config{Model: modeltest.New(modeltest.Reply("done")), Middlewares: []loop.Middleware{&loop.Funcs{
				MiddlewareName:   "cancel",
				BeforeFinishFunc: func(context.Context, *loop.Run, *model.Message) (bool, error) { cancel(); return false, nil },
			}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cp := &cancelCheckpointer{MemoryCheckpointer: loop.NewMemoryCheckpointer()}
			cfg := tc.cfg(cancel)
			cfg.Tools, cfg.Checkpointer = toolRegistry(t, echo), cp
			res, _ := loop.Collect(newRuntime(t, cfg).Run(ctx, &loop.Request{Input: model.UserText("q")}), nil)
			if res.Status != tc.status {
				t.Fatalf("result = %+v", res)
			}
			if _, err := cp.Load(t.Context(), res.RunID); !errors.Is(err, loop.ErrCheckpointNotFound) {
				t.Fatalf("checkpoint of a %s run: %v", res.Status, err)
			}
		})
	}
}

func TestCheckpointFailureBlocksSideEffects(t *testing.T) {
	t.Parallel()
	var writes, reads atomic.Int32
	write := tool.Func("write", "write", func(context.Context, struct{}) (string, error) { writes.Add(1); return "written", nil })
	read := tool.Func("read", "read", func(context.Context, struct{}) (string, error) { reads.Add(1); return "contents", nil }, tool.ReadOnly())
	run := func(name string) (*loop.Result, error) {
		cp := &cancelCheckpointer{MemoryCheckpointer: loop.NewMemoryCheckpointer()}
		cp.failSave.Store(true)
		m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c", Name: name}), modeltest.Reply("done"))
		r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, write, read), Checkpointer: cp})
		return loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil)
	}
	res, err := run("write")
	if err == nil || !strings.Contains(err.Error(), "save checkpoint") || res.Status != loop.StatusFailed || writes.Load() != 0 {
		t.Fatalf("Run = %+v, %v; writes = %d", res, err, writes.Load())
	}
	requireAnswered(t, res.Messages)
	// Read-only calls can be repeated, so they run without a checkpoint.
	if res, err := run("read"); err != nil || res.Status != loop.StatusCompleted || reads.Load() != 1 {
		t.Fatalf("Run = %+v, %v; reads = %d", res, err, reads.Load())
	}
}

// A call that resumes its nested run is not repeated after a crash, since
// the nested run may have executed approved calls.
func TestResumedNestedCallIsNotRepeated(t *testing.T) {
	t.Parallel()
	cp := loop.NewMemoryCheckpointer()
	var effects atomic.Int32
	var crashed *loop.State
	sub := tool.Func("sub", "sub", func(ctx context.Context, _ struct{}) (string, error) {
		if _, approvals, ok := loop.ResumedCall(ctx); ok {
			if approvals["inner"].Approved {
				effects.Add(1)
			}
			st, err := cp.Load(ctx, loop.CallingRun(ctx).ID())
			crashed = st
			return "sub done", err
		}
		loop.InterruptCall(ctx, &loop.Interrupt{RunID: "inner_run", Requests: []*loop.ApprovalRequest{{Call: &model.ToolCall{ID: "inner", Name: "x"}}}})
		return "", nil
	})
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c", Name: "sub"}), modeltest.Reply("done"), modeltest.Reply("done"))
	r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, sub), Checkpointer: cp})
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("q")}), nil)
	if err != nil || res.Status != loop.StatusInterrupted {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	res, err = loop.Collect(r.Resume(t.Context(), res.RunID, map[string]loop.Approval{"inner": loop.Approve}), nil)
	if err != nil || res.Status != loop.StatusCompleted || effects.Load() != 1 {
		t.Fatalf("Resume = %+v, %v; effects = %d", res, err, effects.Load())
	}
	if crashed == nil || !crashed.Pending[0].Resuming {
		t.Fatalf("checkpoint during the resumed call = %+v", crashed)
	}
	if err := cp.Save(t.Context(), crashed); err != nil {
		t.Fatal(err)
	}
	res, err = loop.Collect(r.Resume(t.Context(), crashed.RunID, nil), nil)
	if err != nil || res.Status != loop.StatusCompleted || effects.Load() != 1 {
		t.Fatalf("Resume after crash = %+v, %v; effects = %d", res, err, effects.Load())
	}
	if out := m.Requests()[2].Messages[2].ToolResults()[0].Content; !strings.Contains(out, "may or may not have taken effect") {
		t.Fatalf("result after crash = %q", out)
	}
}
