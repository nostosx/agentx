// Package workflow composes nodes such as agents and functions into graphs
// with conditional branches, loops and dynamic fan-out. Independent nodes
// run concurrently and share a State. Runs can be streamed, persisted
// after every node with a Checkpointer, suspended for human input with
// Suspend and resumed, also after a crash.
package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/nostosx/agentx/loop"
)

// EventType identifies a workflow event.
type EventType string

const (
	// RunStart is the first event of a run or a resumed run.
	RunStart    EventType = "run_start"
	NodeStart   EventType = "node_start"
	NodeEnd     EventType = "node_end"
	NodeSkipped EventType = "node_skipped"
	NodeFailed  EventType = "node_failed"
	// NodeSuspended reports that a node called Suspend; Pending describes
	// it.
	NodeSuspended EventType = "node_suspended"
	// StateUpdated reports the writes a node committed: Delta holds the new
	// values of the keys that changed and Removed the keys deleted.
	StateUpdated EventType = "state_updated"
	// AgentEvent forwards an event of the agent an AgentNode runs.
	AgentEvent EventType = "agent_event"
	// CustomEvent carries a value a node passed to Emit.
	CustomEvent EventType = "custom"
	// RunEnd is the last event; Status says how the run ended.
	RunEnd EventType = "run_end"
)

// Event reports the progress of a run.
type Event struct {
	Type     EventType
	Workflow string
	// RunID identifies the run.
	RunID string
	// Node is the node the event belongs to. Events of a workflow nested
	// with AsNode are forwarded with Node set to "parent/child".
	Node string
	// Index is the item index for events of Map items, and -1 otherwise.
	Index int
	// Attempt counts the attempts of the node in its current execution,
	// starting at 1; see Retry.
	Attempt  int
	Err      error
	Duration time.Duration
	// Step is the number of node executions the run has started; see
	// MaxSteps.
	Step int

	Delta   map[string]any
	Removed []string
	// Pending describes the suspension for NodeSuspended.
	Pending *Pending
	// Agent is the forwarded event for AgentEvent.
	Agent *loop.Event
	// Custom is the value passed to Emit for CustomEvent.
	Custom any

	// Status and, for a suspended run, Suspended are set on RunEnd.
	Status    Status
	Suspended []*Pending
}

// Status is the state of a workflow run.
type Status string

const (
	StatusRunning   Status = "running"
	StatusSuspended Status = "suspended"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
)

// Pending is a suspension waiting for a resume value.
type Pending struct {
	// Key identifies the suspension in the map passed to Resume: the node
	// name, or "node[i]" for item i of a Map node.
	Key   string `json:"key"`
	Node  string `json:"node"`
	Index int    `json:"index"`
	// Payload is the JSON encoding of the value passed to Suspend.
	Payload json.RawMessage `json:"payload,omitempty"`
	// Interrupt is set when an AgentNode suspended because its agent needs
	// tool approvals. Resume the key with a map[string]loop.Approval
	// keyed by call ID; calls without a decision are denied.
	Interrupt *loop.Interrupt `json:"interrupt,omitempty"`
}

// Decode decodes the payload into dst.
func (p *Pending) Decode(dst any) error { return json.Unmarshal(p.Payload, dst) }

// ErrSuspended is returned by Suspend when the node must stop to wait for
// a resume value. Suspension matches it with errors.Is.
var ErrSuspended = errors.New("workflow: suspended")

// Suspension is the error Run and Resume return for a run that suspended.
// Resume it with Workflow.Resume and a value for each pending key.
type Suspension struct {
	RunID   string
	Pending []*Pending
}

func (s *Suspension) Error() string {
	keys := make([]string, len(s.Pending))
	for i, p := range s.Pending {
		keys[i] = p.Key
	}
	return fmt.Sprintf("workflow run %s suspended at %s", s.RunID, strings.Join(keys, ", "))
}

// Is reports whether target is ErrSuspended.
func (s *Suspension) Is(target error) bool { return target == ErrSuspended }

// ErrStepLimit is returned when a run would start more node executions
// than MaxSteps allows.
var ErrStepLimit = errors.New("workflow: step limit reached")

// ConflictError reports that nodes not ordered by the graph, such as
// parallel branches or items of a Map, wrote the same key without a
// Reducer.
type ConflictError struct {
	Key   string
	Nodes []string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("workflow: parallel nodes %s wrote state key %q, which has no reducer", strings.Join(e.Nodes, " and "), e.Key)
}

// Option configures a compiled workflow. Options are also RunOptions:
// passed to Run, Stream or Resume they apply to that run only.
type Option func(*Workflow)

func (o Option) applyRun(c *runConfig) { o(c.w) }

// RunOption configures a single run; see Option and WithRunID.
type RunOption interface{ applyRun(*runConfig) }

type runOptionFunc func(*runConfig)

func (f runOptionFunc) applyRun(c *runConfig) { f(c) }

// runConfig is the configuration of one run.
type runConfig struct {
	w     *Workflow
	runID string
}

// WithRunID sets the ID of a new run. IDs consist of letters, digits, '_'
// and '-', at most 64 of them. Without one, a random ID is generated.
func WithRunID(id string) RunOption { return runOptionFunc(func(c *runConfig) { c.runID = id }) }

// OnEvent registers a callback for node events: NodeStart, NodeEnd,
// NodeSkipped, NodeFailed and NodeSuspended. Stream delivers every event.
// Callbacks run one at a time, in the order of the events, on the
// goroutine consuming the run.
func OnEvent(fn func(Event)) Option {
	return func(w *Workflow) { w.listeners = append(w.listeners, fn) }
}

// MaxConcurrency bounds the number of nodes running at once. Zero means
// unbounded.
func MaxConcurrency(n int) Option { return func(w *Workflow) { w.maxConcurrency = n } }

// Named sets the workflow name reported in events.
func Named(name string) Option { return func(w *Workflow) { w.name = name } }

// WithCheckpointer persists runs with cp: after every node finishes, when
// a node attempt starts, after every Map item and when the run suspends or
// fails. State values must then be encodable as JSON; a run fails with an
// error naming the key otherwise. Failure to save a terminal checkpoint or
// delete a completed one also fails the run, preserving any execution error.
// Resumed runs see the decoded values, so read them with State.Value or
// State.Decode for typed access.
func WithCheckpointer(cp Checkpointer) Option { return func(w *Workflow) { w.checkpointer = cp } }

// DefaultMaxSteps bounds the node executions of a run of a workflow with
// loops when MaxSteps is not set.
const DefaultMaxSteps = 1000

// MaxSteps bounds the number of node executions a run may start, retries
// excluded; the run fails with ErrStepLimit when a node would exceed it.
// Zero means DefaultMaxSteps for workflows with loops and no bound
// otherwise. Resumed runs keep counting.
func MaxSteps(n int) Option { return func(w *Workflow) { w.maxSteps = n } }

// Reducer declares how writes to key are merged. A node's Set of key is an
// update passed to fn with the current value, so writes of parallel nodes
// combine instead of conflicting. Without a reducer, a Set replaces the
// value, and two nodes that the graph does not order, such as parallel
// branches, that both set or delete the same key fail the run with a
// ConflictError, whichever finishes second. Nodes ordered by edges may
// overwrite each other's values. Updates made with State.Update are
// replayed in commit order and never conflict.
//
// Writes are committed when a node succeeds, in the order the node made
// them; Map items are committed in item order. The reducer of a nested
// workflow applies to its own writes, falling back to the parent's.
func Reducer(key string, fn ReducerFunc) Option {
	return func(w *Workflow) {
		if w.reducers == nil {
			w.reducers = map[string]ReducerFunc{}
		}
		w.reducers[key] = fn
	}
}

// clone returns a copy of w that options of a run may change.
func (w *Workflow) clone() *Workflow {
	c := *w
	c.listeners = slices.Clone(w.listeners)
	c.reducers = maps.Clone(w.reducers)
	return &c
}
