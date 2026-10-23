package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/tool/builtin"
)

// CheckpointInfo is an item of GET /sessions/{id}/checkpoints; see
// agent.Checkpoint.
type CheckpointInfo struct {
	// ID is the ID of the run the checkpoint precedes.
	ID string `json:"id"`
	// Index is the position of the run's user message in the session's
	// messages, or -1 when only the code can be rewound.
	Index   int            `json:"index"`
	Message *model.Message `json:"message,omitempty"`
	Time    time.Time      `json:"time"`
	// Agent is the agent that was active at the checkpoint, if not the
	// Runner's own.
	Agent string `json:"agent,omitempty"`
	// Changes are the file changes of the run; Code reports whether
	// rewinding the code would restore files.
	Changes []builtin.FileChange `json:"changes,omitempty"`
	Code    bool                 `json:"code"`
}

// RewindRequest is the body of POST /sessions/{id}/rewind.
type RewindRequest struct {
	// CheckpointID is the ID of a checkpoint listed by
	// GET /sessions/{id}/checkpoints.
	CheckpointID string `json:"checkpoint_id"`
	// Mode is "code", "conversation" or "both"; see agent.RewindMode.
	Mode string `json:"mode"`
}

// RewindResponse is the body of the response to POST /sessions/{id}/rewind;
// see agent.RewindResult.
type RewindResponse struct {
	// Message is the user message removed from the conversation.
	Message  *model.Message       `json:"message,omitempty"`
	Restored []builtin.FileChange `json:"restored,omitempty"`
	Skipped  []builtin.FileChange `json:"skipped,omitempty"`
	Undo     string               `json:"undo,omitempty"`
}

var rewindModes = map[string]agent.RewindMode{
	"code":         agent.RewindCode,
	"conversation": agent.RewindConversation,
	"both":         agent.RewindBoth,
}

func (s *Server) checkpoints(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if e := s.authorize(r, id); e != nil {
		writeError(w, e)
		return
	}
	cps, err := s.runner.Checkpoints(r.Context(), id)
	if err != nil {
		writeError(w, classify(err))
		return
	}
	out := make([]CheckpointInfo, len(cps))
	for i, c := range cps {
		out[i] = CheckpointInfo{ID: c.ID, Index: c.Index, Message: c.Message, Time: c.Time, Agent: c.Agent, Changes: c.Changes, Code: c.Code}
	}
	writeJSON(w, http.StatusOK, map[string]any{"checkpoints": out})
}

func (s *Server) rewind(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if e := s.authorize(r, id); e != nil {
		writeError(w, e)
		return
	}
	var req RewindRequest
	if e := s.decode(w, r, &req, true, false); e != nil {
		writeError(w, e)
		return
	}
	mode, ok := rewindModes[req.Mode]
	if !ok || req.CheckpointID == "" {
		writeError(w, errorf(http.StatusBadRequest, CodeBadRequest, `checkpoint_id and mode ("code", "conversation" or "both") are required`))
		return
	}
	if !s.acquire(id) {
		writeError(w, errBusy(id))
		return
	}
	defer s.release(id)
	if e := s.checkRun(r.Context(), id); e != nil {
		writeErrorBody(w, e.err, e.pending)
		return
	}
	res, err := s.runner.Rewind(r.Context(), id, req.CheckpointID, mode)
	switch {
	case errors.Is(err, agent.ErrSessionBusy):
		writeError(w, errBusy(id))
		return
	case errors.Is(err, builtin.ErrCheckpointNotFound):
		writeError(w, errorf(http.StatusNotFound, CodeNotFound, "%v", err))
		return
	case errors.Is(err, agent.ErrCannotRewind):
		writeError(w, errorf(http.StatusConflict, CodeCannotRewind, "%v", err))
		return
	case err != nil:
		writeError(w, classify(err))
		return
	}
	writeJSON(w, http.StatusOK, RewindResponse{Message: res.Message, Restored: res.Restored, Skipped: res.Skipped, Undo: res.Undo})
}
