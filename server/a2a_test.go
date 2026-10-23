package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
)

type rpcResult struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// rpcPost sends a JSON-RPC request to the A2A endpoint.
func (f *fixture) rpcPost(ctx context.Context, method string, params any, version string) *response {
	f.t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": method, "params": params})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, f.http.URL+"/a2a", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if version != "" {
		req.Header.Set("A2A-Version", version)
	}
	resp := f.roundTrip(req)
	return resp
}

// rpc calls a method and decodes its response.
func (f *fixture) rpc(method string, params any) rpcResult {
	f.t.Helper()
	resp := f.rpcPost(f.t.Context(), method, params, "1.0")
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		f.t.Fatalf("%s: %s %s", method, resp.Status, b)
	}
	return decodeJSON[rpcResult](f.t, resp)
}

// rpcStream calls a streaming method and returns the stream responses.
func (f *fixture) rpcStream(method string, params any) []a2aStreamResponse {
	f.t.Helper()
	resp := f.rpcPost(f.t.Context(), method, params, "1.0")
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		b, _ := io.ReadAll(resp.Body)
		f.t.Fatalf("%s: %s %s", method, ct, b)
	}
	events, _ := parseSSE(f.t, resp.Body)
	var out []a2aStreamResponse
	for _, e := range events {
		var r struct {
			JSONRPC string            `json:"jsonrpc"`
			ID      int               `json:"id"`
			Result  a2aStreamResponse `json:"result"`
		}
		if err := json.Unmarshal([]byte(e.Data), &r); err != nil {
			f.t.Fatal(err)
		}
		if r.JSONRPC != "2.0" || r.ID != 7 {
			f.t.Fatalf("bad envelope %s", e.Data)
		}
		out = append(out, r.Result)
	}
	return out
}

func userMsg(text, contextID, taskID string) map[string]any {
	m := map[string]any{"messageId": "m-" + text, "role": "ROLE_USER", "parts": []any{map[string]any{"text": text}}}
	if contextID != "" {
		m["contextId"] = contextID
	}
	if taskID != "" {
		m["taskId"] = taskID
	}
	return m
}

func decodeResult[T any](t *testing.T, r rpcResult) T {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("error %d: %s", r.Error.Code, r.Error.Message)
	}
	var v T
	if err := json.Unmarshal(r.Result, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestA2AAgentCard(t *testing.T) {
	f := newFixture(t, nil, nil, WithA2A(A2AConfig{Name: "ops", Description: "Deploys builds.", Version: "2.0.0",
		Provider: &A2AProvider{Organization: "Example", URL: "https://example.com"}}))
	resp := f.do(http.MethodGet, "/.well-known/agent-card.json", "", nil)
	card := decodeJSON[map[string]any](t, resp)
	ifaces := card["supportedInterfaces"].([]any)
	iface := ifaces[0].(map[string]any)
	if card["name"] != "ops" || card["version"] != "2.0.0" || iface["url"] != f.http.URL+"/a2a" ||
		iface["protocolBinding"] != "JSONRPC" || iface["protocolVersion"] != "1.0" {
		t.Fatalf("card = %v", card)
	}
	caps := card["capabilities"].(map[string]any)
	skills := card["skills"].([]any)
	if caps["streaming"] != true || caps["pushNotifications"] != false || len(skills) != 1 || card["defaultOutputModes"] == nil {
		t.Fatalf("card = %v", card)
	}
	if resp.Header.Get("Cache-Control") == "" {
		t.Fatal("no Cache-Control")
	}
}

func TestA2AProtocolErrors(t *testing.T) {
	f := newFixture(t, nil, nil)
	if r := decodeJSON[rpcResult](t, f.rpcPost(t.Context(), "SendMessage", map[string]any{}, "")); r.Error == nil || r.Error.Code != a2aVersionNotSupported {
		t.Fatalf("no version: %+v", r)
	}
	if r := decodeJSON[rpcResult](t, f.rpcPost(t.Context(), "SendMessage", map[string]any{}, "0.3")); r.Error == nil || r.Error.Code != a2aVersionNotSupported {
		t.Fatalf("0.3: %+v", r)
	}
	for method, code := range map[string]int{
		"message/send":                     rpcMethodNotFound,
		"GetExtendedAgentCard":             a2aExtendedCardMissing,
		"CreateTaskPushNotificationConfig": a2aPushNotSupported,
		"GetTask":                          a2aTaskNotFound,
		"CancelTask":                       a2aTaskNotFound,
		"SubscribeToTask":                  a2aTaskNotFound,
	} {
		if r := f.rpc(method, map[string]any{"id": "nope"}); r.Error == nil || r.Error.Code != code {
			t.Errorf("%s: %+v", method, r.Error)
		}
	}
	for name, params := range map[string]any{
		"no message":   map[string]any{},
		"agent role":   map[string]any{"message": map[string]any{"messageId": "m", "role": "ROLE_AGENT", "parts": []any{map[string]any{"text": "x"}}}},
		"no parts":     map[string]any{"message": map[string]any{"messageId": "m", "role": "ROLE_USER"}},
		"bad context":  map[string]any{"message": userMsg("x", "../x", "")},
		"bad params":   []any{1},
		"unknown task": map[string]any{"message": userMsg("x", "", "task_nope")},
	} {
		r := f.rpc("SendMessage", params)
		if r.Error == nil || (r.Error.Code != rpcInvalidParams && r.Error.Code != a2aTaskNotFound) {
			t.Errorf("%s: %+v", name, r.Error)
		}
	}
	if r := f.rpc("SendMessage", map[string]any{"message": map[string]any{"messageId": "m", "role": "ROLE_USER",
		"parts": []any{map[string]any{"url": "https://x/a.zip", "mediaType": "application/zip"}}}}); r.Error == nil || r.Error.Code != a2aContentTypeNotSupp {
		t.Errorf("zip: %+v", r.Error)
	}
	if r := f.rpc("SendMessage", map[string]any{"message": map[string]any{"messageId": "m", "role": "ROLE_USER",
		"parts": []any{map[string]any{"mediaType": "application/pdf"}}}}); r.Error == nil || r.Error.Code != rpcInvalidParams {
		t.Errorf("empty file: %+v", r.Error)
	}
	if r := f.rpc("SendMessage", map[string]any{"message": userMsg("x", "", ""), "configuration": map[string]any{"acceptedOutputModes": []string{"image/png"}}}); r.Error == nil || r.Error.Code != a2aContentTypeNotSupp {
		t.Errorf("output modes: %+v", r.Error)
	}

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, f.http.URL+"/a2a", strings.NewReader(`{"jsonrpc":`))
	req.Header.Set("Content-Type", "application/json")
	resp := f.roundTrip(req)
	if r := decodeJSON[rpcResult](t, resp); r.Error == nil || r.Error.Code != rpcParseError || string(r.ID) != "null" {
		t.Fatalf("parse error: %+v", r)
	}
	req, _ = http.NewRequestWithContext(t.Context(), http.MethodPost, f.http.URL+"/a2a", strings.NewReader(`{"jsonrpc":"1.0","id":1,"method":"GetTask"}`))
	req.Header.Set("Content-Type", "application/json")
	resp2 := f.roundTrip(req)
	if r := decodeJSON[rpcResult](t, resp2); r.Error == nil || r.Error.Code != rpcInvalidRequest {
		t.Fatalf("invalid request: %+v", r)
	}
}

func TestA2ASendMessageAndApprovals(t *testing.T) {
	f := newFixture(t, []modeltest.Step{
		modeltest.Reply("Hello"),
		deployStep("call_1", "prod"), modeltest.Reply("Deployed."),
		deployStep("call_2", "prod"), modeltest.Reply("Skipped."),
	}, nil)
	task := decodeResult[struct{ Task a2aTask }](t, f.rpc("SendMessage", map[string]any{"message": userMsg("Hi", "", "")})).Task
	if task.Status.State != stateCompleted || task.ContextID == "" || len(task.Artifacts) != 1 || *task.Artifacts[0].Parts[0].Text != "Hello" {
		t.Fatalf("task = %+v", task)
	}
	if len(task.History) != 1 || task.History[0].TaskID != task.ID {
		t.Fatalf("history = %+v", task.History)
	}
	ctxID := task.ContextID

	// The same context continues the session.
	task = decodeResult[struct{ Task a2aTask }](t, f.rpc("SendMessage", map[string]any{"message": userMsg("Deploy", ctxID, "")})).Task
	if task.Status.State != stateInputRequired || task.Status.Message == nil || !strings.Contains(string(task.Status.Message.Parts[1].Data), "call_1") {
		t.Fatalf("task = %+v", task)
	}
	// Another message in the context is rejected while input is required.
	if r := f.rpc("SendMessage", map[string]any{"message": userMsg("Hi", ctxID, "")}); r.Error == nil || r.Error.Code != a2aUnsupportedOp || !strings.Contains(r.Error.Message, task.ID) {
		t.Fatalf("new task while input is required: %+v", r.Error)
	}
	if r := f.rpc("SendMessage", map[string]any{"message": userMsg("yes", ctxID, task.ID)}); r.Error == nil || r.Error.Code != rpcInvalidParams {
		t.Fatalf("text reply: %+v", r.Error)
	}
	if r := f.rpc("SendMessage", map[string]any{"message": userMsg("yes", "other", task.ID)}); r.Error == nil || r.Error.Code != rpcInvalidParams {
		t.Fatalf("mismatched context: %+v", r.Error)
	}
	reply := map[string]any{"messageId": "m-approve", "role": "ROLE_USER", "taskId": task.ID,
		"parts": []any{map[string]any{"data": map[string]any{"approved": true}}}}
	done := decodeResult[struct{ Task a2aTask }](t, f.rpc("SendMessage", map[string]any{"message": reply, "configuration": map[string]any{"historyLength": 1}})).Task
	if done.ID != task.ID || done.Status.State != stateCompleted || *done.Artifacts[0].Parts[0].Text != "Deployed." || f.deploy.n.Load() != 1 {
		t.Fatalf("task = %+v", done)
	}
	if len(done.History) != 1 {
		t.Fatalf("historyLength 1: %d messages", len(done.History))
	}
	if r := f.rpc("SendMessage", map[string]any{"message": reply}); r.Error == nil || r.Error.Code != a2aUnsupportedOp {
		t.Fatalf("message to a completed task: %+v", r.Error)
	}

	// Denial by call ID.
	task = decodeResult[struct{ Task a2aTask }](t, f.rpc("SendMessage", map[string]any{"message": userMsg("Deploy again", ctxID, "")})).Task
	deny := map[string]any{"messageId": "m-deny", "role": "ROLE_USER", "taskId": task.ID, "contextId": ctxID,
		"parts": []any{map[string]any{"data": map[string]any{"approvals": map[string]any{"call_2": map[string]any{"approved": false}}}}}}
	done = decodeResult[struct{ Task a2aTask }](t, f.rpc("SendMessage", map[string]any{"message": deny})).Task
	if done.Status.State != stateCompleted || f.deploy.n.Load() != 1 {
		t.Fatalf("task = %+v", done)
	}

	got := decodeResult[a2aTask](t, f.rpc("GetTask", map[string]any{"id": task.ID, "historyLength": 0}))
	if got.ID != task.ID || got.Status.State != stateCompleted || len(got.History) != 0 {
		t.Fatalf("GetTask = %+v", got)
	}
	list := decodeResult[struct {
		Tasks         []a2aTask
		NextPageToken string
		PageSize      int
		TotalSize     int
	}](t, f.rpc("ListTasks", map[string]any{"contextId": ctxID, "pageSize": 2}))
	if list.TotalSize != 3 || len(list.Tasks) != 2 || list.NextPageToken != "2" || list.Tasks[0].ID != task.ID || list.Tasks[0].Artifacts != nil {
		t.Fatalf("ListTasks = %+v", list)
	}
	list2 := decodeResult[struct {
		Tasks         []a2aTask
		NextPageToken string
	}](t, f.rpc("ListTasks", map[string]any{"contextId": ctxID, "pageSize": 2, "pageToken": "2", "includeArtifacts": true}))
	if len(list2.Tasks) != 1 || list2.NextPageToken != "" || len(list2.Tasks[0].Artifacts) != 1 {
		t.Fatalf("second page = %+v", list2)
	}
	for _, p := range []map[string]any{{"pageSize": 0}, {"pageToken": "x"}, {"statusTimestampAfter": "yesterday"}} {
		if r := f.rpc("ListTasks", p); r.Error == nil || r.Error.Code != rpcInvalidParams {
			t.Errorf("ListTasks %v: %+v", p, r.Error)
		}
	}
	if r := f.rpc("ListTasks", map[string]any{"status": stateInputRequired}); decodeResult[struct{ TotalSize int }](t, r).TotalSize != 0 {
		t.Error("status filter")
	}
}

func TestA2ACompletionReleasesContextBeforeNotification(t *testing.T) {
	s := &Server{busy: map[string]bool{"context": true}}
	updates := make(chan a2aStreamResponse, 1)
	task := &taskRecord{
		contextID: "context", state: stateWorking, running: true,
		idle: make(chan struct{}), subs: map[chan a2aStreamResponse]bool{updates: true},
	}
	// Hold the session reservation lock to stop release. Neither synchronous
	// callers nor stream subscribers may observe completion while it is held.
	s.mu.Lock()
	done := make(chan struct{})
	started := make(chan struct{})
	go func() {
		defer close(done)
		// An empty segment finishes as failed without taking the session lock
		// to observe an event first.
		s.a2aRun(task, func(func(*loop.Event, error) bool) { close(started) })
	}()
	<-started
	var early bool
	select {
	case <-task.idle:
		early = true
	case <-updates:
		early = true
	case <-time.After(25 * time.Millisecond):
	}
	s.mu.Unlock()
	<-done
	if early {
		t.Fatal("completion was published before the context reservation was released")
	}
	if s.isBusy(task.contextID) {
		t.Fatal("completed task still reserves its context")
	}
	if update := <-updates; update.StatusUpdate == nil || update.StatusUpdate.Status.State != stateFailed {
		t.Fatalf("completion update = %+v", update)
	}
	select {
	case <-task.idle:
	default:
		t.Fatal("completion did not wake the synchronous caller")
	}
}

func TestA2AStreaming(t *testing.T) {
	f := newFixture(t, []modeltest.Step{deployStep("call_1", "prod"), modeltest.Reply("Deployed.")}, nil)
	stream := f.rpcStream("SendStreamingMessage", map[string]any{"message": userMsg("Deploy", "c1", "")})
	if stream[0].Task == nil || stream[0].Task.Status.State != stateSubmitted {
		t.Fatalf("first = %+v", stream[0])
	}
	last := stream[len(stream)-1]
	if last.StatusUpdate == nil || last.StatusUpdate.Status.State != stateInputRequired {
		t.Fatalf("last = %+v", last)
	}
	var sawWorking bool
	for _, s := range stream[1 : len(stream)-1] {
		sawWorking = sawWorking || (s.StatusUpdate != nil && s.StatusUpdate.Status.State == stateWorking)
	}
	if !sawWorking {
		t.Fatal("no working update")
	}
	taskID := stream[0].Task.ID

	// Subscribing to an input-required task returns it and ends.
	sub := f.rpcStream("SubscribeToTask", map[string]any{"id": taskID})
	if len(sub) != 1 || sub[0].Task.Status.State != stateInputRequired {
		t.Fatalf("subscribe = %+v", sub)
	}

	reply := map[string]any{"messageId": "m2", "role": "ROLE_USER", "taskId": taskID,
		"parts": []any{map[string]any{"data": map[string]any{"approved": true}}}}
	stream = f.rpcStream("SendStreamingMessage", map[string]any{"message": reply})
	var artifact *a2aArtifactUpdate
	for _, s := range stream {
		if s.ArtifactUpdate != nil {
			artifact = s.ArtifactUpdate
		}
	}
	last = stream[len(stream)-1]
	if artifact == nil || !artifact.LastChunk || *artifact.Artifact.Parts[0].Text != "Deployed." || last.StatusUpdate.Status.State != stateCompleted {
		t.Fatalf("stream = %+v", stream)
	}
	if r := f.rpc("SubscribeToTask", map[string]any{"id": taskID}); r.Error == nil || r.Error.Code != a2aUnsupportedOp {
		t.Fatalf("subscribe to a completed task: %+v", r.Error)
	}
}

func TestA2ACancelAndBackground(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started, release := make(chan struct{}, 1), make(chan struct{})
		f := newFixture(t, nil, []agent.Option{agent.WithModel(blockingModel(started, release))})
		task := decodeResult[struct{ Task a2aTask }](t, f.rpc("SendMessage", map[string]any{"message": userMsg("Hi", "c1", ""), "configuration": map[string]any{"returnImmediately": true}})).Task
		if terminal(task.Status.State) {
			t.Fatalf("task = %+v", task)
		}
		<-started
		if r := f.rpc("SendMessage", map[string]any{"message": userMsg("Hi", "c1", "")}); r.Error == nil || r.Error.Code != a2aUnsupportedOp {
			t.Fatalf("busy context: %+v", r.Error)
		}

		// A subscriber sees the cancellation.
		subDone := make(chan []byte)
		go func() {
			body := `{"jsonrpc":"2.0","id":7,"method":"SubscribeToTask","params":{"id":"` + task.ID + `"}}`
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, f.http.URL+"/a2a", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("A2A-Version", "1.0")
			resp, err := f.client.Do(req)
			if err != nil {
				subDone <- nil
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			subDone <- b
		}()
		rec := f.server.a2a.get(task.ID)
		synctest.Wait()
		rec.mu.Lock()
		n := len(rec.subs)
		rec.mu.Unlock()
		if n == 0 {
			t.Fatal("no subscriber")
		}
		canceled := decodeResult[a2aTask](t, f.rpc("CancelTask", map[string]any{"id": task.ID}))
		if canceled.Status.State != stateCanceled {
			t.Fatalf("canceled = %+v", canceled)
		}
		events, _ := parseSSE(t, bytes.NewReader(<-subDone))
		var sub []a2aStreamResponse
		for _, e := range events {
			var r struct{ Result a2aStreamResponse }
			if err := json.Unmarshal([]byte(e.Data), &r); err != nil {
				t.Fatal(err)
			}
			sub = append(sub, r.Result)
		}
		if len(sub) == 0 || sub[len(sub)-1].StatusUpdate == nil || sub[len(sub)-1].StatusUpdate.Status.State != stateCanceled {
			t.Fatalf("subscription = %+v", sub)
		}
		if r := f.rpc("CancelTask", map[string]any{"id": task.ID}); r.Error == nil || r.Error.Code != a2aTaskNotCancelable {
			t.Fatalf("second cancel: %+v", r.Error)
		}
		// The context is free again; its failed run is given up.
		close(release)
		task = decodeResult[struct{ Task a2aTask }](t, f.rpc("SendMessage", map[string]any{"message": userMsg("Again", "c1", "")})).Task
		if task.Status.State != stateCompleted {
			t.Fatalf("task = %+v", task)
		}
	})
}

func TestA2ACancelInputRequired(t *testing.T) {
	f := newFixture(t, []modeltest.Step{deployStep("call_1", "prod"), modeltest.Reply("ok")}, nil)
	task := decodeResult[struct{ Task a2aTask }](t, f.rpc("SendMessage", map[string]any{"message": userMsg("Deploy", "c1", "")})).Task
	if task.Status.State != stateInputRequired {
		t.Fatalf("task = %+v", task)
	}
	canceled := decodeResult[a2aTask](t, f.rpc("CancelTask", map[string]any{"id": task.ID}))
	if canceled.Status.State != stateCanceled {
		t.Fatalf("canceled = %+v", canceled)
	}
	// The interrupted run was discarded, so the context continues.
	task = decodeResult[struct{ Task a2aTask }](t, f.rpc("SendMessage", map[string]any{"message": userMsg("Hi", "c1", "")})).Task
	if task.Status.State != stateCompleted || f.deploy.n.Load() != 0 {
		t.Fatalf("task = %+v", task)
	}
}

func TestA2ATaskAccessAndRetention(t *testing.T) {
	f := newFixture(t, []modeltest.Step{modeltest.Reply("a"), modeltest.Reply("b"), modeltest.Reply("c")}, nil,
		WithA2A(A2AConfig{MaxTasks: 2}),
		WithSessionAccess(func(r *http.Request, id string) error {
			if r.Header.Get("X-User") == "eve" {
				return &Error{Status: http.StatusForbidden, Code: CodeForbidden, Message: "no"}
			}
			return nil
		}))
	var ids []string
	for _, text := range []string{"1", "2", "3"} {
		task := decodeResult[struct{ Task a2aTask }](t, f.rpc("SendMessage", map[string]any{"message": userMsg(text, "", "")})).Task
		ids = append(ids, task.ID)
	}
	if r := f.rpc("GetTask", map[string]any{"id": ids[0]}); r.Error == nil || r.Error.Code != a2aTaskNotFound {
		t.Fatalf("the oldest task should be forgotten: %+v", r.Error)
	}
	if r := f.rpc("GetTask", map[string]any{"id": ids[2]}); r.Error != nil {
		t.Fatalf("recent task: %+v", r.Error)
	}

	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "GetTask", "params": map[string]any{"id": ids[2]}})
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, f.http.URL+"/a2a", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("A2A-Version", "1.0")
	req.Header.Set("X-User", "eve")
	resp := f.roundTrip(req)
	if r := decodeJSON[rpcResult](t, resp); r.Error == nil || r.Error.Code != a2aTaskNotFound {
		t.Fatalf("task of another user: %+v", r.Error)
	}
}

func TestA2AVersionFromQuery(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "/a2a?A2A-Version=1.0.2", nil)
	if v := a2aVersion(r); v != "1.0" {
		t.Fatalf("version = %q", v)
	}
	r.Header.Set("A2A-Version", "2")
	if v := a2aVersion(r); v != "2" {
		t.Fatalf("version = %q", v)
	}
}

func TestA2AInputFiles(t *testing.T) {
	text := "hi"
	msg, e := a2aInput([]a2aPart{
		{Text: &text},
		{Raw: []byte("%PDF"), MediaType: "application/pdf", Filename: "r.pdf"},
		{Raw: []byte("RIFF"), MediaType: "audio/wav"},
		{URL: "https://x/v.mp4", MediaType: "video/mp4"},
		{URL: "https://x/a.png"},
		{Raw: []byte("# notes"), MediaType: "text/markdown", Filename: "n.md"},
		{Raw: []byte("plain"), MediaType: "text/plain"},
	})
	if e != nil || len(msg.Parts) != 7 {
		t.Fatalf("message = %+v, %v", msg, e)
	}
	want := []model.PartType{model.PartText, model.PartDocument, model.PartAudio, model.PartVideo, model.PartImage, model.PartDocument, model.PartText}
	for i, p := range msg.Parts {
		if p.Type != want[i] {
			t.Errorf("part %d type %s, want %s", i, p.Type, want[i])
		}
	}
	if doc := msg.Parts[1]; string(doc.Data) != "%PDF" || doc.Filename != "r.pdf" || msg.Parts[4].MediaType != "image/png" || msg.Parts[6].Text != "plain" {
		t.Errorf("parts = %+v", msg.Parts)
	}
}
