package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/tool"
)

func TestToolContent(t *testing.T) {
	res := &model.ToolResult{Content: "shot", Parts: []model.Part{
		model.ImagePart("image/png", []byte("img")),
		model.AudioPart("audio/wav", []byte("wav")),
		model.DocumentPart("application/pdf", []byte("%PDF"), "report.pdf"),
		model.TextDocumentPart("notes.txt", "hello"),
		model.VideoPart("video/mp4", []byte("mp4")),
		model.DocumentURLPart("https://example.com/a.pdf"),
		model.FilePart(model.PartDocument, "openai", "file-1", ""),
		model.TextPart("more"),
		{Type: model.PartReasoning, Text: "hidden"},
	}}
	b, _ := json.Marshal(toolContent(ProtocolVersion, res))
	want := `[{"type":"text","text":"shot"},` +
		`{"type":"image","data":"aW1n","mimeType":"image/png"},` +
		`{"type":"audio","data":"d2F2","mimeType":"audio/wav"},` +
		`{"type":"resource","resource":{"uri":"attachment:///2/report.pdf","mimeType":"application/pdf","blob":"JVBERg=="}},` +
		`{"type":"resource","resource":{"uri":"attachment:///3/notes.txt","mimeType":"text/plain","text":"hello"}},` +
		`{"type":"resource","resource":{"uri":"attachment:///4/file.mp4","mimeType":"video/mp4","blob":"bXA0"}},` +
		`{"type":"resource_link","mimeType":"application/pdf","uri":"https://example.com/a.pdf","name":"a.pdf"},` +
		`{"type":"text","text":"[document: file file-1 of openai]"},` +
		`{"type":"text","text":"more"}]`
	if string(b) != want {
		t.Errorf("modern content =\n%s\nwant\n%s", b, want)
	}

	old, _ := json.Marshal(toolContent("2024-11-05", &model.ToolResult{Parts: []model.Part{
		model.AudioPart("audio/wav", []byte("wav")),
		model.ImageURLPart("https://example.com/a.png"),
	}}))
	want = `[{"type":"resource","resource":{"uri":"attachment:///0/file.wav","mimeType":"audio/wav","blob":"d2F2"}},` +
		`{"type":"text","text":"[image: https://example.com/a.png]"}]`
	if string(old) != want {
		t.Errorf("2024-11-05 content =\n%s\nwant\n%s", old, want)
	}
	if b, _ := json.Marshal(toolContent(ProtocolVersion, &model.ToolResult{})); string(b) != `[{"type":"text"}]` {
		t.Errorf("empty content = %s", b)
	}
}

// TestServerMediaRoundTrip serves a tool returning media and calls it with
// the adapter, so the parts survive both directions in both generations.
func TestServerMediaRoundTrip(t *testing.T) {
	shot := tool.Func("shot", "Screenshot.", func(context.Context, struct{}) (*tool.Result, error) {
		return tool.Media("screen",
			model.ImagePart("image/png", []byte("png")),
			model.AudioPart("audio/wav", []byte("wav")),
			model.DocumentPart("application/pdf", []byte("%PDF"), "r.pdf")), nil
	})
	forEach(t, &ServerOptions{Tools: []tool.Tool{shot}}, nil, func(t *testing.T, _ *srvFixture, c *Client, _ generation) {
		tools, err := Tools(t.Context(), c, "srv", Filter(func(x Tool) bool { return x.Name == "shot" }))
		if err != nil || len(tools) != 1 {
			t.Fatalf("tools = %v, %v", tools, err)
		}
		res, err := tools[0].Call(t.Context(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if res.Content != "screen" || len(res.Parts) != 3 {
			t.Fatalf("result = %+v", res)
		}
		img, audio, doc := res.Parts[0], res.Parts[1], res.Parts[2]
		if img.Type != model.PartImage || string(img.Data) != "png" || img.MediaType != "image/png" ||
			audio.Type != model.PartAudio || string(audio.Data) != "wav" ||
			doc.Type != model.PartDocument || string(doc.Data) != "%PDF" || doc.MediaType != "application/pdf" || doc.Filename != "r.pdf" {
			t.Errorf("parts = %+v", res.Parts)
		}
	})
}

func TestToolResultFrom(t *testing.T) {
	res := ToolResultFrom(&CallToolResult{IsError: true, Content: []Content{
		{Type: "text", Text: "see"},
		{Type: "image", MimeType: "image/png", Data: "aW1n"},
		{Type: "audio", MimeType: "audio/wav", Data: "d2F2"},
		{Type: "resource", Resource: &ResourceContents{URI: "file:///docs/a.pdf", MimeType: "application/pdf", Blob: "JVBERg=="}},
		{Type: "resource", Resource: &ResourceContents{URI: "file:///b.txt", Text: "body"}},
		{Type: "resource_link", Name: "c", URI: "file:///c"},
		{Type: "image", MimeType: "image/png", Data: "!"},
	}})
	if res.Content != "see\nbody\n[resource: c file:///c]\n[image: image/png, 1 bytes base64]" || !res.IsError {
		t.Errorf("content = %q", res.Content)
	}
	if len(res.Parts) != 3 {
		t.Fatalf("parts = %+v", res.Parts)
	}
	img, audio, doc := res.Parts[0], res.Parts[1], res.Parts[2]
	if img.Type != model.PartImage || string(img.Data) != "img" || img.MediaType != "image/png" ||
		audio.Type != model.PartAudio || string(audio.Data) != "wav" || audio.MediaType != "audio/wav" ||
		doc.Type != model.PartDocument || string(doc.Data) != "%PDF" || doc.Filename != "a.pdf" {
		t.Errorf("parts = %+v", res.Parts)
	}
	blob := ToolResultFrom(&CallToolResult{Content: []Content{{Type: "resource", Resource: &ResourceContents{URI: "x:", Blob: "iVBORw0KGgo="}}}})
	if len(blob.Parts) != 1 || blob.Parts[0].Type != model.PartImage || blob.Parts[0].MediaType != "image/png" {
		t.Errorf("sniffed blob = %+v", blob.Parts)
	}
	if s := ToolResultFrom(&CallToolResult{StructuredContent: json.RawMessage(`{"a":1}`)}); s.Content != `{"a":1}` {
		t.Errorf("structured = %q", s.Content)
	}
}
