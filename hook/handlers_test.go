package hook_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nostosx/agentx/actx"
	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/tool"
)

func TestHTTPHook(t *testing.T) {
	t.Setenv("HOOK_TOKEN", "secret")
	t.Setenv("HOOK_HIDDEN", "hidden")
	var (
		mu     sync.Mutex
		bodies []map[string]any
		auth   []string
	)
	status, reply := http.StatusOK, ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var body map[string]any
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			http.Error(w, "read event", http.StatusBadRequest)
			return
		}
		if err := json.Unmarshal(data, &body); err != nil {
			t.Error(err)
			http.Error(w, "decode event", http.StatusBadRequest)
			return
		}
		bodies = append(bodies, body)
		auth = append(auth, r.Header.Get("Authorization")+"|"+r.Header.Get("X-Other")+"|"+r.Header.Get("Content-Type")+"|"+r.Method)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	defer srv.Close()
	h := &hook.HTTP{
		URL:            srv.URL,
		Headers:        map[string]string{"Authorization": "Bearer $HOOK_TOKEN", "X-Other": "${HOOK_HIDDEN}x"},
		AllowedEnvVars: []string{"HOOK_TOKEN"},
	}
	ev := &hook.Event{Type: hook.PreToolUse, SessionID: "s", ToolCall: &model.ToolCall{ID: "c", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)}}

	for _, tt := range []struct {
		name    string
		status  int
		reply   string
		want    hook.Decision
		wantErr bool
	}{
		{"empty", 200, "", hook.Continue, false},
		{"json", 200, `{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"no"}}`, hook.Deny, false},
		{"plain text", 200, "deny", hook.Continue, true},
		{"bad json", 200, `{"decision":`, hook.Continue, true},
		{"non-2xx", 500, `{"decision":"block"}`, hook.Continue, true},
	} {
		mu.Lock()
		status, reply = tt.status, tt.reply
		mu.Unlock()
		res, err := h.Handle(t.Context(), ev)
		if (err != nil) != tt.wantErr {
			t.Fatalf("%s: err = %v", tt.name, err)
		}
		var got hook.Decision
		if res != nil {
			got = res.Decision
		}
		if got != tt.want {
			t.Fatalf("%s: decision = %q", tt.name, got)
		}
	}
	mu.Lock()
	if auth[0] != "Bearer secret|x|application/json|POST" {
		t.Errorf("headers = %q", auth[0])
	}
	if bodies[0]["hook_event_name"] != "PreToolUse" || bodies[0]["tool_name"] != "bash" || bodies[0]["session_id"] != "s" {
		t.Errorf("body = %v", bodies[0])
	}
	mu.Unlock()

	// The handler must receive the request and observe its cancellation.
	t.Run("timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			slow := httptest.NewTestServer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Error(err)
					return
				}
				close(started)
				select {
				case <-r.Context().Done():
					close(canceled)
				case <-release:
				}
			}))
			client := slow.Client()
			defer close(release)
			start := time.Now()
			if _, err := (&hook.HTTP{URL: slow.URL, Client: client, Timeout: 20 * time.Millisecond}).Handle(t.Context(), ev); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("timeout = %v", err)
			}
			if elapsed := time.Since(start); elapsed != 20*time.Millisecond {
				t.Fatalf("timeout took %v", elapsed)
			}
			synctest.Wait()
			select {
			case <-started:
			default:
				t.Fatal("request did not reach the handler")
			}
			select {
			case <-canceled:
			default:
				t.Fatal("handler did not observe cancellation")
			}
		})
	})
	// A closed fixture rejects connections without relying on a fixed port.
	srv.Close()
	if _, err := (&hook.HTTP{URL: srv.URL}).Handle(t.Context(), ev); err == nil {
		t.Fatal("connection failure not reported")
	}
}

func TestPromptHook(t *testing.T) {
	t.Parallel()
	ev := func(t hook.EventType) *hook.Event {
		return &hook.Event{Type: t, Message: "all done", ToolCall: &model.ToolCall{Name: "bash", Arguments: json.RawMessage(`{"command":"rm"}`)}}
	}
	for _, tt := range []struct {
		name            string
		event           hook.EventType
		answer          string
		continueOnBlock bool
		want            hook.Result
		nilResult       bool
		wantErr         bool
	}{
		{"ok", hook.PreToolUse, `{"ok": true}`, false, hook.Result{}, true, false},
		{"stop blocks", hook.Stop, "```json\n{\"ok\": false, \"reason\": \"tests missing\"}\n```", false, hook.Result{Decision: hook.Deny, Reason: "tests missing"}, false, false},
		{"stop impossible", hook.Stop, `{"ok": false, "reason": "x", "impossible": true}`, false, hook.Result{}, true, false},
		{"pre tool ends turn", hook.PreToolUse, `{"ok": false, "reason": "unsafe"}`, false, hook.Result{Decision: hook.Deny, Reason: "unsafe", Halt: true, HaltReason: "unsafe", SystemMessage: "unsafe"}, false, false},
		{"pre tool continues", hook.PreToolUse, `{"ok": false, "reason": "unsafe"}`, true, hook.Result{Decision: hook.Deny, Reason: "unsafe"}, false, false},
		{"post tool ends turn", hook.PostToolUse, `{"ok": false, "reason": "bad"}`, false, hook.Result{Halt: true, HaltReason: "bad", SystemMessage: "bad"}, false, false},
		{"post tool continues", hook.PostToolUse, `{"ok": false, "reason": "bad"}`, true, hook.Result{Decision: hook.Deny, Reason: "bad"}, false, false},
		{"prompt blocked", hook.UserPromptSubmit, `{"ok": false}`, false, hook.Result{Decision: hook.Deny, Reason: "blocked by hook"}, false, false},
		{"not json", hook.Stop, `sure`, false, hook.Result{}, true, true},
		{"no ok", hook.Stop, `{"reason": "x"}`, false, hook.Result{}, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := modeltest.New(modeltest.Reply(tt.answer))
			p := &hook.Prompt{Model: m, Prompt: `Price \$1: check $ARGUMENTS now`, ModelName: "judge", ContinueOnBlock: tt.continueOnBlock}
			res, err := p.Handle(t.Context(), ev(tt.event))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			if tt.nilResult {
				if res != nil {
					t.Fatalf("result = %+v", res)
				}
			} else if res == nil || !reflect.DeepEqual(*res, tt.want) {
				t.Fatalf("result = %+v, want %+v", res, tt.want)
			}
			req := m.Requests()[0]
			text := req.Messages[0].Text()
			if req.Model != "judge" || !strings.HasPrefix(text, "Price $1: check {") || !strings.HasSuffix(text, "} now") || !strings.Contains(text, `"hook_event_name":"`+string(tt.event)+`"`) || req.System == "" {
				t.Fatalf("request = %+v", req)
			}
		})
	}
	// Without $ARGUMENTS the input is appended.
	m := modeltest.New(modeltest.Reply(`{"ok": true}`))
	if _, err := (&hook.Prompt{Model: m, Prompt: "Is it safe?"}).Handle(t.Context(), ev(hook.PreToolUse)); err != nil {
		t.Fatal(err)
	}
	if text := m.Requests()[0].Messages[0].Text(); !strings.HasPrefix(text, "Is it safe?\n\n{") {
		t.Fatalf("prompt = %q", text)
	}
	if _, err := (&hook.Prompt{}).Handle(t.Context(), ev(hook.Stop)); err == nil {
		t.Fatal("prompt hook without a model ran")
	}
}

func TestAgentHook(t *testing.T) {
	t.Parallel()
	var got string
	v := hook.VerifierFunc(func(_ context.Context, model, prompt string) (string, error) {
		got = model + "|" + prompt
		return `Checked. {"ok": false, "reason": "tests fail", "impossible": true}`, nil
	})
	a := &hook.Agent{Verifier: v, Prompt: "Verify $ARGUMENTS", Model: "m"}
	// An agent hook's block on PreToolUse continues, and impossible is
	// ignored.
	res, err := a.Handle(t.Context(), &hook.Event{Type: hook.Stop})
	if err != nil || res.Decision != hook.Deny || res.Reason != "tests fail" || res.Halt {
		t.Fatalf("Stop = %+v, %v", res, err)
	}
	res, err = a.Handle(t.Context(), &hook.Event{Type: hook.PreToolUse, ToolCall: &model.ToolCall{Name: "x"}})
	if err != nil || res.Decision != hook.Deny || res.Halt {
		t.Fatalf("PreToolUse = %+v, %v", res, err)
	}
	if !strings.HasPrefix(got, "m|Verify {") || !strings.Contains(got, `{"ok": true}`) {
		t.Fatalf("verifier prompt = %q", got)
	}
	failing := &hook.Agent{Verifier: hook.VerifierFunc(func(context.Context, string, string) (string, error) { return "", errors.New("down") })}
	if _, err := failing.Handle(t.Context(), &hook.Event{Type: hook.Stop}); err == nil {
		t.Fatal("verifier error not reported")
	}
	if _, err := (&hook.Agent{}).Handle(t.Context(), &hook.Event{Type: hook.Stop}); err == nil {
		t.Fatal("agent hook without a verifier ran")
	}
}

func TestMCPToolHook(t *testing.T) {
	t.Parallel()
	var calls []string
	caller := hook.MCPCallerFunc(func(_ context.Context, server, tool string, args json.RawMessage) (string, bool, error) {
		calls = append(calls, server+"/"+tool+" "+string(args))
		switch tool {
		case "fail":
			return "broken", true, nil
		case "context":
			return "remember the style guide", false, nil
		}
		return `{"decision":"block","reason":"protected file"}`, false, nil
	})
	ev := &hook.Event{Type: hook.PreToolUse, SessionID: "s", ToolCall: &model.ToolCall{Name: "write_file", Arguments: json.RawMessage(`{"file_path":"/etc/passwd","n":3}`)}}
	h := &hook.MCPTool{Caller: caller, Server: "guard", Tool: "check", Input: map[string]any{
		"path":   "${tool_input.file_path}",
		"nested": []any{"${tool_input.n}", map[string]any{"s": "session ${session_id}${missing.x}"}},
		"num":    1,
	}}
	res, err := h.Handle(t.Context(), ev)
	if err != nil || res.Decision != hook.Deny || res.Reason != "protected file" {
		t.Fatalf("Handle = %+v, %v", res, err)
	}
	if calls[0] != `guard/check {"nested":["3",{"s":"session s"}],"num":1,"path":"/etc/passwd"}` {
		t.Fatalf("call = %s", calls[0])
	}
	if _, err := (&hook.MCPTool{Caller: caller, Server: "guard", Tool: "fail"}).Handle(t.Context(), ev); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("isError = %v", err)
	}
	res, err = (&hook.MCPTool{Caller: caller, Server: "s", Tool: "context"}).Handle(t.Context(), &hook.Event{Type: hook.SessionStart})
	if err != nil || res.AdditionalContext != "remember the style guide" || calls[2] != "s/context {}" {
		t.Fatalf("plain text = %+v, %v (%v)", res, err, calls)
	}
	if _, err := (&hook.MCPTool{}).Handle(t.Context(), ev); err == nil {
		t.Fatal("mcp_tool hook without a caller ran")
	}
}

func TestAsyncHookDeliversContext(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	results := make(chan hook.AsyncResult, 4)
	m := hook.NewManager(hook.OnAsyncResult(func(r hook.AsyncResult) { results <- r }))
	if err := m.On(hook.PostToolUse, "", &hook.Async{Hook: hook.Func(func(context.Context, *hook.Event) (*hook.Result, error) {
		<-release
		return &hook.Result{Decision: hook.Deny, AdditionalContext: "tests passed", SystemMessage: "note", Halt: true}, nil
	})}); err != nil {
		t.Fatal(err)
	}
	ok := tool.Func("ok", "Succeed.", func(context.Context, struct{}) (string, error) { return "fine", nil })
	mdl := modeltest.New(
		modeltest.CallTools(modeltest.Call{ID: "a", Name: "ok"}),
		modeltest.CallTools(modeltest.Call{ID: "b", Name: "ok"}),
		modeltest.Reply("done"),
	)
	r := newHookRuntime(t, m, mdl, ok)
	// The first tool result is not held up or changed by the async hook;
	// its result arrives with a later tool result.
	var once sync.Once
	res, err := loop.Collect(r.Run(t.Context(), &loop.Request{SessionID: "s", Input: model.UserText("go")}), func(ev *loop.Event) {
		if ev.Type == loop.EventToolResult {
			once.Do(func() {
				close(release)
				<-results // the first call's hook finished
			})
		}
	})
	if err != nil || res.Status != loop.StatusCompleted {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	reqs := mdl.Requests()
	first := reqs[1].Messages[2].ToolResults()[0].Content
	second := reqs[2].Messages[4].ToolResults()[0].Content
	if first != "fine" || !strings.Contains(second, "tests passed\nnote") {
		t.Fatalf("tool results = %q, %q", first, second)
	}
}

func TestAsyncHookRewakeAndClose(t *testing.T) {
	t.Parallel()
	results := make(chan hook.AsyncResult, 4)
	m := hook.NewManager(hook.OnAsyncResult(func(r hook.AsyncResult) { results <- r }))
	if err := m.On(hook.Stop, "", &hook.Async{Rewake: true, Hook: hook.Func(func(context.Context, *hook.Event) (*hook.Result, error) {
		return &hook.Result{Decision: hook.Deny, Reason: "build broke"}, nil
	})}); err != nil {
		t.Fatal(err)
	}
	res, err := m.Dispatch(t.Context(), &hook.Event{Type: hook.Stop, SessionID: "s"})
	if err != nil || res.Decision != hook.Continue {
		t.Fatalf("Dispatch = %+v, %v", res, err)
	}
	r := <-results
	if !r.Rewake || r.Context != "build broke" || r.SessionID != "s" || r.Event != hook.Stop {
		t.Fatalf("result = %+v", r)
	}
	// The queued context reaches the next run's input.
	mdl := modeltest.New(modeltest.Reply("fixing"))
	if _, err := loop.Collect(newHookRuntime(t, m, mdl).Run(t.Context(), &loop.Request{SessionID: "s", Input: model.UserText("wake up")}), nil); err != nil {
		t.Fatal(err)
	}
	if in := mdl.Requests()[0].Messages[0].Text(); !strings.Contains(in, "build broke") {
		t.Fatalf("input = %q", in)
	}

	// Close cancels hooks still running.
	closed := make(chan hook.AsyncResult, 1)
	blocked := hook.NewManager(hook.OnAsyncResult(func(r hook.AsyncResult) { closed <- r }))
	if err := blocked.On(hook.Stop, "", &hook.Async{Hook: hook.Func(func(ctx context.Context, _ *hook.Event) (*hook.Result, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	if _, err := blocked.Dispatch(ctx, &hook.Event{Type: hook.Stop}); err != nil {
		t.Fatal(err)
	}
	cancel() // the action's context does not cancel the hook
	short, stop := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer stop()
	if err := blocked.Wait(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait = %v", err)
	}
	blocked.Close()
	if r := <-closed; !errors.Is(r.Err, context.Canceled) {
		t.Fatalf("closed result = %+v", r)
	}
	if err := blocked.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestSessionEndBudget(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		slow := hook.Func(func(ctx context.Context, _ *hook.Event) (*hook.Result, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
		m := hook.NewManager()
		on(t, m, hook.SessionEnd, "logout", slow)
		start := time.Now()
		if _, err := m.Dispatch(t.Context(), &hook.Event{Type: hook.SessionEnd, Reason: "logout"}); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Dispatch = %v", err)
		}
		if d := time.Since(start); d != 1500*time.Millisecond {
			t.Fatalf("default budget took %v", d)
		}
		// A configured budget applies, and the matcher filters on the reason.
		m = hook.NewManager(hook.SessionEndTimeout(10 * time.Millisecond))
		on(t, m, hook.SessionEnd, "clear", slow)
		if _, err := m.Dispatch(t.Context(), &hook.Event{Type: hook.SessionEnd, Reason: "logout"}); err != nil {
			t.Fatal(err)
		}
		start = time.Now()
		if _, err := m.Dispatch(t.Context(), &hook.Event{Type: hook.SessionEnd, Reason: "clear"}); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Dispatch = %v", err)
		}
		if d := time.Since(start); d != 10*time.Millisecond {
			t.Fatalf("configured budget took %v", d)
		}
	})
}

func TestMiddlewareStopBlockCapOption(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		cap, calls int
	}{{2, 3}, {0, 13}} { // without a cap, until the scripted model runs out
		m := hook.NewManager(hook.StopBlockCap(tt.cap))
		on(t, m, hook.Stop, "", func(context.Context, *hook.Event) (*hook.Result, error) {
			return &hook.Result{Decision: hook.Deny, Reason: "again"}, nil
		})
		steps := make([]modeltest.Step, 12)
		for i := range steps {
			steps[i] = modeltest.Reply("answer")
		}
		mdl := modeltest.New(steps...)
		_, _ = loop.Collect(newHookRuntime(t, m, mdl).Run(t.Context(), &loop.Request{Input: model.UserText("go")}), nil)
		if len(mdl.Requests()) != tt.calls {
			t.Errorf("cap %d: model calls = %d, want %d", tt.cap, len(mdl.Requests()), tt.calls)
		}
	}
}

func TestPostCompactObserverFiresSessionStartCompact(t *testing.T) {
	t.Parallel()
	m := hook.NewManager()
	var sources []string
	on(t, m, hook.SessionStart, "compact", func(_ context.Context, ev *hook.Event) (*hook.Result, error) {
		sources = append(sources, ev.Reason)
		return &hook.Result{AdditionalContext: "re-read the plan"}, nil
	})
	manager := actx.NewManager(actx.Config{Window: actx.Window{Limit: 100, Threshold: 0.5}, Strategy: actx.StrategyFunc(func(_ context.Context, in *actx.CompactInput) (*actx.CompactOutput, error) {
		return &actx.CompactOutput{Messages: in.Messages[len(in.Messages)-1:], Summary: "compacted"}, nil
	})})
	mdl := modeltest.New(modeltest.Reply("answer"))
	r, err := loop.New(loop.Config{Model: mdl, Middlewares: []loop.Middleware{hook.Middleware(m), actx.Middleware(manager, actx.WithCompactObserver(hook.PostCompactObserver(m)))}})
	if err != nil {
		t.Fatal(err)
	}
	history := []*model.Message{model.UserText(strings.Repeat("old ", 100)), model.AssistantText("old answer")}
	input := model.UserText("new question")
	if _, err := loop.Collect(r.Run(t.Context(), &loop.Request{History: history, Input: input}), nil); err != nil {
		t.Fatal(err)
	}
	msgs := mdl.Requests()[0].Messages
	if len(sources) != 1 || len(msgs) != 1 || !strings.Contains(msgs[0].Text(), "re-read the plan") || !strings.Contains(msgs[0].Text(), "new question") {
		t.Fatalf("sources = %v; request = %+v", sources, msgs)
	}
	if strings.Contains(input.Text(), "re-read") {
		t.Fatal("the input message was modified")
	}
}

func TestSettingsOptionsHandlerTypes(t *testing.T) {
	t.Setenv("CLAUDE_CODE_STOP_HOOK_BLOCK_CAP", "3")
	t.Setenv("CLAUDE_CODE_SESSIONEND_HOOKS_TIMEOUT_MS", "40")
	caller := hook.MCPCallerFunc(func(context.Context, string, string, json.RawMessage) (string, bool, error) {
		return `{"decision":"block","reason":"from mcp"}`, false, nil
	})
	verifier := hook.VerifierFunc(func(context.Context, string, string) (string, error) { return `{"ok": true}`, nil })
	data := []byte(`{"hooks": {
		"PreToolUse": [{"matcher": "Bash", "hooks": [
			{"type": "mcp_tool", "server": "guard", "tool": "check", "input": {"cmd": "${tool_input.command}"}},
			{"type": "prompt", "prompt": "Safe? $ARGUMENTS", "model": "small", "timeout": 5, "continueOnBlock": true},
			{"type": "agent", "prompt": "Verify"},
			{"type": "http", "url": "http://127.0.0.1:1/x", "async": true}
		]}],
		"Stop": [{"hooks": [{"type": "command", "command": "./bg.sh", "asyncRewake": true, "timeout": 2}]}],
		"SessionStart": [{"hooks": [{"type": "http", "url": "http://x"}, {"type": "prompt", "prompt": "x"}]}],
		"SessionEnd": [{"matcher": "logout", "hooks": [{"type": "command", "command": "./end.sh"}]}],
		"PostCompact": [{"hooks": [{"type": "prompt", "prompt": "x"}]}]
	}}`)
	m, warnings, err := hook.SettingsOptions{Model: modeltest.New(modeltest.Reply(`{"ok": true}`)), Verifier: verifier, MCP: caller}.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`hooks.PostCompact[0].hooks[0]: PostCompact does not support prompt hooks`,
		`hooks.PreToolUse[0].hooks[3]: async is only supported for command hooks`,
		`hooks.SessionStart[0].hooks[0]: SessionStart does not support http hooks`,
		`hooks.SessionStart[0].hooks[1]: SessionStart does not support prompt hooks`,
	}
	if len(warnings) != len(want) {
		t.Fatalf("warnings = %q", warnings)
	}
	for i, w := range want {
		if !strings.HasPrefix(warnings[i], w) {
			t.Errorf("warning %d = %q, want %q", i, warnings[i], w)
		}
	}
	if !m.Has(hook.SessionEnd) || !m.Has(hook.Stop) || m.MaxStopBlocks() != 3 {
		t.Fatalf("manager: SessionEnd %v, Stop %v, cap %d", m.Has(hook.SessionEnd), m.Has(hook.Stop), m.MaxStopBlocks())
	}
	res, err := m.Dispatch(t.Context(), &hook.Event{Type: hook.PreToolUse, ToolCall: &model.ToolCall{Name: "Bash", Arguments: json.RawMessage(`{"command":"ls"}`)}})
	if err != nil || res.Decision != hook.Deny || res.Reason != "from mcp" {
		t.Fatalf("PreToolUse = %+v, %v", res, err)
	}
	// Without options, those handler types are skipped with warnings.
	_, warnings, err = hook.ParseSettings(data)
	if err != nil || len(warnings) != 7 {
		t.Fatalf("warnings without options = %q, %v", warnings, err)
	}
}
