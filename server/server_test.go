package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/tool"
)

func TestRunStreamsEvents(t *testing.T) {
	f := newFixture(t, []modeltest.Step{modeltest.Reply("Hello there")}, nil)
	hdr, objs, events := f.stream("/sessions/s1/runs", RunRequest{Input: "Hi"})
	if ct := hdr.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	ts := types(objs)
	want := []string{"run_start", "turn_start", "delta", "delta", "message", "run_end"}
	if strings.Join(ts, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", ts, want)
	}
	checkStream(t, "agentx", events)
	end := objs[len(objs)-1]["result"].(map[string]any)
	if end["status"] != "completed" || end["output"] != "Hello there" {
		t.Fatalf("result = %v", end)
	}
	if _, ok := end["messages"]; ok {
		t.Fatal("the result should not hold the whole conversation")
	}
	if objs[0]["session_id"] != "s1" {
		t.Fatalf("session_id = %v", objs[0]["session_id"])
	}

	info := decodeJSON[SessionInfo](t, f.do(http.MethodGet, "/sessions/s1", "", nil))
	if len(info.Messages) != 2 || info.Messages[1].Text() != "Hello there" || info.Pending != nil || info.Busy {
		t.Fatalf("session = %+v", info)
	}
}

func TestRunJSONAndJSONL(t *testing.T) {
	f := newFixture(t, []modeltest.Step{modeltest.Reply("one"), modeltest.Reply("two")}, nil)
	resp := f.do(http.MethodPost, "/sessions/s1/runs", "application/json", RunRequest{Message: model.AssistantText("role is ignored")})
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("status %s, type %s", resp.Status, resp.Header.Get("Content-Type"))
	}
	res := decodeJSON[loop.Result](t, resp)
	if res.Status != loop.StatusCompleted || res.Output != "one" || res.Messages != nil || len(res.NewMessages) != 2 {
		t.Fatalf("result = %+v", res)
	}
	if res.NewMessages[0].Role != model.RoleUser {
		t.Fatalf("input role = %s", res.NewMessages[0].Role)
	}

	resp = f.do(http.MethodPost, "/sessions/s1/runs", "application/x-ndjson", RunRequest{Input: "again"})
	if resp.Header.Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("Content-Type = %s", resp.Header.Get("Content-Type"))
	}
	sc := bufio.NewScanner(resp.Body)
	var last loop.Event
	n := 0
	for sc.Scan() {
		if err := json.Unmarshal(sc.Bytes(), &last); err != nil {
			t.Fatal(err)
		}
		n++
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if n < 3 || last.Type != loop.EventRunEnd || last.Result.Output != "two" {
		t.Fatalf("%d lines, last %+v", n, last)
	}
}

func TestRunFailureInStreamAndJSON(t *testing.T) {
	boom := errors.New("provider down")
	f := newFixture(t, []modeltest.Step{modeltest.Fail(boom), modeltest.Fail(boom)}, nil)
	_, objs, _ := f.stream("/sessions/s1/runs", RunRequest{Input: "Hi"})
	last := objs[len(objs)-1]
	if last["type"] != "error" || last["code"] != CodeRunFailed || !strings.Contains(last["message"].(string), "provider down") {
		t.Fatalf("last event = %v", last)
	}
	if objs[len(objs)-2]["type"] != "run_end" {
		t.Fatalf("the error should follow run_end: %v", types(objs))
	}
	// The failed run can be resumed or given up; a new run gives it up.
	info := decodeJSON[SessionInfo](t, f.do(http.MethodGet, "/sessions/s1", "", nil))
	if info.Pending == nil || info.Pending.Kind != "failed" {
		t.Fatalf("pending = %+v", info.Pending)
	}
	res := decodeJSON[loop.Result](t, f.do(http.MethodPost, "/sessions/s1/runs", "application/json", RunRequest{Input: "again"}))
	if res.Status != loop.StatusFailed || res.Err == nil || !strings.Contains(res.Err.Error(), "provider down") {
		t.Fatalf("result = %+v", res)
	}
}

func TestInterruptResumeAndDiscard(t *testing.T) {
	f := newFixture(t, []modeltest.Step{
		deployStep("call_1", "prod"), modeltest.Reply("Deployed."),
		deployStep("call_2", "staging"),
	}, nil)
	_, objs, _ := f.stream("/sessions/s1/runs", RunRequest{Input: "Deploy"})
	end := objs[len(objs)-1]
	if end["type"] != "run_end" || end["result"].(map[string]any)["status"] != "interrupted" {
		t.Fatalf("last event = %v", end)
	}
	if strings.Contains(mustJSON(t, objs), `"state"`) {
		t.Fatal("interrupt state leaked")
	}

	info := decodeJSON[SessionInfo](t, f.do(http.MethodGet, "/sessions/s1", "", nil))
	if info.Pending == nil || info.Pending.Kind != "interrupted" || info.Pending.Interrupt == nil || info.Pending.Interrupt.Requests[0].Call.ID != "call_1" {
		t.Fatalf("pending = %+v", info.Pending)
	}

	resp := f.do(http.MethodPost, "/sessions/s1/runs", "", RunRequest{Input: "Something else"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("run of an interrupted session: %s", resp.Status)
	}
	body := decodeJSON[errorBody](t, resp)
	if body.Error.Code != CodeRunInterrupted || body.Pending == nil || body.Pending.Interrupt == nil {
		t.Fatalf("body = %+v", body)
	}

	_, objs, _ = f.stream("/sessions/s1/resume", ResumeRequest{Approvals: map[string]loop.Approval{"call_1": loop.Approve}})
	res := objs[len(objs)-1]["result"].(map[string]any)
	if res["status"] != "completed" || res["output"] != "Deployed." || f.deploy.n.Load() != 1 {
		t.Fatalf("result = %v, deploys %d", res, f.deploy.n.Load())
	}
	if resp := f.do(http.MethodPost, "/sessions/s1/resume", "", ResumeRequest{}); resp.StatusCode != http.StatusConflict || errorCode(t, resp) != CodeNothingToResume {
		t.Fatalf("resume without a run: %s", resp.Status)
	}

	// Interrupt again and discard.
	f.stream("/sessions/s1/runs", RunRequest{Input: "Deploy to staging"})
	if resp := f.do(http.MethodPost, "/sessions/s1/discard", "", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("discard: %s", resp.Status)
	}
	if resp := f.do(http.MethodPost, "/sessions/s1/discard", "", nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("second discard: %s", resp.Status)
	}
	if resp := f.do(http.MethodPost, "/sessions/nope/discard", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("discard of a missing session: %s", resp.Status)
	}
	if f.deploy.n.Load() != 1 {
		t.Fatalf("deploys = %d", f.deploy.n.Load())
	}
}

func TestInterruptFromCheckpointAfterRestart(t *testing.T) {
	f := newFixture(t, []modeltest.Step{deployStep("call_1", "prod")}, nil)
	f.stream("/sessions/s1/runs", RunRequest{Input: "Deploy"})
	sess, err := f.runner.Store().Get(t.Context(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	// A new server does not know the interrupt, unless it has the
	// checkpointer.
	if p := New(f.runner).pendingRun(t.Context(), sess); p == nil || p.Interrupt != nil {
		t.Fatalf("pending without checkpointer = %+v", p)
	}
	p := New(f.runner, WithCheckpointer(f.cp)).pendingRun(t.Context(), sess)
	if p == nil || p.Interrupt == nil || len(p.Interrupt.Requests) != 1 || p.Interrupt.Requests[0].Call.Name != "deploy" || p.Interrupt.State != nil {
		t.Fatalf("pending with checkpointer = %+v", p)
	}
}

func TestBusySession(t *testing.T) {
	started, release := make(chan struct{}, 1), make(chan struct{})
	f := newFixture(t, nil, []agent.Option{agent.WithModel(blockingModel(started, release))})
	saveSession(t, f, "s1")
	done := make(chan string, 1)
	go func() {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, f.http.URL+"/sessions/s1/runs", strings.NewReader(`{"input":"Hi"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		resp, err := f.client.Do(req)
		if err != nil {
			done <- err.Error()
			return
		}
		defer resp.Body.Close()
		var res loop.Result
		_ = json.NewDecoder(resp.Body).Decode(&res)
		done <- res.Output
	}()
	<-started
	for _, req := range []struct{ method, path string }{
		{http.MethodPost, "/sessions/s1/runs"},
		{http.MethodPost, "/sessions/s1/resume"},
		{http.MethodPost, "/sessions/s1/discard"},
		{http.MethodDelete, "/sessions/s1"},
		{http.MethodPost, "/sessions/s1/fork"},
	} {
		var body any
		if strings.HasSuffix(req.path, "runs") {
			body = RunRequest{Input: "again"}
		}
		resp := f.do(req.method, req.path, "", body)
		if resp.StatusCode != http.StatusConflict || errorCode(t, resp) != CodeSessionBusy {
			t.Errorf("%s %s while busy: %s", req.method, req.path, resp.Status)
		}
	}
	if info := decodeJSON[SessionInfo](t, f.do(http.MethodGet, "/sessions/s1", "", nil)); !info.Busy {
		t.Error("session should be busy")
	}
	close(release)
	if out := <-done; out != "released" {
		t.Fatalf("result = %q", out)
	}
}

func TestDisconnectStopsRunResumably(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started, release := make(chan struct{}, 1), make(chan struct{})
		f := newFixture(t, nil, []agent.Option{agent.WithModel(blockingModel(started, release))})
		ctx, cancel := context.WithCancel(t.Context())
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, f.http.URL+"/sessions/s1/runs", strings.NewReader(`{"input":"Hi"}`))
		req.Header.Set("Content-Type", "application/json")
		errc := make(chan error, 1)
		go func() {
			resp, err := f.client.Do(req)
			if err == nil {
				_, err = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			errc <- err
		}()
		<-started
		cancel()
		if err := <-errc; !errors.Is(err, context.Canceled) {
			t.Fatalf("disconnected request: %v, want context.Canceled", err)
		}
		// The server stops the run, saves the session and releases it.
		synctest.Wait()
		if f.server.isBusy("s1") {
			t.Fatal("the session stayed busy")
		}
		info := decodeJSON[SessionInfo](t, f.do(http.MethodGet, "/sessions/s1", "", nil))
		if info.Pending == nil || info.Pending.Kind != "failed" {
			t.Fatalf("pending = %+v", info.Pending)
		}
		close(release)
		_, objs, _ := f.stream("/sessions/s1/resume", nil)
		if res := objs[len(objs)-1]["result"].(map[string]any); res["status"] != "completed" || res["output"] != "released" {
			t.Fatalf("resumed result = %v", res)
		}
	})
}

func TestHeartbeat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started, release := make(chan struct{}, 1), make(chan struct{})
		f := newFixture(t, nil, []agent.Option{agent.WithModel(blockingModel(started, release))}, WithHeartbeat(5*time.Millisecond))
		go func() {
			<-started
			time.Sleep(50 * time.Millisecond)
			close(release)
		}()
		resp := f.do(http.MethodPost, "/sessions/s1/runs", "text/event-stream", RunRequest{Input: "Hi"})
		events, comments := parseSSE(t, resp.Body)
		// The fake clock ticks nine or ten times while the model waits.
		if comments < 9 {
			t.Fatalf("%d heartbeat comments", comments)
		}
		if objs := fields(t, events); objs[len(objs)-1]["type"] != "run_end" {
			t.Fatalf("events = %v", types(objs))
		}
	})
}

func TestProtocolSelection(t *testing.T) {
	f := newFixture(t, []modeltest.Step{modeltest.Reply("a"), modeltest.Reply("b")}, nil, WithProtocol(ProtocolVercel))
	hdr, objs, events := f.stream("/sessions/s1/runs", RunRequest{Input: "Hi"})
	if hdr.Get("X-Vercel-AI-UI-Message-Stream") != "v1" || events[len(events)-1].Data != "[DONE]" || objs[0]["type"] != "start" {
		t.Fatalf("not a UI message stream: %v", events)
	}
	_, objs, _ = f.stream("/sessions/s1/runs?protocol=ag-ui", RunRequest{Input: "Hi"})
	if objs[0]["type"] != "RUN_STARTED" || objs[0]["threadId"] != "s1" || objs[len(objs)-1]["type"] != "RUN_FINISHED" {
		t.Fatalf("not AG-UI: %v", types(objs))
	}
	if resp := f.do(http.MethodPost, "/sessions/s1/runs?protocol=soap", "", RunRequest{Input: "Hi"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown protocol: %s", resp.Status)
	}
}

func TestRequestValidation(t *testing.T) {
	f := newFixture(t, []modeltest.Step{modeltest.Reply("ok")}, nil, WithMaxBodyBytes(160))
	cases := []struct {
		name, path, ctype, body string
		status                  int
		code                    string
	}{
		{"no input", "/sessions/s1/runs", "application/json", `{}`, 400, CodeBadRequest},
		{"input and message", "/sessions/s1/runs", "application/json", `{"input":"a","message":{"role":"user","parts":[{"type":"text","text":"b"}]}}`, 400, CodeBadRequest},
		{"empty message", "/sessions/s1/runs", "application/json", `{"message":{"role":"user","parts":[]}}`, 400, CodeBadRequest},
		{"unknown field", "/sessions/s1/runs", "application/json", `{"inputs":"a"}`, 400, CodeBadRequest},
		{"bad JSON", "/sessions/s1/runs", "application/json", `{`, 400, CodeBadRequest},
		{"trailing data", "/sessions/s1/runs", "application/json", `{"input":"a"} {}`, 400, CodeBadRequest},
		{"form body", "/sessions/s1/runs", "application/x-www-form-urlencoded", `input=a`, 415, CodeUnsupportedMedia},
		{"too large", "/sessions/s1/runs", "application/json", `{"input":"` + strings.Repeat("a", 200) + `"}`, 413, CodePayloadTooLarge},
		{"run options", "/sessions/s1/runs", "application/json", `{"input":"a","max_turns":99}`, 400, CodeBadRequest},
		{"bad session ID", "/sessions/..x/runs", "application/json", `{"input":"a"}`, 400, CodeBadRequest},
		{"unknown approval field", "/sessions/s1/resume", "application/json", `{"approval":{}}`, 400, CodeBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, f.http.URL+c.path, strings.NewReader(c.body))
			req.Header.Set("Content-Type", c.ctype)
			resp := f.roundTrip(req)
			if resp.StatusCode != c.status || errorCode(t, resp) != c.code {
				t.Fatalf("status %s", resp.Status)
			}
		})
	}
	if resp := f.do(http.MethodGet, "/sessions/missing", "", nil); resp.StatusCode != http.StatusNotFound || errorCode(t, resp) != CodeNotFound {
		t.Fatalf("missing session: %s", resp.Status)
	}
}

func TestRunOptions(t *testing.T) {
	f := newFixture(t, []modeltest.Step{deployStep("c1", "a")}, nil, AllowRunOptions())
	resp := f.do(http.MethodPost, "/sessions/s1/runs", "application/json", RunRequest{Input: "go", MaxTurns: 1})
	if res := decodeJSON[loop.Result](t, resp); res.Status != loop.StatusInterrupted {
		t.Fatalf("result = %+v", res)
	}
	if resp := f.do(http.MethodPost, "/sessions/s2/runs", "", RunRequest{Input: "go", MaxTurns: -1}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative max_turns: %s", resp.Status)
	}
}

func TestServerRunOptionsOverrideClientLimits(t *testing.T) {
	var calls atomic.Int32
	echo := tool.Func("echo", "Echo.", func(_ context.Context, _ struct{}) (string, error) {
		calls.Add(1)
		return "echoed", nil
	})
	f := newFixture(t, []modeltest.Step{
		modeltest.CallTools(modeltest.Call{ID: "c1", Name: "echo", Args: struct{}{}}),
		modeltest.Reply("done"),
	}, []agent.Option{agent.WithTools(echo)}, AllowRunOptions(),
		WithRunOptions(func(r *http.Request) []agent.RunOption {
			if r.Header.Get("X-Plan") == "free" {
				return []agent.RunOption{agent.RunLimits(loop.Limits{Requests: 1})}
			}
			return nil
		}))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.http.URL+"/sessions/s1/runs", strings.NewReader(`{"input":"go","limits":{"requests":5}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plan", "free")
	resp := f.roundTrip(req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s", resp.Status)
	}
	res := decodeJSON[loop.Result](t, resp)
	if res.Status != loop.StatusLimitExceeded || res.Reason != "reached the limit of 1 requests" || res.Usage.Requests != 1 {
		t.Fatalf("result = %+v", res)
	}
	if got := len(f.model.Requests()); got != 1 || calls.Load() != 1 {
		t.Fatalf("model requests = %d, tool calls = %d; want 1 each", got, calls.Load())
	}
}

func TestSessionLifecycle(t *testing.T) {
	f := newFixture(t, []modeltest.Step{modeltest.Reply("one")}, nil)
	created := decodeJSON[sessionIDBody](t, f.do(http.MethodPost, "/sessions", "", nil))
	if created.ID == "" {
		t.Fatal("no session ID")
	}
	f.do(http.MethodPost, "/sessions/"+created.ID+"/runs", "application/json", RunRequest{Input: "Hi"})

	list := decodeJSON[struct{ Sessions []SessionSummary }](t, f.do(http.MethodGet, "/sessions", "", nil))
	if len(list.Sessions) != 1 || list.Sessions[0].ID != created.ID || list.Sessions[0].MessageCount != 2 {
		t.Fatalf("list = %+v", list)
	}
	forked := decodeJSON[sessionIDBody](t, f.do(http.MethodPost, "/sessions/"+created.ID+"/fork", "", nil))
	if info := decodeJSON[SessionInfo](t, f.do(http.MethodGet, "/sessions/"+forked.ID, "", nil)); len(info.Messages) != 2 {
		t.Fatalf("fork = %+v", info)
	}
	cleared := decodeJSON[sessionIDBody](t, f.do(http.MethodPost, "/sessions/"+created.ID+"/clear", "", nil))
	if info := decodeJSON[SessionInfo](t, f.do(http.MethodGet, "/sessions/"+cleared.ID, "", nil)); len(info.Messages) != 0 {
		t.Fatalf("clear = %+v", info)
	}
	if resp := f.do(http.MethodPost, "/sessions/missing/fork", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("fork of a missing session: %s", resp.Status)
	}

	if resp := f.do(http.MethodDelete, "/sessions/"+created.ID+"?reason=logout", "", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("end: %s", resp.Status)
	}
	if resp := f.do(http.MethodGet, "/sessions/"+created.ID, "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("an ended session is kept: %s", resp.Status)
	}
	if resp := f.do(http.MethodDelete, "/sessions/"+created.ID+"?purge=true", "", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("purge: %s", resp.Status)
	}
	if resp := f.do(http.MethodGet, "/sessions/"+created.ID, "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a purged session: %s", resp.Status)
	}
	if resp := f.do(http.MethodDelete, "/sessions/"+created.ID, "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("end of a missing session: %s", resp.Status)
	}
}

func TestAuthAccessAndCORS(t *testing.T) {
	f := newFixture(t, []modeltest.Step{modeltest.Reply("ok")}, nil,
		WithAuth(func(r *http.Request) error {
			switch r.Header.Get("Authorization") {
			case "Bearer good":
				return nil
			case "Bearer banned":
				return &Error{Status: http.StatusForbidden, Code: "banned", Message: "banned"}
			}
			return errors.New("missing token")
		}),
		WithSessionAccess(func(_ *http.Request, id string) error {
			if strings.HasPrefix(id, "mine") {
				return nil
			}
			return errors.New("not yours")
		}),
		WithCORS("https://app.example"),
	)
	send := func(method, path, token, origin string) *response {
		req, _ := http.NewRequestWithContext(t.Context(), method, f.http.URL+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if method == http.MethodOptions {
			req.Header.Set("Access-Control-Request-Method", "POST")
		}
		resp := f.roundTrip(req)
		return resp
	}
	if resp := send(http.MethodGet, "/sessions/mine1", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %s", resp.Status)
	}
	if resp := send(http.MethodGet, "/sessions/mine1", "banned", ""); resp.StatusCode != http.StatusForbidden || errorCode(t, resp) != "banned" {
		t.Fatalf("banned: %s", resp.Status)
	}
	if resp := send(http.MethodGet, "/sessions/other", "good", ""); resp.StatusCode != http.StatusForbidden || errorCode(t, resp) != CodeForbidden {
		t.Fatalf("other's session: %s", resp.Status)
	}
	if resp := send(http.MethodGet, "/sessions/mine1", "good", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("own missing session: %s", resp.Status)
	}

	pre := send(http.MethodOptions, "/sessions/mine1/runs", "", "https://app.example")
	if pre.StatusCode != http.StatusNoContent || pre.Header.Get("Access-Control-Allow-Origin") != "https://app.example" ||
		!strings.Contains(pre.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatalf("preflight: %s %v", pre.Status, pre.Header)
	}
	if resp := send(http.MethodGet, "/sessions/mine1", "good", "https://app.example"); resp.Header.Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Fatalf("no CORS header: %v", resp.Header)
	}
	if resp := send(http.MethodOptions, "/sessions/mine1/runs", "", "https://evil.example"); resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("CORS for a foreign origin: %v", resp.Header)
	}

	// Listing shows only the sessions the caller may access.
	saveSession(t, f, "mine1")
	saveSession(t, f, "other")
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, f.http.URL+"/sessions", nil)
	req.Header.Set("Authorization", "Bearer good")
	resp := f.roundTrip(req)
	list := decodeJSON[struct{ Sessions []SessionSummary }](t, resp)
	if len(list.Sessions) != 1 || list.Sessions[0].ID != "mine1" {
		t.Fatalf("list = %+v", list)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
