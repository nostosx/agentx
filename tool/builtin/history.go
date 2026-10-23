package builtin

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/nostosx/agentx/loop"

	"github.com/nostosx/kitx/atomicfile"
)

// History keeps the state files had before the file tools changed them, so
// the files of a session can be restored to a checkpoint, like Claude
// Code's checkpointing (https://code.claude.com/docs/en/checkpointing).
// write_file, edit_file and apply_patch save to Config.History before each
// change. Changes made by bash, by other tools or outside the agent are not
// tracked.
//
// A checkpoint marks a point in a session's history: agent.Runner starts
// one, named after the run's ID, before each run. Restoring a checkpoint
// returns every file changed since then to its state before the first
// change after the checkpoint.
//
// Implementations must be safe for concurrent use.
type History interface {
	// Save records the current state of files, named relative to dir,
	// which a call of the named tool is about to change. The session, run
	// and turn are those of the run executing the call, found with
	// loop.CallingRun; for the call of a nested run, such as a
	// sub-agent's, they are those of the outermost run with a session,
	// found with loop.Run.Caller. Outside a run with a session nothing
	// is saved. If no checkpoint of the session is named after the run,
	// Save starts one.
	Save(ctx context.Context, tool, dir string, edits []FileEdit) error
	// Checkpoint starts a checkpoint of a session, unless the latest one
	// has the same ID.
	Checkpoint(ctx context.Context, sessionID, id string) error
	// Checkpoints returns the checkpoints of a session that are kept,
	// oldest first, each with the changes saved after it and before the
	// next.
	Checkpoints(ctx context.Context, sessionID string) ([]HistoryCheckpoint, error)
	// Restore returns tracked files to their state at the checkpoint and
	// reports files it cannot restore in Restoration.Skipped. On failure it
	// attempts to undo changes already applied; rollback can also fail.
	// It returns ErrCheckpointNotFound for a checkpoint that is not kept.
	Restore(ctx context.Context, sessionID, id string) (*Restoration, error)
	// Fork copies the history of a session to a new session, so changes
	// saved for either later do not affect the other's.
	Fork(ctx context.Context, from, to string) error
	// Delete removes the history of a session.
	Delete(ctx context.Context, sessionID string) error
}

// ErrCheckpointNotFound is returned for a checkpoint that does not exist
// or is no longer kept.
var ErrCheckpointNotFound = errors.New("checkpoint not found")

// ChangeKind is the kind of a file change.
type ChangeKind string

const (
	// FileCreated is the creation of a file that did not exist.
	FileCreated ChangeKind = "created"
	// FileModified is a change to the content of a file.
	FileModified ChangeKind = "modified"
	// FileDeleted is the deletion of a file.
	FileDeleted ChangeKind = "deleted"
)

// FileEdit is a change a tool is about to make to a file.
type FileEdit struct {
	// Path is the path of the file, relative to the tool's working
	// directory.
	Path string
	Kind ChangeKind
	// From and To are set for a file apply_patch moves: From on the file
	// it creates or replaces, To on the file it deletes.
	From, To string
}

// FileChange is a change a tool made to a file, with the state the file
// had before it.
type FileChange struct {
	// Checkpoint is the ID of the checkpoint the change belongs to.
	Checkpoint string `json:"checkpoint"`
	// Run and Turn identify the run, and its turn, that made the change;
	// for a sub-agent, the outermost run with a session.
	Run  string `json:"run,omitempty"`
	Turn int    `json:"turn,omitempty"`
	// Tool is the name of the tool that made the change. Restore records
	// the files' state before it as changes of the tool "rewind".
	Tool string `json:"tool,omitempty"`
	// Dir is the absolute working directory of the tool, and Path the
	// slash-separated path of the file relative to it.
	Dir  string     `json:"dir"`
	Path string     `json:"path"`
	Kind ChangeKind `json:"kind"`
	// From and To are those of the FileEdit.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`

	// Existed reports whether the file existed before the change. Mode,
	// Size and Hash, the hex SHA-256 of the content, describe the file
	// then; Link is the target of a symbolic link.
	Existed bool        `json:"existed"`
	Mode    fs.FileMode `json:"mode,omitempty"`
	Size    int64       `json:"size,omitempty"`
	Hash    string      `json:"hash,omitempty"`
	Link    string      `json:"link,omitempty"`
	// Skipped, if set, says why the content was not saved, or, in a
	// Restoration, why the file was not restored.
	Skipped string `json:"skipped,omitempty"`

	Time time.Time `json:"time"`
	// Seq orders the changes of a session.
	Seq int64 `json:"seq"`
}

// HistoryCheckpoint is a checkpoint of a History.
type HistoryCheckpoint struct {
	ID   string
	Time time.Time
	// Changes are the changes saved after the checkpoint and before the
	// next one, oldest first.
	Changes []FileChange
}

// Restoration is the result of History.Restore.
type Restoration struct {
	// Files are the restored files, each in the state it was restored to.
	Files []FileChange
	// Skipped are files that were changed since the checkpoint but not
	// restored, with the reason in FileChange.Skipped.
	Skipped []FileChange
	// Undo is the ID of a checkpoint holding the state of the restored
	// files before the restore; restoring it undoes the restore. It is
	// empty if no file changed.
	Undo string
}

// FileHistoryConfig configures a FileHistory. Zero limits take their
// defaults; negative ones are unlimited.
type FileHistoryConfig struct {
	// Dir holds the history. The default is .agentx/history in the
	// current directory. Keep it out of the reach of the file tools, or
	// at least out of their working directory's version control.
	Dir string
	// MaxCheckpoints is the number of the most recent checkpoints kept
	// for each session; the default is 100, as in Claude Code. Older
	// checkpoints and their changes are removed.
	MaxCheckpoints int
	// MaxBytes bounds the total size of the file contents kept for each
	// session; the default is 1 GiB. The oldest checkpoints are removed
	// beyond it, except the latest.
	MaxBytes int64
	// MaxFileSize is the largest file whose content is saved; the default
	// is 16 MiB. Larger files are recorded but not restored.
	MaxFileSize int64
	// MaxAge is how long the history of a session is kept after its last
	// change; the default is 30 days, as in Claude Code.
	MaxAge time.Duration
}

const (
	defaultHistoryCheckpoints = 100
	defaultHistoryBytes       = 1 << 30
	defaultHistoryFileSize    = 16 << 20
	defaultHistoryAge         = 30 * 24 * time.Hour
	// historySweep is the interval of the automatic sweep of expired
	// sessions.
	historySweep = time.Hour
	// historyVersion is the version of the index format.
	historyVersion = 1
)

// FileHistory is a History in a directory. File contents are stored once
// by their SHA-256, shared by all sessions, so a fork copies only the
// session's index:
//
//	<Dir>/objects/<2 hex digits>/<62 hex digits>   file contents
//	<Dir>/sessions/<session ID>.json              checkpoints and changes
//
// Contents are checked against their hash before a restore. Files are
// written with mode 0600 in directories with mode 0700. A FileHistory
// assumes it is the only user of its directory. Its lock protects calls on
// this instance, not concurrent changes to the workspace files it tracks.
//
// Restore forgets the restored files in the FileTracker of the tools built
// with the history, so the model must read them again before changing
// them.
type FileHistory struct {
	cfg FileHistoryConfig
	// now returns the current time; tests replace it.
	now func() time.Time

	mu       sync.Mutex
	swept    time.Time
	trackers map[*FileTracker]struct{}
}

// NewFileHistory returns a history in cfg.Dir, creating it if needed.
func NewFileHistory(cfg FileHistoryConfig) (*FileHistory, error) {
	dir, err := filepath.Abs(cmp.Or(cfg.Dir, filepath.Join(".agentx", "history")))
	if err != nil {
		return nil, err
	}
	cfg.Dir = dir
	cfg.MaxCheckpoints = limit(cfg.MaxCheckpoints, defaultHistoryCheckpoints)
	cfg.MaxBytes = limit(cfg.MaxBytes, defaultHistoryBytes)
	cfg.MaxFileSize = limit(cfg.MaxFileSize, defaultHistoryFileSize)
	cfg.MaxAge = limit(cfg.MaxAge, defaultHistoryAge)
	for _, d := range []string{dir, filepath.Join(dir, "objects"), filepath.Join(dir, "sessions")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	return &FileHistory{cfg: cfg, now: time.Now}, nil
}

// limit applies the default of a limit: zero takes def, and negative
// values mean no limit, which is -1.
func limit[T int | int64 | time.Duration](v, def T) T {
	switch {
	case v == 0:
		return def
	case v < 0:
		return -1
	}
	return v
}

// track registers the FileTracker of tools that save to h.
func (h *FileHistory) track(t *FileTracker) {
	if t == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.trackers == nil {
		h.trackers = map[*FileTracker]struct{}{}
	}
	h.trackers[t] = struct{}{}
}

// historyIndex is the stored history of a session.
type historyIndex struct {
	Version     int           `json:"version"`
	Session     string        `json:"session"`
	Next        int64         `json:"next"`
	Checkpoints []historyMark `json:"checkpoints"`
	Changes     []FileChange  `json:"changes"`
}

// historyMark is a checkpoint: the changes from Seq on belong to it, up to
// the next checkpoint.
type historyMark struct {
	ID   string    `json:"id"`
	Seq  int64     `json:"seq"`
	Time time.Time `json:"time"`
}

// find returns the position of the first checkpoint with the ID, or -1.
func (x *historyIndex) find(id string) int {
	return slices.IndexFunc(x.Checkpoints, func(m historyMark) bool { return m.ID == id })
}

// mark starts a checkpoint.
func (x *historyIndex) mark(id string, now time.Time) {
	x.Checkpoints = append(x.Checkpoints, historyMark{ID: id, Seq: x.Next, Time: now})
}

// add appends a change with the next sequence number.
func (x *historyIndex) add(c FileChange) {
	c.Seq = x.Next
	x.Next++
	x.Changes = append(x.Changes, c)
}

// dropOldest removes the n oldest checkpoints and their changes and
// returns the hashes of the contents no longer referenced.
func (x *historyIndex) dropOldest(n int) map[string]bool {
	n = min(n, len(x.Checkpoints))
	if n <= 0 {
		return nil
	}
	from := x.Next
	if n < len(x.Checkpoints) {
		from = x.Checkpoints[n].Seq
	}
	x.Checkpoints = slices.Delete(x.Checkpoints, 0, n)
	dropped := map[string]bool{}
	x.Changes = slices.DeleteFunc(x.Changes, func(c FileChange) bool {
		if c.Seq < from && c.Hash != "" {
			dropped[c.Hash] = true
		}
		return c.Seq < from
	})
	for _, c := range x.Changes {
		delete(dropped, c.Hash)
	}
	return dropped
}

// size returns the total size of the distinct contents of the changes.
func (x *historyIndex) size() int64 {
	seen := map[string]bool{}
	var n int64
	for _, c := range x.Changes {
		if c.Hash != "" && !seen[c.Hash] {
			seen[c.Hash] = true
			n += c.Size
		}
	}
	return n
}

// sessionFilePattern matches the session IDs of session.Store, which are
// usable as file names.
var sessionFilePattern = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]{0,127}$`)

// indexPath returns the file of a session's index. IDs that are not
// usable as file names are hashed.
func (h *FileHistory) indexPath(sessionID string) string {
	name := sessionID
	if !sessionFilePattern.MatchString(name) {
		sum := sha256.Sum256([]byte(sessionID))
		name = "sha256-" + hex.EncodeToString(sum[:])
	}
	return filepath.Join(h.cfg.Dir, "sessions", name+".json")
}

// load reads the index of a session and reports whether it exists.
func (h *FileHistory) load(sessionID string) (*historyIndex, bool, error) {
	if sessionID == "" {
		return nil, false, errors.New("file history: session ID is empty")
	}
	x := &historyIndex{Version: historyVersion, Session: sessionID}
	data, err := os.ReadFile(h.indexPath(sessionID))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return x, false, nil
	case err != nil:
		return nil, false, err
	}
	if err := json.Unmarshal(data, x); err != nil {
		return nil, false, fmt.Errorf("file history of session %s: %w", sessionID, err)
	}
	switch {
	case x.Version > historyVersion:
		return nil, false, fmt.Errorf("file history of session %s has version %d; this version supports up to %d", sessionID, x.Version, historyVersion)
	case x.Session != sessionID:
		return nil, false, fmt.Errorf("file history %s belongs to session %s, not %s", h.indexPath(sessionID), x.Session, sessionID)
	}
	return x, true, nil
}

// store writes the index of a session.
func (h *FileHistory) store(x *historyIndex) error {
	x.Version = historyVersion
	data, err := json.Marshal(x)
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(h.indexPath(x.Session), data, 0o600)
}

// objectPath returns the file of the content with the hex hash.
func (h *FileHistory) objectPath(hash string) string {
	return filepath.Join(h.cfg.Dir, "objects", hash[:2], hash[2:])
}

// validHash reports whether s is a hex SHA-256, so it can name an object.
func validHash(s string) bool {
	if len(s) != 2*sha256.Size {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// writeObject stores content under its hash, unless it is stored already.
func (h *FileHistory) writeObject(hash string, data []byte) error {
	path := h.objectPath(hash)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return atomicfile.WriteFile(path, data, 0o600)
}

// readObject returns the content with the hash, checking it.
func (h *FileHistory) readObject(hash string) ([]byte, error) {
	if !validHash(hash) {
		return nil, fmt.Errorf("file history: invalid content hash %q", hash)
	}
	data, err := os.ReadFile(h.objectPath(hash))
	if err != nil {
		return nil, fmt.Errorf("file history: %w", err)
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != hash {
		return nil, fmt.Errorf("file history: the content %s is corrupt", hash)
	}
	return data, nil
}

// collect removes the contents among candidates that no session
// references.
func (h *FileHistory) collect(candidates map[string]bool) error {
	if len(candidates) == 0 {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(h.cfg.Dir, "sessions"))
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(h.cfg.Dir, "sessions", e.Name()))
		if err != nil {
			return err
		}
		var x historyIndex
		if err := json.Unmarshal(data, &x); err != nil {
			// Keep everything a damaged index may reference.
			return fmt.Errorf("file history %s: %w", e.Name(), err)
		}
		for _, c := range x.Changes {
			delete(candidates, c.Hash)
		}
	}
	var errs []error
	for hash := range candidates {
		if !validHash(hash) {
			continue
		}
		if err := os.Remove(h.objectPath(hash)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// callingSession returns the session, run and turn of the outermost run
// with a session among the run executing the current tool call and the
// runs that started it.
func callingSession(ctx context.Context) (session, run string, turn int) {
	for r := loop.CallingRun(ctx); r != nil; r = r.Caller() {
		if id := r.SessionID(); id != "" {
			session, run, turn = id, r.ID(), r.Turn()
		}
	}
	return session, run, turn
}

// Save implements History.
func (h *FileHistory) Save(ctx context.Context, tool, dir string, edits []FileEdit) error {
	session, run, turn := callingSession(ctx)
	if session == "" || len(edits) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()

	h.mu.Lock()
	defer h.mu.Unlock()
	h.sweep()
	x, _, err := h.load(session)
	if err != nil {
		return err
	}
	now := h.now()
	if x.find(run) < 0 {
		x.mark(run, now)
	}
	checkpoint := x.Checkpoints[len(x.Checkpoints)-1].ID
	for _, e := range edits {
		rel := filepath.Clean(filepath.FromSlash(e.Path))
		st, err := h.capture(root, rel)
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.ToSlash(rel), err)
		}
		if st.data != nil {
			if err := h.writeObject(st.Hash, st.data); err != nil {
				return err
			}
		}
		c := st.FileChange
		c.Checkpoint, c.Run, c.Turn, c.Tool, c.Time = checkpoint, run, turn, tool, now
		c.Dir, c.Path, c.Kind, c.From, c.To = dir, filepath.ToSlash(rel), e.Kind, e.From, e.To
		x.add(c)
	}
	return h.storeTrimmed(x)
}

// storeTrimmed removes the oldest checkpoints beyond MaxCheckpoints and
// MaxBytes, except the latest, writes the index and removes the contents
// no longer referenced.
func (h *FileHistory) storeTrimmed(x *historyIndex) error {
	dropped := map[string]bool{}
	add := func(hashes map[string]bool) {
		for hash := range hashes {
			dropped[hash] = true
		}
	}
	if keep := max(h.cfg.MaxCheckpoints, 1); h.cfg.MaxCheckpoints >= 0 && len(x.Checkpoints) > keep {
		add(x.dropOldest(len(x.Checkpoints) - keep))
	}
	for h.cfg.MaxBytes >= 0 && len(x.Checkpoints) > 1 && x.size() > h.cfg.MaxBytes {
		add(x.dropOldest(1))
	}
	if err := h.store(x); err != nil {
		return err
	}
	return h.collect(dropped)
}

// sweep runs prune if an hour passed since the last sweep. It is best
// effort; Prune reports the errors.
func (h *FileHistory) sweep() {
	if now := h.now(); h.cfg.MaxAge >= 0 && now.Sub(h.swept) >= historySweep {
		h.swept = now
		_ = h.prune()
	}
}

// errIsDir is the error of capture for a directory.
var errIsDir = errors.New("it is a directory")

// fileState is the state of a file with its content, if it is saved.
type fileState struct {
	FileChange
	data []byte
	// dangling is set for a symbolic link to a missing file, and large for
	// a file beyond MaxFileSize; their content is not saved.
	dangling, large bool
}

// capture returns the state of the file at rel.
func (h *FileHistory) capture(root *os.Root, rel string) (fileState, error) {
	var s fileState
	lst, err := root.Lstat(rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return s, nil
	case err != nil:
		return s, err
	}
	s.Existed = true
	if lst.Mode()&fs.ModeSymlink != 0 {
		if s.Link, err = root.Readlink(rel); err != nil {
			return s, err
		}
	}
	st, err := root.Stat(rel)
	switch {
	case err != nil && s.Link != "" && errors.Is(err, fs.ErrNotExist):
		s.dangling, s.Skipped = true, "it was a symbolic link to a missing file"
		return s, nil
	case err != nil:
		return s, err
	case st.IsDir():
		return s, errIsDir
	}
	s.Mode, s.Size = st.Mode().Perm(), st.Size()
	if h.cfg.MaxFileSize >= 0 && s.Size > h.cfg.MaxFileSize {
		s.large, s.Skipped = true, fmt.Sprintf("it was larger than %d bytes", h.cfg.MaxFileSize)
		return s, nil
	}
	if s.data, err = root.ReadFile(rel); err != nil {
		return s, err
	}
	sum := sha256.Sum256(s.data)
	s.Size, s.Hash = int64(len(s.data)), hex.EncodeToString(sum[:])
	return s, nil
}

// Checkpoint implements History. It also removes the oldest checkpoints
// beyond MaxCheckpoints and, at most once an hour, the histories of
// sessions beyond MaxAge, as Prune does.
func (h *FileHistory) Checkpoint(ctx context.Context, sessionID, id string) error {
	if id == "" {
		return errors.New("file history: checkpoint ID is empty")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sweep()
	x, _, err := h.load(sessionID)
	if err != nil {
		return err
	}
	if n := len(x.Checkpoints); n > 0 && x.Checkpoints[n-1].ID == id {
		return nil
	}
	x.mark(id, h.now())
	return h.storeTrimmed(x)
}

// Checkpoints implements History.
func (h *FileHistory) Checkpoints(ctx context.Context, sessionID string) ([]HistoryCheckpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	x, _, err := h.load(sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]HistoryCheckpoint, len(x.Checkpoints))
	for i, m := range x.Checkpoints {
		end := x.Next
		if i+1 < len(x.Checkpoints) {
			end = x.Checkpoints[i+1].Seq
		}
		out[i] = HistoryCheckpoint{ID: m.ID, Time: m.Time}
		for _, c := range x.Changes {
			if c.Seq >= m.Seq && c.Seq < end {
				out[i].Changes = append(out[i].Changes, c)
			}
		}
	}
	return out, nil
}

// restorePlan holds the changes a restore makes in one working directory.
type restorePlan struct {
	dir     string
	root    *os.Root
	changes []fileChange
	// files are the states the files are restored to, and undo their
	// states before, with their contents in data.
	files []FileChange
	undo  []FileChange
	data  [][]byte
}

// Restore implements History. Files are restored like apply_patch commits
// a patch: staging regular files and, if a step fails, attempting to restore
// files already changed. Rollback errors are joined with the original error.
// A file is skipped, and reported in Restoration.Skipped, if its content
// was not saved or a symbolic link would have to be created or replaced;
// a symbolic link that still points to the same file is written through.
// Directories created since the checkpoint are left in place.
func (h *FileHistory) Restore(ctx context.Context, sessionID, id string) (*Restoration, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	x, _, err := h.load(sessionID)
	if err != nil {
		return nil, err
	}
	k := x.find(id)
	if k < 0 {
		return nil, fmt.Errorf("file history of session %s: %w: %s", sessionID, ErrCheckpointNotFound, id)
	}
	// The earliest change to each file since the checkpoint holds its
	// state then.
	from := x.Checkpoints[k].Seq
	type fileKey struct{ dir, path string }
	seen := map[fileKey]bool{}
	var plans []*restorePlan
	defer func() {
		for _, p := range plans {
			p.root.Close()
		}
	}()
	out := &Restoration{}
	for _, t := range x.Changes {
		key := fileKey{t.Dir, t.Path}
		if t.Seq < from || seen[key] {
			continue
		}
		seen[key] = true
		i := slices.IndexFunc(plans, func(p *restorePlan) bool { return p.dir == t.Dir })
		if i < 0 {
			root, err := os.OpenRoot(t.Dir)
			if err != nil {
				return nil, err
			}
			plans = append(plans, &restorePlan{dir: t.Dir, root: root})
			i = len(plans) - 1
		}
		p := plans[i]
		change, cur, skip, err := h.planRestore(p.root, filepath.FromSlash(t.Path), t)
		switch {
		case err != nil:
			return nil, fmt.Errorf("%s: %w", t.Path, err)
		case skip != "":
			t.Skipped = skip
			out.Skipped = append(out.Skipped, t)
		case change != nil:
			p.changes = append(p.changes, *change)
			p.files = append(p.files, t)
			p.undo = append(p.undo, cur.FileChange)
			p.data = append(p.data, cur.data)
		}
	}
	plans = slices.DeleteFunc(plans, func(p *restorePlan) bool {
		if len(p.changes) == 0 {
			p.root.Close()
			return true
		}
		return false
	})
	if len(plans) == 0 {
		return out, nil
	}

	// Save the current states first, so the restore can be undone.
	undo := "rewind_" + uuid.NewV7().String()
	checkpoints, changes, next := len(x.Checkpoints), len(x.Changes), x.Next
	now := h.now()
	x.mark(undo, now)
	for _, p := range plans {
		for i, c := range p.undo {
			if p.data[i] != nil {
				if err := h.writeObject(c.Hash, p.data[i]); err != nil {
					return nil, err
				}
			}
			c.Checkpoint, c.Tool, c.Time, c.Dir, c.Path = undo, "rewind", now, p.dir, p.files[i].Path
			x.add(c)
		}
	}
	if err := h.store(x); err != nil {
		return nil, err
	}
	for i, p := range plans {
		if err := commit(p.root, p.changes); err != nil {
			for _, done := range slices.Backward(plans[:i]) {
				for _, c := range slices.Backward(done.changes) {
					if uerr := c.undo(done.root); uerr != nil {
						err = errors.Join(err, fmt.Errorf("restoring a file failed: %w", uerr))
					}
				}
			}
			x.Checkpoints, x.Changes, x.Next = x.Checkpoints[:checkpoints], x.Changes[:changes], next
			if serr := h.store(x); serr != nil {
				err = errors.Join(err, serr)
			}
			return nil, fmt.Errorf("restoring the files failed, and no file was changed: %w", err)
		}
	}
	for _, p := range plans {
		out.Files = append(out.Files, p.files...)
		for _, c := range p.changes {
			for t := range h.trackers {
				t.forget(filepath.Join(p.dir, c.rel))
			}
		}
	}
	out.Undo = undo
	return out, nil
}

// planRestore returns the change that restores the file at rel to its
// state before t, and the file's current state, or a nil change if the file
// is in that state already, or the reason the file is skipped.
func (h *FileHistory) planRestore(root *os.Root, rel string, t FileChange) (*fileChange, fileState, string, error) {
	cur, err := h.capture(root, rel)
	switch {
	case errors.Is(err, errIsDir):
		return nil, cur, "it is now a directory", nil
	case err != nil:
		return nil, cur, "", err
	case t.Skipped != "":
		return nil, cur, "its state was not saved: " + t.Skipped, nil
	case cur.large:
		// Its current state could not be kept to undo the restore.
		return nil, cur, fmt.Sprintf("it is now larger than %d bytes", h.cfg.MaxFileSize), nil
	}
	c := &fileChange{rel: rel, existed: cur.Existed, old: cur.data, oldPerm: cur.Mode, link: cur.Link}
	switch {
	case !t.Existed && !cur.Existed:
		return nil, cur, "", nil
	case !t.Existed:
		c.remove, cur.Kind = true, FileDeleted
		return c, cur, "", nil
	case t.Link != "" && (cur.Link != t.Link || cur.dangling):
		return nil, cur, "it was a symbolic link, which is restored only while it points to the same file", nil
	case t.Link == "" && cur.Link != "":
		return nil, cur, "it is now a symbolic link", nil
	}
	if c.data, err = h.readObject(t.Hash); err != nil {
		return nil, cur, "", err
	}
	if cur.Existed && bytes.Equal(c.data, cur.data) && (t.Link != "" || cur.Mode == t.Mode) {
		return nil, cur, "", nil
	}
	c.perm, cur.Kind = t.Mode, FileModified
	if !cur.Existed {
		cur.Kind = FileCreated
	}
	return c, cur, "", nil
}

// Fork implements History. The copy shares the stored contents.
func (h *FileHistory) Fork(ctx context.Context, from, to string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	x, ok, err := h.load(from)
	if err != nil || !ok {
		return err
	}
	if to == "" {
		return errors.New("file history: session ID is empty")
	}
	x.Session = to
	return h.store(x)
}

// Delete implements History. Contents no other session references are
// removed.
func (h *FileHistory) Delete(ctx context.Context, sessionID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	x, ok, err := h.load(sessionID)
	if err != nil || !ok {
		return err
	}
	if err := os.Remove(h.indexPath(sessionID)); err != nil {
		return err
	}
	return h.collect(x.dropOldest(len(x.Checkpoints)))
}

// Prune removes the histories of sessions without changes for MaxAge, like
// Claude Code's retention sweep, and the contents no session references.
// Checkpoint runs it at most once an hour.
func (h *FileHistory) Prune(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.prune()
}

func (h *FileHistory) prune() error {
	sessions := filepath.Join(h.cfg.Dir, "sessions")
	entries, err := os.ReadDir(sessions)
	if err != nil {
		return err
	}
	var errs []error
	if h.cfg.MaxAge >= 0 {
		for _, e := range entries {
			info, err := e.Info()
			if err != nil || e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			if h.now().Sub(info.ModTime()) > h.cfg.MaxAge {
				if err := os.Remove(filepath.Join(sessions, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
					errs = append(errs, err)
				}
			}
		}
	}
	all := map[string]bool{}
	objects := filepath.Join(h.cfg.Dir, "objects")
	err = filepath.WalkDir(objects, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(objects, path)
		if err != nil {
			return err
		}
		if hash := strings.ReplaceAll(filepath.ToSlash(rel), "/", ""); validHash(hash) {
			all[hash] = true
		}
		return nil
	})
	if err != nil {
		errs = append(errs, err)
	}
	if err := h.collect(all); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
