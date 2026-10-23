package builtin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nostosx/agentx/tool"
)

func call(t *testing.T, tl tool.Tool, args any) *tool.Result {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tl.Call(t.Context(), b)
	if err != nil {
		t.Fatalf("%s: %v", tl.Info().Name, err)
	}
	return res
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// setModTimes sets the modification time of every file under dir.
func setModTimes(t *testing.T, dir string, mod time.Time) {
	t.Helper()
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		return os.Chtimes(p, mod, mod)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestToolsInfo(t *testing.T) {
	approval := map[string]bool{"write_file": true, "edit_file": true, "apply_patch": true, "bash": true}
	readOnly := map[string]bool{"read_file": true, "glob": true, "grep": true, "web_fetch": true}
	for _, tl := range Tools(Config{Dir: t.TempDir()}) {
		info := tl.Info()
		if err := tool.ValidateName(info.Name); err != nil {
			t.Errorf("%s: %v", info.Name, err)
		}
		if info.RequiresApproval != approval[info.Name] {
			t.Errorf("%s: RequiresApproval = %v", info.Name, info.RequiresApproval)
		}
		if info.ReadOnly != readOnly[info.Name] {
			t.Errorf("%s: ReadOnly = %v", info.Name, info.ReadOnly)
		}
	}
	for _, tl := range Tools(Config{NoApproval: true}) {
		if tl.Info().RequiresApproval {
			t.Errorf("%s requires approval with NoApproval", tl.Info().Name)
		}
	}
}

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"a.txt":   "one\ntwo\nthree\n",
		"empty":   "",
		"bin.dat": "ab\x00cd",
	})
	rf := ReadFile(Config{Dir: dir})

	res := call(t, rf, map[string]any{"path": "a.txt"})
	if want := "     1\tone\n     2\ttwo\n     3\tthree"; res.Content != want || res.IsError {
		t.Fatalf("got %q", res.Content)
	}
	res = call(t, rf, map[string]any{"path": filepath.Join(dir, "a.txt"), "offset": 2, "limit": 1})
	if !strings.HasPrefix(res.Content, "     2\ttwo\n") || !strings.Contains(res.Content, "offset=3") {
		t.Fatalf("paged read: %q", res.Content)
	}
	if res := call(t, rf, map[string]any{"path": "empty"}); res.Content != "(empty file)" {
		t.Fatalf("empty: %q", res.Content)
	}
	for _, p := range []string{"bin.dat", "missing", "../outside", "/etc/passwd", "a.txt/.."} {
		if res := call(t, rf, map[string]any{"path": p}); !res.IsError {
			t.Errorf("%s: expected error, got %q", p, res.Content)
		}
	}
	if res := call(t, rf, map[string]any{"path": "a.txt", "offset": 10}); !res.IsError {
		t.Errorf("offset past end: %q", res.Content)
	}
}

func TestReadFileSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir, outside := t.TempDir(), t.TempDir()
	writeFiles(t, outside, map[string]string{"secret": "s"})
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if res := call(t, ReadFile(Config{Dir: dir}), map[string]any{"path": "link"}); !res.IsError {
		t.Fatalf("read through symlink: %q", res.Content)
	}
}

func TestWriteAndEditFile(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Dir: dir}
	res := call(t, WriteFile(cfg), map[string]any{"path": "sub/x.go", "content": "a b a\n"})
	if res.IsError || !strings.HasPrefix(res.Content, "Created sub/x.go") {
		t.Fatalf("write: %q", res.Content)
	}
	p := filepath.Join(dir, "sub", "x.go")
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}

	edit := EditFile(cfg)
	if res := call(t, edit, map[string]any{"path": "sub/x.go", "old_string": "a", "new_string": "c"}); !res.IsError {
		t.Fatalf("ambiguous edit succeeded: %q", res.Content)
	}
	if res := call(t, edit, map[string]any{"path": "sub/x.go", "old_string": "zzz", "new_string": "c"}); !res.IsError {
		t.Fatalf("missing old_string succeeded: %q", res.Content)
	}
	if res := call(t, edit, map[string]any{"path": "sub/x.go", "old_string": "b", "new_string": "B"}); res.IsError {
		t.Fatalf("edit: %q", res.Content)
	}
	res = call(t, edit, map[string]any{"path": "sub/x.go", "old_string": "a", "new_string": "c", "replace_all": true})
	if res.IsError || !strings.Contains(res.Content, "2 replacements") {
		t.Fatalf("replace_all: %q", res.Content)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "c B c\n" {
		t.Fatalf("content %q, %v", b, err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v, want 0600", st.Mode().Perm())
		}
	}
	if res := call(t, WriteFile(cfg), map[string]any{"path": "../escape", "content": "x"}); !res.IsError {
		t.Fatalf("write outside dir succeeded: %q", res.Content)
	}
}

func TestGlob(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"main.go":            "",
		"pkg/a.go":           "",
		"pkg/deep/b.go":      "",
		"pkg/deep/c.ts":      "",
		"web/app.tsx":        "",
		".git/config.go":     "",
		"node_modules/x.go":  "",
		"pkg/deep/README.md": "",
	})
	// Equal modification times leave the name order as the tie-breaker.
	setModTimes(t, dir, time.Unix(1_700_000_000, 0))
	g := Glob(Config{Dir: dir})
	cases := []struct {
		pattern, path, want string
	}{
		{"**/*.go", "", "main.go\npkg/a.go\npkg/deep/b.go"},
		{"*.go", "", "main.go"},
		{"*.go", "pkg", "pkg/a.go"},
		{"**/*.{ts,tsx}", "", "pkg/deep/c.ts\nweb/app.tsx"},
		{"pkg/**", "", "pkg/a.go\npkg/deep/README.md\npkg/deep/b.go\npkg/deep/c.ts"},
		{"*.rs", "", "No files found."},
	}
	for _, c := range cases {
		res := call(t, g, map[string]any{"pattern": c.pattern, "path": c.path})
		if res.Content != c.want {
			t.Errorf("glob %q in %q:\ngot  %q\nwant %q", c.pattern, c.path, res.Content, c.want)
		}
	}
	if res := call(t, g, map[string]any{"pattern": "[", "path": ""}); !res.IsError {
		t.Errorf("bad pattern: %q", res.Content)
	}
}

func TestGrep(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"a.go":          "package a\nfunc Hello() {}\n",
		"b/b.go":        "package b\n// hello world\n",
		"b/notes.txt":   "Hello notes\n",
		"bin.dat":       "Hello\x00",
		".git/HEAD.txt": "Hello",
	})
	g := Grep(Config{Dir: dir})
	res := call(t, g, map[string]any{"pattern": "Hello"})
	if want := "a.go:2:func Hello() {}\nb/notes.txt:1:Hello notes"; res.Content != want {
		t.Fatalf("got %q", res.Content)
	}
	res = call(t, g, map[string]any{"pattern": "hello", "case_insensitive": true, "include": "*.go", "files_only": true})
	if want := "a.go\nb/b.go"; res.Content != want {
		t.Fatalf("files_only: %q", res.Content)
	}
	res = call(t, g, map[string]any{"pattern": "package", "max_results": 1})
	if !strings.HasPrefix(res.Content, "a.go:1:package a\n") || !strings.Contains(res.Content, "limited to 1") {
		t.Fatalf("limit: %q", res.Content)
	}
	res = call(t, g, map[string]any{"pattern": "package", "path": "b/b.go"})
	if res.Content != "b/b.go:1:package b" {
		t.Fatalf("single file: %q", res.Content)
	}
	if res := call(t, g, map[string]any{"pattern": "nothing-here"}); res.Content != "No matches found." {
		t.Fatalf("no match: %q", res.Content)
	}
	if res := call(t, g, map[string]any{"pattern": "("}); !res.IsError {
		t.Fatalf("bad regexp: %q", res.Content)
	}
}

func TestBash(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell syntax")
	}
	dir := t.TempDir()
	b := Bash(Config{Dir: dir, Env: []string{"AGENTX_TEST=ok"}})
	res := call(t, b, map[string]any{"command": "pwd; echo $AGENTX_TEST"})
	real, _ := filepath.EvalSymlinks(dir)
	if res.IsError || !strings.Contains(res.Content, "ok") {
		t.Fatalf("got %q", res.Content)
	}
	if got, _, _ := strings.Cut(res.Content, "\n"); got != dir && got != real {
		t.Errorf("pwd = %q, want %q", got, dir)
	}
	res = call(t, b, map[string]any{"command": "echo oops >&2; exit 3"})
	if !res.IsError || !strings.Contains(res.Content, "oops") || !strings.Contains(res.Content, "[exit code 3]") {
		t.Fatalf("exit: %q", res.Content)
	}
	res = call(t, b, map[string]any{"command": "sleep 5", "timeout_ms": 100})
	if !res.IsError || !strings.Contains(res.Content, "timed out") {
		t.Fatalf("timeout: %q", res.Content)
	}
	res = call(t, Bash(Config{Dir: dir, MaxOutput: 64}), map[string]any{"command": "yes | head -c 1000"})
	if !strings.Contains(res.Content, "bytes truncated") {
		t.Fatalf("truncation: %q", res.Content)
	}
}

func TestWebFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<html><head><title>T</title><style>p{}</style><script>var x=1</script></head>
<body><h1>Title</h1><p>Hello &amp; <b>welcome</b></p><!-- hidden --><ul><li>one</li><li>two</li></ul></body></html>`))
		case "/json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"a":1}`))
		case "/image":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte{0x89, 'P', 'N', 'G'})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	f := WebFetch(Config{HTTPClient: srv.Client()})

	res := call(t, f, map[string]any{"url": srv.URL + "/page"})
	if res.IsError {
		t.Fatal(res.Content)
	}
	body := res.Content[strings.Index(res.Content, "\n\n")+2:]
	if want := "T\nTitle\nHello & welcome\none\ntwo"; body != want {
		t.Errorf("html:\ngot  %q\nwant %q", body, want)
	}
	if res := call(t, f, map[string]any{"url": srv.URL + "/json"}); !strings.HasSuffix(res.Content, `{"a":1}`) {
		t.Errorf("json: %q", res.Content)
	}
	for _, u := range []string{srv.URL + "/image", srv.URL + "/missing", "file:///etc/passwd", "not a url"} {
		if res := call(t, f, map[string]any{"url": u}); !res.IsError {
			t.Errorf("%s: expected error, got %q", u, res.Content)
		}
	}
}

func TestWebFetchOutputBudget(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(strings.Repeat("你好世界🌍", 100)))
	}))
	t.Cleanup(srv.Close)
	for _, limit := range []int{1, 7, 24, 100, 200} {
		res := call(t, WebFetch(Config{HTTPClient: srv.Client(), MaxOutput: limit}), map[string]any{"url": srv.URL})
		if res.IsError || len(res.Content) > limit || !utf8.ValidString(res.Content) {
			t.Errorf("limit %d: output = %q, isError = %v", limit, res.Content, res.IsError)
		}
	}
}

func TestWebFetchErrorOutputBudget(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/unsupported" {
			w.Header().Set("Content-Type", "application/"+strings.Repeat("x", 300))
		} else {
			w.WriteHeader(http.StatusBadGateway)
		}
		_, _ = w.Write([]byte(strings.Repeat("上游错误🌍", 100)))
	}))
	t.Cleanup(srv.Close)
	for _, tc := range []struct{ name, url string }{
		{"http error", srv.URL},
		{"unsupported type", srv.URL + "/unsupported"},
		{"invalid URL", "file:///" + strings.Repeat("私密路径", 100)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, limit := range []int{1, 7, 24, 100, 200} {
				res := call(t, WebFetch(Config{HTTPClient: srv.Client(), MaxOutput: limit}), map[string]any{"url": tc.url})
				if !res.IsError || len(res.Content) > limit || !utf8.ValidString(res.Content) {
					t.Errorf("limit %d: output = %q, isError = %v", limit, res.Content, res.IsError)
				}
			}
		})
	}
}

func TestTodoWrite(t *testing.T) {
	list := &TodoList{}
	tw := TodoWrite(list)
	res := call(t, tw, map[string]any{"todos": []Todo{
		{Content: "plan", Status: TodoCompleted},
		{Content: "build", Status: TodoInProgress},
		{Content: "ship", Status: TodoPending},
	}})
	if want := "[x] plan\n[~] build\n[ ] ship"; res.Content != want {
		t.Fatalf("got %q", res.Content)
	}
	if n := len(list.Items()); n != 3 {
		t.Fatalf("items = %d", n)
	}
	if res := call(t, tw, map[string]any{"todos": []Todo{{Content: "x", Status: "done"}}}); !res.IsError {
		t.Fatalf("invalid status accepted: %q", res.Content)
	}
	if res := call(t, tw, map[string]any{"todos": []Todo{}}); res.Content != "(no todos)" {
		t.Fatalf("clear: %q", res.Content)
	}
}
