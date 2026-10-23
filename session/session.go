// Package session stores agent conversations and per-session state.
package session

import (
	"encoding/json"
	"maps"
	"sync"
	"time"
	"uuid"

	"github.com/nostosx/agentx/model"
)

// Session is a conversation with an agent.
//
// Implementations must be safe for concurrent use.
type Session interface {
	ID() string
	// Messages returns a copy of the conversation. Values within message
	// metadata may remain shared, as in model.Message.Clone.
	Messages() []*model.Message
	// Append adds messages to the conversation.
	Append(msgs ...*model.Message)
	// Replace swaps the whole conversation, for example after compaction.
	Replace(msgs []*model.Message)
	// State holds arbitrary per-session values.
	State() *State
	CreatedAt() time.Time
	UpdatedAt() time.Time
}

// Snapshot is the serializable form of a session.
type Snapshot struct {
	ID        string           `json:"id"`
	Messages  []*model.Message `json:"messages"`
	State     map[string]any   `json:"state,omitempty"`
	CreatedAt time.Time        `json:"created_at"`
	UpdatedAt time.Time        `json:"updated_at"`
	// Revision counts the saves of the session in its store. Stores use it
	// to detect concurrent modification; see ErrConflict.
	Revision int64 `json:"revision,omitempty"`
}

// TakeSnapshot captures a session through separate accessor calls. Callers
// must prevent concurrent changes if they need a consistent snapshot.
// State values and message metadata values remain shared.
func TakeSnapshot(s Session) *Snapshot {
	return &Snapshot{
		ID:        s.ID(),
		Messages:  s.Messages(),
		State:     s.State().All(),
		CreatedAt: s.CreatedAt(),
		UpdatedAt: s.UpdatedAt(),
		Revision:  revisionOf(s),
	}
}

// FromSnapshot builds an in-memory session from a snapshot. It copies the
// messages and state map; state values and message metadata values remain shared.
func FromSnapshot(snap *Snapshot) Session {
	return &basic{
		id:        snap.ID,
		messages:  model.CloneMessages(snap.Messages),
		state:     &State{values: maps.Clone(snap.State)},
		createdAt: snap.CreatedAt,
		updatedAt: snap.UpdatedAt,
		rev:       snap.Revision,
	}
}

// State is a key-value map whose operations are safe for concurrent use.
// Values are stored and returned without copying; callers must treat mutable
// values as immutable or synchronize access to them. Values should be JSON
// serializable when the session is persisted.
type State struct {
	mu     sync.RWMutex
	values map[string]any
}

// Get returns the value for key.
func (s *State) Get(key string) (any, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.values[key]
	return v, ok
}

// Set stores a value.
func (s *State) Set(key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = map[string]any{}
	}
	s.values[key] = value
}

// Delete removes a value.
func (s *State) Delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, key)
}

// All returns a shallow copy of the map; its values remain shared.
func (s *State) All() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return maps.Clone(s.values)
}

// Decode unmarshals the value for key into dst, converting through JSON.
// This recovers typed values from sessions loaded from disk.
func (s *State) Decode(key string, dst any) (bool, error) {
	v, ok := s.Get(key)
	if !ok {
		return false, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return true, err
	}
	return true, json.Unmarshal(b, dst)
}

// NewID returns a random session identifier: a version 4 UUID, which
// carries 122 random bits and, unlike a version 7 UUID, does not reveal
// when the session was created.
func NewID() string { return uuid.New().String() }
