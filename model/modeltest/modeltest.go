// Package modeltest provides a scripted model for tests.
package modeltest

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"slices"
	"sync"

	"github.com/nostosx/agentx/model"
)

// ErrExhausted is returned when the script has no more steps.
var ErrExhausted = errors.New("modeltest: script exhausted")

// Step produces the response for one model call.
type Step func(req *model.Request) (*model.Response, error)

// Model replays a fixed script of steps and records every request.
type Model struct {
	mu       sync.Mutex
	steps    []Step
	requests []*model.Request
}

// New returns a model that answers calls with steps, in order.
func New(steps ...Step) *Model { return &Model{steps: steps} }

// Append adds steps to the script.
func (m *Model) Append(steps ...Step) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.steps = append(m.steps, steps...)
}

// Requests returns copies of the requests received so far.
func (m *Model) Requests() []*model.Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.requests)
}

func (m *Model) next(req *model.Request) (*model.Response, error) {
	m.mu.Lock()
	c := req.Clone()
	c.Messages = model.CloneMessages(req.Messages)
	m.requests = append(m.requests, c)
	if len(m.steps) == 0 {
		m.mu.Unlock()
		return nil, ErrExhausted
	}
	step := m.steps[0]
	m.steps = m.steps[1:]
	m.mu.Unlock()
	return step(req)
}

// Generate returns the next scripted response.
func (m *Model) Generate(ctx context.Context, req *model.Request) (*model.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return m.next(req)
}

// Stream streams the next scripted response. Text parts are split into
// several deltas so callers see incremental output. A step that returns no
// response and no error makes the stream fail.
func (m *Model) Stream(ctx context.Context, req *model.Request) iter.Seq2[*model.StreamEvent, error] {
	return func(yield func(*model.StreamEvent, error) bool) {
		resp, err := m.Generate(ctx, req)
		if err == nil && resp == nil {
			err = errors.New("modeltest: step returned no response")
		}
		if err != nil {
			yield(nil, err)
			return
		}
		var parts []model.Part
		if resp.Message != nil {
			parts = resp.Message.Parts
		}
		for i, p := range parts {
			if p.Type == model.PartText && len(p.Text) > 1 {
				runes := []rune(p.Text)
				half := len(runes) / 2
				first := p
				first.Text = string(runes[:half])
				if !yield(&model.StreamEvent{Type: model.StreamDelta, Index: i, Delta: &first}, nil) {
					return
				}
				rest := &model.Part{Type: model.PartText, Text: string(runes[half:])}
				if !yield(&model.StreamEvent{Type: model.StreamDelta, Index: i, Delta: rest}, nil) {
					return
				}
				continue
			}
			part := p
			if !yield(&model.StreamEvent{Type: model.StreamDelta, Index: i, Delta: &part}, nil) {
				return
			}
		}
		yield(&model.StreamEvent{Type: model.StreamDone, Response: resp}, nil)
	}
}

// Reply returns a step that answers with text.
func Reply(text string) Step {
	return func(*model.Request) (*model.Response, error) {
		return &model.Response{
			Message:      model.AssistantText(text),
			FinishReason: model.FinishStop,
			Usage:        model.Usage{InputTokens: 10, OutputTokens: 5},
		}, nil
	}
}

// Call describes a tool call made by CallTools.
type Call struct {
	ID   string
	Name string
	Args any
}

// CallTools returns a step that requests the given tool calls.
func CallTools(calls ...Call) Step {
	return func(*model.Request) (*model.Response, error) {
		msg := &model.Message{Role: model.RoleAssistant}
		for _, c := range calls {
			args, err := json.Marshal(c.Args)
			if err != nil {
				return nil, err
			}
			if c.Args == nil {
				args = []byte("{}")
			}
			msg.Parts = append(msg.Parts, model.Part{
				Type:     model.PartToolCall,
				ToolCall: &model.ToolCall{ID: c.ID, Name: c.Name, Arguments: args},
			})
		}
		return &model.Response{
			Message:      msg,
			FinishReason: model.FinishToolCalls,
			Usage:        model.Usage{InputTokens: 10, OutputTokens: 5},
		}, nil
	}
}

// Fail returns a step that fails with err.
func Fail(err error) Step {
	return func(*model.Request) (*model.Response, error) { return nil, err }
}

// Respond returns a step that answers with resp.
func Respond(resp *model.Response) Step {
	return func(*model.Request) (*model.Response, error) { return resp, nil }
}
