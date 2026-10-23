package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/tool"
)

// inRun calls f inside a tool call of a run with the session and run ID,
// so the file history attributes the changes f makes to them.
func inRun(t *testing.T, sessionID, runID string, f func(ctx context.Context)) {
	t.Helper()
	inRunCtx(t, t.Context(), sessionID, runID, f)
}

// inRunCtx is inRun with the context of the run.
func inRunCtx(t *testing.T, ctx context.Context, sessionID, runID string, f func(ctx context.Context)) {
	t.Helper()
	probe := tool.Func("probe", "probe", func(ctx context.Context, _ struct{}) (string, error) {
		f(ctx)
		return "ok", nil
	})
	reg := &tool.Registry{}
	if err := reg.Register(probe); err != nil {
		t.Error(err)
		return
	}
	m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "probe", Name: "probe"}), modeltest.Reply("done"))
	rt, err := loop.New(loop.Config{Model: m, Tools: reg})
	if err != nil {
		t.Error(err)
		return
	}
	res, err := loop.Collect(rt.Run(ctx, &loop.Request{RunID: runID, SessionID: sessionID, Input: model.UserText("go")}), nil)
	if err != nil || res.Status != loop.StatusCompleted {
		t.Errorf("run = %+v, %v", res, err)
	}
}

// callIn calls a tool with a context and fails the test on an error
// result.
func callIn(t *testing.T, ctx context.Context, tl tool.Tool, args any) *tool.Result {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Error(err)
		return tool.Errorf("encode arguments: %v", err)
	}
	res, err := tl.Call(ctx, b)
	if err != nil {
		t.Errorf("%s: %v", tl.Info().Name, err)
		return tool.Errorf("%v", err)
	}
	if res.IsError {
		t.Errorf("%s: %s", tl.Info().Name, res.Content)
	}
	return res
}

func newHistory(t *testing.T, cfg FileHistoryConfig) *FileHistory {
	t.Helper()
	if cfg.Dir == "" {
		cfg.Dir = t.TempDir()
	}
	h, err := NewFileHistory(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// objects returns the number of stored contents.
func objects(t *testing.T, h *FileHistory) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(filepath.Join(h.cfg.Dir, "objects"), func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestFileHistoryRestoresCheckpoints(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"edit.txt": "one\n", "gone.txt": "bye\n", "from.txt": "move me\n"})
	if goruntime.GOOS != "windows" {
		if err := os.Chmod(filepath.Join(dir, "edit.txt"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h := newHistory(t, FileHistoryConfig{})
	cfg := Config{Dir: dir, History: h, NoApproval: true, Files: &FileTracker{}}
	readAll(t, cfg, "edit.txt", "gone.txt", "from.txt")
	if err := h.Checkpoint(t.Context(), "s", "r1"); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, dir)
	inRun(t, "s", "r1", func(ctx context.Context) {
		callIn(t, ctx, EditFile(cfg), map[string]any{"path": "edit.txt", "old_string": "one", "new_string": "two"})
		callIn(t, ctx, WriteFile(cfg), map[string]any{"path": "new/created.txt", "content": "new\n"})
	})
	if err := h.Checkpoint(t.Context(), "s", "r2"); err != nil {
		t.Fatal(err)
	}
	middle := snapshot(t, dir)
	inRun(t, "s", "r2", func(ctx context.Context) {
		callIn(t, ctx, ApplyPatch(cfg), map[string]any{"input": wrapPatch("*** Delete File: gone.txt\n*** Update File: from.txt\n*** Move to: to.txt\n@@\n-move me\n+moved\n*** Update File: edit.txt\n@@\n-two\n+three")})
	})

	cps, err := h.Checkpoints(t.Context(), "s")
	if err != nil || len(cps) != 2 || cps[0].ID != "r1" || cps[1].ID != "r2" {
		t.Fatalf("Checkpoints = %+v, %v", cps, err)
	}
	var kinds []string
	for _, c := range cps[1].Changes {
		kinds = append(kinds, string(c.Kind)+" "+c.Path+" "+c.From+c.To)
		if c.Checkpoint != "r2" || c.Run != "r2" || c.Turn != 1 || c.Tool != "apply_patch" || c.Dir == "" {
			t.Errorf("change = %+v", c)
		}
	}
	// apply_patch lists files in the order the patch touches them.
	if want := []string{"deleted gone.txt ", "deleted from.txt to.txt", "created to.txt from.txt", "modified edit.txt "}; !slices.Equal(kinds, want) {
		t.Errorf("changes = %q, want %q", kinds, want)
	}
	if c := cps[0].Changes; len(c) != 2 || c[0].Kind != FileModified || c[1].Kind != FileCreated || c[1].Existed {
		t.Errorf("first checkpoint changes = %+v", c)
	}

	rest, err := h.Restore(t.Context(), "s", "r2")
	if err != nil || len(rest.Files) != 4 || len(rest.Skipped) != 0 || rest.Undo == "" {
		t.Fatalf("Restore(r2) = %+v, %v", rest, err)
	}
	if got := snapshot(t, dir); !mapsEqual(got, middle) {
		t.Fatalf("after Restore(r2) = %v, want %v", got, middle)
	}
	rest, err = h.Restore(t.Context(), "s", "r1")
	if err != nil {
		t.Fatal(err)
	}
	// The directory created after the checkpoint stays.
	before["new/"] = ""
	if got := snapshot(t, dir); !mapsEqual(got, before) {
		t.Fatalf("after Restore(r1) = %v, want %v", got, before)
	}
	if goruntime.GOOS != "windows" {
		if st, err := os.Stat(filepath.Join(dir, "edit.txt")); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("mode after restore = %v, %v", st.Mode(), err)
		}
	}
	// The restore can be undone.
	if _, err := h.Restore(t.Context(), "s", rest.Undo); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, dir); !mapsEqual(got, middle) {
		t.Fatalf("after undoing Restore(r1) = %v, want %v", got, middle)
	}
	// Restoring again to the original state at r2 changes nothing new.
	if rest, err := h.Restore(t.Context(), "s", "r2"); err != nil || len(rest.Files) != 0 || rest.Undo != "" {
		t.Fatalf("Restore(r2) in the state of r2 = %+v, %v", rest, err)
	}
	if _, err := h.Restore(t.Context(), "s", "missing"); !errors.Is(err, ErrCheckpointNotFound) {
		t.Fatalf("Restore(missing) = %v", err)
	}
}

func TestFileHistoryForgetsRestoredFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a.txt": "a\n"})
	h := newHistory(t, FileHistoryConfig{})
	cfg := Config{Dir: dir, History: h, NoApproval: true}
	tools := Tools(cfg)
	find := func(name string) tool.Tool {
		i := slices.IndexFunc(tools, func(tl tool.Tool) bool { return tl.Info().Name == name })
		return tools[i]
	}
	inRun(t, "s", "r1", func(ctx context.Context) {
		callIn(t, ctx, find("read_file"), map[string]any{"path": "a.txt"})
		callIn(t, ctx, find("edit_file"), map[string]any{"path": "a.txt", "old_string": "a", "new_string": "b"})
	})
	if _, err := h.Restore(t.Context(), "s", "r1"); err != nil {
		t.Fatal(err)
	}
	// The tracker would also refuse because the content changed; it must
	// refuse even if the restored content matched what the model read.
	res := call(t, find("edit_file"), map[string]any{"path": "a.txt", "old_string": "a", "new_string": "c"})
	if !res.IsError || !strings.Contains(res.Content, "has not been read") {
		t.Fatalf("edit after restore = %+v", res)
	}
}

func TestFileHistoryOutsideRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newHistory(t, FileHistoryConfig{})
	res := call(t, WriteFile(Config{Dir: dir, History: h, NoApproval: true}), map[string]any{"path": "a.txt", "content": "x"})
	if res.IsError {
		t.Fatal(res.Content)
	}
	entries, err := os.ReadDir(filepath.Join(h.cfg.Dir, "sessions"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("sessions = %v, %v", entries, err)
	}
	// A run without a session is not recorded either.
	inRun(t, "", "r", func(ctx context.Context) {
		callIn(t, ctx, WriteFile(Config{Dir: dir, History: h, NoApproval: true}), map[string]any{"path": "b.txt", "content": "x"})
	})
	if entries, err := os.ReadDir(filepath.Join(h.cfg.Dir, "sessions")); err != nil || len(entries) != 0 {
		t.Fatalf("sessions = %v, %v", entries, err)
	}
}

// failingHistory fails every save.
type failingHistory struct{ History }

func (failingHistory) Save(context.Context, string, string, []FileEdit) error {
	return errors.New("disk full")
}

func TestFileHistorySaveFailureChangesNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a.txt": "a\n"})
	cfg := Config{Dir: dir, History: failingHistory{}, NoApproval: true}
	before := snapshot(t, dir)
	for _, c := range []struct {
		tl   tool.Tool
		args map[string]any
	}{
		{WriteFile(cfg), map[string]any{"path": "a.txt", "content": "b"}},
		{WriteFile(cfg), map[string]any{"path": "new.txt", "content": "b"}},
		{EditFile(cfg), map[string]any{"path": "a.txt", "old_string": "a", "new_string": "b"}},
		{ApplyPatch(cfg), map[string]any{"input": wrapPatch("*** Delete File: a.txt")}},
	} {
		res := call(t, c.tl, c.args)
		if !res.IsError || !strings.Contains(res.Content, "disk full") || !strings.Contains(res.Content, "no file was changed") {
			t.Errorf("%s = %+v", c.tl.Info().Name, res)
		}
	}
	if got := snapshot(t, dir); !mapsEqual(got, before) {
		t.Fatalf("files = %v, want %v", got, before)
	}
}

func TestFileHistoryRetention(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newHistory(t, FileHistoryConfig{MaxCheckpoints: 2, MaxFileSize: 8})
	cfg := Config{Dir: dir, History: h, NoApproval: true}
	for i, content := range []string{"one", "two", "three"} {
		id := []string{"r1", "r2", "r3"}[i]
		if err := h.Checkpoint(t.Context(), "s", id); err != nil {
			t.Fatal(err)
		}
		inRun(t, "s", id, func(ctx context.Context) {
			callIn(t, ctx, WriteFile(cfg), map[string]any{"path": "a.txt", "content": content})
		})
	}
	cps, err := h.Checkpoints(t.Context(), "s")
	if err != nil || len(cps) != 2 || cps[0].ID != "r2" {
		t.Fatalf("Checkpoints = %+v, %v", cps, err)
	}
	if _, err := h.Restore(t.Context(), "s", "r1"); !errors.Is(err, ErrCheckpointNotFound) {
		t.Fatalf("Restore(r1) = %v", err)
	}
	// The contents "one" and "two" are the states before r2 and r3.
	if n := objects(t, h); n != 2 {
		t.Fatalf("objects = %d, want 2", n)
	}

	// Files larger than MaxFileSize are recorded but not restored.
	inRun(t, "s", "r4", func(ctx context.Context) {
		callIn(t, ctx, WriteFile(cfg), map[string]any{"path": "a.txt", "content": "larger than eight"})
	})
	inRun(t, "s", "r5", func(ctx context.Context) {
		callIn(t, ctx, WriteFile(cfg), map[string]any{"path": "a.txt", "content": "small"})
		callIn(t, ctx, WriteFile(cfg), map[string]any{"path": "b.txt", "content": "b"})
	})
	cps, err = h.Checkpoints(t.Context(), "s")
	if err != nil || len(cps) == 0 {
		t.Fatalf("Checkpoints = %+v, %v", cps, err)
	}
	if last := cps[len(cps)-1]; last.ID != "r5" || len(last.Changes) != 2 || last.Changes[0].Skipped == "" || last.Changes[0].Hash != "" {
		t.Fatalf("checkpoint r5 = %+v", last)
	}
	rest, err := h.Restore(t.Context(), "s", "r5")
	if err != nil || len(rest.Files) != 1 || rest.Files[0].Path != "b.txt" || len(rest.Skipped) != 1 || !strings.Contains(rest.Skipped[0].Skipped, "larger than 8 bytes") {
		t.Fatalf("Restore(r5) = %+v, %v", rest, err)
	}
	if got := snapshot(t, dir); !mapsEqual(got, map[string]string{"a.txt": "small"}) {
		t.Fatalf("files = %v", got)
	}
	// The state before the large file was saved.
	if _, err := h.Restore(t.Context(), "s", "r4"); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, dir); !mapsEqual(got, map[string]string{"a.txt": "three"}) {
		t.Fatalf("files = %v", got)
	}
}

func TestFileHistoryMaxBytes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newHistory(t, FileHistoryConfig{MaxBytes: 10})
	writeFiles(t, dir, map[string]string{"a.txt": "123456", "b.txt": "abcdef"})
	cfg := Config{Dir: dir, History: h, NoApproval: true}
	inRun(t, "s", "r1", func(ctx context.Context) {
		callIn(t, ctx, WriteFile(cfg), map[string]any{"path": "a.txt", "content": "x"})
	})
	inRun(t, "s", "r2", func(ctx context.Context) {
		callIn(t, ctx, WriteFile(cfg), map[string]any{"path": "b.txt", "content": "y"})
	})
	cps, err := h.Checkpoints(t.Context(), "s")
	if err != nil || len(cps) != 1 || cps[0].ID != "r2" {
		t.Fatalf("Checkpoints = %+v, %v", cps, err)
	}
	// The latest checkpoint is kept even beyond the limit.
	inRun(t, "s", "r3", func(ctx context.Context) {
		callIn(t, ctx, WriteFile(cfg), map[string]any{"path": "c.txt", "content": strings.Repeat("z", 20)})
		callIn(t, ctx, WriteFile(cfg), map[string]any{"path": "c.txt", "content": ""})
	})
	cps, err = h.Checkpoints(t.Context(), "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(cps) != 1 || cps[0].ID != "r3" || len(cps[0].Changes) != 2 {
		t.Fatalf("Checkpoints = %+v", cps)
	}
}

func TestFileHistoryPruneByAge(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newHistory(t, FileHistoryConfig{MaxAge: time.Hour})
	cfg := Config{Dir: dir, History: h, NoApproval: true}
	for _, s := range []string{"old", "new"} {
		inRun(t, s, "r", func(ctx context.Context) {
			callIn(t, ctx, WriteFile(cfg), map[string]any{"path": s + ".txt", "content": s})
			callIn(t, ctx, WriteFile(cfg), map[string]any{"path": s + ".txt", "content": s + "!"})
		})
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(h.indexPath("old"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := h.Prune(t.Context()); err != nil {
		t.Fatal(err)
	}
	if cps, err := h.Checkpoints(t.Context(), "old"); err != nil || len(cps) != 0 {
		t.Fatalf("old checkpoints = %+v, %v", cps, err)
	}
	if cps, err := h.Checkpoints(t.Context(), "new"); err != nil || len(cps) != 1 {
		t.Fatalf("new checkpoints = %+v, %v", cps, err)
	}
	if n := objects(t, h); n != 1 {
		t.Fatalf("objects = %d, want 1", n)
	}
	// Checkpoint sweeps automatically once the interval passed.
	if err := os.Chtimes(h.indexPath("new"), old, old); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.swept = time.Now().Add(-2 * historySweep)
	h.mu.Unlock()
	if err := h.Checkpoint(t.Context(), "other", "c"); err != nil {
		t.Fatal(err)
	}
	if n := objects(t, h); n != 0 {
		t.Fatalf("objects after sweep = %d", n)
	}
}

func TestFileHistoryRestoreIsAtomic(t *testing.T) {
	origHook := commitHook
	t.Cleanup(func() { commitHook = origHook })
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a.txt": "a", "b.txt": "b"})
	h := newHistory(t, FileHistoryConfig{})
	cfg := Config{Dir: dir, History: h, NoApproval: true}
	inRun(t, "s", "r1", func(ctx context.Context) {
		callIn(t, ctx, WriteFile(cfg), map[string]any{"path": "a.txt", "content": "A"})
		callIn(t, ctx, WriteFile(cfg), map[string]any{"path": "b.txt", "content": "B"})
		callIn(t, ctx, WriteFile(cfg), map[string]any{"path": "c.txt", "content": "C"})
	})
	before := snapshot(t, dir)
	cps, err := h.Checkpoints(t.Context(), "s")
	if err != nil || len(cps) != 1 {
		t.Fatalf("Checkpoints = %+v, %v", cps, err)
	}
	commitHook = func(rel string) error {
		if rel == "c.txt" {
			return errors.New("injected")
		}
		return nil
	}
	_, err = h.Restore(t.Context(), "s", "r1")
	commitHook = nil
	if err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("Restore = %v", err)
	}
	if got := snapshot(t, dir); !mapsEqual(got, before) {
		t.Fatalf("files = %v, want %v", got, before)
	}
	// The checkpoint that would undo the restore is gone too.
	if after, err := h.Checkpoints(t.Context(), "s"); err != nil || len(after) != len(cps) {
		t.Fatalf("checkpoints = %+v, %v", after, err)
	}

	// A corrupt content fails the restore before any file changes.
	for _, c := range cps[0].Changes {
		if c.Hash != "" {
			if err := os.WriteFile(h.objectPath(c.Hash), []byte("tampered"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := h.Restore(t.Context(), "s", "r1"); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("Restore with corrupt content = %v", err)
	}
	if got := snapshot(t, dir); !mapsEqual(got, before) {
		t.Fatalf("files = %v, want %v", got, before)
	}
}

func TestFileHistoryConfinement(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	dir := filepath.Join(parent, "work")
	writeFiles(t, parent, map[string]string{"outside.txt": "keep", "work/a.txt": "a"})
	h := newHistory(t, FileHistoryConfig{})
	cfg := Config{Dir: dir, History: h, NoApproval: true}
	inRun(t, "s", "r1", func(ctx context.Context) {
		callIn(t, ctx, WriteFile(cfg), map[string]any{"path": "a.txt", "content": "b"})
	})
	// Point the change outside the working directory, as a tampered index
	// would.
	data, err := os.ReadFile(h.indexPath("s"))
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `"path":"a.txt"`, `"path":"../outside.txt"`, 1))
	if err := os.WriteFile(h.indexPath("s"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Restore(t.Context(), "s", "r1"); err == nil {
		t.Fatal("Restore outside the working directory succeeded")
	}
	if b, _ := os.ReadFile(filepath.Join(parent, "outside.txt")); string(b) != "keep" {
		t.Fatalf("outside.txt = %q", b)
	}
}

func TestFileHistoryForkAndDelete(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a.txt": "a"})
	h := newHistory(t, FileHistoryConfig{})
	cfg := Config{Dir: dir, History: h, NoApproval: true}
	inRun(t, "s", "r1", func(ctx context.Context) {
		callIn(t, ctx, WriteFile(cfg), map[string]any{"path": "a.txt", "content": "b"})
	})
	if err := h.Fork(t.Context(), "s", "f"); err != nil {
		t.Fatal(err)
	}
	if err := h.Fork(t.Context(), "none", "g"); err != nil {
		t.Fatalf("Fork of a session without history = %v", err)
	}
	inRun(t, "f", "r2", func(ctx context.Context) {
		callIn(t, ctx, WriteFile(cfg), map[string]any{"path": "b.txt", "content": "fork"})
	})
	orig, err := h.Checkpoints(t.Context(), "s")
	if err != nil {
		t.Fatal(err)
	}
	fork, err := h.Checkpoints(t.Context(), "f")
	if err != nil {
		t.Fatal(err)
	}
	if len(orig) != 1 || len(fork) != 2 || fork[0].ID != "r1" {
		t.Fatalf("checkpoints = %+v, %+v", orig, fork)
	}
	if _, err := h.Restore(t.Context(), "f", "r1"); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, dir); !mapsEqual(got, map[string]string{"a.txt": "a"}) {
		t.Fatalf("files = %v", got)
	}
	// Deleting the fork keeps the contents the original references.
	if err := h.Delete(t.Context(), "f"); err != nil {
		t.Fatal(err)
	}
	if n := objects(t, h); n != 1 {
		t.Fatalf("objects = %d, want 1", n)
	}
	if _, err := h.Restore(t.Context(), "s", "r1"); err != nil {
		t.Fatal(err)
	}
	if err := h.Delete(t.Context(), "s"); err != nil {
		t.Fatal(err)
	}
	if n := objects(t, h); n != 0 {
		t.Fatalf("objects = %d, want 0", n)
	}
}

func TestFileHistorySubAgentRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newHistory(t, FileHistoryConfig{})
	cfg := Config{Dir: dir, History: h, NoApproval: true}
	inRun(t, "s", "outer", func(ctx context.Context) {
		// A nested run without a session, as of a sub-agent.
		inRunCtx(t, ctx, "", "inner", func(ctx context.Context) {
			callIn(t, ctx, WriteFile(cfg), map[string]any{"path": "a.txt", "content": "x"})
		})
	})
	cps, err := h.Checkpoints(t.Context(), "s")
	if err != nil || len(cps) != 1 || cps[0].ID != "outer" || len(cps[0].Changes) != 1 || cps[0].Changes[0].Run != "outer" {
		t.Fatalf("Checkpoints = %+v, %v", cps, err)
	}
}

func TestFileHistorySessionFileNames(t *testing.T) {
	t.Parallel()
	h := newHistory(t, FileHistoryConfig{})
	for _, id := range []string{"plain-id", "../evil", ".hidden", "a/b"} {
		if err := h.Checkpoint(t.Context(), id, "c"); err != nil {
			t.Fatal(err)
		}
		p := h.indexPath(id)
		if filepath.Dir(p) != filepath.Join(h.cfg.Dir, "sessions") {
			t.Fatalf("index of %q = %s", id, p)
		}
		if cps, err := h.Checkpoints(t.Context(), id); err != nil || len(cps) != 1 {
			t.Fatalf("Checkpoints(%q) = %+v, %v", id, cps, err)
		}
	}
	if err := h.Checkpoint(t.Context(), "", "c"); err == nil {
		t.Fatal("Checkpoint without a session succeeded")
	}
	if err := h.Checkpoint(t.Context(), "s", "c"); err != nil {
		t.Fatal(err)
	}
	// Repeating the latest checkpoint does nothing.
	if err := h.Checkpoint(t.Context(), "s", "c"); err != nil {
		t.Fatal(err)
	}
	if cps, err := h.Checkpoints(t.Context(), "s"); err != nil || len(cps) != 1 {
		t.Fatalf("checkpoints = %+v, %v", cps, err)
	}
}

func TestFileHistorySymlinks(t *testing.T) {
	t.Parallel()
	if goruntime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on Windows")
	}
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"target.txt": "target"})
	if err := os.Symlink("target.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	h := newHistory(t, FileHistoryConfig{})
	cfg := Config{Dir: dir, History: h, NoApproval: true}
	inRun(t, "s", "r1", func(ctx context.Context) {
		callIn(t, ctx, WriteFile(cfg), map[string]any{"path": "link.txt", "content": "through"})
	})
	inRun(t, "s", "r2", func(ctx context.Context) {
		callIn(t, ctx, ApplyPatch(cfg), map[string]any{"input": wrapPatch("*** Delete File: link.txt")})
	})
	// A deleted link is not created again.
	rest, err := h.Restore(t.Context(), "s", "r2")
	if err != nil || len(rest.Files) != 0 || len(rest.Skipped) != 1 || !strings.Contains(rest.Skipped[0].Skipped, "symbolic link") {
		t.Fatalf("Restore(r2) = %+v, %v", rest, err)
	}
	if err := os.Symlink("target.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	// A link that still points to the same file is written through.
	rest, err = h.Restore(t.Context(), "s", "r1")
	if err != nil || len(rest.Files) != 1 {
		t.Fatalf("Restore(r1) = %+v, %v", rest, err)
	}
	if target, err := os.Readlink(filepath.Join(dir, "link.txt")); err != nil || target != "target.txt" {
		t.Fatalf("link = %q, %v", target, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "target.txt")); string(b) != "target" {
		t.Fatalf("target.txt = %q", b)
	}
}

func TestFileHistoryConcurrentUse(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newHistory(t, FileHistoryConfig{})
	cfg := Config{Dir: dir, History: h, NoApproval: true}
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			s := string(rune('a' + i))
			inRun(t, s, "r", func(ctx context.Context) {
				callIn(t, ctx, WriteFile(cfg), map[string]any{"path": s + ".txt", "content": s})
			})
			if _, err := h.Checkpoints(t.Context(), s); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	for i := range 4 {
		s := string(rune('a' + i))
		if _, err := h.Restore(t.Context(), s, "r"); err != nil {
			t.Fatal(err)
		}
	}
	if got := snapshot(t, dir); len(got) != 0 {
		t.Fatalf("files = %v", got)
	}
}

func TestFileHistoryRestoreSkips(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"dir.txt": "d", "big.txt": "b", "kept.txt": "k"})
	h := newHistory(t, FileHistoryConfig{MaxFileSize: 4, MaxCheckpoints: -1, MaxBytes: -1, MaxAge: -1})
	if h.cfg.MaxCheckpoints != -1 || h.cfg.MaxBytes != -1 || h.cfg.MaxAge != -1 {
		t.Fatalf("unlimited config = %+v", h.cfg)
	}
	cfg := Config{Dir: dir, History: h, NoApproval: true}
	inRun(t, "s", "r1", func(ctx context.Context) {
		for _, p := range []string{"dir.txt", "big.txt", "kept.txt"} {
			callIn(t, ctx, WriteFile(cfg), map[string]any{"path": p, "content": "new"})
		}
	})
	// A directory replaced a file, and a file grew beyond MaxFileSize.
	if err := os.Remove(filepath.Join(dir, "dir.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "dir.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte("too large now"), 0o644); err != nil {
		t.Fatal(err)
	}
	rest, err := h.Restore(t.Context(), "s", "r1")
	if err != nil || len(rest.Files) != 1 || rest.Files[0].Path != "kept.txt" || len(rest.Skipped) != 2 {
		t.Fatalf("Restore = %+v, %v", rest, err)
	}
	reasons := rest.Skipped[0].Skipped + "; " + rest.Skipped[1].Skipped
	if !strings.Contains(reasons, "now a directory") || !strings.Contains(reasons, "now larger than 4 bytes") {
		t.Fatalf("reasons = %s", reasons)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "kept.txt")); string(b) != "k" {
		t.Fatalf("kept.txt = %q", b)
	}
	if goruntime.GOOS != "windows" {
		// A file replaced by a symbolic link is not written through.
		if err := os.Remove(filepath.Join(dir, "kept.txt")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "other"), []byte("o"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("other", filepath.Join(dir, "kept.txt")); err != nil {
			t.Fatal(err)
		}
		rest, err = h.Restore(t.Context(), "s", "r1")
		if err != nil || len(rest.Files) != 0 || len(rest.Skipped) != 3 || !strings.Contains(rest.Skipped[2].Skipped, "now a symbolic link") {
			t.Fatalf("Restore = %+v, %v", rest, err)
		}
	}
}

func TestFileHistoryDamagedIndex(t *testing.T) {
	t.Parallel()
	h := newHistory(t, FileHistoryConfig{})
	for name, content := range map[string]string{
		"corrupt":  "{",
		"newer":    `{"version":99,"session":"newer"}`,
		"misnamed": `{"version":1,"session":"other"}`,
	} {
		if err := os.WriteFile(h.indexPath(name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := h.Checkpoints(t.Context(), name); err == nil {
			t.Errorf("Checkpoints(%s) succeeded", name)
		}
		if err := h.Checkpoint(t.Context(), name, "c"); err == nil {
			t.Errorf("Checkpoint(%s) succeeded", name)
		}
	}
	// A damaged index keeps the contents it may reference.
	if err := h.writeObject(strings.Repeat("ab", 32), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := h.Prune(t.Context()); err == nil {
		t.Fatal("Prune with a damaged index succeeded")
	}
	if n := objects(t, h); n != 1 {
		t.Fatalf("objects = %d", n)
	}
	if _, err := h.readObject("../escape"); err == nil {
		t.Fatal("readObject accepted an invalid hash")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := h.Checkpoint(ctx, "s", "c"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Checkpoint with a cancelled context = %v", err)
	}
}

func TestFileHistoryStoreFailureCleansUp(t *testing.T) {
	t.Parallel()
	h := newHistory(t, FileHistoryConfig{})
	target := h.indexPath("occupied")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := h.store(&historyIndex{Session: "occupied"}); err == nil {
		t.Fatal("stored index over a directory")
	}
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil || len(entries) != 1 || entries[0].Name() != "occupied.json" {
		t.Fatalf("temporary files leaked: %v, %v", entries, err)
	}
}
