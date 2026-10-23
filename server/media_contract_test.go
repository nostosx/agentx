package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
)

func TestDataURLMediaInput(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		url  string
		code string
	}{
		{"parameters", "data:text/plain;charset=utf-8;name=a%3Bb,hello", ""},
		{"invalid type", "data:not-a-type,private-payload", CodeBadRequest},
		{"invalid payload", "data:text/plain;base64,private-payload!", CodeBadRequest},
		{"unsupported inferred type", "data:application/octet-stream,hello", CodeUnsupportedMedia},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, protocol := range []string{"file", "agui"} {
				t.Run(protocol, func(t *testing.T) {
					var part model.Part
					var err *Error
					if protocol == "file" {
						part, err = filePart("", nil, tc.url, "note.txt")
					} else {
						body, marshalErr := json.Marshal(map[string]any{"type": "document", "source": map[string]string{"type": "url", "value": tc.url}})
						if marshalErr != nil {
							t.Fatal(marshalErr)
						}
						var input aguiPart
						if decodeErr := json.Unmarshal(body, &input); decodeErr != nil {
							t.Fatal(decodeErr)
						}
						part, err = aguiMediaPart(input)
					}
					if tc.code != "" {
						if err == nil || err.Code != tc.code {
							t.Fatalf("error = %v, want %s", err, tc.code)
						}
						if strings.Contains(err.Message, "private-payload") {
							t.Fatalf("error exposes payload: %s", err.Message)
						}
						return
					}
					if err != nil || part.MediaType != `text/plain; charset=utf-8; name="a;b"` || string(part.Data) != "hello" || part.URL != "" {
						t.Fatalf("part = %+v, error = %v", part, err)
					}
				})
			}
		})
	}
}

func TestVercelInvalidInlineMedia(t *testing.T) {
	t.Parallel()
	out := encode(t, newVercelEncoder("", nil), []item{
		ev(loop.EventMessage, 1, func(e *loop.Event) {
			e.Message = model.NewMessage(model.RoleAssistant, model.ImagePart("invalid", []byte("private-payload")))
		}),
	})
	if !strings.Contains(out, `"type":"error"`) || !strings.Contains(out, `"finishReason":"error"`) {
		t.Fatalf("missing stream failure: %s", out)
	}
	if strings.Contains(out, `"type":"file"`) || strings.Contains(out, "private-payload") {
		t.Fatalf("invalid media published: %s", out)
	}
}
