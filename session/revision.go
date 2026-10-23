package session

// The functions below let Store implementations outside this package
// detect concurrent modification like MemoryStore; see ErrConflict.

// Loaded returns the session a Store returns from Get for snap, which it
// loaded at snap.Revision. Saving the session expects that revision; see
// ExpectedRevision.
func Loaded(snap *Snapshot) Session { return tracked(snap) }

// ExpectedRevision reports the stored revision a Store must find to save s
// without ErrConflict. ok is false for sessions not loaded or saved by a
// store, such as those from New or FromSnapshot, which overwrite the stored
// session. rev is -1 for a session GetOrCreate made, which conflicts with
// any stored session. When the session is not stored at all, for example
// because it was deleted, it is saved whatever rev is, as revision
// max(rev, 0)+1 so that copies loaded before the deletion conflict.
func ExpectedRevision(s Session) (rev int64, ok bool) {
	if b, isBasic := s.(*basic); isBasic {
		return b.expected()
	}
	return 0, false
}

// MarkSaved records that a Store saved s as revision rev, which its next
// save expects.
func MarkSaved(s Session, rev int64) { saved(s, rev) }

// validID reports whether id is a valid session ID: 1 to 128 ASCII
// letters, digits, '_', '-' and '.', not starting with '.'.
func validID(id string) bool { return idPattern.MatchString(id) }
