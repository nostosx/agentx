package model_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/nostosx/agentx/model"
)

func TestMessageContent(t *testing.T) {
	t.Parallel()
	call := &model.ToolCall{ID: "c1", Name: "lookup"}
	result := &model.ToolResult{CallID: "c1", Content: "found"}
	msg := model.NewMessage(model.RoleAssistant,
		model.TextPart("hello"), model.Part{Type: model.PartReasoning, Text: "hidden"},
		model.Part{Type: model.PartToolCall, ToolCall: call},
		model.Part{Type: model.PartToolCall}, model.TextPart(" world"),
		model.Part{Type: model.PartToolResult, ToolResult: result})
	if msg.Text() != "hello world" || !reflect.DeepEqual(msg.ToolCalls(), []*model.ToolCall{call}) || !reflect.DeepEqual(msg.ToolResults(), []*model.ToolResult{result}) {
		t.Fatalf("unexpected message content: %#v", msg)
	}
	var empty *model.Message
	if empty.Text() != "" || empty.ToolCalls() != nil || empty.ToolResults() != nil || empty.Clone() != nil {
		t.Fatal("nil message must have empty content")
	}
}

func TestCloneMessagesIsolation(t *testing.T) {
	t.Parallel()
	original := []*model.Message{nil, {
		Role: model.RoleAssistant, Metadata: map[string]any{"source": "original"},
		Parts: []model.Part{
			model.ImagePart("image/png", []byte{1, 2}),
			{Type: model.PartRaw, Raw: json.RawMessage(`{"a":1}`)},
			{Type: model.PartToolCall, ToolCall: &model.ToolCall{ID: "c1", Arguments: json.RawMessage(`{"x":1}`)}},
			{Type: model.PartToolResult, ToolResult: &model.ToolResult{CallID: "c1", Content: "original"}},
		},
	}}
	clone := model.CloneMessages(original)
	if !reflect.DeepEqual(clone, original) || model.CloneMessages(nil) != nil {
		t.Fatal("clone must preserve content and nil messages")
	}
	clone[1].Parts[0].Data[0] = 9
	clone[1].Parts[1].Raw[2] = 'b'
	clone[1].Parts[2].ToolCall.Arguments[2] = 'y'
	clone[1].Parts[2].ToolCall.ID = "changed"
	clone[1].Parts[3].ToolResult.Content = "changed"
	clone[1].Metadata["source"] = "changed"
	if original[1].Parts[0].Data[0] != 1 || string(original[1].Parts[1].Raw) != `{"a":1}` || string(original[1].Parts[2].ToolCall.Arguments) != `{"x":1}` || original[1].Parts[2].ToolCall.ID != "c1" || original[1].Parts[3].ToolResult.Content != "original" || original[1].Metadata["source"] != "original" {
		t.Fatal("modifying clone changed original content")
	}
}

func TestMediaParts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		part model.Part
		want model.PartType
	}{
		{model.DocumentPart("application/pdf", []byte("%PDF"), "a.pdf"), model.PartDocument},
		{model.TextDocumentPart("notes", "text"), model.PartDocument},
		{model.DocumentURLPart("https://example.com/a.pdf"), model.PartDocument},
		{model.AudioPart("audio/wav", []byte("RIFF")), model.PartAudio},
		{model.AudioURLPart("gs://bucket/a.mp3"), model.PartAudio},
		{model.VideoPart("video/mp4", []byte("x")), model.PartVideo},
		{model.VideoURLPart("https://youtu.be/x"), model.PartVideo},
		{model.FilePart(model.PartImage, "openai", "file-1", ""), model.PartImage},
		{model.MediaPart("Image/PNG", nil), model.PartImage},
		{model.MediaPart("audio/L16;rate=24000", nil), model.PartAudio},
		{model.MediaPart("video/webm", nil), model.PartVideo},
		{model.MediaPart("application/pdf", nil), model.PartDocument},
		{model.MediaPart("", nil), model.PartDocument},
	} {
		if tc.part.Type != tc.want || !tc.part.IsMedia() {
			t.Errorf("part %+v: type %s, want %s", tc.part, tc.part.Type, tc.want)
		}
	}
	if p := model.TextDocumentPart("notes", "text"); p.MediaType != "text/plain" || string(p.Data) != "text" || p.Filename != "notes" {
		t.Errorf("text document = %+v", p)
	}
	if p := model.FilePart(model.PartDocument, "anthropic", "file_1", "application/pdf"); p.Provider != "anthropic" || p.FileID != "file_1" || p.MediaType != "application/pdf" {
		t.Errorf("file part = %+v", p)
	}
	for _, p := range []model.Part{model.TextPart("x"), {Type: model.PartToolResult}, {Type: model.PartReasoning}} {
		if p.IsMedia() {
			t.Errorf("%s part counted as media", p.Type)
		}
	}
}

// Messages with media and tool results with parts survive the JSON
// encoding that checkpoints and sessions use; data is encoded as base64.
func TestMediaJSONRoundTrip(t *testing.T) {
	t.Parallel()
	doc := model.DocumentPart("application/pdf", []byte{0, 1, 0xff}, "a.pdf")
	doc.Citations, doc.Detail = true, "high"
	msgs := []*model.Message{
		model.NewMessage(model.RoleUser, doc, model.AudioPart("audio/wav", []byte("RIFF")), model.FilePart(model.PartVideo, "gemini", "https://example.com/files/v", "video/mp4")),
		model.ToolResults(&model.ToolResult{CallID: "c", Name: "read", Content: "image", Parts: []model.Part{model.ImagePart("image/png", []byte{0x89, 'P'})}, Metadata: map[string]any{"path": "a.png"}}),
	}
	b, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"data":"AAH/"`) || !strings.Contains(string(b), `"parts":[{"type":"image","media_type":"image/png","data":"iVA="}]`) {
		t.Fatalf("encoding = %s", b)
	}
	var got []*model.Message
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, msgs) {
		t.Fatalf("round trip = %s", b)
	}
}

func TestCloneToolResultParts(t *testing.T) {
	t.Parallel()
	msg := model.ToolResults(&model.ToolResult{CallID: "c", Parts: []model.Part{model.ImagePart("image/png", []byte{1})}, Metadata: map[string]any{"k": "v"}})
	c := msg.Clone()
	c.Parts[0].ToolResult.Parts[0].Data[0] = 9
	c.Parts[0].ToolResult.Parts[0].MediaType = "changed"
	c.Parts[0].ToolResult.Metadata["k"] = "changed"
	r := msg.Parts[0].ToolResult
	if r.Parts[0].Data[0] != 1 || r.Parts[0].MediaType != "image/png" || r.Metadata["k"] != "v" {
		t.Fatalf("clone shares result parts: %+v", r)
	}
}
