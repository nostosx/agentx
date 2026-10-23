package model

import (
	"bytes"
	"encoding/json"
	"maps"
	"strings"
)

// Role identifies the author of a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	// RoleTool marks a message that carries tool results back to the model.
	RoleTool Role = "tool"
)

// PartType identifies the kind of content held by a Part.
type PartType string

const (
	PartText  PartType = "text"
	PartImage PartType = "image"
	// PartDocument is a document, such as a PDF or a plain text file.
	PartDocument PartType = "document"
	// PartAudio is an audio clip.
	PartAudio PartType = "audio"
	// PartVideo is a video clip.
	PartVideo      PartType = "video"
	PartReasoning  PartType = "reasoning"
	PartToolCall   PartType = "tool_call"
	PartToolResult PartType = "tool_result"
	// PartRaw carries a provider-specific content block that has no neutral
	// representation. Providers echo raw parts they produced back verbatim and
	// ignore raw parts from other providers.
	PartRaw PartType = "raw"
)

// Part is one piece of message content.
type Part struct {
	Type PartType `json:"type"`

	// Text holds text content, the visible reasoning for PartReasoning, or
	// the transcript of audio a model produced.
	Text string `json:"text,omitempty"`

	// MediaType, Data and URL describe an image, document, audio or video
	// part: its content is either Data, a URL or FileID. Data is encoded as
	// base64 in JSON.
	MediaType string `json:"media_type,omitempty"`
	Data      []byte `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
	// FileID refers to a file uploaded to the provider's Files API, such
	// as an OpenAI or Anthropic file ID, in place of Data or URL. Gemini
	// files are referenced by their URI in URL. Set Provider to the
	// provider that issued the ID, so other providers reject the part
	// instead of sending an ID they do not know.
	FileID string `json:"file_id,omitempty"`
	// Filename names a document, audio or video part. Providers send it as
	// the file name or title where the API takes one.
	Filename string `json:"filename,omitempty"`
	// Detail is the resolution at which providers that support it process
	// an image or video: "low", "medium", "high" or "auto". It maps to
	// OpenAI's detail and Gemini's mediaResolution.
	Detail string `json:"detail,omitempty"`
	// Citations asks providers that support it, Anthropic and Bedrock, to
	// cite a document part in the answer.
	Citations bool `json:"citations,omitempty"`

	// Signature is an opaque provider token that must be returned unchanged
	// with the part, such as a reasoning signature.
	Signature string `json:"signature,omitempty"`
	// Redacted marks reasoning whose content is encrypted in Data.
	Redacted bool `json:"redacted,omitempty"`

	ToolCall   *ToolCall   `json:"tool_call,omitempty"`
	ToolResult *ToolResult `json:"tool_result,omitempty"`

	// Provider and Raw hold a PartRaw block. Provider also names the
	// provider that issued Signature, when the provider records it, so
	// signatures are not sent to providers that cannot verify them.
	Provider string          `json:"provider,omitempty"`
	Raw      json.RawMessage `json:"raw,omitempty"`
}

// ToolCall is a model request to invoke a tool.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// ToolResult is the outcome of a tool call.
type ToolResult struct {
	CallID  string `json:"call_id"`
	Name    string `json:"name,omitempty"`
	Content string `json:"content"`
	// Parts holds content that follows Content, such as the images and
	// documents a tool returned. Providers put them in the tool result
	// where the API allows it, or else in a user message after the
	// results; see their package documentation.
	Parts   []Part `json:"parts,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
	// Metadata is what the tool reported for the application in
	// tool.Result.Metadata. It is not sent to models, and is stored with
	// the conversation, so its values should be encodable as JSON.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Message is a single conversation turn.
type Message struct {
	Role     Role           `json:"role"`
	Parts    []Part         `json:"parts"`
	Name     string         `json:"name,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// TextPart returns a text part.
func TextPart(text string) Part { return Part{Type: PartText, Text: text} }

// ImagePart returns an inline image part.
func ImagePart(mediaType string, data []byte) Part {
	return Part{Type: PartImage, MediaType: mediaType, Data: data}
}

// ImageURLPart returns an image part referenced by URL.
func ImageURLPart(url string) Part { return Part{Type: PartImage, URL: url} }

// DocumentPart returns an inline document part, such as a PDF, named
// filename. The name may be empty.
func DocumentPart(mediaType string, data []byte, filename string) Part {
	return Part{Type: PartDocument, MediaType: mediaType, Data: data, Filename: filename}
}

// TextDocumentPart returns a plain text document part titled title.
// Providers with document support, such as Anthropic and Bedrock, can
// cite it.
func TextDocumentPart(title, text string) Part {
	return DocumentPart("text/plain", []byte(text), title)
}

// DocumentURLPart returns a document part referenced by URL.
func DocumentURLPart(url string) Part { return Part{Type: PartDocument, URL: url} }

// AudioPart returns an inline audio part.
func AudioPart(mediaType string, data []byte) Part {
	return Part{Type: PartAudio, MediaType: mediaType, Data: data}
}

// AudioURLPart returns an audio part referenced by URL.
func AudioURLPart(url string) Part { return Part{Type: PartAudio, URL: url} }

// VideoPart returns an inline video part.
func VideoPart(mediaType string, data []byte) Part {
	return Part{Type: PartVideo, MediaType: mediaType, Data: data}
}

// VideoURLPart returns a video part referenced by URL.
func VideoURLPart(url string) Part { return Part{Type: PartVideo, URL: url} }

// FilePart returns a part of type t, such as PartDocument or PartImage,
// referring to the file fileID that was uploaded to provider's Files API.
// mediaType may be empty when the provider knows the file's type.
func FilePart(t PartType, provider, fileID, mediaType string) Part {
	return Part{Type: t, Provider: provider, FileID: fileID, MediaType: mediaType}
}

// MediaPart returns an inline part whose type follows mediaType: image/*
// becomes an image part, audio/* an audio part, video/* a video part and
// any other type a document part.
func MediaPart(mediaType string, data []byte) Part {
	return Part{Type: MediaPartType(mediaType), MediaType: mediaType, Data: data}
}

// MediaPartType returns the part type for content of mediaType; see
// MediaPart.
func MediaPartType(mediaType string) PartType {
	major, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(mediaType)), "/")
	switch major {
	case "image":
		return PartImage
	case "audio":
		return PartAudio
	case "video":
		return PartVideo
	}
	return PartDocument
}

// IsMedia reports whether p is an image, document, audio or video part.
func (p Part) IsMedia() bool {
	switch p.Type {
	case PartImage, PartDocument, PartAudio, PartVideo:
		return true
	}
	return false
}

// NewMessage returns a message with the given role and parts.
func NewMessage(role Role, parts ...Part) *Message {
	return &Message{Role: role, Parts: parts}
}

// UserText returns a user message containing text.
func UserText(text string) *Message { return NewMessage(RoleUser, TextPart(text)) }

// AssistantText returns an assistant message containing text.
func AssistantText(text string) *Message { return NewMessage(RoleAssistant, TextPart(text)) }

// SystemText returns a system message containing text.
func SystemText(text string) *Message { return NewMessage(RoleSystem, TextPart(text)) }

// ToolResults returns a tool message carrying the given results.
func ToolResults(results ...*ToolResult) *Message {
	m := &Message{Role: RoleTool}
	for _, r := range results {
		m.Parts = append(m.Parts, Part{Type: PartToolResult, ToolResult: r})
	}
	return m
}

// Text concatenates the message's text parts.
func (m *Message) Text() string {
	if m == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range m.Parts {
		if p.Type == PartText {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// ToolCalls returns the tool calls in the message, in order.
func (m *Message) ToolCalls() []*ToolCall {
	if m == nil {
		return nil
	}
	var calls []*ToolCall
	for _, p := range m.Parts {
		if p.Type == PartToolCall && p.ToolCall != nil {
			calls = append(calls, p.ToolCall)
		}
	}
	return calls
}

// ToolResults returns the tool results in the message, in order.
func (m *Message) ToolResults() []*ToolResult {
	if m == nil {
		return nil
	}
	var results []*ToolResult
	for _, p := range m.Parts {
		if p.Type == PartToolResult && p.ToolResult != nil {
			results = append(results, p.ToolResult)
		}
	}
	return results
}

// Clone copies the message and its parts, including media data and tool
// payloads. Metadata maps are copied shallowly: their values remain shared.
// A nil receiver returns nil.
func (m *Message) Clone() *Message {
	if m == nil {
		return nil
	}
	c := *m
	c.Parts = make([]Part, len(m.Parts))
	for i, p := range m.Parts {
		c.Parts[i] = p.clone()
	}
	c.Metadata = maps.Clone(m.Metadata)
	return &c
}

func (p Part) clone() Part {
	p.Data = bytes.Clone(p.Data)
	p.Raw = bytes.Clone(p.Raw)
	if p.ToolCall != nil {
		tc := *p.ToolCall
		tc.Arguments = bytes.Clone(tc.Arguments)
		p.ToolCall = &tc
	}
	if p.ToolResult != nil {
		tr := *p.ToolResult
		if tr.Parts != nil {
			tr.Parts = make([]Part, len(p.ToolResult.Parts))
			for i, rp := range p.ToolResult.Parts {
				tr.Parts[i] = rp.clone()
			}
		}
		tr.Metadata = maps.Clone(tr.Metadata)
		p.ToolResult = &tr
	}
	return p
}

// CloneMessages copies a message slice, cloning each message with Message.Clone.
func CloneMessages(msgs []*Message) []*Message {
	if msgs == nil {
		return nil
	}
	out := make([]*Message, len(msgs))
	for i, m := range msgs {
		out[i] = m.Clone()
	}
	return out
}
