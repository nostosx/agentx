package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// helperEnv selects the behavior of TestHelperServer.
const helperEnv = "AGENTX_MCP_HELPER"

// TestHelperServer is the MCP server run by the stdio tests. It does
// nothing unless started by one of them.
func TestHelperServer(t *testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		t.Skip("helper process")
	}
	if mode == "grandchild" {
		// A descendant that holds the output, as node does under npx.
		child := exec.Command("sleep", "30")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		_ = os.WriteFile(os.Getenv("AGENTX_MCP_PIDFILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600)
		fmt.Fprintln(os.Stderr, "helper started")
	}
	var delayedProbe json.RawMessage
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var req rpcRequest
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			continue
		}
		switch req.Method {
		case "server/discover":
			if mode == "slow" {
				// Wait until initialize proves that the probe timed out.
				delayedProbe = req.ID
				continue
			}
			if mode == "legacy" {
				// A server implementing 2025-06-18 only.
				fmt.Println(rpcError(req.ID, CodeMethodNotFound, "Method not found"))
				continue
			}
			fmt.Println(result(req.ID, discoverResult))
		case "initialize":
			if len(delayedProbe) > 0 {
				fmt.Println(result(delayedProbe, discoverResult))
			}
			fmt.Println(initResult(req.ID, "2025-06-18", `{"tools":{}}`))
		case "tools/list":
			fmt.Println(result(req.ID, `{"tools":[{"name":"t","inputSchema":{"type":"object"}}]}`))
		case "burst":
			// Write many messages and exit at once; the client must still
			// receive all of them.
			for i := range 500 {
				fmt.Printf(`{"jsonrpc":"2.0","method":"notifications/message","params":{"n":%d}}`+"\n", i)
			}
			os.Exit(0)
		case "exit":
			os.Exit(0)
		}
	}
	os.Exit(0)
}

func helperTransport(t *testing.T, mode string, env ...string) *StdioTransport {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("helper uses POSIX commands")
	}
	tr := Stdio(os.Args[0], "-test.run=^TestHelperServer$")
	tr.Env = append([]string{helperEnv + "=" + mode}, env...)
	return tr
}

func TestStdioReceivesMessagesBeforeExit(t *testing.T) {
	var got atomic.Int64
	tr := helperTransport(t, "burst")
	// A slow handler leaves messages in the pipe when the server exits.
	c, err := Connect(t.Context(), tr, OnNotification(func(string, json.RawMessage) {
		time.Sleep(time.Millisecond)
		got.Add(1)
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Notify(t.Context(), "burst", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("connection did not end after the server exited")
	}
	if n := got.Load(); n != 500 {
		t.Fatalf("received %d of 500 messages; err = %v", n, c.Err())
	}
	if err := c.Err(); !errors.Is(err, io.EOF) {
		t.Errorf("Err = %v, want EOF", err)
	}
}

func TestStdioLegacyServer(t *testing.T) {
	c, err := Connect(t.Context(), helperTransport(t, "legacy"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.ProtocolVersion() != "2025-06-18" {
		t.Errorf("ProtocolVersion = %q", c.ProtocolVersion())
	}
	tools, err := c.ListTools(t.Context())
	if err != nil || len(tools) != 1 || tools[0].Name != "t" {
		t.Fatalf("tools = %+v, err = %v", tools, err)
	}
}

func TestStdioSlowModernServer(t *testing.T) {
	// The server holds its probe answer until initialize arrives, then
	// answers server/discover before initialize without a startup sleep.
	c, err := Connect(t.Context(), helperTransport(t, "slow"), WithProbeTimeout(20*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.ProtocolVersion() != ProtocolVersion {
		t.Errorf("ProtocolVersion = %q", c.ProtocolVersion())
	}
	if tools, err := c.ListTools(t.Context()); err != nil || len(tools) != 1 {
		t.Fatalf("tools = %+v, err = %v", tools, err)
	}
}

func TestStdioCloseKillsDescendants(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	tr := helperTransport(t, "grandchild", "AGENTX_MCP_PIDFILE="+pidFile)
	var stderr syncBuffer
	tr.Stderr = &stderr // a pipe that the grandchild holds open
	c, err := Connect(t.Context(), tr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(b))
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close hung on a descendant holding standard error")
	}
	if !strings.Contains(stderr.String(), "helper started") {
		t.Errorf("stderr = %q", stderr.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, err := exec.Command("ps", "-o", "stat=", "-p", pid).Output()
		if err != nil || strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("descendant %s survived Close", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestStdioServerExitEndsConnection(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	tr := helperTransport(t, "grandchild", "AGENTX_MCP_PIDFILE="+pidFile)
	c, err := Connect(t.Context(), tr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Notify(t.Context(), "exit", nil); err != nil {
		t.Fatal(err)
	}
	// The descendant holding the server's output must not hide the exit.
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("connection did not end after the server exited")
	}
}

// syncBuffer is a strings.Builder safe for concurrent use.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// blockingWriter blocks every Write until unblock is closed.
type blockingWriter struct{ unblock chan struct{} }

func (w *blockingWriter) Write(p []byte) (int, error) {
	<-w.unblock
	return 0, io.ErrClosedPipe
}

func TestIOTransportSendHonorsContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := &blockingWriter{unblock: make(chan struct{})}
		r, pw := io.Pipe()
		tr := NewIOTransport(r, w, func() error {
			close(w.unblock)
			return pw.Close()
		})
		if err := tr.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		defer tr.Close()
		for i := range 2 {
			// The first Send blocks in Write, the second waits behind it.
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			err := tr.Send(ctx, []byte(`{}`))
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Send %d = %v, want DeadlineExceeded", i, err)
			}
		}
		if err := tr.Close(); err != nil {
			t.Fatal(err)
		}
		if err := tr.Send(t.Context(), []byte(`{}`)); !errors.Is(err, ErrClosed) {
			t.Errorf("Send after Close = %v, want ErrClosed", err)
		}
	})
}

// gateWriter forwards writes to w, holding those that contain hold until
// gate is closed.
type gateWriter struct {
	w       io.Writer
	hold    string
	gate    chan struct{}
	blocked chan struct{}
	once    sync.Once
}

func (g *gateWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), g.hold) {
		g.once.Do(func() { close(g.blocked) })
		<-g.gate
	}
	return g.w.Write(p)
}

// TestCancelDuringWriteSendsNotification checks that a request whose
// context ends while it is being written is still cancelled: the write
// completes in the background, so the server receives the request.
func TestCancelDuringWriteSendsNotification(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cr, sw := io.Pipe()
		sr, cw := io.Pipe()
		gw := &gateWriter{w: cw, hold: `"tools/call"`, gate: make(chan struct{}), blocked: make(chan struct{})}
		tr := NewIOTransport(cr, gw, func() error {
			_ = cw.Close()
			return cr.Close()
		})
		release := sync.OnceFunc(func() { close(gw.gate) })
		t.Cleanup(func() {
			release()
			_ = tr.Close()
		})
		methods := make(chan string, 8)
		go func() {
			sc := bufio.NewScanner(sr)
			for sc.Scan() {
				var req rpcRequest
				if json.Unmarshal(sc.Bytes(), &req) != nil {
					continue
				}
				if req.Method == "server/discover" {
					fmt.Fprintln(sw, result(req.ID, discoverResult))
					continue
				}
				methods <- req.Method
			}
			_ = sw.Close()
		}()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		c, err := Connect(ctx, tr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()

		callCtx, cancelCall := context.WithCancel(t.Context())
		defer cancelCall()
		go func() {
			<-gw.blocked
			cancelCall()
		}()
		if _, err := c.CallTool(callCtx, "destroy", nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("CallTool = %v, want context.Canceled", err)
		}
		release() // the write completes
		for _, want := range []string{"tools/call", "notifications/cancelled"} {
			select {
			case got := <-methods:
				if got != want {
					t.Fatalf("server received %s, want %s", got, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("server did not receive %s", want)
			}
		}
	})
}

func TestIOTransportSendReportsMaybeSent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := &blockingWriter{unblock: make(chan struct{})}
		r, pw := io.Pipe()
		tr := NewIOTransport(r, w, func() error {
			close(w.unblock)
			return pw.Close()
		})
		if err := tr.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		defer tr.Close()
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		// The first Send was being written, the second never started.
		if err := tr.Send(ctx, []byte(`{}`)); !errors.Is(err, ErrMaybeSent) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Send during the write = %v, want ErrMaybeSent and DeadlineExceeded", err)
		}
		if err := tr.Send(ctx, []byte(`{}`)); errors.Is(err, ErrMaybeSent) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Send behind the write = %v, want only DeadlineExceeded", err)
		}
	})
}

func TestStdioCloseWithBlockedStderrWriter(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	tr := helperTransport(t, "grandchild", "AGENTX_MCP_PIDFILE="+pidFile)
	w := &blockingWriter{unblock: make(chan struct{})}
	defer close(w.unblock)
	tr.Stderr = w
	c, err := Connect(t.Context(), tr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Close hung on a blocked Stderr writer")
	}
}

// A stdio server inherits only the variables it needs to start, so a
// third-party server cannot read the client's API keys.
func TestStdioEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	t.Setenv("AGENTX_TEST_API_KEY", "secret")
	t.Setenv("AGENTX_TEST_FUNC", "() { :; }")
	start := func(inherit bool) []string {
		tr := Stdio("sh", "-c", "cat >/dev/null")
		tr.Env = []string{"EXTRA=1"}
		tr.InheritEnv = inherit
		if err := tr.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		env := slices.Clone(tr.cmd.Env)
		_ = tr.Close()
		return env
	}
	has := func(env []string, prefix string) bool {
		return slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, prefix) })
	}
	env := start(false)
	if !has(env, "PATH=") || !has(env, "EXTRA=1") || has(env, "AGENTX_TEST_API_KEY=") {
		t.Fatalf("default environment = %q", env)
	}
	if !has(start(true), "AGENTX_TEST_API_KEY=secret") {
		t.Fatal("InheritEnv did not pass the environment")
	}
	t.Setenv("PATH", "() { evil; }")
	if has(DefaultEnvironment(), "PATH=") {
		t.Fatal("a shell function was inherited")
	}
}
