//go:build linux

package builtin

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// bubblewrap returns a sandbox for policy, skipping the test when
// bubblewrap is missing or cannot create namespaces here.
func bubblewrap(t *testing.T, p SandboxPolicy) *SystemSandbox {
	t.Helper()
	s := NewSandbox(p)
	t.Cleanup(func() { s.Close() })
	if s.Backend() != BackendBubblewrap {
		t.Fatalf("backend = %q", s.Backend())
	}
	if err := s.Available(); err != nil {
		t.Skipf("bubblewrap is not available: %v", err)
	}
	return s
}

func TestBubblewrapConfinesWrites(t *testing.T) {
	t.Parallel()
	dir, outside := t.TempDir(), t.TempDir()
	writeFiles(t, dir, map[string]string{".git/HEAD": "ref: refs/heads/main\n", "src/a.txt": "a\n"})
	sb := bubblewrap(t, SandboxPolicy{NoTempDirs: true})
	bash := Bash(Config{Dir: dir, Sandbox: sb})
	script := strings.Join([]string{
		"echo inside > src/b.txt && echo wrote-inside",
		"echo x > " + shellQuote(filepath.Join(outside, "x")) + " || echo denied-outside",
		"echo x > .git/HEAD || echo denied-git",
		"cat src/a.txt",
		"cd src",
	}, "; ")
	res := call(t, bash, map[string]any{"command": script})
	for _, want := range []string{"wrote-inside", "denied-outside", "denied-git", "a\n"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("output lacks %q:\n%s", want, res.Content)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "x")); err == nil {
		t.Error("the sandbox let the command write outside")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".git", "HEAD")); string(b) != "ref: refs/heads/main\n" {
		t.Errorf(".git/HEAD = %q", b)
	}
	// The working directory carries over through the sandbox.
	if res := call(t, bash, map[string]any{"command": "pwd"}); strings.TrimSpace(res.Content) != filepath.Join(dir, "src") {
		t.Errorf("pwd = %q", res.Content)
	}
}

func TestBubblewrapProtectsOverlappingRoots(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		policy func(parent, dir string) SandboxPolicy
	}{
		{"temporary roots", func(_, _ string) SandboxPolicy { return SandboxPolicy{} }},
		{"parent root", func(parent, _ string) SandboxPolicy {
			return SandboxPolicy{NoTempDirs: true, WritableRoots: []string{parent}}
		}},
		{"protected root", func(_, dir string) SandboxPolicy {
			return SandboxPolicy{NoTempDirs: true, WritableRoots: []string{filepath.Join(dir, ".git")}}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			parent := t.TempDir()
			dir := filepath.Join(parent, "work")
			const head = "ref: refs/heads/main\n"
			writeFiles(t, dir, map[string]string{".git/HEAD": head})
			sb := bubblewrap(t, c.policy(parent, dir))
			res := call(t, Bash(Config{Dir: dir, Sandbox: sb}), map[string]any{
				"command": "echo changed > .git/HEAD && echo changed-git; echo ordinary > ordinary && echo wrote-ordinary",
			})
			if strings.Contains(res.Content, "changed-git") || !strings.Contains(res.Content, "wrote-ordinary") {
				t.Errorf("overlapping roots: %s", res.Content)
			}
			if got, err := os.ReadFile(filepath.Join(dir, ".git", "HEAD")); err != nil || string(got) != head {
				t.Errorf("protected HEAD = %q, %v", got, err)
			}
			if got, err := os.ReadFile(filepath.Join(dir, "ordinary")); err != nil || string(got) != "ordinary\n" {
				t.Errorf("ordinary file = %q, %v", got, err)
			}
		})
	}
}

func TestBubblewrapDenyRead(t *testing.T) {
	t.Parallel()
	dir, secret := t.TempDir(), t.TempDir()
	writeFiles(t, secret, map[string]string{"key": "secret-content\n"})
	sb := bubblewrap(t, SandboxPolicy{DenyRead: []string{secret}})
	res := call(t, Bash(Config{Dir: dir, Sandbox: sb}), map[string]any{
		"command": "cat " + shellQuote(filepath.Join(secret, "key")) + " || echo denied-read",
	})
	if strings.Contains(res.Content, "secret-content") || !strings.Contains(res.Content, "denied-read") {
		t.Errorf("deny read: %s", res.Content)
	}
}

func TestBubblewrapNetwork(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not on PATH")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "hello from server") }))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	res := call(t, Bash(Config{Dir: dir, Sandbox: bubblewrap(t, SandboxPolicy{})}), map[string]any{
		"command": "curl -sS --noproxy '*' --max-time 10 " + srv.URL + " || echo curl-failed",
	})
	if strings.Contains(res.Content, "hello from server") || !strings.Contains(res.Content, "curl-failed") {
		t.Errorf("no network: %s", res.Content)
	}
	if _, err := exec.LookPath("socat"); err != nil {
		t.Skip("socat is not on PATH")
	}
	sb := bubblewrap(t, SandboxPolicy{AllowedDomains: []string{"127.0.0.1"}})
	bash := Bash(Config{Dir: dir, Sandbox: sb})
	res = call(t, bash, map[string]any{"command": "curl -sS --max-time 10 " + srv.URL})
	if !strings.Contains(res.Content, "hello from server") {
		t.Errorf("allowed through the proxy: %s", res.Content)
	}
	res = call(t, bash, map[string]any{"command": "curl -sS --max-time 10 http://blocked.invalid/ || echo refused"})
	if !strings.Contains(res.Content, "refused") || !strings.Contains(res.Content, "the sandbox refused connections to blocked.invalid:80") {
		t.Errorf("refused host: %s", res.Content)
	}
}

func TestBubblewrapBackgroundCommand(t *testing.T) {
	t.Parallel()
	dir, outside := t.TempDir(), t.TempDir()
	sb := bubblewrap(t, SandboxPolicy{NoTempDirs: true})
	shells := &Shells{}
	t.Cleanup(func() { shells.Close() })
	cfg := Config{Dir: dir, Sandbox: sb, Shells: shells}
	res := call(t, Bash(cfg), map[string]any{
		"command":           "echo x > " + shellQuote(filepath.Join(outside, "bg")) + " || echo bg-denied",
		"run_in_background": true,
	})
	if out := waitShell(t, cfg, shellID(t, res.Content), 10*time.Second); !strings.Contains(out, "bg-denied") {
		t.Fatalf("background command was not sandboxed: %s", out)
	}
}
