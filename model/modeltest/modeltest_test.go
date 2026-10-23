package modeltest_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
)

func TestScriptAndRequestIsolation(t *testing.T) {
	t.Parallel()
	m := modeltest.New(modeltest.Reply("first"))
	req := &model.Request{Messages: []*model.Message{model.UserText("original")}, Extra: map[string]any{"seed": 1}}
	resp, err := m.Generate(t.Context(), req)
	if err != nil || resp.Text() != "first" {
		t.Fatalf("reply = %+v, %v", resp, err)
	}
	req.Messages[0].Parts[0].Text = "changed"
	req.Extra["seed"] = 2
	recorded := m.Requests()
	if len(recorded) != 1 || recorded[0].Messages[0].Text() != "original" || recorded[0].Extra["seed"] != 1 {
		t.Fatalf("recorded request changed: %+v", recorded)
	}
	recorded[0] = nil
	if m.Requests()[0] == nil {
		t.Fatal("Requests exposes its slice")
	}
	if _, err := m.Generate(t.Context(), req); !errors.Is(err, modeltest.ErrExhausted) {
		t.Fatalf("exhaustion = %v", err)
	}
	m.Append(modeltest.CallTools(modeltest.Call{ID: "c", Name: "lookup"}))
	resp, err = model.Collect(m.Stream(t.Context(), req))
	if err != nil || len(resp.ToolCalls()) != 1 || string(resp.ToolCalls()[0].Arguments) != `{}` {
		t.Fatalf("tool response = %+v, %v", resp, err)
	}
}

func TestStreamUnicodeAndFailures(t *testing.T) {
	t.Parallel()
	m := modeltest.New(modeltest.Reply("你好世界"))
	var acc model.Accumulator
	count := 0
	for ev, err := range m.Stream(t.Context(), &model.Request{}) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == model.StreamDelta {
			acc.Add(ev)
			count++
		}
	}
	if acc.Message().Text() != "你好世界" || count != 2 {
		t.Fatalf("unicode stream = %q in %d deltas", acc.Message().Text(), count)
	}
	sentinel := errors.New("failed")
	m.Append(modeltest.Fail(sentinel))
	if _, err := model.Collect(m.Stream(t.Context(), &model.Request{})); !errors.Is(err, sentinel) {
		t.Fatalf("failure = %v", err)
	}
	// Responses without a message, or no response at all, do not panic.
	m.Append(modeltest.Respond(&model.Response{FinishReason: model.FinishStop}), modeltest.Respond(nil))
	if resp, err := model.Collect(m.Stream(t.Context(), &model.Request{})); err != nil || resp.Message == nil {
		t.Fatalf("response without message = %+v, %v", resp, err)
	}
	if _, err := model.Collect(m.Stream(t.Context(), &model.Request{})); err == nil {
		t.Fatal("nil response streamed without an error")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before := len(m.Requests())
	if _, err := m.Generate(ctx, &model.Request{}); !errors.Is(err, context.Canceled) || len(m.Requests()) != before {
		t.Fatalf("canceled request = %v", err)
	}
}

func TestConcurrentScriptConsumption(t *testing.T) {
	t.Parallel()
	m := modeltest.New()
	for range 16 {
		m.Append(modeltest.Reply("answer"))
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if resp, err := m.Generate(t.Context(), &model.Request{}); err != nil || resp.Text() != "answer" {
				t.Errorf("concurrent Generate = %+v, %v", resp, err)
			}
		})
	}
	wg.Wait()
	if len(m.Requests()) != 16 {
		t.Fatalf("recorded %d requests", len(m.Requests()))
	}
}
