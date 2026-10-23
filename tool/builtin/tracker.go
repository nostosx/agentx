package builtin

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"sync"
	"time"
)

// maxStampHash is the largest file whose content hash the tracker keeps.
// Larger files are compared by modification time and size only.
const maxStampHash = 8 << 20

// FileTracker remembers the files read_file has shown to the model, so
// write_file, edit_file and apply_patch can refuse to change or delete an
// existing file the model has not read, or one that changed after it was
// read. The zero value is
// ready to use, and a FileTracker is safe for concurrent use.
//
// The check guards against changing a file from a stale view of it, not
// against concurrent writers: a file can still change between the check
// and the write.
type FileTracker struct {
	mu    sync.Mutex
	files map[string]fileStamp
}

// fileStamp identifies the state of a file the model has seen.
type fileStamp struct {
	mod  time.Time
	size int64
	// sum is the SHA-256 of the content, if hashed is set. It catches
	// changes that keep the size and modification time.
	sum    [sha256.Size]byte
	hashed bool
}

// newStamp returns the stamp of a file in state st with the given content,
// which is nil if it is unknown.
func newStamp(st fs.FileInfo, content []byte) fileStamp {
	s := fileStamp{mod: st.ModTime(), size: st.Size()}
	if content != nil && int64(len(content)) == s.size && s.size <= maxStampHash {
		s.sum, s.hashed = sha256.Sum256(content), true
	}
	return s
}

// matches reports whether a file now in state cur is unchanged since s.
// With both content hashes, a file that was only touched is unchanged.
func (s fileStamp) matches(cur fileStamp) bool {
	if s.hashed && cur.hashed {
		return s.size == cur.size && s.sum == cur.sum
	}
	return s.mod.Equal(cur.mod) && s.size == cur.size
}

// record notes that the model has seen the file at path in state st with
// the given content, or nil if the content is unknown. A nil tracker
// records nothing.
func (t *FileTracker) record(path string, st fs.FileInfo, content []byte) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.files == nil {
		t.files = map[string]fileStamp{}
	}
	t.files[path] = newStamp(st, content)
}

// recordSum is record for a caller that hashed the content itself.
func (t *FileTracker) recordSum(path string, st fs.FileInfo, sum [sha256.Size]byte) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.files == nil {
		t.files = map[string]fileStamp{}
	}
	t.files[path] = fileStamp{mod: st.ModTime(), size: st.Size(), sum: sum, hashed: true}
}

// check fails unless the file at path, now in state st with the given
// content (nil if unknown), was read and has not changed since. A nil
// tracker allows every change.
func (t *FileTracker) check(path, name string, st fs.FileInfo, content []byte) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	seen, ok := t.files[path]
	switch {
	case !ok:
		return fmt.Errorf("%s has not been read; read it with read_file before changing it", name)
	case !seen.matches(newStamp(st, content)):
		return fmt.Errorf("%s has changed since it was last read; read it again before changing it", name)
	}
	return nil
}

// tracks reports whether t checks files.
func (t *FileTracker) tracks() bool { return t != nil }

// forget removes a file the tools deleted. A nil tracker does nothing.
func (t *FileTracker) forget(path string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.files, path)
}
