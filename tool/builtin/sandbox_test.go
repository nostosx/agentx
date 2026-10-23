package builtin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nostosx/agentx/tool"
)

// fakeSandbox marks wrapped commands with an environment variable.
type fakeSandbox struct {
	unavailable error
	mu          sync.Mutex
	dirs        []string
}

func (f *fakeSandbox) Available() error { return f.unavailable }

func (f *fakeSandbox) Wrap(cmd *exec.Cmd, dir string) error {
	if f.unavailable != nil {
		return f.unavailable
	}
	f.mu.Lock()
	f.dirs = append(f.dirs, dir)
	f.mu.Unlock()
	cmd.Env = append(cmd.Env, "AGENTX_FAKE_SANDBOX=on")
	return nil
}

func needsApproval(t *testing.T, tl tool.Tool, args map[string]any) bool {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return tool.NeedsApproval(tl, b)
}

func TestBashApprovalWithSandbox(t *testing.T) {
	t.Parallel()
	unavailable := &fakeSandbox{unavailable: fmt.Errorf("%w: test", ErrSandboxUnavailable)}
	plain := map[string]any{"command": "ls"}
	escape := map[string]any{"command": "ls", "dangerously_disable_sandbox": true}
	cases := []struct {
		name        string
		cfg         Config
		plain, esc  bool
		infoApprove bool
	}{
		{"no sandbox", Config{}, true, true, true},
		{"no sandbox, NoApproval", Config{NoApproval: true}, false, false, false},
		{"sandbox", Config{Sandbox: &fakeSandbox{}}, false, true, true},
		{"sandbox, NoApproval", Config{Sandbox: &fakeSandbox{}, NoApproval: true}, false, true, true},
		{"sandbox, AskSandboxed", Config{Sandbox: &fakeSandbox{}, AskSandboxed: true}, true, true, true},
		{"sandbox, AskSandboxed, NoApproval", Config{Sandbox: &fakeSandbox{}, AskSandboxed: true, NoApproval: true}, false, true, true},
		{"sandbox, NoSandboxEscape", Config{Sandbox: &fakeSandbox{}, NoSandboxEscape: true}, false, false, true},
		{"unavailable, required", Config{Sandbox: unavailable}, false, true, true},
		{"unavailable, best effort", Config{Sandbox: unavailable, SandboxFallback: SandboxBestEffort, NoApproval: true}, true, true, true},
	}
	for _, c := range cases {
		c.cfg.Dir = t.TempDir()
		b := Bash(c.cfg)
		if got := needsApproval(t, b, plain); got != c.plain {
			t.Errorf("%s: plain command needs approval = %v", c.name, got)
		}
		if got := needsApproval(t, b, escape); got != c.esc {
			t.Errorf("%s: escape needs approval = %v", c.name, got)
		}
		if got := b.Info().RequiresApproval; got != c.infoApprove {
			t.Errorf("%s: Info().RequiresApproval = %v", c.name, got)
		}
		if got := tool.NeedsApproval(b, json.RawMessage(`{"command": 1}`)); c.cfg.Sandbox != nil && !got {
			t.Errorf("%s: invalid arguments do not need approval", c.name)
		}
		_, offered := b.Info().Parameters.Properties["dangerously_disable_sandbox"]
		if want := c.cfg.Sandbox != nil && !c.cfg.NoSandboxEscape; offered != want {
			t.Errorf("%s: dangerously_disable_sandbox offered = %v", c.name, offered)
		}
	}
}

func TestBashUsesSandbox(t *testing.T) {
	t.Parallel()
	skipWindows(t)
	dir := t.TempDir()
	sb := &fakeSandbox{}
	b := Bash(Config{Dir: dir, Sandbox: sb})
	res := call(t, b, map[string]any{"command": `echo "sandbox=$AGENTX_FAKE_SANDBOX"`})
	if !strings.Contains(res.Content, "sandbox=on") {
		t.Fatalf("not wrapped: %q", res.Content)
	}
	if real, _ := filepath.EvalSymlinks(dir); len(sb.dirs) != 1 || (sb.dirs[0] != dir && sb.dirs[0] != real) {
		t.Fatalf("Wrap dirs = %v", sb.dirs)
	}
	res = call(t, b, map[string]any{"command": `echo "sandbox=$AGENTX_FAKE_SANDBOX"`, "dangerously_disable_sandbox": true})
	if strings.Contains(res.Content, "sandbox=on") {
		t.Fatalf("escape was wrapped: %q", res.Content)
	}
	res = call(t, Bash(Config{Dir: dir, Sandbox: &fakeSandbox{unavailable: fmt.Errorf("%w: missing", ErrSandboxUnavailable)}}), map[string]any{"command": "echo ran"})
	if !res.IsError || strings.Contains(res.Content, "ran") || !strings.Contains(res.Content, "sandbox is not available") {
		t.Fatalf("required sandbox: %q", res.Content)
	}
	res = call(t, Bash(Config{Dir: dir, Sandbox: &fakeSandbox{unavailable: fmt.Errorf("%w: missing", ErrSandboxUnavailable)}, SandboxFallback: SandboxBestEffort}), map[string]any{"command": "echo ran"})
	if res.IsError || !strings.Contains(res.Content, "ran") || !strings.Contains(res.Content, "ran outside it") {
		t.Fatalf("best-effort sandbox: %q", res.Content)
	}
}

func TestUnsupportedSandbox(t *testing.T) {
	t.Parallel()
	s := NewSandbox(SandboxPolicy{})
	defer s.Close()
	if err := s.Available(); err != nil {
		if !errors.Is(err, ErrSandboxUnavailable) {
			t.Fatalf("Available = %v", err)
		}
		if err := s.Wrap(exec.Command("true"), t.TempDir()); !errors.Is(err, ErrSandboxUnavailable) {
			t.Fatalf("Wrap = %v", err)
		}
	}
	switch runtime.GOOS {
	case "darwin":
		if s.Backend() != BackendSeatbelt {
			t.Fatalf("backend = %q", s.Backend())
		}
	case "linux":
		if s.Backend() != BackendBubblewrap {
			t.Fatalf("backend = %q", s.Backend())
		}
	default:
		if s.Backend() != "" || s.Available() == nil {
			t.Fatalf("backend %q, available %v", s.Backend(), s.Available())
		}
	}
	s.Close()
	if err := s.Available(); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("Available after Close = %v", err)
	}
}

func TestSandboxSpec(t *testing.T) {
	t.Parallel()
	skipWindows(t)
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{".git": "gitdir: ../repo/.git/worktrees/wt\n", "sub/x": ""})
	gitdir := filepath.Join(filepath.Dir(dir), "repo", ".git", "worktrees", "wt")
	s := NewSandbox(SandboxPolicy{WritableRoots: []string{"sub"}, DenyRead: []string{"~/.ssh"}, AllowedDomains: []string{"example.com"}})
	sp, err := s.spec(dir)
	if err != nil {
		t.Fatal(err)
	}
	real := canonicalPath(dir)
	if sp.writable[0].path != real || sp.writable[1].path != filepath.Join(real, "sub") {
		t.Fatalf("writable = %+v", sp.writable)
	}
	if !slices.Contains(sp.writable[0].protected, filepath.Join(real, ".git")) ||
		!slices.Contains(sp.writable[0].protected, filepath.Join(real, ".claude")) ||
		!slices.Contains(sp.writable[0].protected, canonicalPath(gitdir)) {
		t.Fatalf("protected = %v", sp.writable[0].protected)
	}
	tmp := canonicalPath(os.TempDir())
	if !slices.ContainsFunc(sp.writable, func(w writableRoot) bool { return w.path == tmp }) {
		t.Fatalf("temp dir %s not writable: %+v", tmp, sp.writable)
	}
	home, _ := os.UserHomeDir()
	if !slices.Contains(sp.denyRead, filepath.Join(home, ".ssh")) {
		t.Fatalf("denyRead = %v", sp.denyRead)
	}
	if sp.network != networkProxy || sp.readable != nil {
		t.Fatalf("network %v, readable %v", sp.network, sp.readable)
	}

	// No temp dirs, no protection.
	sp, err = NewSandbox(SandboxPolicy{NoTempDirs: true, ProtectedNames: []string{}}).spec(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sp.writable) != 1 || len(sp.writable[0].protected) != 0 {
		t.Fatalf("writable = %+v", sp.writable)
	}

	// A writable root through a link a command could retarget is refused.
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSandbox(SandboxPolicy{WritableRoots: []string{"link"}}).spec(dir); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("root through a writable link: %v", err)
	}
}

func TestSeatbeltProfile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Seatbelt profiles use macOS paths, including literal backslashes")
	}
	t.Parallel()
	sp := &sandboxSpec{
		writable: []writableRoot{
			{path: `/w/a "quoted" \dir`, protected: []string{`/w/a "quoted" \dir/.git`, `/w/a "quoted" \dir/x/y/gitdir`}},
			{path: "/tmp"},
		},
		denyRead:  []string{"/home/u/.ssh"},
		network:   networkProxy,
		proxyPort: 4242,
	}
	profile, params := seatbeltProfile(sp)
	values := map[string]string{}
	for _, p := range params {
		values[p.key] = p.value
	}
	// Paths never appear in the profile itself.
	if strings.Contains(profile, "quoted") || strings.Contains(profile, ".ssh") {
		t.Fatalf("a path leaked into the profile:\n%s", profile)
	}
	for _, want := range []string{
		"(deny default)",
		"(allow file-read*)",
		`(require-not (literal (param "PROTECTED_1"))) (require-not (subpath (param "PROTECTED_1")))`,
		`(subpath (param "WRITABLE_ROOT_3"))`,
		`(allow network-outbound (remote ip "localhost:4242"))`,
		`(deny file-write-unlink (require-all (literal (param "WRITABLE_ROOT_0")) (vnode-type DIRECTORY)))`,
		`(deny file-write-unlink (require-all (literal (param "PROTECTED_ANCESTOR_`,
		`(deny file-read* file-write* (literal (param "DENY_`,
	} {
		if !strings.Contains(profile, want) {
			t.Errorf("profile lacks %s:\n%s", want, profile)
		}
	}
	if values["WRITABLE_ROOT_0"] != `/w/a "quoted" \dir` || values["PROTECTED_1"] != `/w/a "quoted" \dir/.git` {
		t.Fatalf("params = %v", values)
	}
	if !slices.Contains(slices.Collect(mapsValues(values)), `/w/a "quoted" \dir/x/y`) ||
		!slices.Contains(slices.Collect(mapsValues(values)), `/w/a "quoted" \dir/x`) {
		t.Fatalf("protected ancestors missing: %v", values)
	}
	// Denials come after the allowances they restrict.
	if strings.Index(profile, "(deny file-read*") < strings.Index(profile, "(allow file-read*)") {
		t.Fatal("deny-read rule precedes the read allowance")
	}

	full, _ := seatbeltProfile(&sandboxSpec{network: networkFull})
	if !strings.Contains(full, "(allow network-outbound)\n") || strings.Contains(full, "localhost:") {
		t.Fatalf("full network:\n%s", full)
	}
	none, _ := seatbeltProfile(&sandboxSpec{readable: []string{"/r"}})
	if strings.Contains(none, "network-outbound") || strings.Contains(none, "(allow file-read*)\n") || !strings.Contains(none, "(allow file-read-metadata)") {
		t.Fatalf("no network, restricted reads:\n%s", none)
	}
	bind, _ := seatbeltProfile(&sandboxSpec{localBinding: true})
	if !strings.Contains(bind, `(allow network-bind (local ip "*:*"))`) {
		t.Fatalf("local binding:\n%s", bind)
	}
}

func mapsValues(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for _, v := range m {
			if !yield(v) {
				return
			}
		}
	}
}

func TestBwrapArgs(t *testing.T) {
	t.Parallel()
	skipWindows(t)
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{".git/HEAD": "", "secret/k": "", "f": ""})
	sp := &sandboxSpec{
		writable: []writableRoot{
			{path: dir, protected: []string{filepath.Join(dir, ".git"), filepath.Join(dir, ".claude")}},
			{path: filepath.Join(dir, "missing")},
		},
		denyRead:    []string{filepath.Join(dir, "secret"), filepath.Join(dir, "f"), filepath.Join(dir, "nope")},
		network:     networkProxy,
		proxySocket: "/run/p/proxy.sock",
		sockets:     []string{"/var/run/docker.sock"},
	}
	args := strings.Join(bwrapArgs(sp, dir, []string{"/bin/bash", "-c", "echo hi"}), " ")
	for _, want := range []string{
		"--new-session --die-with-parent --ro-bind / / --dev /dev",
		"--bind " + dir + " " + dir,
		"--ro-bind " + filepath.Join(dir, ".git") + " " + filepath.Join(dir, ".git"),
		"--tmpfs " + filepath.Join(dir, "secret") + " --remount-ro " + filepath.Join(dir, "secret"),
		"--ro-bind /dev/null " + filepath.Join(dir, "f"),
		"--ro-bind /dev/null /var/run/docker.sock",
		"--bind /run/p /run/p",
		"--unshare-user --unshare-pid --unshare-ipc --unshare-net --proc /proc --chdir " + dir + " --cap-drop ALL -- /bin/bash -c echo hi",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args lack %q:\n%s", want, args)
		}
	}
	for _, unwanted := range []string{".claude", "missing", "nope"} {
		if strings.Contains(args, unwanted) {
			t.Errorf("args mount the missing path %q:\n%s", unwanted, args)
		}
	}
	full := strings.Join(bwrapArgs(&sandboxSpec{network: networkFull, readable: []string{"/data"}}, "/", []string{"true"}), " ")
	if strings.Contains(full, "--unshare-net") || !strings.Contains(full, "--tmpfs / ") || !strings.Contains(full, "--ro-bind-try /data /data") {
		t.Errorf("full network, restricted reads:\n%s", full)
	}
	script := bwrapBridgeScript("/usr/bin/socat", "/run/p/proxy.sock")
	if !strings.Contains(script, `'/usr/bin/socat' 'TCP-LISTEN:3128,bind=127.0.0.1,fork,reuseaddr' 'UNIX-CONNECT:/run/p/proxy.sock'`) ||
		!strings.HasSuffix(script, `exec "$@"`) {
		t.Errorf("bridge script:\n%s", script)
	}
	if got := shellQuote("it's"); got != `'it'\''s'` {
		t.Errorf("shellQuote = %s", got)
	}
}

func TestBwrapProtectsOverlappingRoots(t *testing.T) {
	t.Parallel()
	skipWindows(t)
	parent := t.TempDir()
	dir := filepath.Join(parent, "work")
	git := filepath.Join(dir, ".git")
	writeFiles(t, dir, map[string]string{".git/HEAD": "ref: refs/heads/main\n"})
	cases := []struct {
		name  string
		roots []writableRoot
	}{
		{"parent last", []writableRoot{{path: dir, protected: []string{git}}, {path: parent}}},
		{"child last", []writableRoot{{path: dir, protected: []string{git}}, {path: git}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := bwrapArgs(&sandboxSpec{writable: c.roots}, dir, []string{"true"})
			lastBind, protection := -1, -1
			for i, arg := range args {
				if arg == "--bind" {
					lastBind = i
				}
				if arg == "--ro-bind" && i+2 < len(args) && args[i+1] == git && args[i+2] == git {
					protection = i
				}
			}
			if lastBind < 0 || protection <= lastBind {
				t.Fatalf("protected mount can be covered by a writable mount: %v", args)
			}
		})
	}
}

func TestDomainRules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		rule  string
		host  string
		port  int
		match bool
	}{
		{"example.com", "example.com", 443, true},
		{"Example.COM.", "example.com", 80, true},
		{"example.com", "api.example.com", 443, false},
		{"*.example.com", "api.example.com", 443, true},
		{"*.example.com", "a.b.example.com", 443, true},
		{"*.example.com", "example.com", 443, false},
		{"*.example.com", "badexample.com", 443, false},
		{"example.com:443", "example.com", 443, true},
		{"example.com:443", "example.com", 80, false},
		{"*", "anything.org", 22, true},
		// Wildcards do not reach local addresses written as IP literals.
		{"*", "127.0.0.1", 2375, false},
		{"*", "169.254.169.254", 80, false},
		{"*", "::1", 80, false},
		{"*", "93.184.216.34", 443, true},
		{"*.example.com", "93.184.216.34", 443, false},
		{"127.0.0.1", "127.0.0.1", 8080, true},
		{"[::1]", "::1", 8080, true},
		{"[::1]:443", "::1", 80, false},
	}
	for _, c := range cases {
		r, err := parseDomainRule(c.rule)
		if err != nil {
			t.Fatalf("%s: %v", c.rule, err)
		}
		if got := r.matches(c.host, c.port); got != c.match {
			t.Errorf("%s matches %s:%d = %v", c.rule, c.host, c.port, got)
		}
	}
	for _, bad := range []string{"", "example.*", "a*.com", "::1", "[::1", "[nope]", "example.com:0", "example.com:x", "[::1]x"} {
		if _, err := parseDomainRule(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if !isLocalAddr(netip.MustParseAddr("127.0.0.1")) || !isLocalAddr(netip.MustParseAddr("169.254.169.254")) ||
		!isLocalAddr(netip.MustParseAddr("::1")) || isLocalAddr(netip.MustParseAddr("93.184.216.34")) {
		t.Error("isLocalAddr misclassifies")
	}
}

// readProxyResponse checks the complete response and releases its body even
// when a read fails, so partial output cannot satisfy a content assertion.
func readProxyResponse(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestDomainProxy(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "host=%s path=%s", r.Host, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "tls ok") }))
	t.Cleanup(tlsSrv.Close)
	p, err := newDomainProxy([]string{"127.0.0.1", "localhost"}, runtime.GOOS != "windows")
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	proxyURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", p.port()))
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: 10 * time.Second}
	defer client.CloseIdleConnections()

	// Plain HTTP is forwarded.
	resp, err := client.Get(srv.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	body := readProxyResponse(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "path=/x") {
		t.Fatalf("plain: %d %s", resp.StatusCode, body)
	}

	// The Host header cannot name another host than the checked URL.
	raw, err := net.Dial("tcp", proxyURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := raw.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(raw, "GET %s/spoof HTTP/1.1\r\nHost: evil.example\r\nConnection: close\r\n\r\n", srv.URL); err != nil {
		t.Fatal(err)
	}
	reply, err := io.ReadAll(raw)
	if err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if !strings.Contains(string(reply), "host="+strings.TrimPrefix(srv.URL, "http://")+" path=/spoof") {
		t.Fatalf("spoofed Host: %s", reply)
	}

	// HTTPS goes through a CONNECT tunnel.
	tr := tlsSrv.Client().Transport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyURL(proxyURL)
	defer tr.CloseIdleConnections()
	resp, err = (&http.Client{Transport: tr, Timeout: 10 * time.Second}).Get(tlsSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	body = readProxyResponse(t, resp)
	if string(body) != "tls ok" {
		t.Fatalf("tunnel: %s", body)
	}

	// Other hosts are refused and recorded.
	since := time.Now()
	resp, err = client.Get("http://not-allowed.invalid/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("refused host: %d", resp.StatusCode)
	}
	conn, err := net.Dial("tcp", proxyURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, "CONNECT blocked.invalid:443 HTTP/1.1\r\nHost: blocked.invalid:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if !strings.Contains(line, "403") {
		t.Fatalf("refused CONNECT: %q", line)
	}
	if got := p.deniedSince(since); !slices.Equal(got, []string{"not-allowed.invalid:80", "blocked.invalid:443"}) {
		t.Fatalf("denied = %v", got)
	}
	if got := p.deniedSince(time.Now().Add(time.Hour)); len(got) != 0 {
		t.Fatalf("denied later = %v", got)
	}

	// Requests that are not proxy requests are rejected.
	resp, err = http.Get(proxyURL.String() + "/direct")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("origin-form request: %d", resp.StatusCode)
	}

	// The Unix socket serves the same proxy.
	if p.socket != "" {
		uc := &http.Client{Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", p.socket)
			},
		}, Timeout: 10 * time.Second}
		defer uc.CloseIdleConnections()
		resp, err := uc.Get(srv.URL + "/unix")
		if err != nil {
			t.Fatal(err)
		}
		body := readProxyResponse(t, resp)
		if !strings.Contains(string(body), "path=/unix") {
			t.Fatalf("unix socket: %s", body)
		}
	}
}

func TestDomainProxyRefusesLocalNames(t *testing.T) {
	t.Parallel()
	p, err := newDomainProxy([]string{"*"}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	// localhost may resolve to loopback; an IP address was allowed as is.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	for _, host := range []string{"localhost", "127.0.0.1"} {
		c, err := p.dial(t.Context(), "tcp", net.JoinHostPort(host, port))
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		c.Close()
	}
	// Other names must not resolve to local addresses only.
	p.lookup = func(_ context.Context, _, host string) ([]netip.Addr, error) {
		switch host {
		case "metadata.test":
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
		case "loopback.test":
			return []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1")}, nil
		}
		return nil, errors.New("no such host")
	}
	for _, host := range []string{"metadata.test", "loopback.test"} {
		if _, err := p.dial(t.Context(), "tcp", net.JoinHostPort(host, port)); err == nil || !strings.Contains(err.Error(), "local address") {
			t.Errorf("%s: %v", host, err)
		}
	}
}

func TestClose(t *testing.T) {
	t.Parallel()
	shells := &Shells{}
	tools := Tools(Config{Dir: t.TempDir(), Shells: shells})
	if err := Close(tools); err != nil {
		t.Fatal(err)
	}
	shells.mu.Lock()
	closed := shells.closed
	shells.mu.Unlock()
	if !closed {
		t.Fatal("Close did not close the shells")
	}
}
