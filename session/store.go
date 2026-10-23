package session

import (
	"context"
	"errors"
	"regexp"
	"time"
)

// ErrNotFound is returned when a session does not exist.
var ErrNotFound = errors.New("session not found")

// ErrConflict is returned by Save when the stored session changed since the
// session being saved was loaded or last saved, for example by another
// Runner or process, or when GetOrCreate made a session that another writer
// created in the meantime. Load the session again and reapply the change.
//
// MemoryStore detects conflicts for the sessions it returns from Get,
// sessions made by GetOrCreate and sessions it has saved before. Saving any
// other Session overwrites the stored one. Stores in other packages do the
// same with Loaded, ExpectedRevision and MarkSaved.
var ErrConflict = errors.New("session was modified concurrently")

// Info summarizes a stored session.
type Info struct {
	ID        string
	Messages  int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Store persists sessions. Session IDs are 1 to 128 ASCII letters, digits,
// '_', '-' and '.', not starting with '.'; the built-in stores reject
// others.
type Store interface {
	// Get loads a session or returns ErrNotFound.
	Get(ctx context.Context, id string) (Session, error)
	Save(ctx context.Context, s Session) error
	Delete(ctx context.Context, id string) error
	// List returns sessions, most recently updated first.
	List(ctx context.Context) ([]Info, error)
}

// GetOrCreate loads a session, creating an empty one if it does not exist.
// The boolean reports whether the session was created.
func GetOrCreate(ctx context.Context, store Store, id string) (Session, bool, error) {
	if id != "" {
		s, err := store.Get(ctx, id)
		if err == nil {
			return s, false, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return nil, false, err
		}
	}
	s := New(id)
	// The session is new to the store; saving it over one created in the
	// meantime is a conflict.
	s.(*basic).track(-1)
	return s, true, nil
}

// idPattern matches session IDs. A leading dot is rejected so that stores
// keeping one file per session can skip hidden files.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]{0,127}$`)
