package builtin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestReadBeforeWrite(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a.txt": "one\n", "b.txt": "bee\n"})
	cfg := Config{Dir: dir, Files: &FileTracker{}}
	read, write, edit := ReadFile(cfg), WriteFile(cfg), EditFile(cfg)

	if res := call(t, edit, map[string]any{"path": "a.txt", "old_string": "one", "new_string": "two"}); !res.IsError || !strings.Contains(res.Content, "not been read") {
		t.Fatalf("edit before read: %q", res.Content)
	}
	if res := call(t, write, map[string]any{"path": "b.txt", "content": "x"}); !res.IsError {
		t.Fatalf("overwrite before read: %q", res.Content)
	}
	if res := call(t, write, map[string]any{"path": "new.txt", "content": "fresh\n"}); res.IsError {
		t.Fatalf("create: %q", res.Content)
	}
	// A file the tools wrote can be edited without reading it again.
	if res := call(t, edit, map[string]any{"path": "new.txt", "old_string": "fresh", "new_string": "edited"}); res.IsError {
		t.Fatalf("edit after create: %q", res.Content)
	}

	call(t, read, map[string]any{"path": "a.txt", "limit": 1})
	if res := call(t, edit, map[string]any{"path": "a.txt", "old_string": "one", "new_string": "two"}); res.IsError {
		t.Fatalf("edit after read: %q", res.Content)
	}
	if res := call(t, edit, map[string]any{"path": filepath.Join(dir, "a.txt"), "old_string": "two", "new_string": "three"}); res.IsError {
		t.Fatalf("second edit via absolute path: %q", res.Content)
	}

	// An outside change invalidates the read.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed by the user\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(filepath.Join(dir, "a.txt"), later, later); err != nil {
		t.Fatal(err)
	}
	if res := call(t, edit, map[string]any{"path": "a.txt", "old_string": "user", "new_string": "model"}); !res.IsError || !strings.Contains(res.Content, "changed since") {
		t.Fatalf("edit after outside change: %q", res.Content)
	}

	// Tools shares one tracker between its tools by default.
	tools := Tools(Config{Dir: dir})
	if res := call(t, tools[2], map[string]any{"path": "b.txt", "old_string": "bee", "new_string": "b"}); !res.IsError {
		t.Fatalf("Tools did not install a tracker: %q", res.Content)
	}
	call(t, tools[0], map[string]any{"path": "b.txt"})
	if res := call(t, tools[2], map[string]any{"path": "b.txt", "old_string": "bee", "new_string": "b"}); res.IsError {
		t.Fatalf("Tools tracker not shared: %q", res.Content)
	}
}

func TestBashKillsProcessTreeOnTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell syntax")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	start := time.Now()
	res := call(t, Bash(Config{Dir: dir}), map[string]any{
		"command":    "sleep 30 & echo $! > pid; wait",
		"timeout_ms": 300,
	})
	if !res.IsError || !strings.Contains(res.Content, "timed out") || time.Since(start) > 10*time.Second {
		t.Fatalf("timeout: %q after %s", res.Content, time.Since(start))
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(b))
	// The child was killed with the shell; give the kernel a moment.
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(t, pid) {
		if time.Now().After(deadline) {
			t.Fatalf("child process %s survived the timeout", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func processAlive(t *testing.T, pid string) bool {
	t.Helper()
	// ps prints nothing (and fails) once the process is gone; a zombie
	// awaiting its reaper counts as gone.
	out, err := runCmd(t, "ps", "-o", "stat=", "-p", pid)
	if err != nil {
		if exit, ok := errors.AsType[*osexec.ExitError](err); ok && exit.ExitCode() == 1 && strings.TrimSpace(out) == "" {
			return false
		}
		t.Fatalf("ps %s: %v", pid, err)
	}
	return !strings.HasPrefix(strings.TrimSpace(out), "Z")
}

func TestBashKeepsHeadAndTail(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell syntax")
	}
	res := call(t, Bash(Config{Dir: t.TempDir(), MaxOutput: 90}), map[string]any{
		"command": "echo BEGIN; i=0; while [ $i -lt 200 ]; do echo filler; i=$((i+1)); done; echo FAILURE AT END; exit 1",
	})
	if !res.IsError || !strings.HasPrefix(res.Content, "BEGIN") || !strings.Contains(res.Content, "FAILURE AT END") || !strings.Contains(res.Content, "bytes truncated") {
		t.Fatalf("got %q", res.Content)
	}
}

func TestLimitWriter(t *testing.T) {
	for _, chunk := range []int{1, 3, 7, 50, 1000} {
		w := &limitWriter{max: 30}
		input := strings.Repeat("0123456789", 20)
		for s := input; s != ""; {
			n := min(chunk, len(s))
			_, _ = w.Write([]byte(s[:n]))
			s = s[n:]
		}
		want := input[:20] + "\n... [170 bytes truncated] ...\n" + input[190:]
		if got := w.String(); got != want {
			t.Errorf("chunk %d: got %q", chunk, got)
		}
	}
	w := &limitWriter{max: 30}
	_, _ = w.Write([]byte("short"))
	if w.String() != "short" {
		t.Errorf("short output: %q", w.String())
	}
}

func TestIsPublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8":         true,
		"2606:4700::1111": true,
		"198.18.0.1":      true, // fake-IP ranges of local proxies stay usable
		"127.0.0.1":       false,
		"10.1.2.3":        false,
		"172.16.0.1":      false,
		"192.168.1.1":     false,
		"169.254.169.254": false,
		"100.64.0.1":      false,
		"0.0.0.0":         false,
		"::1":             false,
		"fe80::1":         false,
		"fd00::1":         false,
		"::ffff:10.0.0.1": false,
		"64:ff9b::a00:1":  false,
		"224.0.0.1":       false,
	} {
		if got := isPublic(netip.MustParseAddr(addr)); got != want {
			t.Errorf("isPublic(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestWebFetchRefusesPrivateAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte("internal")); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("http_proxy", "")
	f := WebFetch(Config{})
	for _, u := range []string{srv.URL, "http://localhost:1/", "http://[::1]:1/", "http://169.254.169.254/"} {
		res := call(t, f, map[string]any{"url": u})
		if !res.IsError || !strings.Contains(res.Content, "refusing") {
			t.Errorf("%s: %q", u, res.Content)
		}
	}
	if res := call(t, WebFetch(Config{AllowPrivateNetwork: true}), map[string]any{"url": srv.URL}); res.IsError || !strings.HasSuffix(res.Content, "internal") {
		t.Errorf("AllowPrivateNetwork: %q", res.Content)
	}

	// The dialed address is checked even when the host check is skipped,
	// as happens when DNS changes between the check and the connection.
	client := publicClient()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
	}
	if !errors.Is(err, errPrivateAddress) {
		t.Errorf("direct dial to %s: %v", srv.URL, err)
	}
	// Redirects to a private address are refused.
	redirect, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://169.254.169.254/latest/meta-data", nil)
	if err := client.CheckRedirect(redirect, []*http.Request{req}); !errors.Is(err, errPrivateAddress) {
		t.Errorf("redirect: %v", err)
	}
}

func runCmd(t *testing.T, name string, args ...string) (string, error) {
	t.Helper()
	out, err := osexec.Command(name, args...).Output()
	return string(out), err
}

func TestWebFetchTimesOutSlowServers(t *testing.T) {
	defer func(total, header time.Duration) { fetchTimeout, fetchHeaderTimeout = total, header }(fetchTimeout, fetchHeaderTimeout)
	fetchTimeout, fetchHeaderTimeout = 500*time.Millisecond, 200*time.Millisecond
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/drip" {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			for {
				if _, err := w.Write([]byte("x")); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				select {
				case <-release:
					return
				case <-r.Context().Done():
					return
				case <-time.After(50 * time.Millisecond):
				}
			}
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("http_proxy", "")
	f := WebFetch(Config{AllowPrivateNetwork: true})
	for _, path := range []string{"/headers", "/drip"} {
		start := time.Now()
		res := call(t, f, map[string]any{"url": srv.URL + path})
		if !res.IsError || time.Since(start) > 5*time.Second {
			t.Errorf("%s: %q after %s", path, res.Content, time.Since(start))
		}
	}
	if c := publicClient(); c.Timeout == 0 || c.Transport.(*http.Transport).ResponseHeaderTimeout == 0 {
		t.Error("publicClient has no timeouts")
	}
}

func TestReadBeforeWriteNeedsShownContent(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a.txt": "one\ntwo\n", "empty.txt": ""})
	cfg := Config{Dir: dir, Files: &FileTracker{}}
	read, write, edit := ReadFile(cfg), WriteFile(cfg), EditFile(cfg)

	if res := call(t, read, map[string]any{"path": "a.txt", "offset": 10}); !res.IsError {
		t.Fatalf("offset past the end: %q", res.Content)
	}
	if res := call(t, edit, map[string]any{"path": "a.txt", "old_string": "one", "new_string": "1"}); !res.IsError || !strings.Contains(res.Content, "not been read") {
		t.Fatalf("edit after reading nothing: %q", res.Content)
	}
	call(t, read, map[string]any{"path": "empty.txt"})
	if res := call(t, write, map[string]any{"path": "empty.txt", "content": "now full\n"}); res.IsError {
		t.Fatalf("write after reading an empty file: %q", res.Content)
	}
}

func TestReadBeforeWriteComparesContent(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "a.txt")
	writeFiles(t, dir, map[string]string{"a.txt": "one\ntwo\nthree\n"})
	cfg := Config{Dir: dir, Files: &FileTracker{}}
	read, edit := ReadFile(cfg), EditFile(cfg)
	st, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	// Only the first line is shown, but the whole file is stamped.
	call(t, read, map[string]any{"path": "a.txt", "limit": 1})

	// Same size and modification time, different content.
	if err := os.WriteFile(name, []byte("one\nTWO\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(name, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	if res := call(t, edit, map[string]any{"path": "a.txt", "old_string": "one", "new_string": "1"}); !res.IsError || !strings.Contains(res.Content, "changed since") {
		t.Fatalf("edit after a same-size change: %q", res.Content)
	}

	// A file that was only touched is unchanged.
	call(t, read, map[string]any{"path": "a.txt"})
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(name, later, later); err != nil {
		t.Fatal(err)
	}
	if res := call(t, edit, map[string]any{"path": "a.txt", "old_string": "one", "new_string": "1"}); res.IsError {
		t.Fatalf("edit after touch: %q", res.Content)
	}
}
