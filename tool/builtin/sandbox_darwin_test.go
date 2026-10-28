//go:build darwin

package builtin

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seatbelt returns a sandbox for policy, skipping the test when Seatbelt
// cannot run here, for example inside another sandbox.
func seatbelt(t *testing.T, p SandboxPolicy) *SystemSandbox {
	t.Helper()
	s := NewSandbox(p)
	t.Cleanup(func() { s.Close() })
	if s.Backend() != BackendSeatbelt {
		t.Fatalf("backend = %q", s.Backend())
	}
	if err := s.Available(); err != nil {
		t.Skipf("Seatbelt is not available: %v", err)
	}
	return s
}

func TestSeatbeltConfinesWrites(t *testing.T) {
	t.Parallel()
	dir, outside := t.TempDir(), t.TempDir()
	home := t.TempDir()
	writeFiles(t, dir, map[string]string{".git/HEAD": "ref: refs/heads/main\n", "src/a.txt": "a\n"})
	sb := seatbelt(t, SandboxPolicy{NoTempDirs: true})
	bash := Bash(Config{Dir: dir, Sandbox: sb, Env: []string{"HOME=" + home}})
	script := strings.Join([]string{
		"echo inside > src/b.txt && echo wrote-inside",
		"echo x > " + shellQuote(filepath.Join(outside, "x")) + " || echo denied-outside",
		"echo x > .git/HEAD || echo denied-git",
		"mkdir .claude || echo denied-claude",
		"mv .git moved || echo denied-rename",
		"cat src/a.txt",
		"touch ~/agentx-sandbox-probe 2>/dev/null || echo denied-home",
	}, "; ")
	res := call(t, bash, map[string]any{"command": script})
	for _, want := range []string{"wrote-inside", "denied-outside", "denied-git", "denied-claude", "denied-rename", "a\n", "denied-home"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("output lacks %q:\n%s", want, res.Content)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "x")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("outside write: Stat = %v, want os.ErrNotExist", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".git", "HEAD")); string(b) != "ref: refs/heads/main\n" {
		t.Errorf(".git/HEAD = %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("protected directory: Stat = %v, want os.ErrNotExist", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "src", "b.txt")); string(b) != "inside\n" {
		t.Errorf("src/b.txt = %q", b)
	}
	// The working directory carries over through the sandbox.
	call(t, bash, map[string]any{"command": "cd src"})
	if res := call(t, bash, map[string]any{"command": "pwd"}); strings.TrimSpace(res.Content) != filepath.Join(dir, "src") {
		t.Errorf("pwd = %q", res.Content)
	}
}

func TestSeatbeltTempDirsAndExtraRoots(t *testing.T) {
	t.Parallel()
	dir, extra := t.TempDir(), t.TempDir()
	sb := seatbelt(t, SandboxPolicy{WritableRoots: []string{extra}})
	res := call(t, Bash(Config{Dir: dir, Sandbox: sb}), map[string]any{
		"command": `f=$(mktemp) && echo tmp-ok > "$f" && cat "$f" && rm "$f"; echo x > ` + shellQuote(filepath.Join(extra, "y")) + ` && echo extra-ok; ` +
			`echo z > /tmp/agentx-sandbox-test-$$ && rm /tmp/agentx-sandbox-test-$$ && echo slash-tmp-ok`,
	})
	for _, want := range []string{"tmp-ok", "extra-ok", "slash-tmp-ok"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("output lacks %q:\n%s", want, res.Content)
		}
	}
}

func TestSeatbeltDenyAndRestrictReads(t *testing.T) {
	t.Parallel()
	dir, secret, other := t.TempDir(), t.TempDir(), t.TempDir()
	writeFiles(t, secret, map[string]string{"key": "secret\n"})
	writeFiles(t, other, map[string]string{"note": "other-content\n"})
	writeFiles(t, dir, map[string]string{"a.txt": "mine\n"})

	sb := seatbelt(t, SandboxPolicy{DenyRead: []string{secret}})
	res := call(t, Bash(Config{Dir: dir, Sandbox: sb}), map[string]any{
		"command": "cat " + shellQuote(filepath.Join(secret, "key")) + " || echo denied-read; cat " + shellQuote(filepath.Join(other, "note")),
	})
	if strings.Contains(res.Content, "secret") || !strings.Contains(res.Content, "denied-read") || !strings.Contains(res.Content, "other-content") {
		t.Errorf("deny read: %s", res.Content)
	}

	sb = seatbelt(t, SandboxPolicy{ReadableRoots: []string{secret}, NoTempDirs: true})
	res = call(t, Bash(Config{Dir: dir, Sandbox: sb}), map[string]any{
		"command": "cat a.txt; cat " + shellQuote(filepath.Join(secret, "key")) + "; cat " + shellQuote(filepath.Join(other, "note")) + " || echo denied-other; ls / >/dev/null && echo ls-ok",
	})
	for _, want := range []string{"mine", "secret", "denied-other", "ls-ok"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("restricted reads: output lacks %q:\n%s", want, res.Content)
		}
	}
	if strings.Contains(res.Content, "other-content") {
		t.Errorf("restricted reads let the command read another directory:\n%s", res.Content)
	}
}

func TestSeatbeltNetwork(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not on PATH")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "hello from server") }))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	curl := "curl -sS --max-time 10 " + srv.URL + " || echo curl-failed"

	// No network.
	res := call(t, Bash(Config{Dir: dir, Sandbox: seatbelt(t, SandboxPolicy{})}), map[string]any{"command": "curl -sS --noproxy '*' --max-time 10 " + srv.URL + " || echo curl-failed"})
	if strings.Contains(res.Content, "hello from server") || !strings.Contains(res.Content, "curl-failed") {
		t.Errorf("no network: %s", res.Content)
	}

	// Full network.
	res = call(t, Bash(Config{Dir: dir, Sandbox: seatbelt(t, SandboxPolicy{AllowNetwork: true})}), map[string]any{"command": "curl -sS --noproxy '*' --max-time 10 " + srv.URL})
	if !strings.Contains(res.Content, "hello from server") {
		t.Errorf("full network: %s", res.Content)
	}

	// The proxy forwards allowed hosts only, and direct connections fail.
	sb := seatbelt(t, SandboxPolicy{AllowedDomains: []string{"127.0.0.1"}})
	bash := Bash(Config{Dir: dir, Sandbox: sb})
	res = call(t, bash, map[string]any{"command": curl})
	if !strings.Contains(res.Content, "hello from server") {
		t.Errorf("allowed through the proxy: %s", res.Content)
	}
	res = call(t, bash, map[string]any{"command": "curl -sS --noproxy '*' --max-time 10 " + srv.URL + " || echo direct-failed"})
	if strings.Contains(res.Content, "hello from server") || !strings.Contains(res.Content, "direct-failed") {
		t.Errorf("direct connection: %s", res.Content)
	}
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	res = call(t, bash, map[string]any{"command": fmt.Sprintf("curl -sS --max-time 10 http://localhost:%s/ || echo refused", port)})
	if strings.Contains(res.Content, "hello from server") || !strings.Contains(res.Content, "the sandbox refused connections to localhost:"+port) {
		t.Errorf("refused host: %s", res.Content)
	}
}

func TestSeatbeltLocalBinding(t *testing.T) {
	t.Parallel()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := "AGENTX_TEST_BIND_HELPER=1 " + shellQuote(binary) + " -test.run='^TestSeatbeltLocalBindingHelper$' || echo bind-failed"
	dir := t.TempDir()
	res := call(t, Bash(Config{Dir: dir, Sandbox: seatbelt(t, SandboxPolicy{})}), map[string]any{"command": script})
	if !strings.Contains(res.Content, "bind-failed") {
		t.Errorf("binding without AllowLocalBinding: %s", res.Content)
	}
	res = call(t, Bash(Config{Dir: dir, Sandbox: seatbelt(t, SandboxPolicy{AllowLocalBinding: true})}), map[string]any{"command": script})
	if !strings.Contains(res.Content, "bound") || strings.Contains(res.Content, "bind-failed") {
		t.Errorf("binding with AllowLocalBinding: %s", res.Content)
	}
}

func TestSeatbeltLocalBindingHelper(t *testing.T) {
	if os.Getenv("AGENTX_TEST_BIND_HELPER") != "1" {
		return
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	fmt.Println("bound")
}

func TestSeatbeltEscapeHatch(t *testing.T) {
	t.Parallel()
	dir, outside := t.TempDir(), t.TempDir()
	sb := seatbelt(t, SandboxPolicy{NoTempDirs: true})
	target := filepath.Join(outside, "escaped")
	cmd := "echo x > " + shellQuote(target) + " && echo wrote || echo denied"
	bash := Bash(Config{Dir: dir, Sandbox: sb})
	if res := call(t, bash, map[string]any{"command": cmd}); !strings.Contains(res.Content, "denied") {
		t.Fatalf("sandboxed: %s", res.Content)
	}
	if res := call(t, bash, map[string]any{"command": cmd, "dangerously_disable_sandbox": true}); !strings.Contains(res.Content, "wrote") {
		t.Fatalf("unsandboxed: %s", res.Content)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	strict := Bash(Config{Dir: dir, Sandbox: sb, NoSandboxEscape: true})
	res := call(t, strict, map[string]any{"command": cmd, "dangerously_disable_sandbox": true})
	if !strings.Contains(res.Content, "denied") || !strings.Contains(res.Content, "dangerously_disable_sandbox is disabled") {
		t.Fatalf("NoSandboxEscape: %s", res.Content)
	}
}

func TestSeatbeltBackgroundCommand(t *testing.T) {
	t.Parallel()
	dir, outside := t.TempDir(), t.TempDir()
	sb := seatbelt(t, SandboxPolicy{NoTempDirs: true})
	shells := &Shells{}
	t.Cleanup(func() { shells.Close() })
	cfg := Config{Dir: dir, Sandbox: sb, Shells: shells}
	res := call(t, Bash(cfg), map[string]any{
		"command":           "echo x > " + shellQuote(filepath.Join(outside, "bg")) + " || echo bg-denied",
		"run_in_background": true,
	})
	id := shellID(t, res.Content)
	out := waitShell(t, cfg, id, 10*time.Second)
	if !strings.Contains(out, "bg-denied") {
		t.Fatalf("background command was not sandboxed: %s", out)
	}
}
