package loop

import (
	"context"
	"sync"

	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/tool"
)

// relayed is an event that a call running concurrently hands to the
// goroutine iterating the run. ok receives whether it was delivered.
type relayed struct {
	ev    *Event
	stamp bool
	ok    chan bool
}

// send hands an event to the goroutine iterating the run, which delivers it
// while the calls of a concurrent batch run, and waits until it was.
func (l *execution) send(ev *Event, stamp bool) bool {
	r := &relayed{ev: ev, stamp: stamp, ok: make(chan bool, 1)}
	l.relay <- r
	return <-r.ok
}

// forward delivers an event forwarded from a nested run; see ForwardEvent.
func (l *execution) forward(ev *Event) bool {
	if l.relay != nil {
		return l.send(ev, false)
	}
	return l.deliver(ev, false)
}

// runConcurrently runs the calls of a batch concurrently, at most
// Parallelism at a time and started in order, and fills in their results
// and scopes.
//
// Meanwhile the goroutine iterating the run delivers the events the calls
// emit or forward, one at a time, so the consumer sees events only from
// that goroutine, and none after it stopped iterating. Calls are started
// by a separate goroutine, so a call waiting for its event to be delivered
// never blocks the start of the others.
func (l *execution) runConcurrently(ctx context.Context, batch []*PendingCall, wrap []tool.Middleware, results []*model.ToolResult, scopes []*callScope) {
	relay := make(chan *relayed)
	done := make(chan struct{})
	l.relay = relay
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		sem := make(chan struct{}, l.r.cfg.Parallelism)
		for k := range batch {
			sem <- struct{}{}
			wg.Go(func() {
				defer func() { <-sem }()
				results[k], scopes[k] = l.callOne(ctx, batch[k], wrap)
			})
		}
		wg.Wait()
	}()
	served := false
	defer func() {
		if !served {
			// The consumer panicked while handling an event. Refuse the
			// events of the calls still running so they can finish.
			go func() {
				for {
					select {
					case r := <-relay:
						r.ok <- false
					case <-done:
						return
					}
				}
			}()
		}
	}()
	for {
		select {
		case r := <-relay:
			r.ok <- l.deliver(r.ev, r.stamp)
		case <-done:
			served = true
			l.relay = nil
			return
		}
	}
}
