package builtin

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

func TestReadOnlyTools(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a.txt": "one\n"})
	files := &FileTracker{}
	tools := ReadOnlyTools(Config{Dir: dir, Files: files})
	var names []string
	for _, tl := range tools {
		info := tl.Info()
		names = append(names, info.Name)
		if !info.ReadOnly || info.RequiresApproval {
			t.Errorf("%s: ReadOnly = %v, RequiresApproval = %v", info.Name, info.ReadOnly, info.RequiresApproval)
		}
	}
	if want := []string{"read_file", "glob", "grep", "web_fetch"}; !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}
	// They are the read-only subset of Tools.
	var all []string
	for _, tl := range Tools(Config{Dir: dir}) {
		if tl.Info().ReadOnly {
			all = append(all, tl.Info().Name)
		}
	}
	if !slices.Equal(all, names) {
		t.Fatalf("read-only tools of Tools = %v, ReadOnlyTools = %v", all, names)
	}

	// A file read through the shared tracker may be edited by the parent.
	if res := call(t, tools[0], map[string]any{"path": "a.txt"}); res.IsError {
		t.Fatalf("read: %q", res.Content)
	}
	if res := call(t, EditFile(Config{Dir: dir, Files: files}), map[string]any{"path": "a.txt", "old_string": "one", "new_string": "two"}); res.IsError {
		t.Fatalf("edit after shared read: %q", res.Content)
	}
	if res := call(t, EditFile(Config{Dir: dir, Files: &FileTracker{}}), map[string]any{"path": "a.txt", "old_string": "two", "new_string": "three"}); !res.IsError || !strings.Contains(res.Content, "not been read") {
		t.Fatalf("edit with another tracker: %q", res.Content)
	}
}

func TestStoppableWriter(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	w := &stoppableWriter{w: &buf}
	if n, err := w.Write([]byte("kept")); n != 4 || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	w.stop()
	// Writes after stop are discarded but reported as complete, so the
	// copy draining a held pipe keeps reading instead of failing.
	if n, err := w.Write([]byte("dropped")); n != 7 || err != nil {
		t.Fatalf("Write after stop = %d, %v", n, err)
	}
	w.stop()
	if buf.String() != "kept" {
		t.Fatalf("buffer = %q", buf.String())
	}
}
