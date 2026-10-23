package loop_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
)

func TestEventText(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		ev   loop.Event
		want string
	}{
		{name: "text delta", ev: loop.Event{Type: loop.EventDelta, Delta: &model.StreamEvent{Delta: &model.Part{Type: model.PartText, Text: "delta"}}}, want: "delta"},
		{name: "message", ev: loop.Event{Type: loop.EventMessage, Message: model.AssistantText("message")}, want: "message"},
		{name: "reasoning", ev: loop.Event{Type: loop.EventDelta, Delta: &model.StreamEvent{Delta: &model.Part{Type: model.PartReasoning, Text: "private"}}}},
		{name: "empty", ev: loop.Event{}},
		{name: "unrelated", ev: loop.Event{Type: loop.EventRunStart, Message: model.UserText("hidden")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ev.Text(); got != tc.want {
				t.Fatalf("Text = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEventJSON(t *testing.T) {
	t.Parallel()
	ev := &loop.Event{Type: loop.EventRunEnd, RunID: "r1", Turn: 2, Result: &loop.Result{
		RunID: "r1", Status: loop.StatusFailed, Reason: "down", Output: "partial", Turns: 2, Err: errors.New("down"),
	}}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"run_end","run_id":"r1","turn":2,"result":{"run_id":"r1","status":"failed","reason":"down","output":"partial","usage":{"input_tokens":0,"output_tokens":0},"turns":2,"error":"down"}}`
	if string(b) != want {
		t.Fatalf("JSON = %s\nwant %s", b, want)
	}
	var got loop.Event
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Result == nil || got.Result.Err == nil || got.Result.Err.Error() != "down" || got.Result.Output != "partial" || got.Turn != 2 {
		t.Fatalf("decoded %+v", got)
	}
	if err := json.Unmarshal([]byte(`{"result":{"status":1}}`), &got); err == nil {
		t.Fatal("expected a decoding error")
	}
}
