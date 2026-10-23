package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/nostosx/kitx/syncmap"
)

// ErrCheckpointNotFound is returned when a checkpoint does not exist.
var ErrCheckpointNotFound = errors.New("checkpoint not found")

// Checkpointer persists run state so interrupted or crashed runs can be
// resumed. Checkpoints are keyed by run ID: 1 to 128 ASCII letters,
// digits, '_' and '-'; the built-in checkpointers reject others. A run
// overwrites its checkpoint once its input is added, after each turn,
// before each batch of tool calls, and when it is interrupted, fails or
// its consumer stops iterating; it deletes the checkpoint when it ends in
// a status that is not resumable or fails before its input is added.
type Checkpointer interface {
	Save(ctx context.Context, st *State) error
	Load(ctx context.Context, runID string) (*State, error)
	Delete(ctx context.Context, runID string) error
}

// MemoryCheckpointer keeps checkpoints in process memory, encoded as JSON
// like a persistent checkpointer, so a state that cannot be stored fails
// here too. The zero value is ready to use.
type MemoryCheckpointer struct {
	states syncmap.Map[string, []byte]
}

// NewMemoryCheckpointer returns an empty in-memory checkpointer.
func NewMemoryCheckpointer() *MemoryCheckpointer { return &MemoryCheckpointer{} }

// Save implements Checkpointer.
func (c *MemoryCheckpointer) Save(_ context.Context, st *State) error {
	if err := checkRunID(st.RunID); err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	c.states.Set(st.RunID, b)
	return nil
}

// Load implements Checkpointer.
func (c *MemoryCheckpointer) Load(_ context.Context, runID string) (*State, error) {
	if err := checkRunID(runID); err != nil {
		return nil, err
	}
	b, ok := c.states.Get(runID)
	if !ok {
		return nil, ErrCheckpointNotFound
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// Delete implements Checkpointer.
func (c *MemoryCheckpointer) Delete(_ context.Context, runID string) error {
	if err := checkRunID(runID); err != nil {
		return err
	}
	c.states.Delete(runID)
	return nil
}

func checkRunID(id string) error {
	if !runIDPattern.MatchString(id) {
		return fmt.Errorf("invalid checkpoint id %q", id)
	}
	return nil
}

var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// checkpoint saves st on a best-effort basis: failures are logged only.
func (r *Runtime) checkpoint(ctx context.Context, st *State) {
	_ = r.saveCheckpoint(ctx, st)
}

// saveCheckpoint saves st. Failures are logged and returned; callers that
// must not continue without a checkpoint, such as before calls with side
// effects, act on the error. It uses a context without cancellation, so a
// cancelled run is still saved.
func (r *Runtime) saveCheckpoint(ctx context.Context, st *State) error {
	if r.cfg.Checkpointer == nil {
		return nil
	}
	st.Version = StateVersion
	st.UpdatedAt = time.Now().UTC()
	if err := r.cfg.Checkpointer.Save(context.WithoutCancel(ctx), st); err != nil {
		r.log.Warn("save checkpoint", "run_id", st.RunID, "error", err)
		return err
	}
	return nil
}

// deleteCheckpoint deletes the checkpoint of a run that cannot be resumed.
func (r *Runtime) deleteCheckpoint(ctx context.Context, runID string) {
	if r.cfg.Checkpointer == nil {
		return
	}
	if err := r.cfg.Checkpointer.Delete(context.WithoutCancel(ctx), runID); err != nil && !errors.Is(err, ErrCheckpointNotFound) {
		r.log.Warn("delete checkpoint", "run_id", runID, "error", err)
	}
}
