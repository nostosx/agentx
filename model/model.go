// Package model defines the provider-neutral interface to large language
// models: messages, requests, responses, streaming and token usage.
package model

import (
	"context"
	"encoding/json"
	"iter"
	"maps"
	"slices"
)

// Model generates responses from a conversation.
//
// Implementations must be safe for concurrent use.
type Model interface {
	// Generate returns a complete response.
	Generate(ctx context.Context, req *Request) (*Response, error)
	// Stream yields incremental events and finishes with a StreamDone event
	// that carries the complete response. A non-nil error ends the stream.
	Stream(ctx context.Context, req *Request) iter.Seq2[*StreamEvent, error]
}

// Request is a provider-neutral generation request.
type Request struct {
	// Model overrides the provider's configured model name.
	Model string `json:"model,omitempty"`
	// System is the system prompt.
	System   string     `json:"system,omitempty"`
	Messages []*Message `json:"messages"`

	Tools      []*ToolSpec `json:"tools,omitempty"`
	ToolChoice *ToolChoice `json:"tool_choice,omitempty"`

	MaxTokens   int      `json:"max_tokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	Stop        []string `json:"stop,omitempty"`

	Reasoning      *Reasoning      `json:"reasoning,omitempty"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`

	// Extra is merged into the provider request body, overriding generated
	// fields with the same name. Use it for provider-specific parameters.
	Extra map[string]any `json:"extra,omitempty"`
}

// Clone copies the request's Messages, Tools and Stop slices and Extra map.
// Messages, tool specs, option pointers and values in Extra remain shared.
func (r *Request) Clone() *Request {
	if r == nil {
		return nil
	}
	c := *r
	c.Messages = slices.Clone(r.Messages)
	c.Tools = slices.Clone(r.Tools)
	c.Stop = slices.Clone(r.Stop)
	c.Extra = maps.Clone(r.Extra)
	return &c
}

// ToolSpec describes a tool the model may call.
type ToolSpec struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Parameters is a JSON Schema object describing the arguments.
	Parameters json.RawMessage `json:"parameters,omitempty"`
	// Strict asks providers that support it to guarantee schema-valid
	// arguments.
	Strict bool `json:"strict,omitempty"`
	// Grammar, when set, makes the tool freeform where the provider
	// supports it, as the OpenAI Responses API does: the model writes the
	// input as plain text matching the grammar instead of JSON, and the
	// provider passes the text as the string argument FreeformInput.
	// Other providers send Parameters, which must declare that argument.
	Grammar *ToolGrammar `json:"grammar,omitempty"`
}

// FreeformInput is the argument that carries the text input of a call to a
// freeform tool; see ToolSpec.Grammar.
const FreeformInput = "input"

// ToolGrammar constrains the text input of a freeform tool.
type ToolGrammar struct {
	// Syntax is the grammar language, "lark" or "regex".
	Syntax string `json:"syntax"`
	// Definition is the grammar. An empty definition accepts any text.
	Definition string `json:"definition,omitempty"`
}

// ToolChoiceMode controls whether and which tools the model must call.
type ToolChoiceMode string

const (
	ToolChoiceAuto     ToolChoiceMode = "auto"
	ToolChoiceNone     ToolChoiceMode = "none"
	ToolChoiceRequired ToolChoiceMode = "required"
	ToolChoiceTool     ToolChoiceMode = "tool"
)

// ToolChoice selects the tool-calling mode. Name is used with ToolChoiceTool.
//
// Some recent models reject forced tool use (required or a named tool); use
// auto with a prompt instruction for those models.
type ToolChoice struct {
	Mode ToolChoiceMode `json:"mode"`
	Name string         `json:"name,omitempty"`
}

// Reasoning configures model reasoning ("thinking").
type Reasoning struct {
	// Effort is a provider-defined level such as "low", "medium" or "high".
	Effort string `json:"effort,omitempty"`
	// Display controls whether reasoning text is returned, for providers that
	// support it ("summarized" or "omitted").
	Display string `json:"display,omitempty"`
	// BudgetTokens limits reasoning tokens for providers that use budgets.
	BudgetTokens int `json:"budget_tokens,omitempty"`
}

// ResponseFormat constrains the response to JSON matching Schema.
type ResponseFormat struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
	Strict bool            `json:"strict,omitempty"`
}

// Description identifies a model for telemetry. Names follow the
// OpenTelemetry GenAI semantic conventions.
type Description struct {
	// Provider is the gen_ai.provider.name, such as "openai", "anthropic"
	// or "gcp.gemini".
	Provider string
	// Operation is the gen_ai.operation.name of a call, such as "chat" or
	// "generate_content".
	Operation string
	// Model is the model used when a request names none.
	Model string
	// ServerAddress and ServerPort locate the API endpoint.
	ServerAddress string
	ServerPort    int
}

// Describer is implemented by models that describe themselves for
// telemetry. The agentx providers implement it, and model wrappers should
// forward it.
type Describer interface {
	Describe() Description
}

// Describe returns m's description, or a zero Description if m does not
// implement Describer.
func Describe(m Model) Description {
	if d, ok := m.(Describer); ok {
		return d.Describe()
	}
	return Description{}
}
