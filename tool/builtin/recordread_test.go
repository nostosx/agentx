package builtin

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// growingReader supplies bytes past the size reported by Stat, but stops
// a regressed unbounded copy without leaving a busy goroutine behind.
type growingReader struct {
	remaining int64
	exceeded  bool
}

func (r *growingReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		r.exceeded = true
		return 0, errors.New("read past the growth probe")
	}
	n := min(int64(len(p)), r.remaining)
	for i := range p[:n] {
		p[i] = 'x'
	}
	r.remaining -= n
	return int(n), nil
}

func TestRecordReadStopsAtStatSize(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"log.txt": "line\n"})
	st, err := os.Stat(filepath.Join(dir, "log.txt"))
	if err != nil {
		t.Fatal(err)
	}
	files := &FileTracker{}
	e := newEnv(Config{Dir: dir, Files: files})
	r := &growingReader{remaining: st.Size() + 1}
	e.recordRead("log.txt", st, r, sha256.New(), 0)
	if r.exceeded || r.remaining != 0 {
		t.Fatalf("read must stop one byte past Stat size: remaining=%d, exceeded=%v", r.remaining, r.exceeded)
	}
	files.mu.Lock()
	defer files.mu.Unlock()
	stamp, ok := files.files[e.abs("log.txt")]
	if !ok {
		t.Fatal("file not recorded")
	}
	if stamp.hashed {
		t.Error("a file that grew past its size was recorded with a hash")
	}
}
