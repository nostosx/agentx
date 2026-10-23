package mcp

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
)

func TestSamplingContentsJSON(t *testing.T) {
	var msg SamplingMessage
	if err := json.Unmarshal([]byte(`{"role":"user","content":{"type":"text","text":"hi"}}`), &msg); err != nil ||
		len(msg.Content) != 1 || msg.Content[0].Text != "hi" {
		t.Fatalf("single block = %+v, %v", msg, err)
	}
	if err := json.Unmarshal([]byte(`{"role":"user","content":[{"type":"text","text":"a"},{"type":"text"}]}`), &msg); err != nil ||
		len(msg.Content) != 2 {
		t.Fatalf("array = %+v, %v", msg, err)
	}
	if err := json.Unmarshal([]byte(`{"role":"user","content":"text"}`), &msg); err == nil {
		t.Error("string content accepted")
	}
	for v, want := range map[*SamplingMessage]string{
		{Role: "user", Content: SamplingContents{{Type: "text", Text: "a"}}}:             `{"role":"user","content":{"type":"text","text":"a"}}`,
		{Role: "user", Content: SamplingContents{{Type: "text"}, {Type: "text"}}}:        `{"role":"user","content":[{"type":"text","text":""},{"type":"text","text":""}]}`,
		{Role: "user", Content: SamplingContents{{Type: "tool_result", ToolUseID: "c"}}}: `{"role":"user","content":{"type":"tool_result","toolUseId":"c","content":[]}}`,
	} {
		b, err := json.Marshal(v)
		if err != nil || string(b) != want {
			t.Errorf("marshal = %s, %v; want %s", b, err, want)
		}
	}
}

func TestModelRequest(t *testing.T) {
	temp := 0.2
	var req SamplingRequest
	if err := json.Unmarshal([]byte(`{
		"messages":[
			{"role":"user","content":[{"type":"text","text":"Weather?"},{"type":"image","data":"aGk=","mimeType":"image/png"},{"type":"audio","data":"aGk=","mimeType":"audio/wav"}]},
			{"role":"assistant","content":{"type":"tool_use","id":"c1","name":"weather","input":{"city":"Paris"}}},
			{"role":"user","content":{"type":"tool_result","toolUseId":"c1","content":[{"type":"text","text":"18C"},{"type":"image","data":"aGk=","mimeType":"image/png"}],"isError":true}}],
		"systemPrompt":"sys","maxTokens":64,"stopSequences":["END"],
		"tools":[{"name":"weather","description":"Get weather","inputSchema":{"type":"object"}}],
		"toolChoice":{"mode":"required"}}`), &req); err != nil {
		t.Fatal(err)
	}
	req.Temperature = &temp
	got, err := req.ModelRequest()
	if err != nil {
		t.Fatal(err)
	}
	if got.System != "sys" || got.MaxTokens != 64 || got.Temperature == nil || *got.Temperature != 0.2 || len(got.Stop) != 1 || got.Stop[0] != "END" ||
		got.ToolChoice.Mode != model.ToolChoiceRequired || len(got.Tools) != 1 || got.Tools[0].Name != "weather" ||
		string(got.Tools[0].Parameters) != `{"type":"object"}` {
		t.Errorf("request = %+v", got)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("messages = %+v", got.Messages)
	}
	user, call, res := got.Messages[0], got.Messages[1], got.Messages[2]
	if user.Role != model.RoleUser || len(user.Parts) != 3 || user.Parts[0].Text != "Weather?" || string(user.Parts[1].Data) != "hi" ||
		user.Parts[1].MediaType != "image/png" || user.Parts[2].Type != model.PartAudio || user.Parts[2].MediaType != "audio/wav" {
		t.Errorf("user = %+v", user)
	}
	if tc := call.ToolCalls(); call.Role != model.RoleAssistant || len(tc) != 1 || tc[0].ID != "c1" ||
		string(tc[0].Arguments) != `{"city":"Paris"}` {
		t.Errorf("tool call = %+v", call)
	}
	if tr := res.ToolResults(); res.Role != model.RoleTool || len(tr) != 1 || tr[0].CallID != "c1" ||
		tr[0].Content != "18C" || !tr[0].IsError || len(tr[0].Parts) != 1 || tr[0].Parts[0].Type != model.PartImage {
		t.Errorf("tool result = %+v", res)
	}
	for name, raw := range map[string]string{
		"audio base64":  `{"messages":[{"role":"user","content":{"type":"audio","data":"!","mimeType":"audio/wav"}}],"maxTokens":1}`,
		"role":          `{"messages":[{"role":"system","content":{"type":"text","text":"x"}}],"maxTokens":1}`,
		"tool choice":   `{"messages":[],"maxTokens":1,"toolChoice":{"mode":"any"}}`,
		"base64":        `{"messages":[{"role":"user","content":{"type":"image","data":"!","mimeType":"image/png"}}],"maxTokens":1}`,
		"user tool use": `{"messages":[{"role":"user","content":{"type":"tool_use","id":"x","name":"y","input":{}}}],"maxTokens":1}`,
	} {
		var r SamplingRequest
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			t.Fatal(err)
		}
		if _, err := r.ModelRequest(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestSamplingResultFrom(t *testing.T) {
	for name, tc := range map[string]struct {
		resp *model.Response
		want string
	}{
		"text": {&model.Response{Model: "m", Message: model.AssistantText("hi"), FinishReason: model.FinishStop},
			`{"role":"assistant","content":{"type":"text","text":"hi"},"model":"m","stopReason":"endTurn"}`},
		"max tokens": {&model.Response{Model: "m", Message: model.AssistantText("h"), FinishReason: model.FinishLength},
			`{"role":"assistant","content":{"type":"text","text":"h"},"model":"m","stopReason":"maxTokens"}`},
		"tool calls and image": {&model.Response{Model: "m", FinishReason: model.FinishToolCalls, Message: model.NewMessage(model.RoleAssistant,
			model.Part{Type: model.PartReasoning, Text: "thinking"},
			model.ImagePart("image/png", []byte("hi")),
			model.AudioPart("audio/wav", []byte("hi")),
			model.Part{Type: model.PartToolCall, ToolCall: &model.ToolCall{ID: "c", Name: "f"}})},
			`{"role":"assistant","content":[{"type":"image","data":"aGk=","mimeType":"image/png"},` +
				`{"type":"audio","data":"aGk=","mimeType":"audio/wav"},` +
				`{"type":"tool_use","id":"c","name":"f","input":{}}],"model":"m","stopReason":"toolUse"}`},
		"refusal": {&model.Response{Model: "m", Message: model.NewMessage(model.RoleAssistant), FinishReason: model.FinishRefusal},
			`{"role":"assistant","content":{"type":"text","text":""},"model":"m","stopReason":"refusal"}`},
	} {
		res, err := SamplingResultFrom(tc.resp)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if b, _ := json.Marshal(res); string(b) != tc.want {
			t.Errorf("%s: result = %s, want %s", name, b, tc.want)
		}
	}
	if _, err := SamplingResultFrom(&model.Response{}); err == nil {
		t.Error("response without a message accepted")
	}
	if _, err := SamplingResultFrom(&model.Response{Message: model.NewMessage(model.RoleAssistant, model.ImageURLPart("https://x"))}); err == nil {
		t.Error("image URL accepted")
	}
}

func TestModelSampler(t *testing.T) {
	sentinel := errors.New("down")
	m := modeltest.New(modeltest.Reply("Paris"), modeltest.Fail(sentinel))
	h := ModelSampler(m)
	req := &SamplingRequest{MaxTokens: 10, Messages: []SamplingMessage{{Role: "user", Content: SamplingContents{{Type: "text", Text: "?"}}}}}
	res, err := h(t.Context(), req)
	if err != nil || res == nil || len(res.Content) != 1 || res.Content[0].Text != "Paris" || res.Role != "assistant" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if _, err := h(t.Context(), req); !errors.Is(err, sentinel) {
		t.Errorf("err = %v", err)
	}
	if _, err := h(t.Context(), &SamplingRequest{Messages: []SamplingMessage{{Role: "bot"}}}); err == nil {
		t.Error("invalid request accepted")
	}
}
