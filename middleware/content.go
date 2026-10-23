package middleware

import (
	"encoding/json"

	"github.com/nostosx/agentx/model"
)

// The content attributes follow the JSON schemas of the GenAI semantic
// conventions for gen_ai.input.messages, gen_ai.output.messages,
// gen_ai.system_instructions and gen_ai.tool.definitions. Spans carry them
// as JSON strings, as the conventions ask where spans do not support
// structured attributes.

// contentPart is a message part of the conventions' schemas.
type contentPart struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Response  *string         `json:"response,omitempty"`
	Modality  string          `json:"modality,omitempty"`
	MIMEType  string          `json:"mime_type,omitempty"`
	URI       string          `json:"uri,omitempty"`
	FileID    string          `json:"file_id,omitempty"`
	// Content is the text of text and reasoning parts, or the data of a
	// blob part, which encoding/json encodes as base64.
	Content any `json:"content,omitempty"`
}

type contentMessage struct {
	Role  string        `json:"role"`
	Parts []contentPart `json:"parts"`
	Name  string        `json:"name,omitempty"`
}

func contentParts(parts []model.Part) []contentPart {
	out := make([]contentPart, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case model.PartText:
			out = append(out, contentPart{Type: "text", Content: &p.Text})
		case model.PartReasoning:
			// Redacted reasoning keeps its encrypted data out of telemetry.
			out = append(out, contentPart{Type: "reasoning", Content: &p.Text})
		case model.PartImage, model.PartDocument, model.PartAudio, model.PartVideo:
			// The conventions' modalities are image, video and audio;
			// documents use their part type.
			modality := string(p.Type)
			switch {
			case len(p.Data) > 0:
				out = append(out, contentPart{Type: "blob", Modality: modality, MIMEType: p.MediaType, Content: p.Data})
			case p.URL != "":
				out = append(out, contentPart{Type: "uri", Modality: modality, MIMEType: p.MediaType, URI: p.URL})
			case p.FileID != "":
				out = append(out, contentPart{Type: "file", Modality: modality, MIMEType: p.MediaType, FileID: p.FileID})
			}
		case model.PartToolCall:
			if c := p.ToolCall; c != nil {
				out = append(out, contentPart{Type: "tool_call", ID: c.ID, Name: c.Name, Arguments: jsonArguments(c.Arguments)})
			}
		case model.PartToolResult:
			if r := p.ToolResult; r != nil {
				out = append(out, contentPart{Type: "tool_call_response", ID: r.CallID, Response: &r.Content})
			}
		}
		// Raw parts are opaque provider blocks and are not recorded.
	}
	return out
}

// jsonArguments returns tool call arguments as JSON: the arguments
// themselves when they are valid JSON, else a JSON string holding them.
func jsonArguments(args json.RawMessage) json.RawMessage {
	if len(args) == 0 {
		return nil
	}
	if json.Valid(args) {
		return args
	}
	b, _ := json.Marshal(string(args))
	return b
}

func contentMessages(msgs []*model.Message) []contentMessage {
	out := make([]contentMessage, 0, len(msgs))
	for _, m := range msgs {
		if m == nil {
			continue
		}
		out = append(out, contentMessage{Role: string(m.Role), Parts: contentParts(m.Parts), Name: m.Name})
	}
	return out
}

func marshalContent(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// requestContent returns the opt-in content attributes of a model request.
func requestContent(system string, msgs []*model.Message, tools []*model.ToolSpec) []Attr {
	var attrs []Attr
	if system != "" {
		attrs = append(attrs, Attr{"gen_ai.system_instructions", marshalContent([]contentPart{{Type: "text", Content: &system}})})
	}
	attrs = append(attrs, Attr{"gen_ai.input.messages", marshalContent(contentMessages(msgs))})
	if len(tools) > 0 {
		// The conventions advise against recording descriptions and
		// parameters by default, as they can be large.
		type definition struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		defs := make([]definition, len(tools))
		for i, t := range tools {
			defs[i] = definition{Type: "function", Name: t.Name}
		}
		attrs = append(attrs, Attr{"gen_ai.tool.definitions", marshalContent(defs)})
	}
	return attrs
}

// outputContent returns gen_ai.output.messages for msg.
func outputContent(msg *model.Message) []Attr {
	if msg == nil {
		return nil
	}
	out := contentMessage{Role: string(msg.Role), Parts: contentParts(msg.Parts), Name: msg.Name}
	if out.Role == "" {
		out.Role = string(model.RoleAssistant)
	}
	return []Attr{{"gen_ai.output.messages", marshalContent([]contentMessage{out})}}
}
