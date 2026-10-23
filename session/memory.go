package session

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nostosx/kitx/syncmap"

	"github.com/nostosx/agentx/model"
)

// basic is the in-memory Session implementation.
type basic struct {
	mu        sync.RWMutex
	id        string
	messages  []*model.Message
	state     *State
	createdAt time.Time
	updatedAt time.Time

	// rev is the store revision the session was loaded at or last saved
	// as. When tracked is set, stores refuse to overwrite a different
	// revision; see ErrConflict. rev is -1 for a session GetOrCreate made,
	// which must not exist yet.
	rev     int64
	tracked bool
}

// track marks s as loaded from or saved to a store at revision rev.
func (s *basic) track(rev int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rev, s.tracked = rev, true
}

func (s *basic) expected() (int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rev, s.tracked
}

func revisionOf(s Session) int64 {
	if b, ok := s.(*basic); ok {
		if rev, _ := b.expected(); rev > 0 {
			return rev
		}
	}
	return 0
}

// tracked returns a session built from snap that a store has just loaded.
func tracked(snap *Snapshot) Session {
	s := FromSnapshot(snap).(*basic)
	s.tracked = true
	return s
}

// checkRevision decides the revision of a save of sess over a stored
// revision current (exists reports whether one is stored). It returns
// ErrConflict when sess was loaded from or saved to the store and the
// stored session changed since. A tracked session saved after the stored
// one was deleted continues from its own revision.
func checkRevision(sess Session, current int64, exists bool) (int64, error) {
	if b, ok := sess.(*basic); ok {
		rev, tracked := b.expected()
		switch {
		case tracked && exists && rev != current:
			return 0, fmt.Errorf("session %s: %w", sess.ID(), ErrConflict)
		case tracked && !exists && rev > current:
			// The stored session was deleted. Continuing from rev makes
			// copies loaded before the deletion conflict.
			return rev + 1, nil
		}
	}
	return current + 1, nil
}

// saved records that sess was stored at rev.
func saved(sess Session, rev int64) {
	if b, ok := sess.(*basic); ok {
		b.track(rev)
	}
}

// New returns an empty in-memory session. An empty id is replaced by NewID.
func New(id string) Session {
	if id == "" {
		id = NewID()
	}
	now := time.Now()
	return &basic{id: id, state: &State{values: map[string]any{}}, createdAt: now, updatedAt: now}
}

func (s *basic) ID() string { return s.id }

func (s *basic) Messages() []*model.Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return model.CloneMessages(s.messages)
}

func (s *basic) Append(msgs ...*model.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range msgs {
		s.messages = append(s.messages, m.Clone())
	}
	s.updatedAt = time.Now()
}

func (s *basic) Replace(msgs []*model.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = model.CloneMessages(msgs)
	s.updatedAt = time.Now()
}

func (s *basic) State() *State { return s.state }

func (s *basic) CreatedAt() time.Time { return s.createdAt }

func (s *basic) UpdatedAt() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.updatedAt
}

// MemoryStore keeps sessions in process memory as JSON snapshots, with the
// state representation and session IDs of a persistent store. Later
// changes to a session are visible only after Save, which returns
// ErrConflict for concurrent modification. The zero value is ready to use.
type MemoryStore struct {
	mu       sync.Mutex // serializes Save
	sessions syncmap.Map[string, *Snapshot]
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

// Get implements Store.
func (s *MemoryStore) Get(_ context.Context, id string) (Session, error) {
	if err := checkID(id); err != nil {
		return nil, err
	}
	snap, ok := s.sessions.Get(id)
	if !ok {
		return nil, ErrNotFound
	}
	copy, err := cloneSnapshot(snap)
	if err != nil {
		return nil, err
	}
	return tracked(copy), nil
}

// Save implements Store.
func (s *MemoryStore) Save(_ context.Context, sess Session) error {
	if err := checkID(sess.ID()); err != nil {
		return err
	}
	snap, err := cloneSnapshot(TakeSnapshot(sess))
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var current int64
	old, exists := s.sessions.Get(snap.ID)
	if exists {
		current = old.Revision
	}
	if snap.Revision, err = checkRevision(sess, current, exists); err != nil {
		return err
	}
	s.sessions.Set(snap.ID, snap)
	saved(sess, snap.Revision)
	return nil
}

func checkID(id string) error {
	if !validID(id) {
		return fmt.Errorf("invalid session id %q", id)
	}
	return nil
}

func cloneSnapshot(snap *Snapshot) (*Snapshot, error) {
	b, err := json.Marshal(snap)
	if err != nil {
		return nil, err
	}
	var clone Snapshot
	if err := json.Unmarshal(b, &clone); err != nil {
		return nil, err
	}
	return &clone, nil
}

// Delete implements Store.
func (s *MemoryStore) Delete(_ context.Context, id string) error {
	if err := checkID(id); err != nil {
		return err
	}
	s.sessions.Delete(id)
	return nil
}

// List implements Store.
func (s *MemoryStore) List(context.Context) ([]Info, error) {
	snaps := s.sessions.Values()
	out := make([]Info, 0, len(snaps))
	for _, snap := range snaps {
		out = append(out, snap.info())
	}
	sortInfos(out)
	return out, nil
}

func (snap *Snapshot) info() Info {
	return Info{ID: snap.ID, Messages: len(snap.Messages), CreatedAt: snap.CreatedAt, UpdatedAt: snap.UpdatedAt}
}

func sortInfos(infos []Info) {
	slices.SortFunc(infos, func(a, b Info) int {
		if c := b.UpdatedAt.Compare(a.UpdatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
}
