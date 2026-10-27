package agent

import (
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/session"
	"github.com/nostosx/agentx/tool/builtin"
)

// CheckpointsKey is the session state key holding the checkpoints of a
// session; see Runner.Checkpoints.
const CheckpointsKey = "agentx.checkpoints"

// MaxCheckpoints is the number of the most recent checkpoints a Runner
// keeps for each session, as Claude Code keeps file snapshots for the 100
// most recent checkpoints of a session.
const MaxCheckpoints = 100

// ErrSessionBusy is returned by Runner.Rewind while the session has a run
// or another operation of the Runner in progress.
var ErrSessionBusy = errors.New("agent: the session is busy")

// ErrCannotRewind is returned by Runner.Rewind for a checkpoint that cannot
// be rewound in the mode asked: the agent has no file history to rewind
// the code, or the checkpoint's message is no longer in the conversation.
var ErrCannotRewind = errors.New("agent: cannot rewind")

// HistoryProvider is implemented by agents with a file history, such as an
// LLMAgent built with WithFileHistory. A Runner starts a checkpoint of the
// history for each run and restores files from it in Rewind.
type HistoryProvider interface {
	FileHistory() builtin.History
}

// FileHistory returns the agent's file history, or nil if it has none.
func (a *LLMAgent) FileHistory() builtin.History { return a.cfg.FileHistory }

// Checkpoint is a point of a session that Runner.Rewind can return to: the
// start of a run, before its user message, like the checkpoint Claude
// Code creates for each prompt. See
// https://code.claude.com/docs/en/checkpointing.
type Checkpoint struct {
	// ID identifies the checkpoint. It is the ID of the run, which the
	// run's events carry in Event.RunID.
	ID string
	// Index is the position of the run's user message in the session's
	// messages, or -1 if it is no longer there, for example after
	// compaction, so that only the code can be rewound.
	Index int
	// Message is the run's original user input before context and middleware
	// augmentation, if Index is not -1.
	Message *model.Message
	// Time is when the run started.
	Time time.Time
	// Agent names the agent the session's message went to when the run
	// started: the agent of ActiveAgentKey, or "" for the Runner's own
	// agent. Rewinding the conversation makes it active again, so the
	// conversation continues with the agent that had it before handoffs.
	Agent string
	// Changes are the file changes saved to the agent's file history
	// during the run, oldest first.
	Changes []builtin.FileChange
	// Code reports whether rewinding the code to the checkpoint would
	// restore files: the file history keeps the checkpoint and changes
	// saved since.
	Code bool
}

// RewindMode selects what Runner.Rewind restores.
type RewindMode int

const (
	// RewindCode restores the files the tools changed since the
	// checkpoint and keeps the conversation.
	RewindCode RewindMode = 1 << iota
	// RewindConversation removes the checkpoint's user message and the
	// messages after it from the session and keeps the files.
	RewindConversation
	// RewindBoth restores the files and the conversation.
	RewindBoth = RewindCode | RewindConversation
)

// RewindResult is the result of Runner.Rewind.
type RewindResult struct {
	// Message is the original user input of the removed run, before context
	// and middleware augmentation, to send again or edit, as Claude Code
	// restores the prompt into the input.
	Message *model.Message
	// Restored are the restored files and Skipped the changed files that
	// were not restored; see builtin.Restoration.
	Restored []builtin.FileChange
	Skipped  []builtin.FileChange
	// Undo is the file history checkpoint holding the files' state before
	// the rewind; restoring it with History.Restore undoes the code
	// rewind.
	Undo string
}

// checkpointMessageKey identifies the input in the stored conversation even
// when compaction moves it or another prompt has identical content. It is
// reserved Runner metadata; model providers do not send message metadata.
const checkpointMessageKey = "agentx.checkpoint"

// checkpointRecord is a checkpoint as kept in the session state.
type checkpointRecord struct {
	ID string `json:"id"`
	// Message is the caller's original input before context providers and
	// middleware augment it.
	Message *model.Message `json:"message,omitempty"`
	Time    time.Time      `json:"time"`
	// Agent is the active agent when the run started; see
	// Checkpoint.Agent.
	Agent string `json:"agent,omitempty"`
}

// history returns the agent's file history, if it has one.
func (r *Runner) history() builtin.History {
	if p, ok := r.agent.(HistoryProvider); ok {
		return p.FileHistory()
	}
	return nil
}

// beginCheckpoint names the run of in, if it has no ID yet, and starts the
// checkpoint of the file history named after it.
func (r *Runner) beginCheckpoint(ctx context.Context, sessionID string, in *Input) (*checkpointRecord, error) {
	if in.RunID == "" {
		in.RunID = "run_" + uuid.NewV7().String()
	}
	if h := r.history(); h != nil {
		if err := h.Checkpoint(ctx, sessionID, in.RunID); err != nil {
			return nil, fmt.Errorf("file history: %w", err)
		}
	}
	cp := &checkpointRecord{ID: in.RunID, Message: in.Message.Clone(), Time: time.Now(), Agent: in.Agent}
	in.Message = in.Message.Clone()
	if in.Message != nil {
		if in.Message.Metadata == nil {
			in.Message.Metadata = make(map[string]any)
		}
		in.Message.Metadata[checkpointMessageKey] = cp.ID
	}
	return cp, nil
}

// addCheckpoint records the checkpoint of a run that ended in res, unless
// the run did not add its message.
func addCheckpoint(sess session.Session, cp *checkpointRecord, res *loop.Result) {
	if cp == nil || len(res.NewMessages) == 0 || res.NewMessages[0].Role != model.RoleUser {
		return
	}
	list := checkpointsOf(sess)
	for _, c := range list {
		if c.ID == cp.ID {
			return
		}
	}
	list = append(list, *cp)
	if n := len(list) - MaxCheckpoints; n > 0 {
		list = list[n:]
	}
	sess.State().Set(CheckpointsKey, list)
}

// checkpointsOf returns the checkpoints kept with sess, oldest first.
func checkpointsOf(sess session.Session) []checkpointRecord {
	list, _, err := sess.State().Value[[]checkpointRecord](CheckpointsKey)
	if err != nil {
		return nil
	}
	return list
}

// findMessage returns the unique position of a checkpoint's user message
// in msgs, or -1 if it was removed or its identity was duplicated. Content
// cannot identify a prompt: repeated prompts and compaction summaries may
// have identical content.
func findMessage(msgs []*model.Message, rec checkpointRecord) int {
	index := -1
	for i, msg := range msgs {
		if msg == nil || msg.Role != model.RoleUser || msg.Metadata[checkpointMessageKey] != rec.ID {
			continue
		}
		if index >= 0 {
			return -1
		}
		index = i
	}
	return index
}

// Checkpoints returns the checkpoints of a session, oldest first: one for
// each of its last MaxCheckpoints runs that added a user message, except
// those removed by rewinding the conversation. With a file history (see
// HistoryProvider), each lists the files changed during its run.
func (r *Runner) Checkpoints(ctx context.Context, sessionID string) ([]Checkpoint, error) {
	sess, err := r.store.Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	var hist []builtin.HistoryCheckpoint
	if h := r.history(); h != nil {
		if hist, err = h.Checkpoints(ctx, sessionID); err != nil {
			return nil, fmt.Errorf("file history: %w", err)
		}
	}
	pos := make(map[string]int, len(hist))
	for i, c := range hist {
		if _, ok := pos[c.ID]; !ok {
			pos[c.ID] = i
		}
	}
	msgs := sess.Messages()
	recs := checkpointsOf(sess)
	out := make([]Checkpoint, len(recs))
	for i, rec := range recs {
		cp := Checkpoint{ID: rec.ID, Index: findMessage(msgs, rec), Time: rec.Time, Agent: rec.Agent}
		if cp.Index >= 0 {
			cp.Message = rec.Message.Clone()
		}
		if k, ok := pos[rec.ID]; ok {
			cp.Changes = hist[k].Changes
			for _, c := range hist[k:] {
				if len(c.Changes) > 0 {
					cp.Code = true
					break
				}
			}
		}
		out[i] = cp
	}
	return out, nil
}

// Rewind returns a session to a checkpoint listed by Checkpoints, like
// Claude Code's /rewind (https://code.claude.com/docs/en/checkpointing).
// mode selects what is restored:
//
//   - RewindCode restores files tracked since the checkpoint from the agent's
//     file history (see HistoryProvider and builtin.Config.History). Files
//     that cannot be restored are reported in RewindResult.Skipped; failures
//     trigger an attempt to undo changes already applied. Untracked files
//     are left alone, but later changes to tracked files, including changes
//     by bash or another session, can be overwritten. The restored files
//     are forgotten by the tools' FileTracker, so the model reads them again
//     before changing them.
//   - RewindConversation removes the checkpoint's user message and the
//     messages after it, and the later checkpoints, from the session.
//     RewindResult.Message holds the original input to send again. The
//     agent that was active at the checkpoint (Checkpoint.Agent) becomes
//     active again, undoing later handoffs. The rest of the session state,
//     such as values set with SessionState and session-scoped run values
//     like remembered permission grants, is not rewound, as Claude Code
//     keeps permissions across a rewind.
//
// Rewind refuses with ErrSessionBusy while a run of the session or another
// operation of this Runner on it is in progress, with ErrCannotRewind when
// mode cannot be rewound to the checkpoint, and refuses a session
// with an interrupted run: resume it or discard it first. A failed run is
// given up, as by the next Run. The file history is kept: a later rewind
// to an earlier checkpoint still restores the files, and RewindResult.Undo
// undoes the code rewind. If saving the session fails after the files
// were restored, Rewind attempts to undo the code rewind and includes any
// undo failure in the returned error.
func (r *Runner) Rewind(ctx context.Context, sessionID, checkpointID string, mode RewindMode) (*RewindResult, error) {
	if mode&RewindBoth == 0 || mode&^RewindBoth != 0 {
		return nil, fmt.Errorf("agent: invalid rewind mode %d", mode)
	}
	unlock, ok := r.tryLock(sessionID)
	if !ok {
		return nil, fmt.Errorf("session %s: %w", sessionID, ErrSessionBusy)
	}
	defer unlock()
	sess, err := r.store.Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if runID := runIDOf(sess, PendingRunKey); runID != "" {
		return nil, fmt.Errorf("session %s has an interrupted run %s; resume it with Runner.Resume or discard it with Runner.Discard before rewinding", sessionID, runID)
	}
	recs := checkpointsOf(sess)
	k := -1
	for i, rec := range recs {
		if rec.ID == checkpointID {
			k = i
			break
		}
	}
	if k < 0 {
		return nil, fmt.Errorf("session %s: %w: %s", sessionID, builtin.ErrCheckpointNotFound, checkpointID)
	}
	msgs := sess.Messages()
	index := -1
	if mode&RewindConversation != 0 {
		if index = findMessage(msgs, recs[k]); index < 0 {
			return nil, fmt.Errorf("session %s: %w: the message of checkpoint %s is no longer in the conversation, for example after compaction; rewind only the code", sessionID, ErrCannotRewind, checkpointID)
		}
	}
	h := r.history()
	if mode&RewindCode != 0 && h == nil {
		return nil, fmt.Errorf("%w: agent %s has no file history to rewind the code; see WithFileHistory", ErrCannotRewind, r.agent.Name())
	}

	res := &RewindResult{}
	if mode&RewindCode != 0 {
		rest, err := h.Restore(ctx, sessionID, checkpointID)
		if err != nil {
			return nil, fmt.Errorf("rewind the code of session %s: %w", sessionID, err)
		}
		res.Restored, res.Skipped, res.Undo = rest.Files, rest.Skipped, rest.Undo
	}
	failed := runIDOf(sess, FailedRunKey)
	if failed == "" && index < 0 {
		return res, nil
	}
	sess.State().Delete(FailedRunKey)
	if index >= 0 {
		res.Message = recs[k].Message.Clone()
		sess.Replace(msgs[:index])
		if recs[k].Agent == "" {
			sess.State().Delete(ActiveAgentKey)
		} else {
			sess.State().Set(ActiveAgentKey, recs[k].Agent)
		}
		if k == 0 {
			sess.State().Delete(CheckpointsKey)
		} else {
			sess.State().Set(CheckpointsKey, recs[:k])
		}
	}
	if err := r.store.Save(context.WithoutCancel(ctx), sess); err != nil {
		err = fmt.Errorf("save session: %w", err)
		if res.Undo != "" {
			if _, uerr := h.Restore(context.WithoutCancel(ctx), sessionID, res.Undo); uerr != nil {
				err = errors.Join(err, fmt.Errorf("undo the code rewind: %w", uerr))
			}
		}
		return nil, err
	}
	if d, ok := r.agent.(Discarder); ok && failed != "" {
		// The failed run is no longer referenced; deleting its checkpoint
		// is best effort, as in save.
		_ = d.Discard(context.WithoutCancel(ctx), failed)
	}
	return res, nil
}

// tryLock locks a session like lock, unless it is locked or waited for.
func (r *Runner) tryLock(id string) (func(), bool) {
	r.mu.Lock()
	if _, busy := r.locks[id]; busy {
		r.mu.Unlock()
		return nil, false
	}
	l := &sessionLock{refs: 1}
	l.mu.Lock()
	r.locks[id] = l
	r.mu.Unlock()
	return func() {
		l.mu.Unlock()
		r.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(r.locks, id)
		}
		r.mu.Unlock()
	}, true
}

// forkHistory copies the file history of a session to its fork. If that
// fails, the fork is deleted.
func (r *Runner) forkHistory(ctx context.Context, from, to string) error {
	h := r.history()
	if h == nil {
		return nil
	}
	if err := h.Fork(ctx, from, to); err != nil {
		_ = r.store.Delete(context.WithoutCancel(ctx), to)
		return fmt.Errorf("fork the file history: %w", err)
	}
	return nil
}
