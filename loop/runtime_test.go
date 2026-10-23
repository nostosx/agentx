package loop_test

import (
	"context"
	"errors"
	"iter"
	"reflect"
	"strings"
	"testing"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
)

func newRuntime(t *testing.T, cfg loop.Config) *loop.Runtime {
	t.Helper()
	r, err := loop.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestNew(t *testing.T) {
	t.Parallel()
	if _, err := loop.New(loop.Config{}); err == nil {
		t.Fatal("New accepted missing model")
	}
	r := newRuntime(t, loop.Config{Model: modeltest.New()})
	if cfg := r.Config(); cfg.MaxTurns != loop.DefaultMaxTurns || cfg.Parallelism != 8 || cfg.Tools == nil {
		t.Fatalf("defaults = %+v", cfg)
	}
}

func TestRuntimeRun(t *testing.T) {
	t.Parallel()
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "generate", true: "stream"}[stream], func(t *testing.T) {
			t.Parallel()
			m := modeltest.New(modeltest.Reply("你好，world"))
			r := newRuntime(t, loop.Config{Name: "assistant", Model: m, Instructions: "Be helpful", Stream: stream, MaxTokens: 123, Extra: map[string]any{"custom": true}})
			history := []*model.Message{model.UserText("old"), model.AssistantText("prior")}
			input := model.NewMessage("", model.TextPart("new"))
			var events []loop.EventType
			var deltas strings.Builder
			res, err := loop.Collect(r.Run(t.Context(), &loop.Request{RunID: "run-1", SessionID: "session-1", History: history, Input: input}), func(ev *loop.Event) {
				events = append(events, ev.Type)
				if ev.RunID != "run-1" || ev.SessionID != "session-1" || ev.Agent != "assistant" {
					t.Errorf("event identity = %+v", ev)
				}
				if ev.Type == loop.EventDelta {
					deltas.WriteString(ev.Text())
				}
			})
			if err != nil || res.Status != loop.StatusCompleted || res.Output != "你好，world" || res.Turns != 1 || res.Usage.TotalTokens() != 15 {
				t.Fatalf("Run = %+v, %v", res, err)
			}
			if len(res.Messages) != 4 || len(res.NewMessages) != 2 || res.NewMessages[0].Role != model.RoleUser {
				t.Fatalf("conversation = %#v; new = %#v", res.Messages, res.NewMessages)
			}
			if input.Role != "" || len(history) != 2 || history[0].Text() != "old" {
				t.Fatal("Run mutated caller input/history")
			}
			if events[0] != loop.EventRunStart || events[len(events)-1] != loop.EventRunEnd {
				t.Fatalf("event boundaries = %v", events)
			}
			if stream && deltas.String() != res.Output {
				t.Fatalf("streamed text = %q, want %q", deltas.String(), res.Output)
			}
			if !stream && !reflect.DeepEqual(events, []loop.EventType{loop.EventRunStart, loop.EventTurnStart, loop.EventMessage, loop.EventRunEnd}) {
				t.Fatalf("events = %v", events)
			}
			req := m.Requests()[0]
			if req.System != "Be helpful" || req.MaxTokens != 123 || req.Extra["custom"] != true || len(req.Messages) != 3 {
				t.Fatalf("model request = %+v", req)
			}
		})
	}
}

func TestRuntimeInvalidRequests(t *testing.T) {
	t.Parallel()
	r := newRuntime(t, loop.Config{Model: modeltest.New()})
	for _, req := range []*loop.Request{nil, {}} {
		if res, err := loop.Collect(r.Run(t.Context(), req), nil); err == nil || res.Status != loop.StatusFailed {
			t.Fatalf("Run invalid input = %+v, %v", res, err)
		}
	}
	if _, err := loop.Collect(r.Resume(t.Context(), "missing", nil), nil); err == nil {
		t.Fatal("Resume accepted missing checkpointer")
	}
	for _, st := range []*loop.State{nil, {Status: loop.StatusCompleted}} {
		if _, err := loop.Collect(r.ResumeState(t.Context(), st, nil), nil); err == nil {
			t.Fatal("ResumeState accepted non-interrupted state")
		}
	}
}

func TestRuntimeConsumerStops(t *testing.T) {
	t.Parallel()
	for _, stopAt := range []loop.EventType{loop.EventRunStart, loop.EventTurnStart, loop.EventDelta, loop.EventMessage} {
		t.Run(string(stopAt), func(t *testing.T) {
			t.Parallel()
			m := modeltest.New(modeltest.Reply("response"))
			r := newRuntime(t, loop.Config{Model: m, Stream: true})
			seen := false
			for ev, err := range r.Run(t.Context(), &loop.Request{Input: model.UserText("question")}) {
				if err != nil {
					t.Fatal(err)
				}
				if ev.Type == stopAt {
					seen = true
					break
				}
			}
			if !seen {
				t.Fatalf("event %s was not emitted", stopAt)
			}
			if (stopAt == loop.EventRunStart || stopAt == loop.EventTurnStart) && len(m.Requests()) != 0 {
				t.Fatal("model ran after consumer stopped")
			}
		})
	}
}

func TestCollect(t *testing.T) {
	t.Parallel()
	if _, err := loop.Collect(func(func(*loop.Event, error) bool) {}, nil); err == nil {
		t.Fatal("Collect accepted missing run result")
	}
	wantErr := errors.New("failed")
	want := &loop.Result{Status: loop.StatusFailed, Err: wantErr}
	seq := func(yield func(*loop.Event, error) bool) {
		if yield(&loop.Event{Type: loop.EventRunEnd, Result: want}, nil) {
			yield(nil, wantErr)
		}
	}
	got, err := loop.Collect(seq, nil)
	if got != want || !errors.Is(err, wantErr) {
		t.Fatalf("Collect failure = %+v, %v", got, err)
	}
}

type streamModel struct {
	events iter.Seq2[*model.StreamEvent, error]
}

func (m streamModel) Generate(context.Context, *model.Request) (*model.Response, error) {
	panic("unexpected Generate")
}

func (m streamModel) Stream(context.Context, *model.Request) iter.Seq2[*model.StreamEvent, error] {
	return m.events
}
