package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
)

// scope connects a node attempt, or a Map item, to its run.
type scope struct {
	r       *run
	node    string
	index   int
	attempt int
	try     int
	visit   int
	// next is the index of the next Suspend call.
	next int
	// suspended is set once Suspend suspended the attempt.
	suspended bool
}

type scopeKey struct{}

func withScope(ctx context.Context, sc *scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, sc)
}

func scopeOf(ctx context.Context) *scope {
	sc, _ := ctx.Value(scopeKey{}).(*scope)
	return sc
}

// key returns the key of the scope's suspensions.
func (sc *scope) key() string {
	if sc.index >= 0 {
		return sc.node + "[" + strconv.Itoa(sc.index) + "]"
	}
	return sc.node
}

// Info describes the node attempt a context belongs to.
type Info struct {
	RunID string
	Node  string
	// Index is the Map item index, or -1.
	Index int
	// Attempt counts all attempts of the node in the run, including those
	// of earlier executions in loops; it is part of SubRunID. Try counts
	// the attempts of the current execution and Visit the executions.
	Attempt, Try, Visit int
}

// NodeInfo returns information about the node attempt running with ctx.
// It reports false outside a workflow node.
func NodeInfo(ctx context.Context) (Info, bool) {
	sc := scopeOf(ctx)
	if sc == nil {
		return Info{}, false
	}
	return Info{RunID: sc.r.id, Node: sc.node, Index: sc.index, Attempt: sc.attempt, Try: sc.try, Visit: sc.visit}, true
}

// Suspend pauses the run for human input, or any value from outside the
// run, describing what it needs with payload, which must be encodable as
// JSON.
//
// The first time, Suspend returns ErrSuspended, which the node should
// return: the node stops, its writes are discarded and, once the nodes
// still running finish, the run ends with status suspended and a Pending
// entry for the node. Workflow.Resume with a value for the entry's key runs
// the node again from its start, and this time Suspend returns the value
// encoded as JSON. A node that calls Suspend several times gets the values
// in order, so it must call Suspend in the same order each time, and code
// before a Suspend runs again on resume: keep it free of side effects or
// idempotent.
//
// Outside a workflow node Suspend returns an error.
func Suspend(ctx context.Context, payload any) (json.RawMessage, error) {
	sc := scopeOf(ctx)
	if sc == nil {
		return nil, errors.New("workflow: Suspend called outside a workflow node")
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("workflow: suspend payload: %w", err)
	}
	return sc.suspend(b, false)
}

// suspend implements Suspend with an encoded payload; agent marks the
// payload as a loop.Interrupt.
func (sc *scope) suspend(payload json.RawMessage, agent bool) (json.RawMessage, error) {
	r := sc.r
	key := sc.key()
	r.mu.Lock()
	defer r.mu.Unlock()
	points := r.suspends[key]
	if sc.next < len(points) && points[sc.next].Resume != nil {
		v := points[sc.next].Resume
		sc.next++
		return v, nil
	}
	if sc.next < len(points) {
		points[sc.next].Payload, points[sc.next].Agent = payload, agent
		r.suspends[key] = points[:sc.next+1]
	} else {
		r.suspends[key] = append(points, &SuspendPoint{Payload: payload, Agent: agent})
	}
	sc.suspended = true
	return nil, ErrSuspended
}

// lastResumed returns the latest suspension of the scope that has a resume
// value, skipping the earlier ones, and its index. ok is false if there is
// none.
func (sc *scope) lastResumed() (point SuspendPoint, ok bool) {
	r := sc.r
	r.mu.Lock()
	defer r.mu.Unlock()
	points := r.suspends[sc.key()]
	for i, p := range slices.Backward(points) {
		if p.Resume != nil {
			sc.next = i + 1
			return *p, true
		}
	}
	return SuspendPoint{}, false
}

// Emit sends v to the consumers of the run as a CustomEvent of the current
// node. It reports false outside a workflow node.
func Emit(ctx context.Context, v any) bool {
	sc := scopeOf(ctx)
	if sc == nil {
		return false
	}
	sc.r.emit(Event{Type: CustomEvent, Node: sc.node, Index: sc.index, Attempt: sc.try, Custom: v})
	return true
}
