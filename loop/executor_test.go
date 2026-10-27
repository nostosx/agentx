package loop_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nostosx/agentx/guard"
	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/permission"
	"github.com/nostosx/agentx/tool"
)

type echoArgs struct {
	Text string `json:"text"`
}

func toolRegistry(t *testing.T, tools ...tool.Tool) *tool.Registry {
	t.Helper()
	reg := &tool.Registry{}
	if err := reg.Register(tools...); err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestRuntimeToolLoop(t *testing.T) {
	t.Parallel()
	reg := toolRegistry(t, tool.Func("echo", "echo input", func(_ context.Context, in echoArgs) (string, error) { return in.Text, nil }))
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "call", Name: "echo", Args: echoArgs{Text: "value"}}), modeltest.Reply("done"))
	r := newRuntime(t, loop.Config{Model: m, Tools: reg})
	var toolEvents []loop.EventType
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("question")}), func(ev *loop.Event) {
		if ev.Type == loop.EventToolCall || ev.Type == loop.EventToolResult {
			toolEvents = append(toolEvents, ev.Type)
			if ev.ToolCall.ID != "call" || ev.Turn != 1 {
				t.Errorf("tool event = %+v", ev)
			}
		}
	})
	if err != nil || res.Output != "done" || res.Turns != 2 || res.Usage.TotalTokens() != 30 || len(res.NewMessages) != 4 {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if !reflect.DeepEqual(toolEvents, []loop.EventType{loop.EventToolCall, loop.EventToolResult}) {
		t.Fatalf("tool events = %v", toolEvents)
	}
	results := m.Requests()[1].Messages[2].ToolResults()
	if len(results) != 1 || results[0].Content != "value" || results[0].CallID != "call" || results[0].IsError {
		t.Fatalf("model tool results = %+v", results)
	}
}

func TestRuntimeReadOnlyToolsRunConcurrentlyInOrder(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		started := make(chan struct{}, 2)
		release := make(chan struct{})
		var active atomic.Int32
		var writes atomic.Int32
		read := tool.Func("read", "read", func(ctx context.Context, in echoArgs) (string, error) {
			active.Add(1)
			defer active.Add(-1)
			started <- struct{}{}
			select {
			case <-release:
				return in.Text, nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}, tool.ReadOnly())
		write := tool.Func("write", "write", func(context.Context, struct{}) (string, error) {
			if active.Load() != 0 {
				t.Error("write overlaps previous read-only group")
			}
			writes.Add(1)
			return "written", nil
		})
		m := modeltest.New(modeltest.CallTools(
			modeltest.Call{ID: "first", Name: "read", Args: echoArgs{Text: "first"}},
			modeltest.Call{ID: "second", Name: "read", Args: echoArgs{Text: "second"}},
			modeltest.Call{ID: "third", Name: "write"},
		), modeltest.Reply("done"))
		r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, read, write), Parallelism: 2})
		type outcome struct {
			res *loop.Result
			err error
		}
		done := make(chan outcome, 1)
		go func() {
			res, err := loop.Collect(r.Run(ctx, &loop.Request{Input: model.UserText("question")}), nil)
			done <- outcome{res, err}
		}()
		for range 2 {
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("read-only calls did not start concurrently")
			}
		}
		if active.Load() != 2 || writes.Load() != 0 {
			t.Fatalf("active reads = %d, writes = %d", active.Load(), writes.Load())
		}
		close(release)
		out := <-done
		if out.err != nil || out.res.Status != loop.StatusCompleted || writes.Load() != 1 {
			t.Fatalf("Run = %+v, %v; writes = %d", out.res, out.err, writes.Load())
		}
		results := m.Requests()[1].Messages[2].ToolResults()
		for i, want := range []string{"first", "second", "written"} {
			if len(results) != 3 || results[i].Content != want {
				t.Fatalf("results = %+v, want position %d = %s", results, i, want)
			}
		}
	})
}

func TestRuntimeApprovalResume(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		approvals map[string]loop.Approval
		want      string
		error     bool
	}{
		{name: "approved updated input", approvals: map[string]loop.Approval{"call": {Approved: true, UpdatedInput: json.RawMessage(`{"text":"updated"}`)}}, want: "updated"},
		{name: "denied", approvals: map[string]loop.Approval{"call": {Reason: "not allowed"}}, want: "not allowed", error: true},
		{name: "missing decision", want: "No approval was given", error: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			echo := tool.Func("echo", "echo", func(_ context.Context, in echoArgs) (string, error) { calls++; return in.Text, nil }, tool.RequireApproval())
			cp := loop.NewMemoryCheckpointer()
			m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "call", Name: "echo", Args: echoArgs{Text: "original"}}), modeltest.Reply("done"))
			r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, echo), Checkpointer: cp})
			res, err := loop.Collect(r.Run(t.Context(), &loop.Request{RunID: "approve", Input: model.UserText("question")}), nil)
			if err != nil || res.Status != loop.StatusInterrupted || res.Interrupt == nil || len(res.Interrupt.Requests) != 1 || res.Interrupt.Requests[0].Call.ID != "call" || calls != 0 {
				t.Fatalf("interrupted Run = %+v, %v; tool calls = %d", res, err, calls)
			}
			st, err := cp.Load(t.Context(), "approve")
			if err != nil || len(st.Pending) != 1 || !st.Pending[0].NeedsApproval {
				t.Fatalf("checkpoint = %+v, %v", st, err)
			}
			res, err = loop.Collect(r.Resume(t.Context(), "approve", tc.approvals), nil)
			if err != nil || res.Status != loop.StatusCompleted || res.Turns != 2 || res.Output != "done" {
				t.Fatalf("Resume = %+v, %v", res, err)
			}
			result := m.Requests()[1].Messages[2].ToolResults()[0]
			if !strings.Contains(result.Content, tc.want) || result.IsError != tc.error || calls != map[bool]int{true: 0, false: 1}[tc.error] {
				t.Fatalf("tool result = %+v; executions = %d", result, calls)
			}
			if _, err := cp.Load(t.Context(), "approve"); !errors.Is(err, loop.ErrCheckpointNotFound) {
				t.Fatalf("completed checkpoint retained: %v", err)
			}
		})
	}
}

func TestRuntimeApprovalUpdatedInputChecks(t *testing.T) {
	t.Parallel()
	for _, resumed := range []bool{false, true} {
		t.Run(map[bool]string{false: "inline", true: "resume"}[resumed], func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name       string
				updated    json.RawMessage
				wantRan    string
				wantError  string
				wantGuards int
				blocked    bool
			}{
				{name: "deny rule", updated: json.RawMessage(`{"command":"rm -rf build"}`), wantError: "denied by the permission rule", wantGuards: 1},
				{name: "guard trip", updated: json.RawMessage(`{"command":"guarded"}`), blocked: true, wantGuards: 2},
				{name: "approved ask rule", updated: json.RawMessage(`{"command":"git push origin main"}`), wantRan: "git push origin main", wantGuards: 2},
				{name: "trusted user edit", updated: json.RawMessage(`{"command":"npm run edited"}`), wantRan: "npm run edited", wantGuards: 2},
				{name: "unchanged approval", wantRan: "npm run checked", wantGuards: 1},
				{name: "schema still checked", updated: json.RawMessage(`{"command":42}`), wantError: "command", wantGuards: 2},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					type input struct {
						Command string `json:"command"`
					}
					var ran []string
					bash := tool.Func("bash", "run", func(_ context.Context, in input) (string, error) {
						ran = append(ran, in.Command)
						return in.Command, nil
					}, tool.RequireApproval())
					policy := permission.MustNew(permission.Config{Dir: t.TempDir(), Rules: []permission.Rules{{
						Deny: []string{"Bash(rm *)"}, Ask: []string{"Bash(git push *)"},
					}}})
					hooks := hook.NewManager()
					var hookCalls, guards, asked int
					if err := hooks.OnFunc(hook.PreToolUse, "bash", func(context.Context, *hook.Event) (*hook.Result, error) {
						hookCalls++
						return &hook.Result{UpdatedInput: json.RawMessage(`{"command":"npm run checked"}`)}, nil
					}); err != nil {
						t.Fatal(err)
					}
					_, guardMW := guard.Middleware(guard.Config{Tool: []guard.ToolGuardrail{{Name: "command guard",
						Before: func(_ context.Context, _ *loop.Run, call *model.ToolCall) (guard.Result, error) {
							guards++
							return guard.Result{Tripwire: strings.Contains(string(call.Arguments), "guarded")}, nil
						},
					}}})
					m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "call", Name: "bash", Args: input{Command: "npm test"}}), modeltest.Reply("done"))
					approval := loop.Approval{Approved: true, UpdatedInput: tc.updated}
					cfg := loop.Config{Model: m, Tools: toolRegistry(t, bash), Middlewares: []loop.Middleware{hook.Middleware(hooks), policy, guardMW}}
					if !resumed {
						cfg.Approver = loop.ApproverFunc(func(_ context.Context, req *loop.ApprovalRequest) (loop.Approval, error) {
							asked++
							if asked > 1 || string(req.Call.Arguments) != `{"command":"npm run checked"}` {
								t.Errorf("unexpected approval request: %+v", req)
								return loop.Deny, nil
							}
							return approval, nil
						})
					}
					r := newRuntime(t, cfg)
					res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("go")}), nil)
					if resumed {
						if err != nil || res.Status != loop.StatusInterrupted || len(ran) != 0 {
							t.Fatalf("interrupted run = %+v, %v; ran %q", res, err, ran)
						}
						res, err = loop.Collect(r.ResumeState(t.Context(), res.Interrupt.State, map[string]loop.Approval{"call": approval}), nil)
					}
					wantStatus := loop.StatusCompleted
					if tc.blocked {
						wantStatus = loop.StatusBlocked
					}
					if err != nil || res.Status != wantStatus {
						t.Fatalf("run = %+v, %v; ran %q", res, err, ran)
					}
					if tc.wantRan == "" {
						if len(ran) != 0 {
							t.Errorf("unauthorized arguments executed: %q", ran)
						}
					} else if len(ran) != 1 || ran[0] != tc.wantRan {
						t.Errorf("ran %q, want %q", ran, tc.wantRan)
					}
					if hookCalls != 1 || guards != tc.wantGuards || (!resumed && asked != 1) {
						t.Errorf("hooks = %d, guards = %d, asked = %d", hookCalls, guards, asked)
					}
					if tc.wantError != "" {
						result := m.Requests()[1].Messages[2].ToolResults()[0]
						if !result.IsError || !strings.Contains(result.Content, tc.wantError) {
							t.Errorf("tool result = %+v, want %q", result, tc.wantError)
						}
					}
				})
			}
		})
	}
}

func TestRuntimeApprovalRevalidator(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		original  string
		updated   string
		check     func(*loop.ToolPlan) error
		wantError string
	}{
		{
			name: "large integers stay distinct", original: `{"text":"safe","id":9007199254740992}`, updated: `{"text":"safe","id":9007199254740993}`,
			check: func(plan *loop.ToolPlan) error {
				if strings.Contains(string(plan.Call.Arguments), "9007199254740993") {
					plan.Deny("restricted id")
				}
				return nil
			},
			wantError: "restricted id",
		},
		{
			name: "revalidator cannot rewrite", original: `{"text":"original"}`, updated: `{"text":"approved"}`,
			check: func(plan *loop.ToolPlan) error {
				plan.Call.Arguments[9] = 'X'
				return nil
			},
			wantError: "revalidator changed the approved call",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var ran bool
			echo := tool.Func("echo", "echo", func(context.Context, echoArgs) (string, error) {
				ran = true
				return "ran", nil
			}, tool.RequireApproval())
			mw := &loop.Funcs{MiddlewareName: "check", RevalidateToolFunc: func(_ context.Context, _ *loop.Run, plan *loop.ToolPlan) error {
				approval, ok := plan.Approval()
				if !ok || !approval.Approved {
					t.Error("revalidator did not receive approval")
				}
				return tc.check(plan)
			}}
			m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "call", Name: "echo", Args: json.RawMessage(tc.original)}), modeltest.Reply("done"))
			r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, echo), Middlewares: []loop.Middleware{mw},
				Approver: loop.ApproverFunc(func(context.Context, *loop.ApprovalRequest) (loop.Approval, error) {
					return loop.Approval{Approved: true, UpdatedInput: json.RawMessage(tc.updated)}, nil
				})})
			res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("go")}), nil)
			if err != nil || res.Status != loop.StatusCompleted || ran {
				t.Fatalf("run = %+v, %v; ran %v", res, err, ran)
			}
			result := m.Requests()[1].Messages[2].ToolResults()[0]
			if !result.IsError || !strings.Contains(result.Content, tc.wantError) {
				t.Errorf("tool result = %+v, want %q", result, tc.wantError)
			}
		})
	}
}

func TestRuntimeApprovalRevalidationFailureClosesCall(t *testing.T) {
	t.Parallel()
	checkErr := errors.New("policy unavailable")
	var checks, ran int
	echo := tool.Func("echo", "echo", func(context.Context, echoArgs) (string, error) {
		ran++
		return "ran", nil
	}, tool.RequireApproval())
	mw := &loop.Funcs{MiddlewareName: "check", RevalidateToolFunc: func(context.Context, *loop.Run, *loop.ToolPlan) error {
		checks++
		return checkErr
	}}
	cp := loop.NewMemoryCheckpointer()
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "call", Name: "echo", Args: echoArgs{Text: "original"}}), modeltest.Reply("done"))
	r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, echo), Middlewares: []loop.Middleware{mw}, Checkpointer: cp,
		Approver: loop.ApproverFunc(func(context.Context, *loop.ApprovalRequest) (loop.Approval, error) {
			return loop.Approval{Approved: true, UpdatedInput: json.RawMessage(`{"text":"edited"}`)}, nil
		})})
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{RunID: "revalidate", Input: model.UserText("go")}), nil)
	if !errors.Is(err, checkErr) || res.Status != loop.StatusFailed || ran != 0 {
		t.Fatalf("failed run = %+v, %v; ran %d", res, err, ran)
	}
	res, err = loop.Collect(r.Resume(t.Context(), "revalidate", nil), nil)
	if err != nil || res.Status != loop.StatusCompleted || ran != 0 || checks != 1 {
		t.Fatalf("resume = %+v, %v; ran %d, checks %d", res, err, ran, checks)
	}
	result := m.Requests()[1].Messages[2].ToolResults()[0]
	if !result.IsError || !strings.Contains(result.Content, "run failed before this tool call ran") {
		t.Errorf("tool result = %+v", result)
	}
}

func TestRuntimeResumeStateDoesNotMutateCaller(t *testing.T) {
	t.Parallel()
	st := &loop.State{Version: loop.StateVersion, RunID: "resumed", Status: loop.StatusInterrupted, Messages: []*model.Message{model.UserText("question"), model.NewMessage(model.RoleAssistant, model.Part{Type: model.PartToolCall, ToolCall: &model.ToolCall{ID: "call", Name: "echo", Arguments: json.RawMessage(`{"text":"original"}`)}})}, Pending: []*loop.PendingCall{{Call: &model.ToolCall{ID: "call", Name: "echo", Arguments: json.RawMessage(`{"text":"original"}`)}, NeedsApproval: true}}}
	echo := tool.Func("echo", "echo", func(_ context.Context, in echoArgs) (string, error) { return in.Text, nil })
	r := newRuntime(t, loop.Config{Model: modeltest.New(modeltest.Reply("done")), Tools: toolRegistry(t, echo)})
	res, err := loop.Collect(r.ResumeState(t.Context(), st, map[string]loop.Approval{"call": loop.Approve}), nil)
	if err != nil || res.Output != "done" || st.Status != loop.StatusInterrupted || len(st.Messages) != 2 || !st.Pending[0].NeedsApproval {
		t.Fatalf("ResumeState = %+v, %v; original = %+v", res, err, st)
	}
}

func TestRuntimeSynchronousApprover(t *testing.T) {
	t.Parallel()
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "approve", true: "failure becomes denial"}[fail], func(t *testing.T) {
			t.Parallel()
			var executed bool
			echo := tool.Func("echo", "echo", func(context.Context, struct{}) (string, error) { executed = true; return "ok", nil }, tool.RequireApproval())
			m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "call", Name: "echo"}), modeltest.Reply("done"))
			r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, echo), Approver: loop.ApproverFunc(func(_ context.Context, req *loop.ApprovalRequest) (loop.Approval, error) {
				if req.Call.ID != "call" {
					t.Errorf("approval request = %+v", req)
				}
				if fail {
					return loop.Approval{}, errors.New("approval unavailable")
				}
				return loop.Approve, nil
			})})
			res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("question")}), nil)
			if err != nil || res.Status != loop.StatusCompleted || executed == fail {
				t.Fatalf("Run = %+v, %v; executed = %v", res, err, executed)
			}
			if result := m.Requests()[1].Messages[2].ToolResults()[0]; result.IsError != fail || (fail && !strings.Contains(result.Content, "Approval failed")) {
				t.Fatalf("tool result = %+v", result)
			}
		})
	}
}

func TestRuntimeToolHooks(t *testing.T) {
	t.Parallel()
	for _, decision := range []hook.Decision{hook.Allow, hook.Deny, hook.Ask} {
		t.Run(string(decision), func(t *testing.T) {
			t.Parallel()
			hooks := hook.NewManager()
			if err := hooks.OnFunc(hook.PreToolUse, "echo", func(context.Context, *hook.Event) (*hook.Result, error) {
				return &hook.Result{Decision: decision, Reason: "policy", UpdatedInput: json.RawMessage(`{"text":"hook input"}`), AdditionalContext: "pre context"}, nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := hooks.OnFunc(hook.PostToolUse, "echo", func(_ context.Context, ev *hook.Event) (*hook.Result, error) {
				// The tool ran with the hook's arguments, and the result tells
				// the model so because its own call shows the original ones.
				if decision == hook.Allow && (!strings.HasSuffix(ev.ToolResult.Content, "]\nhook input") || !strings.Contains(ev.ToolResult.Content, `changed before execution: {"text":"hook input"}`)) {
					t.Errorf("tool received wrong arguments: %+v", ev.ToolResult)
				}
				return &hook.Result{UpdatedOutput: new("rewritten"), AdditionalContext: "post context", Decision: hook.Deny, Reason: "post reason"}, nil
			}); err != nil {
				t.Fatal(err)
			}
			calls := 0
			echo := tool.Func("echo", "echo", func(_ context.Context, in echoArgs) (string, error) { calls++; return in.Text, nil }, tool.RequireApproval())
			m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "call", Name: "echo", Args: echoArgs{Text: "original"}}), modeltest.Reply("done"))
			r := newRuntime(t, loop.Config{Model: m, Middlewares: []loop.Middleware{hook.Middleware(hooks)}, Tools: toolRegistry(t, echo)})
			res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("question")}), nil)
			if err != nil {
				t.Fatal(err)
			}
			if decision == hook.Ask {
				if res.Status != loop.StatusInterrupted || calls != 0 || res.Interrupt.Requests[0].Reason != "policy" {
					t.Fatalf("Ask = %+v; calls = %d", res, calls)
				}
				return
			}
			if res.Status != loop.StatusCompleted || calls != map[hook.Decision]int{hook.Allow: 1, hook.Deny: 0}[decision] {
				t.Fatalf("Run = %+v; calls = %d", res, calls)
			}
			result := m.Requests()[1].Messages[2].ToolResults()[0]
			want := []string{"rewritten", "pre context", "post context", "post reason"}
			if decision == hook.Deny {
				// Denied calls do not run, so no PostToolUse hook fires.
				want = []string{"policy", "pre context"}
				if strings.Contains(result.Content, "post") {
					t.Errorf("denied result %q has PostToolUse output", result.Content)
				}
			}
			for _, want := range want {
				if !strings.Contains(result.Content, want) {
					t.Errorf("result %q missing %q", result.Content, want)
				}
			}
		})
	}
}

func TestRuntimeToolErrorsAreVisibleToModel(t *testing.T) {
	t.Parallel()
	failing := tool.Func("failing", "fail", func(context.Context, struct{}) (string, error) { return "", errors.New("tool exploded") })
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "unknown", Name: "unknown"}, modeltest.Call{ID: "failure", Name: "failing"}), modeltest.Reply("recovered"))
	r := newRuntime(t, loop.Config{Model: m, Tools: toolRegistry(t, failing)})
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{Input: model.UserText("question")}), nil)
	if err != nil || res.Status != loop.StatusCompleted || res.Output != "recovered" {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	results := m.Requests()[1].Messages[2].ToolResults()
	if len(results) != 2 || !results[0].IsError || !results[1].IsError || !strings.Contains(results[1].Content, "tool exploded") {
		t.Fatalf("tool errors = %+v", results)
	}
}

// argApprover asks for approval only for calls whose text is "risky".
type argApprover struct{ tool.Tool }

func (a argApprover) RequiresApprovalFor(args json.RawMessage) bool {
	var in echoArgs
	return json.Unmarshal(args, &in) != nil || in.Text == "risky"
}

func TestRuntimeCallApprover(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		text    string
		rewrite bool
		asked   bool
	}{
		{"safe call", "safe", false, false},
		{"risky call", "risky", false, true},
		{"rewritten call", "safe", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			echo := argApprover{tool.Func("echo", "echo", func(_ context.Context, in echoArgs) (string, error) { return in.Text, nil }, tool.RequireApproval())}
			m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "call", Name: "echo", Args: echoArgs{Text: c.text}}), modeltest.Reply("done"))
			var asked bool
			cfg := loop.Config{Model: m, Tools: toolRegistry(t, echo), Approver: loop.ApproverFunc(func(context.Context, *loop.ApprovalRequest) (loop.Approval, error) {
				asked = true
				return loop.Approve, nil
			})}
			if c.rewrite {
				cfg.Middlewares = []loop.Middleware{&loop.Funcs{MiddlewareName: "rewrite", BeforeToolFunc: func(_ context.Context, _ *loop.Run, p *loop.ToolPlan) error {
					if !p.Info.RequiresApproval == (c.text == "risky") {
						t.Errorf("plan Info.RequiresApproval = %v", p.Info.RequiresApproval)
					}
					p.Call.Arguments = json.RawMessage(`{"text":"risky"}`)
					return nil
				}}}
			}
			res, err := loop.Collect(newRuntime(t, cfg).Run(t.Context(), &loop.Request{Input: model.UserText("question")}), nil)
			if err != nil || res.Status != loop.StatusCompleted {
				t.Fatalf("Run = %+v, %v", res, err)
			}
			if asked != c.asked {
				t.Fatalf("asked = %v, want %v", asked, c.asked)
			}
		})
	}
}
