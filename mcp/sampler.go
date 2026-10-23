package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/nostosx/agentx/model"
)

// ModelSampler returns a SamplingHandler that generates with m. It converts
// the request with ModelRequest and the response with SamplingResultFrom.
// Model preferences are advisory and ignored, as are includeContext and
// metadata; to pick a model from the hints, set the Model field of the
// converted request in a handler of your own. Pair it with ApproveSampling
// so that a human can review what servers ask the model.
func ModelSampler(m model.Model) SamplingHandler {
	return func(ctx context.Context, req *SamplingRequest) (*SamplingResult, error) {
		mreq, err := req.ModelRequest()
		if err != nil {
			return nil, err
		}
		resp, err := m.Generate(ctx, mreq)
		if err != nil {
			return nil, err
		}
		return SamplingResultFrom(resp)
	}
}

// ModelRequest converts the sampling request into a model request: the
// system prompt, messages with text, image, audio, tool use and tool
// result content, maxTokens, temperature, stop sequences, tools and the
// tool choice. Media in tool results becomes the parts of the result, as
// in ToolResultFrom.
func (r *SamplingRequest) ModelRequest() (*model.Request, error) {
	req := &model.Request{
		System:      r.SystemPrompt,
		MaxTokens:   r.MaxTokens,
		Temperature: r.Temperature,
		Stop:        r.StopSequences,
	}
	for i, m := range r.Messages {
		msg, err := modelMessage(m)
		if err != nil {
			return nil, fmt.Errorf("mcp: sampling message %d: %w", i, err)
		}
		req.Messages = append(req.Messages, msg)
	}
	for _, t := range r.Tools {
		req.Tools = append(req.Tools, &model.ToolSpec{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		})
	}
	if r.ToolChoice != nil {
		switch r.ToolChoice.Mode {
		case "", "auto":
			req.ToolChoice = &model.ToolChoice{Mode: model.ToolChoiceAuto}
		case "required":
			req.ToolChoice = &model.ToolChoice{Mode: model.ToolChoiceRequired}
		case "none":
			req.ToolChoice = &model.ToolChoice{Mode: model.ToolChoiceNone}
		default:
			return nil, fmt.Errorf("mcp: unknown tool choice mode %q", r.ToolChoice.Mode)
		}
	}
	return req, nil
}

func modelMessage(m SamplingMessage) (*model.Message, error) {
	var role model.Role
	switch m.Role {
	case "user":
		role = model.RoleUser
	case "assistant":
		role = model.RoleAssistant
	default:
		return nil, fmt.Errorf("unknown role %q", m.Role)
	}
	msg := &model.Message{Role: role}
	for _, b := range m.Content {
		switch b.Type {
		case "text":
			msg.Parts = append(msg.Parts, model.TextPart(b.Text))
		case "image", "audio":
			data, err := base64.StdEncoding.DecodeString(b.Data)
			if err != nil {
				return nil, fmt.Errorf("%s data: %w", b.Type, err)
			}
			if b.Type == "image" {
				msg.Parts = append(msg.Parts, model.ImagePart(b.MimeType, data))
			} else {
				msg.Parts = append(msg.Parts, model.AudioPart(b.MimeType, data))
			}
		case "tool_use":
			if role != model.RoleAssistant {
				return nil, fmt.Errorf("tool_use in a %s message", m.Role)
			}
			args := b.Input
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			msg.Parts = append(msg.Parts, model.Part{Type: model.PartToolCall,
				ToolCall: &model.ToolCall{ID: b.ID, Name: b.Name, Arguments: args}})
		case "tool_result":
			if role != model.RoleUser {
				return nil, fmt.Errorf("tool_result in a %s message", m.Role)
			}
			// Tool results travel in a tool message of their own; the server
			// may not mix them with other content.
			msg.Role = model.RoleTool
			res := ToolResultFrom(&CallToolResult{Content: b.Content, StructuredContent: b.StructuredContent})
			msg.Parts = append(msg.Parts, model.Part{Type: model.PartToolResult,
				ToolResult: &model.ToolResult{CallID: b.ToolUseID, Content: res.Content, Parts: res.Parts, IsError: b.IsError}})
		default:
			return nil, fmt.Errorf("%s content is not supported", b.Type)
		}
	}
	return msg, nil
}

// SamplingResultFrom converts a model response into a sampling result:
// text, image, audio and tool call parts become text, image, audio and
// tool_use blocks,
// and the finish reason becomes the stop reason (endTurn, maxTokens or
// toolUse; other reasons pass through as they are).
func SamplingResultFrom(resp *model.Response) (*SamplingResult, error) {
	if resp == nil || resp.Message == nil {
		return nil, fmt.Errorf("mcp: model returned no message")
	}
	res := &SamplingResult{Role: "assistant", Model: resp.Model}
	tools := false
	for _, p := range resp.Message.Parts {
		switch p.Type {
		case model.PartText:
			res.Content = append(res.Content, SamplingContent{Type: "text", Text: p.Text})
		case model.PartImage, model.PartAudio:
			if len(p.Data) == 0 {
				return nil, fmt.Errorf("mcp: %s by URL cannot be returned to the server", p.Type)
			}
			res.Content = append(res.Content, SamplingContent{Type: string(p.Type),
				Data: base64.StdEncoding.EncodeToString(p.Data), MimeType: p.MediaType})
		case model.PartToolCall:
			if p.ToolCall == nil {
				continue
			}
			tools = true
			input := p.ToolCall.Arguments
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			res.Content = append(res.Content, SamplingContent{Type: "tool_use",
				ID: p.ToolCall.ID, Name: p.ToolCall.Name, Input: input})
		}
	}
	if len(res.Content) == 0 {
		res.Content = SamplingContents{{Type: "text"}}
	}
	switch {
	case tools || resp.FinishReason == model.FinishToolCalls:
		res.StopReason = StopToolUse
	case resp.FinishReason == model.FinishStop:
		res.StopReason = StopEndTurn
	case resp.FinishReason == model.FinishLength:
		res.StopReason = StopMaxTokens
	case resp.FinishReason != "":
		res.StopReason = string(resp.FinishReason)
	}
	return res, nil
}
