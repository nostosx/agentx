package model

import (
	"bytes"
	"context"
	"errors"
	"iter"
)

// StreamEventType identifies a streaming event.
type StreamEventType string

const (
	// StreamDelta carries an incremental piece of a message part.
	StreamDelta StreamEventType = "delta"
	// StreamDone is the final event and carries the complete response.
	StreamDone StreamEventType = "done"
)

// StreamEvent is one event of a streaming response.
type StreamEvent struct {
	Type StreamEventType `json:"type"`
	// Index is the position of the part that Delta extends.
	Index int `json:"index"`
	// Delta is a partial part. Text, Signature and ToolCall.Arguments are
	// appended to the part at Index; other fields are set when first seen.
	Delta *Part `json:"delta,omitempty"`
	// Response is set on StreamDone.
	Response *Response `json:"response,omitempty"`
}

// Accumulator assembles stream deltas into a complete message.
type Accumulator struct {
	parts []*Part
	index map[int]int
}

// Add merges a delta event into the accumulated message.
func (a *Accumulator) Add(ev *StreamEvent) {
	if ev == nil || ev.Type != StreamDelta || ev.Delta == nil {
		return
	}
	if a.index == nil {
		a.index = make(map[int]int)
	}
	pos, ok := a.index[ev.Index]
	if !ok {
		p := ev.Delta.clone()
		a.index[ev.Index] = len(a.parts)
		a.parts = append(a.parts, &p)
		return
	}
	p := a.parts[pos]
	d := ev.Delta
	p.Text += d.Text
	p.Signature += d.Signature
	if d.Redacted {
		p.Redacted = true
	}
	if len(d.Data) > 0 {
		p.Data = append(p.Data, d.Data...)
	}
	if d.MediaType != "" {
		p.MediaType = d.MediaType
	}
	if d.URL != "" {
		p.URL = d.URL
	}
	if d.FileID != "" {
		p.FileID = d.FileID
	}
	if d.Filename != "" {
		p.Filename = d.Filename
	}
	if d.Detail != "" {
		p.Detail = d.Detail
	}
	if d.Citations {
		p.Citations = true
	}
	if len(d.Raw) > 0 {
		p.Raw = bytes.Clone(d.Raw)
	}
	if d.Provider != "" {
		p.Provider = d.Provider
	}
	if d.ToolCall != nil {
		if p.ToolCall == nil {
			p.ToolCall = &ToolCall{}
		}
		if d.ToolCall.ID != "" {
			p.ToolCall.ID = d.ToolCall.ID
		}
		if d.ToolCall.Name != "" {
			p.ToolCall.Name = d.ToolCall.Name
		}
		p.ToolCall.Arguments = append(p.ToolCall.Arguments, d.ToolCall.Arguments...)
	}
}

// Message returns the accumulated assistant message.
func (a *Accumulator) Message() *Message {
	m := &Message{Role: RoleAssistant, Parts: make([]Part, 0, len(a.parts))}
	for _, p := range a.parts {
		part := p.clone()
		if part.Type == PartToolCall && part.ToolCall != nil && len(part.ToolCall.Arguments) == 0 {
			part.ToolCall.Arguments = []byte("{}")
		}
		m.Parts = append(m.Parts, part)
	}
	return m
}

// Collect consumes a stream and returns its final response. A done event
// without a response or without a message gets the message accumulated from
// the deltas. Nil events are skipped.
func Collect(stream iter.Seq2[*StreamEvent, error]) (*Response, error) {
	var acc Accumulator
	for ev, err := range stream {
		if err != nil {
			return nil, err
		}
		if ev == nil {
			continue
		}
		if ev.Type == StreamDone {
			resp := ev.Response
			if resp == nil {
				resp = &Response{FinishReason: FinishUnknown}
			}
			if resp.Message == nil {
				resp.Message = acc.Message()
			}
			return resp, nil
		}
		acc.Add(ev)
	}
	return nil, errors.New("model: stream ended without a done event")
}

// errNoResponse reports a nil response without an error.
var errNoResponse = errors.New("model: no response")

// StreamFromResponse converts a complete response into a stream with one
// delta per part followed by the done event. A nil response yields an
// error.
func StreamFromResponse(resp *Response) iter.Seq2[*StreamEvent, error] {
	return func(yield func(*StreamEvent, error) bool) {
		if resp == nil {
			yield(nil, errNoResponse)
			return
		}
		if resp.Message != nil {
			for i := range resp.Message.Parts {
				p := resp.Message.Parts[i].clone()
				if !yield(&StreamEvent{Type: StreamDelta, Index: i, Delta: &p}, nil) {
					return
				}
			}
		}
		yield(&StreamEvent{Type: StreamDone, Response: resp}, nil)
	}
}

// Func adapts a generate function into a Model. Stream is derived from
// Generate.
type Func func(ctx context.Context, req *Request) (*Response, error)

// Generate calls f.
func (f Func) Generate(ctx context.Context, req *Request) (*Response, error) { return f(ctx, req) }

// Stream calls f and streams its response. A nil response without an
// error yields an error.
func (f Func) Stream(ctx context.Context, req *Request) iter.Seq2[*StreamEvent, error] {
	return func(yield func(*StreamEvent, error) bool) {
		resp, err := f(ctx, req)
		if err == nil && resp == nil {
			err = errNoResponse
		}
		if err != nil {
			yield(nil, err)
			return
		}
		for ev, err := range StreamFromResponse(resp) {
			if !yield(ev, err) {
				return
			}
		}
	}
}
