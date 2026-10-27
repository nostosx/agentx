package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/nostosx/kitx/syncmap"

	"github.com/nostosx/agentx/loop"
)

// ErrCheckpointNotFound is returned when a workflow checkpoint does not
// exist. It wraps loop.ErrCheckpointNotFound, so errors.Is matches either:
// check this error for workflow runs only, or loop's for any checkpoint.
var ErrCheckpointNotFound = fmt.Errorf("workflow: %w", loop.ErrCheckpointNotFound)

// CheckpointVersion is the version of the Checkpoint format written by
// this package.
const CheckpointVersion = 1

// Checkpoint is the persisted state of a workflow run.
//
// Node sets describe the current execution of each node: a node that
// loops back is reset to pending together with the nodes reachable from
// its loop target.
type Checkpoint struct {
	Version  int    `json:"version"`
	RunID    string `json:"run_id"`
	Workflow string `json:"workflow,omitempty"`
	Status   Status `json:"status"`
	Error    string `json:"error,omitempty"`
	// Values holds the committed state values.
	Values map[string]json.RawMessage `json:"values"`
	// Done lists the nodes that finished, Skipped those skipped, Running
	// those that started and had not finished, Failed those that failed
	// and Suspended those waiting for a resume value.
	Done      []string `json:"done,omitempty"`
	Skipped   []string `json:"skipped,omitempty"`
	Running   []string `json:"running,omitempty"`
	Failed    []string `json:"failed,omitempty"`
	Suspended []string `json:"suspended,omitempty"`
	// Routes holds the successors each finished node, and Start, took.
	Routes map[string][]string `json:"routes,omitempty"`
	// Activated lists loop targets activated by a node that looped back.
	Activated []string `json:"activated,omitempty"`
	// Attempts counts every attempt of each node in the run, Tries the
	// attempts of its current execution and Visits its executions.
	Attempts map[string]int `json:"attempts,omitempty"`
	Tries    map[string]int `json:"tries,omitempty"`
	Visits   map[string]int `json:"visits,omitempty"`
	// Steps counts the node executions started; see MaxSteps.
	Steps int `json:"steps"`
	// Writers lists, per key, the nodes of the current executions that set
	// or deleted it without a reducer, for conflict detection.
	Writers map[string][]string `json:"writers,omitempty"`
	// Suspends holds, per pending key, the suspensions of the current
	// execution in the order Suspend was called.
	Suspends map[string][]*SuspendPoint `json:"suspends,omitempty"`
	// Maps holds the progress of Map nodes.
	Maps map[string]*MapProgress `json:"maps,omitempty"`
	// Writes holds the net writes committed by a nested workflow run
	// (see AsNode), replayed onto the parent's state when it resumes.
	Writes    map[string]ItemWrite `json:"writes,omitempty"`
	UpdatedAt time.Time            `json:"updated_at"`
}

// SuspendPoint is a call of Suspend and, once given, its resume value.
type SuspendPoint struct {
	// Agent is set for the suspensions of an AgentNode, whose payload is
	// a loop.Interrupt.
	Agent   bool            `json:"agent,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Resume  json.RawMessage `json:"resume,omitempty"`
}

// MapProgress is the progress of a Map node's current execution.
type MapProgress struct {
	// Items are the elements of the list, captured when the execution
	// started.
	Items []json.RawMessage `json:"items"`
	// Done holds the writes of each finished item.
	Done map[int][]ItemWrite `json:"done,omitempty"`
	// values are the items as captured, for runs without a Checkpointer.
	values []any
}

// ItemWrite is a write of a finished Map item.
type ItemWrite struct {
	Key     string `json:"key"`
	Value   any    `json:"value,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
	// Replace marks a value produced by State.Update, which is stored
	// without its reducer.
	Replace bool `json:"replace,omitempty"`
}

// clone returns a copy of cp that shares only immutable values.
func (cp *Checkpoint) clone() *Checkpoint {
	c := *cp
	c.Done, c.Skipped, c.Running = slices.Clone(cp.Done), slices.Clone(cp.Skipped), slices.Clone(cp.Running)
	c.Failed, c.Suspended, c.Activated = slices.Clone(cp.Failed), slices.Clone(cp.Suspended), slices.Clone(cp.Activated)
	c.Values = maps.Clone(cp.Values)
	c.Attempts, c.Tries, c.Visits = maps.Clone(cp.Attempts), maps.Clone(cp.Tries), maps.Clone(cp.Visits)
	c.Writes = maps.Clone(cp.Writes)
	c.Routes = map[string][]string{}
	for k, v := range cp.Routes {
		c.Routes[k] = slices.Clone(v)
	}
	c.Writers = map[string][]string{}
	for k, v := range cp.Writers {
		c.Writers[k] = slices.Clone(v)
	}
	c.Suspends = map[string][]*SuspendPoint{}
	for k, ps := range cp.Suspends {
		for _, p := range ps {
			pc := *p
			c.Suspends[k] = append(c.Suspends[k], &pc)
		}
	}
	c.Maps = map[string]*MapProgress{}
	for k, m := range cp.Maps {
		c.Maps[k] = &MapProgress{Items: slices.Clone(m.Items), Done: maps.Clone(m.Done), values: m.values}
	}
	return &c
}

// Checkpointer persists workflow runs by run ID: 1 to 64 ASCII letters,
// digits, '_' and '-'; the built-in checkpointers reject others.
type Checkpointer interface {
	Save(ctx context.Context, cp *Checkpoint) error
	Load(ctx context.Context, runID string) (*Checkpoint, error)
	Delete(ctx context.Context, runID string) error
}

// MemoryCheckpointer keeps checkpoints in process memory, encoded as JSON
// like a persistent checkpointer, so a state that cannot be stored fails
// here too. The zero value is ready to use.
type MemoryCheckpointer struct {
	cps syncmap.Map[string, []byte]
}

// NewMemoryCheckpointer returns an empty in-memory checkpointer.
func NewMemoryCheckpointer() *MemoryCheckpointer { return &MemoryCheckpointer{} }

// Save implements Checkpointer.
func (c *MemoryCheckpointer) Save(_ context.Context, cp *Checkpoint) error {
	if err := checkRunID(cp.RunID); err != nil {
		return err
	}
	b, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	c.cps.Set(cp.RunID, b)
	return nil
}

// Load implements Checkpointer.
func (c *MemoryCheckpointer) Load(_ context.Context, runID string) (*Checkpoint, error) {
	if err := checkRunID(runID); err != nil {
		return nil, err
	}
	b, ok := c.cps.Get(runID)
	if !ok {
		return nil, ErrCheckpointNotFound
	}
	var cp Checkpoint
	if err := json.Unmarshal(b, &cp); err != nil {
		return nil, err
	}
	return &cp, nil
}

// Delete implements Checkpointer.
func (c *MemoryCheckpointer) Delete(_ context.Context, runID string) error {
	if err := checkRunID(runID); err != nil {
		return err
	}
	c.cps.Delete(runID)
	return nil
}

func checkRunID(id string) error {
	if !runIDPattern.MatchString(id) {
		return fmt.Errorf("invalid workflow checkpoint id %q", id)
	}
	return nil
}

// runIDPattern matches run IDs; it is narrower than the runtime's so
// derived agent run IDs stay valid there.
var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// nodeToken is a run-ID-safe token for a node name: the name itself if it
// is short and alphanumeric, or "_" and a hash of it.
func nodeToken(node string) string {
	ok := len(node) <= 24
	for i := 0; ok && i < len(node); i++ {
		c := node[i]
		ok = c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' && i > 0
	}
	if ok {
		return node
	}
	sum := sha256.Sum256([]byte(node))
	return "_" + hex.EncodeToString(sum[:8])
}

// SubRunID returns the run ID used for the agent of an AgentNode, or the
// nested workflow of AsNode, in the given attempt of node in workflow run
// runID; index is the Map item index, or -1. It is
// "<runID>-<node>-<attempt>[-i<index>]", where node is replaced by "_" and
// a hash of it unless it is at most 24 letters, digits and inner
// underscores. Runtime checkpointers do not accept dots in run IDs, so
// '-' separates the parts; the node part never contains one, so IDs of
// different runs, nodes or attempts never collide.
func SubRunID(runID, node string, attempt, index int) string {
	id := runID + "-" + nodeToken(node) + "-" + strconv.Itoa(attempt)
	if index >= 0 {
		id += "-i" + strconv.Itoa(index)
	}
	return id
}

// encodeValues encodes state values for a checkpoint.
func encodeValues(values map[string]any) (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage, len(values))
	for k, v := range values {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("workflow: state value %q is not JSON-encodable, which a Checkpointer requires: %w", k, err)
		}
		out[k] = b
	}
	return out, nil
}

// decodeValues decodes the state values of a checkpoint.
func decodeValues(values map[string]json.RawMessage) (map[string]any, error) {
	out := make(map[string]any, len(values))
	for k, b := range values {
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			return nil, fmt.Errorf("workflow: decode state value %q: %w", k, err)
		}
		out[k] = v
	}
	return out, nil
}
