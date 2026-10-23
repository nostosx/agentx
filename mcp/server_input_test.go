package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestInputRetryRestoresRequestBeforeAnswers(t *testing.T) {
	t.Parallel()
	s := NewServer(Implementation{}, nil)
	t.Cleanup(func() { _ = s.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c := &incoming{modern: true, method: "tools/call", params: json.RawMessage(`{}`)}
	inv := &invocation{s: s}
	first, second := make(chan json.RawMessage), make(chan json.RawMessage)
	k := &continuation{
		id: "waiting", digest: paramsDigest(c.method, c.params), inv: inv, cancel: cancel,
		signal: make(chan struct{}, 1), done: make(chan struct{}),
		answers: map[string]answer{},
		pending: map[string]*pendingInput{
			"1": {digest: "first", ch: first},
			"2": {digest: "second", ch: second},
		},
	}
	inv.k = k
	s.conts[k.id] = k
	state := s.seal(&requestState{
		Handle: k.id, Method: c.method, Digest: k.digest,
		Expires: time.Now().Add(time.Minute).UnixMilli(),
		Pending: map[string]string{"1": "first", "2": "second"},
	})
	var err error
	c.params, err = json.Marshal(map[string]any{
		"requestState":   state,
		"inputResponses": map[string]any{"1": "accepted", "2": "accepted"},
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(k.done)
		<-first
		current := inv.current()
		// The second handoff keeps delivery blocked until the resumed handler
		// has observed its request, independently of scheduler timing.
		<-second
		if current != c {
			k.err = ErrNoRequest
		}
		k.res = "resumed"
	}()
	got, err := s.withInput(ctx, c, func(context.Context, *incoming) (any, error) {
		return nil, errors.New("unexpected handler restart")
	})
	if err != nil || got != "resumed" {
		t.Fatalf("retry = %v, %v; want resumed, nil", got, err)
	}
}
