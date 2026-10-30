// Package imagegen defines independent image generation and editing contracts.
// Provider implementations live in agentx-contrib. Images use model.Part so
// applications can pass them directly to conversations and tool results.
package imagegen

import (
	"context"
	"iter"
	"time"

	"github.com/nostosx/agentx/model"
)

// Generator creates images from text. Implementations must be safe for concurrent use.
type Generator interface {
	Generate(ctx context.Context, req *Request) (*Response, error)
}

// Editor creates images from a prompt and existing images.
type Editor interface {
	Edit(ctx context.Context, req *EditRequest) (*Response, error)
}

// Streamer generates previews followed by exactly one final response.
// A non-nil error terminates the stream; stopping iteration cancels reading.
type Streamer interface {
	Stream(ctx context.Context, req *Request) iter.Seq2[*StreamEvent, error]
}

// EditStreamer streams previews and a final response for an image edit.
type EditStreamer interface {
	StreamEdit(ctx context.Context, req *EditRequest) iter.Seq2[*StreamEvent, error]
}

// Request describes image generation. Empty options leave provider defaults.
// A request and its referenced data must not be mutated during a call.
type Request struct {
	Model  string `json:"model,omitempty"`
	Prompt string `json:"prompt"`
	N      int    `json:"n,omitempty"`
	Size   string `json:"size,omitempty"`
	// AspectRatio is used by providers with ratio-based image settings.
	AspectRatio  string `json:"aspect_ratio,omitempty"`
	Quality      string `json:"quality,omitempty"`
	OutputFormat string `json:"output_format,omitempty"`
	Background   string `json:"background,omitempty"`
	// OutputCompression is a percentage; nil leaves the provider default,
	// while a pointer to zero explicitly requests zero compression quality.
	OutputCompression *int   `json:"output_compression,omitempty"`
	SafetyIdentifier  string `json:"safety_identifier,omitempty"`
	// PartialImages requests a number of previews when streaming.
	PartialImages int `json:"partial_images,omitempty"`
	// ProviderOptions carries extra fields keyed by provider, as in model.Request.
	// Implementations document which fields may override typed options.
	ProviderOptions map[string]map[string]any `json:"provider_options,omitempty"`
}

// EditRequest edits one or more images. Mask selects the editable area;
// Markup supplies a visual annotation. Providers may not support both together.
type EditRequest struct {
	Request
	Images []model.Part `json:"images"`
	Mask   *model.Part  `json:"mask,omitempty"`
	Markup *model.Part  `json:"markup,omitempty"`
}

// Image is a generated image and, when supplied, the provider's revised prompt.
type Image struct {
	Part          model.Part `json:"part"`
	RevisedPrompt string     `json:"revised_prompt,omitempty"`
}

// Usage preserves image and text token accounting. Absent usage is nil in Response.
type Usage struct {
	InputTokens       int `json:"input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	TotalTokens       int `json:"total_tokens"`
	InputTextTokens   int `json:"input_text_tokens"`
	InputImageTokens  int `json:"input_image_tokens"`
	OutputTextTokens  int `json:"output_text_tokens"`
	OutputImageTokens int `json:"output_image_tokens"`
}

// Response contains complete images and the settings reported by the provider.
// Model is the selected model when the provider does not report a resolved name.
type Response struct {
	Provider     string    `json:"provider"`
	Model        string    `json:"model"`
	RequestID    string    `json:"request_id,omitempty"`
	Created      time.Time `json:"created,omitzero"`
	Images       []Image   `json:"images"`
	Size         string    `json:"size,omitempty"`
	Quality      string    `json:"quality,omitempty"`
	Background   string    `json:"background,omitempty"`
	OutputFormat string    `json:"output_format,omitempty"`
	Usage        *Usage    `json:"usage,omitempty"`
}

// StreamEvent contains either a preview Image or the final Response.
// A preview is not a completed result and must not be treated as one.
type StreamEvent struct {
	Image        *Image    `json:"image,omitempty"`
	PartialIndex int       `json:"partial_index,omitempty"`
	Response     *Response `json:"response,omitempty"`
}
