package model_test

import (
	"context"
	"errors"
	"testing"

	"github.com/nostosx/agentx/model"
)

func TestAccumulator(t *testing.T) {
	t.Parallel()
	var acc model.Accumulator
	acc.Add(nil)
	acc.Add(&model.StreamEvent{Type: model.StreamDone})
	add := func(index int, part model.Part) {
		acc.Add(&model.StreamEvent{Type: model.StreamDelta, Index: index, Delta: &part})
	}
	add(4, model.Part{Type: model.PartReasoning, Text: "think", Signature: "sig"})
	add(9, model.Part{Type: model.PartToolCall, ToolCall: &model.ToolCall{ID: "c", Name: "lookup", Arguments: []byte(`{"q":`)}})
	add(4, model.Part{Type: model.PartReasoning, Text: " more", Signature: "nature"})
	add(9, model.Part{Type: model.PartToolCall, ToolCall: &model.ToolCall{Arguments: []byte(`"go"}`)}})
	add(10, model.Part{Type: model.PartToolCall, ToolCall: &model.ToolCall{ID: "empty"}})
	msg := acc.Message()
	if len(msg.Parts) != 3 || msg.Parts[0].Text != "think more" || msg.Parts[0].Signature != "signature" || string(msg.ToolCalls()[0].Arguments) != `{"q":"go"}` || string(msg.ToolCalls()[1].Arguments) != `{}` {
		t.Fatalf("unexpected assembled message: %#v", msg)
	}
	msg.Parts[1].ToolCall.Arguments[2] = 'x'
	if string(acc.Message().ToolCalls()[0].Arguments) != `{"q":"go"}` {
		t.Fatal("returned message aliases accumulator")
	}
}

func TestAccumulatorMedia(t *testing.T) {
	t.Parallel()
	var acc model.Accumulator
	add := func(part model.Part) {
		acc.Add(&model.StreamEvent{Type: model.StreamDelta, Index: 0, Delta: &part})
	}
	add(model.Part{Type: model.PartAudio, Data: []byte{1}, MediaType: "audio/pcm", Text: "he"})
	add(model.Part{Data: []byte{2}, Text: "llo", FileID: "file-1", Filename: "a.wav", Detail: "low", Citations: true})
	p := acc.Message().Parts[0]
	if p.Type != model.PartAudio || string(p.Data) != "\x01\x02" || p.MediaType != "audio/pcm" || p.Text != "hello" || p.FileID != "file-1" || p.Filename != "a.wav" || p.Detail != "low" || !p.Citations {
		t.Fatalf("part = %+v", p)
	}
}

func TestCollect(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("stream failed")
	tests := []struct {
		name    string
		events  []*model.StreamEvent
		err     error
		wantErr bool
	}{
		{name: "assembled fallback", events: []*model.StreamEvent{{Type: model.StreamDelta, Delta: &model.Part{Type: model.PartText, Text: "hello"}}, {Type: model.StreamDone}}},
		{name: "explicit response", events: []*model.StreamEvent{{Type: model.StreamDone, Response: &model.Response{Message: model.AssistantText("hello")}}}},
		{name: "response without message", events: []*model.StreamEvent{nil, {Type: model.StreamDelta, Delta: &model.Part{Type: model.PartText, Text: "hello"}}, {Type: model.StreamDone, Response: &model.Response{FinishReason: model.FinishStop}}}},
		{name: "missing done", wantErr: true},
		{name: "error", err: sentinel, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stream := func(yield func(*model.StreamEvent, error) bool) {
				for _, ev := range tt.events {
					if !yield(ev, nil) {
						return
					}
				}
				if tt.err != nil {
					yield(nil, tt.err)
				}
			}
			resp, err := model.Collect(stream)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Collect error = %v", err)
			}
			if tt.err != nil && !errors.Is(err, sentinel) {
				t.Fatalf("lost error: %v", err)
			}
			if !tt.wantErr && resp.Text() != "hello" {
				t.Fatalf("response = %#v", resp)
			}
		})
	}
}

func TestStreamFromResponseAndFunc(t *testing.T) {
	t.Parallel()
	resp := &model.Response{Message: model.AssistantText("hello"), FinishReason: model.FinishStop}
	f := model.Func(func(context.Context, *model.Request) (*model.Response, error) { return resp, nil })
	got, err := model.Collect(f.Stream(t.Context(), &model.Request{}))
	if err != nil || got != resp {
		t.Fatalf("round trip = %#v, %v", got, err)
	}
	count := 0
	for _, err := range model.StreamFromResponse(resp) {
		if err != nil {
			t.Fatal(err)
		}
		count++
		break
	}
	if count != 1 {
		t.Fatalf("early stop yielded %d events", count)
	}
	sentinel := errors.New("generate failed")
	f = func(context.Context, *model.Request) (*model.Response, error) { return nil, sentinel }
	if _, err := model.Collect(f.Stream(t.Context(), nil)); !errors.Is(err, sentinel) {
		t.Fatalf("stream error = %v", err)
	}
	// A nil response is an error, not a panic.
	f = func(context.Context, *model.Request) (*model.Response, error) { return nil, nil }
	if _, err := model.Collect(f.Stream(t.Context(), nil)); err == nil {
		t.Fatal("nil response streamed without an error")
	}
	if _, err := model.Collect(model.StreamFromResponse(nil)); err == nil {
		t.Fatal("StreamFromResponse(nil) streamed without an error")
	}
}
