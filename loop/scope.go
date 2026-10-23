package loop

import (
	"context"
	"sync"

	"github.com/nostosx/agentx/model"
)

// callScope connects a tool call to the run executing it, so tools that
// run nested runs, such as sub-agents, can report usage and interrupts.
type callScope struct {
	// run is the run executing the call.
	run *Run
	// callID is the ID of the call.
	callID    string
	mu        sync.Mutex
	usage     model.Usage
	interrupt *Interrupt
	// closed is set once the call returned; see ForwardEvent.
	closed bool
	// resumed and approvals are set when the call is repeated after the
	// run resumed from an interrupt the call raised.
	resumed   *Interrupt
	approvals map[string]Approval
}

type scopeKey struct{}

func withScope(ctx context.Context, s *callScope) context.Context {
	return context.WithValue(ctx, scopeKey{}, s)
}

func scopeOf(ctx context.Context) *callScope {
	s, _ := ctx.Value(scopeKey{}).(*callScope)
	return s
}

// CallingRun returns the run executing the current tool call, or nil
// outside a tool call of a run. Calls may run concurrently, so only the read
// methods of the run may be used.
func CallingRun(ctx context.Context) *Run {
	if s := scopeOf(ctx); s != nil {
		return s.run
	}
	return nil
}

// Caller returns the run whose tool call started this run, such as the run
// that called a sub-agent, or nil if the run was not started by a tool
// call. Following Caller from CallingRun leads to the outermost run, which
// holds the session of a sub-agent's work. Only the read methods of the
// returned run may be used.
func (r *Run) Caller() *Run {
	if s := r.l.caller; s != nil {
		return s.run
	}
	return nil
}

// ReportUsage adds usage, for example of a sub-agent's run, to the run
// executing the current tool call, so the run's usage covers the work done
// on its behalf. Outside a tool call it does nothing.
func ReportUsage(ctx context.Context, u model.Usage) {
	if s := scopeOf(ctx); s != nil {
		s.mu.Lock()
		s.usage.Add(u)
		s.mu.Unlock()
	}
}

// InterruptCall asks the run executing the current tool call to interrupt
// with the approval requests of intr, which typically come from a nested
// run such as a sub-agent. The tool's result is discarded. When the run is
// resumed, the tool is called again and ResumedCall returns intr and the
// decisions for its requests.
//
// InterruptCall reports false outside a tool call of a run; the tool must
// then handle the interrupt itself.
func InterruptCall(ctx context.Context, intr *Interrupt) bool {
	s := scopeOf(ctx)
	if s == nil || intr == nil || len(intr.Requests) == 0 {
		return false
	}
	s.mu.Lock()
	s.interrupt = intr
	s.mu.Unlock()
	return true
}

// ResumedCall reports whether the current tool call repeats a call that
// raised an interrupt with InterruptCall, and returns that interrupt and
// the decisions for its requests.
func ResumedCall(ctx context.Context) (intr *Interrupt, approvals map[string]Approval, ok bool) {
	s := scopeOf(ctx)
	if s == nil || s.resumed == nil {
		return nil, nil, false
	}
	return s.resumed, s.approvals, true
}

// close marks the call as returned.
func (s *callScope) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// ForwardEvent passes ev, an event of a nested run that the current tool
// call runs, such as a sub-agent's run, to the consumer of the run
// executing the call. The consumer gets a copy with Depth increased by one
// and, for an event of the nested run itself, ParentCallID set to the ID of
// the call; events the nested run forwarded keep theirs. The copy keeps the
// nested run's RunID, Agent and Turn.
//
// ForwardEvent is safe for concurrent use, also by calls running
// concurrently. It reports false, without delivering the event, outside a
// tool call of a run, once the call has returned, or when the consumer
// stopped iterating. The nested run need not stop then: like any tool
// call, the call keeps its result.
func ForwardEvent(ctx context.Context, ev *Event) bool {
	s := scopeOf(ctx)
	if s == nil || ev == nil {
		return false
	}
	c := *ev
	if c.Depth == 0 {
		c.ParentCallID = s.callID
	}
	c.Depth++
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	return s.run.l.forward(&c)
}
