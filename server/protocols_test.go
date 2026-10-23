package server

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
)

// useChatBody is a request body as useChat's default transport sends it.
func useChatBody(id string, msgs ...map[string]any) map[string]any {
	return map[string]any{"id": id, "messages": msgs, "trigger": "submit-message", "messageId": nil}
}

func userUI(text string) map[string]any {
	return map[string]any{"id": "u1", "role": "user", "parts": []any{map[string]any{"type": "text", "text": text}}}
}

func TestVercelChat(t *testing.T) {
	f := newFixture(t, []modeltest.Step{
		modeltest.Reply("Hello!"),
		deployStep("call_1", "prod"), modeltest.Reply("Deployed."),
		deployStep("call_2", "prod"), modeltest.Reply("Not deployed."),
	}, nil)

	hdr, objs, events := f.stream("/chat", useChatBody("chat1", userUI("Hi")))
	if hdr.Get("X-Vercel-AI-UI-Message-Stream") != "v1" {
		t.Fatalf("headers = %v", hdr)
	}
	checkStream(t, "vercel-ui", events)
	if got := strings.Join(types(objs), ","); got != "start,start-step,text-start,text-delta,text-delta,text-end,finish-step,finish" {
		t.Fatalf("chunks = %s", got)
	}

	// A tool call that needs approval ends the stream with an approval
	// request; the approval response resumes the run.
	_, objs, events = f.stream("/chat", useChatBody("chat1", userUI("Hi"), userUI("Deploy")))
	checkStream(t, "vercel-ui", events)
	approval := find(objs, "tool-approval-request")
	if approval == nil || approval["approvalId"] != "call_1" || objs[len(objs)-1]["finishReason"] != "tool-calls" {
		t.Fatalf("chunks = %v", objs)
	}
	messageID := objs[0]["messageId"].(string)
	approved := map[string]any{
		"id": messageID, "role": "assistant", "parts": []any{
			map[string]any{"type": "step-start"},
			map[string]any{"type": "tool-deploy", "toolCallId": "call_1", "state": "approval-responded",
				"input": map[string]any{"env": "prod"}, "approval": map[string]any{"id": "call_1", "approved": true}},
		},
	}
	_, objs, events = f.stream("/chat", useChatBody("chat1", userUI("Deploy"), approved))
	checkStream(t, "vercel-ui", events)
	if objs[0]["messageId"] != messageID {
		t.Fatalf("the resumed stream should continue message %s: %v", messageID, objs[0])
	}
	out := find(objs, "tool-output-available")
	if out == nil || out["output"] != "deployed to prod" || f.deploy.n.Load() != 1 {
		t.Fatalf("chunks = %v", objs)
	}

	// A denial reports the call as denied.
	f.stream("/chat", useChatBody("chat1", userUI("Deploy again")))
	denied := approved
	denied["parts"] = []any{map[string]any{"type": "tool-deploy", "toolCallId": "call_2", "state": "approval-responded",
		"approval": map[string]any{"id": "call_2", "approved": false, "reason": "not now"}}}
	_, objs, _ = f.stream("/chat", useChatBody("chat1", denied))
	if find(objs, "tool-output-denied") == nil || f.deploy.n.Load() != 1 {
		t.Fatalf("chunks = %v", objs)
	}
}

func find(objs []map[string]any, typ string) map[string]any {
	for _, o := range objs {
		if o["type"] == typ {
			return o
		}
	}
	return nil
}

func TestVercelChatErrors(t *testing.T) {
	f := newFixture(t, []modeltest.Step{deployStep("call_1", "prod")}, nil)
	cases := []struct {
		name   string
		body   any
		status int
	}{
		{"regenerate", map[string]any{"id": "c1", "messages": []any{userUI("Hi")}, "trigger": "regenerate-message"}, 400},
		{"no messages", useChatBody("c1"), 400},
		{"bad id", useChatBody("../x", userUI("Hi")), 400},
		{"system role", useChatBody("c1", map[string]any{"role": "system", "parts": []any{}}), 400},
		{"assistant without approvals", useChatBody("c1", map[string]any{"role": "assistant", "parts": []any{map[string]any{"type": "text", "text": "x"}}}), 400},
		{"zip file", useChatBody("c1", map[string]any{"role": "user", "parts": []any{map[string]any{"type": "file", "mediaType": "application/zip", "url": "https://x/y.zip"}}}), 415},
		{"approvals without a run", useChatBody("c1", map[string]any{"role": "assistant", "parts": []any{map[string]any{"type": "tool-deploy", "toolCallId": "x", "state": "approval-responded", "approval": map[string]any{"id": "x", "approved": true}}}}), 404},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if resp := f.do(http.MethodPost, "/chat", "", c.body); resp.StatusCode != c.status {
				t.Fatalf("status %s, want %d", resp.Status, c.status)
			}
		})
	}
	if resp := f.do(http.MethodGet, "/chat/c1/stream", "", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reconnect: %s", resp.Status)
	}
	// A chat waiting for approvals rejects a new user message.
	f.stream("/chat", useChatBody("c2", userUI("Deploy")))
	if resp := f.do(http.MethodPost, "/chat", "", useChatBody("c2", userUI("Hi"))); resp.StatusCode != http.StatusConflict {
		t.Fatalf("new message while interrupted: %s", resp.Status)
	}
}

func TestUIUserMessageFiles(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G'}
	msg, e := uiUserMessage(uiMessage{Role: "user", Parts: []uiPart{
		{Type: "text", Text: "Look"},
		{Type: "file", MediaType: "image/png", URL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)},
		{Type: "file", MediaType: "image/jpeg", URL: "https://example.com/a.jpg"},
		{Type: "step-start"},
	}})
	if e != nil {
		t.Fatal(e)
	}
	if len(msg.Parts) != 3 || string(msg.Parts[1].Data) != string(png) || msg.Parts[2].URL != "https://example.com/a.jpg" || msg.Parts[2].MediaType != "image/jpeg" {
		t.Fatalf("message = %+v", msg)
	}
	// Documents, audio and video become parts of their type.
	pdf := []byte("%PDF-1.7")
	msg, e = uiUserMessage(uiMessage{Role: "user", Parts: []uiPart{
		{Type: "file", MediaType: "application/pdf", URL: "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(pdf), Filename: "r.pdf"},
		{Type: "file", MediaType: "audio/wav", URL: "data:audio/wav;base64,UklGRg=="},
		{Type: "file", MediaType: "video/mp4", URL: "https://example.com/v.mp4"},
		{Type: "file", URL: "https://example.com/notes.md"},
		{Type: "file", MediaType: "text/plain", URL: "data:text/plain,hello%20world", Filename: "n.txt"},
	}})
	if e != nil {
		t.Fatal(e)
	}
	doc, audio, video, md, txt := msg.Parts[0], msg.Parts[1], msg.Parts[2], msg.Parts[3], msg.Parts[4]
	if doc.Type != model.PartDocument || string(doc.Data) != string(pdf) || doc.Filename != "r.pdf" || doc.MediaType != "application/pdf" ||
		audio.Type != model.PartAudio || string(audio.Data) != "RIFF" ||
		video.Type != model.PartVideo || video.URL != "https://example.com/v.mp4" ||
		md.Type != model.PartDocument || md.MediaType != "text/markdown" ||
		txt.Type != model.PartDocument || string(txt.Data) != "hello world" {
		t.Fatalf("parts = %+v", msg.Parts)
	}
	for _, tc := range []struct {
		part   uiPart
		status int
	}{
		{uiPart{Type: "file", MediaType: "image/png", URL: "data:image/png;base64,!!"}, http.StatusBadRequest},
		{uiPart{Type: "file", MediaType: "image/png"}, http.StatusBadRequest},
		{uiPart{Type: "file", MediaType: "application/zip", URL: "https://example.com/a.zip"}, http.StatusUnsupportedMediaType},
		{uiPart{Type: "file", URL: "https://example.com/a"}, http.StatusUnsupportedMediaType},
	} {
		if _, e := uiUserMessage(uiMessage{Role: "user", Parts: []uiPart{tc.part}}); e == nil || e.Status != tc.status {
			t.Errorf("%+v: error %v, want status %d", tc.part, e, tc.status)
		}
	}
	if _, e := uiUserMessage(uiMessage{Role: "user"}); e == nil {
		t.Error("an empty message should be rejected")
	}
}

// aguiInputBody is a RunAgentInput.
func aguiInputBody(thread string, resume []any, msgs ...any) map[string]any {
	b := map[string]any{"threadId": thread, "runId": "client_run", "messages": msgs, "tools": []any{}, "context": []any{}, "state": map[string]any{}}
	if resume != nil {
		b["resume"] = resume
	}
	return b
}

func TestAGUI(t *testing.T) {
	f := newFixture(t, []modeltest.Step{
		modeltest.Reply("Hello"),
		deployStep("call_1", "prod"), modeltest.Reply("Deployed."),
	}, nil)
	_, objs, events := f.stream("/agui", aguiInputBody("t1", nil, map[string]any{"id": "m1", "role": "user", "content": "Hi"}))
	checkStream(t, "ag-ui", events)
	if objs[0]["runId"] != "client_run" || objs[0]["threadId"] != "t1" || objs[len(objs)-1]["result"] != "Hello" {
		t.Fatalf("events = %v", objs)
	}

	parts := []any{map[string]any{"type": "text", "text": "Deploy"}}
	_, objs, events = f.stream("/agui", aguiInputBody("t1", nil, map[string]any{"id": "m2", "role": "user", "content": parts}))
	checkStream(t, "ag-ui", events)
	fin := objs[len(objs)-1]
	outcome := fin["outcome"].(map[string]any)
	interrupts := outcome["interrupts"].([]any)
	if outcome["type"] != "interrupt" || len(interrupts) != 1 || interrupts[0].(map[string]any)["toolCallId"] != "call_1" {
		t.Fatalf("finish = %v", fin)
	}

	// New input without resume entries is rejected while interrupted.
	_, objs, _ = f.stream("/agui", aguiInputBody("t1", nil, map[string]any{"id": "m3", "role": "user", "content": "Hi"}))
	if len(objs) != 1 || objs[0]["type"] != "RUN_ERROR" || objs[0]["code"] != "interrupt_pending" {
		t.Fatalf("events = %v", objs)
	}
	// Resume entries must answer every open interrupt.
	_, objs, _ = f.stream("/agui", aguiInputBody("t1", []any{map[string]any{"interruptId": "other", "status": "resolved", "payload": map[string]any{"approved": true}}}))
	if objs[0]["type"] != "RUN_ERROR" || objs[0]["code"] != "invalid_resume" {
		t.Fatalf("events = %v", objs)
	}
	_, objs, _ = f.stream("/agui", aguiInputBody("t1", []any{map[string]any{"interruptId": "call_1", "status": "resolved", "payload": map[string]any{}}}))
	if objs[0]["type"] != "RUN_ERROR" || objs[0]["code"] != "invalid_resume" {
		t.Fatalf("events = %v", objs)
	}

	resume := []any{map[string]any{"interruptId": "call_1", "status": "resolved", "payload": map[string]any{"approved": true, "editedArgs": map[string]any{"env": "staging"}}}}
	_, objs, events = f.stream("/agui", aguiInputBody("t1", resume))
	checkStream(t, "ag-ui", events)
	res := find(objs, "TOOL_CALL_RESULT")
	if res == nil || !strings.Contains(res["content"].(string), "deployed to staging") || objs[len(objs)-1]["result"] != "Deployed." {
		t.Fatalf("events = %v", objs)
	}

	if resp := f.do(http.MethodPost, "/agui", "", map[string]any{"messages": []any{}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing threadId: %s", resp.Status)
	}
	_, objs, _ = f.stream("/agui", aguiInputBody("t2", nil, map[string]any{"id": "m", "role": "tool", "content": "x", "toolCallId": "c"}))
	if objs[0]["type"] != "RUN_ERROR" {
		t.Fatalf("frontend tool result: %v", objs)
	}
}

func TestAGUISubAgent(t *testing.T) {
	sub := agent.MustNew("research", agent.WithModel(modeltest.New(modeltest.Reply("Found it."))), agent.WithDescription("Researches."))
	f := newFixture(t, []modeltest.Step{
		modeltest.CallTools(modeltest.Call{ID: "call_0", Name: "research", Args: map[string]string{"prompt": "look"}}),
		modeltest.Reply("Here it is."),
	}, []agent.Option{agent.WithSubAgents(sub), agent.WithSubAgentEvents()})
	_, objs, events := f.stream("/agui", aguiInputBody("t1", nil, map[string]any{"id": "m1", "role": "user", "content": "Find it"}))
	checkStream(t, "ag-ui", events)
	started, finished := find(objs, "SUBAGENT_STARTED"), find(objs, "SUBAGENT_FINISHED")
	if started == nil || started["name"] != "research" || started["parentToolCallId"] != "call_0" || finished == nil || finished["result"] != "Found it." {
		t.Fatalf("events = %v", types(objs))
	}
	for _, o := range objs {
		if o["type"] == "TEXT_MESSAGE_CONTENT" && o["delta"] == "Found it." && o["subagentRunId"] != started["subagentRunId"] {
			t.Fatalf("sub-agent text not attributed: %v", o)
		}
	}
}

func TestAGUIUserMessage(t *testing.T) {
	msg, e := aguiUserMessage(aguiMessage{Role: "user", Content: []byte(`[
		{"type":"text","text":"Look"},
		{"type":"image","source":{"type":"data","value":"iVBO","mimeType":"image/png"}},
		{"type":"image","source":{"type":"url","value":"https://x/a.png","mimeType":"image/png"}}]`)})
	if e != nil || len(msg.Parts) != 3 || msg.Parts[1].Type != model.PartImage || msg.Parts[2].URL != "https://x/a.png" {
		t.Fatalf("message = %+v, %v", msg, e)
	}
	msg, e = aguiUserMessage(aguiMessage{Role: "user", Content: []byte(`[
		{"type":"document","source":{"type":"data","value":"JVBERg==","mimeType":"application/pdf"},"metadata":{"filename":"r.pdf"}},
		{"type":"audio","source":{"type":"url","value":"data:audio/wav;base64,UklGRg=="}},
		{"type":"video","source":{"type":"url","value":"https://x/v.mp4","mimeType":"video/mp4"}},
		{"type":"document","source":{"type":"file","value":"file_1","provider":"openai","mimeType":"application/pdf"}}]`)})
	if e != nil || len(msg.Parts) != 4 {
		t.Fatalf("media message = %+v, %v", msg, e)
	}
	if doc, audio, video, file := msg.Parts[0], msg.Parts[1], msg.Parts[2], msg.Parts[3]; doc.Type != model.PartDocument || string(doc.Data) != "%PDF" || doc.Filename != "r.pdf" ||
		audio.Type != model.PartAudio || string(audio.Data) != "RIFF" || audio.MediaType != "audio/wav" ||
		video.Type != model.PartVideo || video.URL != "https://x/v.mp4" ||
		file.Type != model.PartDocument || file.FileID != "file_1" || file.Provider != "openai" {
		t.Fatalf("media parts = %+v", msg.Parts)
	}
	if _, e := aguiUserMessage(aguiMessage{Content: []byte(`[{"type":"document","source":{"type":"url","value":"https://x/a.zip","mimeType":"application/zip"}}]`)}); e == nil || e.Status != http.StatusUnsupportedMediaType {
		t.Errorf("zip document: %v", e)
	}
	for _, content := range []string{`""`, `[]`, `[{"type":"audio"}]`, `[{"type":"binary"}]`, `[{"type":"image","source":{"type":"data","value":"!!","mimeType":"image/png"}}]`,
		`[{"type":"image","source":{"type":"blob","value":"x"}}]`, `[{"type":"image","source":{"type":"url","value":""}}]`, `42`} {
		if _, e := aguiUserMessage(aguiMessage{Content: []byte(content)}); e == nil {
			t.Errorf("%s: expected an error", content)
		}
	}
	if _, msg := aguiApprovals([]aguiResume{{InterruptID: "a", Status: "weird"}}); msg == "" {
		t.Error("invalid status accepted")
	}
	if _, msg := aguiApprovals([]aguiResume{{Status: "resolved"}}); msg == "" {
		t.Error("missing interruptId accepted")
	}
	got, msg2 := aguiApprovals([]aguiResume{{InterruptID: "a", Status: "cancelled"}})
	if msg2 != "" || got["a"].Approved || got["a"].Reason == "" {
		t.Errorf("cancelled = %+v", got)
	}
}
