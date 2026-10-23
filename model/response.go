package model

// FinishReason explains why the model stopped generating.
type FinishReason string

const (
	FinishStop      FinishReason = "stop"
	FinishLength    FinishReason = "length"
	FinishToolCalls FinishReason = "tool_calls"
	// FinishRefusal reports that the provider declined the request.
	FinishRefusal FinishReason = "refusal"
	// FinishContentFilter reports that output was withheld by a filter.
	FinishContentFilter FinishReason = "content_filter"
	// FinishPause reports that a long-running server-side turn was paused and
	// should be continued by sending the conversation again.
	FinishPause   FinishReason = "pause"
	FinishUnknown FinishReason = "unknown"
)

// Response is a complete model output.
type Response struct {
	ID           string       `json:"id,omitempty"`
	Model        string       `json:"model,omitempty"`
	Message      *Message     `json:"message"`
	FinishReason FinishReason `json:"finish_reason"`
	Usage        Usage        `json:"usage"`
}

// Text returns the text of the response message.
func (r *Response) Text() string {
	if r == nil {
		return ""
	}
	return r.Message.Text()
}

// ToolCalls returns the tool calls requested by the response.
func (r *Response) ToolCalls() []*ToolCall {
	if r == nil {
		return nil
	}
	return r.Message.ToolCalls()
}
