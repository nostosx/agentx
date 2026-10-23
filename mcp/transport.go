// Package mcp implements a Model Context Protocol client and server over
// stdio and Streamable HTTP, with OAuth authorization, and adapts MCP tools
// to agentx tools and agentx tools to MCP.
package mcp

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nostosx/agentx/internal/httpclient"
	"github.com/nostosx/agentx/internal/process"
)

// ErrClosed is returned when using a closed transport.
var ErrClosed = errors.New("mcp: transport closed")

// ErrMaybeSent is wrapped by the error of a Send that failed after the
// message may already have reached the peer, such as a Send whose context
// was done while the message was being written. The client then treats a
// request as issued and asks the server to cancel it.
var ErrMaybeSent = errors.New("mcp: message may have been sent")

// ErrStreamLost is returned when a response stream ends before the response
// arrives and cannot be resumed; the request may be issued again. Only the
// streams of the initialization-based revisions can be resumed.
var ErrStreamLost = errors.New("mcp: response stream ended without a response")

// Transport carries JSON-RPC messages between client and server.
type Transport interface {
	// Start connects the transport. It is called once before Send.
	Start(ctx context.Context) error
	// Send writes one JSON-RPC message. If it fails after the message may
	// have reached the peer, the error wraps ErrMaybeSent.
	Send(ctx context.Context, msg []byte) error
	// Receive returns incoming messages. The channel is closed when the
	// connection ends; Err then reports why.
	Receive() <-chan []byte
	// Err returns the error that ended the connection, if any.
	Err() error
	// Close shuts the transport down.
	Close() error
}

// streamTransport is implemented by transports that carry each request on
// its own response stream.
type streamTransport interface {
	// cancelRequest closes the response stream of a request, which cancels
	// it.
	cancelRequest(id json.RawMessage)
	// onLost registers fn, called when a response stream ends before the
	// response arrives.
	onLost(fn func(id json.RawMessage, err error))
}

// sessionTransport is implemented by transports that carry the sessions of
// the initialization-based revisions.
type sessionTransport interface {
	// beginSession switches to the requests of those revisions and forgets
	// any session, so that the next initialize starts a new one. Messages
	// other than initialize and notifications/initialized wait until
	// endSession.
	beginSession()
	// setVersion sets the negotiated revision sent with later requests.
	setVersion(v string)
	// endSession releases the waiting messages once the session is
	// established or, if ok is false, could not be; they then fail with
	// ErrSessionExpired.
	endSession(ok bool)
	// openStream opens a stream for the server messages that belong to no
	// request. The channel receives why the stream ended.
	openStream(ctx context.Context) (<-chan error, error)
}

// HTTPStatusError is an HTTP error response whose body is not a JSON-RPC
// error.
type HTTPStatusError struct {
	StatusCode int
	// Body is the start of the response body.
	Body string
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("mcp: http %d: %s", e.StatusCode, e.Body)
}

// inbox is the receive side shared by the transports.
type inbox struct {
	ch       chan []byte
	done     chan struct{}
	wg       sync.WaitGroup
	once     sync.Once
	mu       sync.Mutex
	err      error
	shutdown sync.Once
}

func newInbox() *inbox {
	return &inbox{ch: make(chan []byte, 64), done: make(chan struct{})}
}

func (b *inbox) push(msg []byte) bool {
	select {
	case b.ch <- msg:
		return true
	case <-b.done:
		return false
	}
}

func (b *inbox) fail(err error) {
	b.mu.Lock()
	if b.err == nil {
		b.err = err
	}
	b.mu.Unlock()
}

func (b *inbox) Err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

// begin registers a producer. It reports false once the inbox is stopped.
func (b *inbox) begin() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	select {
	case <-b.done:
		return false
	default:
	}
	b.wg.Add(1)
	return true
}

// stop signals producers and closes the channel after they exit.
func (b *inbox) stop() {
	b.once.Do(func() {
		b.mu.Lock()
		close(b.done)
		b.mu.Unlock()
		go func() {
			b.wg.Wait()
			close(b.ch)
		}()
	})
}

// IOTransport exchanges newline-delimited JSON over a reader and writer.
type IOTransport struct {
	r     io.Reader
	w     io.Writer
	close func() error
	in    *inbox
	// wsem serializes writes. It is a channel rather than a mutex so that
	// a Send waiting behind a blocked write can be cancelled.
	wsem chan struct{}
}

// NewIOTransport returns a transport reading from r and writing to w. If
// closer is non-nil it is called by Close.
func NewIOTransport(r io.Reader, w io.Writer, closer func() error) *IOTransport {
	return &IOTransport{r: r, w: w, close: closer, in: newInbox(), wsem: make(chan struct{}, 1)}
}

// Start implements Transport.
func (t *IOTransport) Start(context.Context) error {
	if !t.in.begin() {
		return ErrClosed
	}
	go func() {
		defer t.in.wg.Done()
		defer t.in.stop()
		sc := bufio.NewScanner(t.r)
		sc.Buffer(make([]byte, 64*1024), 16<<20)
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) == 0 {
				continue
			}
			if !t.in.push(bytes.Clone(line)) {
				return
			}
		}
		if err := sc.Err(); err != nil {
			t.in.fail(fmt.Errorf("mcp: read: %w", err))
		} else {
			t.in.fail(io.EOF)
		}
	}()
	return nil
}

// Send implements Transport. Messages are written whole and in the order
// their Sends acquire the writer. If ctx is done while the message waits
// for an earlier write, Send returns ctx.Err() without writing it; if ctx
// is done while the message is being written, the write completes in the
// background, so the stream stays well formed and the peer may still
// receive the message, and Send returns an error wrapping both
// ErrMaybeSent and ctx.Err(). Close unblocks such writes when its closer
// closes the writer, as StdioTransport does.
func (t *IOTransport) Send(ctx context.Context, msg []byte) error {
	select {
	case <-t.in.done:
		return ErrClosed
	default:
	}
	if bytes.ContainsAny(msg, "\n\r") {
		return errors.New("mcp: message contains a newline")
	}
	select {
	case t.wsem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-t.in.done:
		return ErrClosed
	}
	line := append(bytes.Clone(msg), '\n')
	if ctx.Done() == nil {
		defer func() { <-t.wsem }()
		_, err := t.w.Write(line)
		return err
	}
	written := make(chan error, 1)
	go func() {
		_, err := t.w.Write(line)
		<-t.wsem
		written <- err
	}()
	select {
	case err := <-written:
		return err
	case <-ctx.Done():
		return fmt.Errorf("%w: %w", ErrMaybeSent, ctx.Err())
	}
}

// Receive implements Transport.
func (t *IOTransport) Receive() <-chan []byte { return t.in.ch }

// Err implements Transport.
func (t *IOTransport) Err() error { return t.in.Err() }

// Close implements Transport.
func (t *IOTransport) Close() error {
	var err error
	t.in.shutdown.Do(func() {
		t.in.fail(ErrClosed)
		t.in.stop()
		if t.close != nil {
			err = t.close()
		}
	})
	return err
}

// StdioTransport launches a server process and talks to it over its standard
// input and output.
type StdioTransport struct {
	Command string
	Args    []string
	// Env holds KEY=value entries added to the server's environment. The
	// server inherits only a few variables of the current process, those
	// DefaultEnvironment lists, unless InheritEnv is set: a third-party
	// server must not read the API keys and tokens in the client's
	// environment.
	Env []string
	// InheritEnv passes the whole environment of the current process to
	// the server, before Env.
	InheritEnv bool
	Dir        string
	// Stderr receives the server's standard error. Defaults to discarding.
	Stderr io.Writer
	// ShutdownTimeout bounds how long Close waits for the process to exit
	// after closing its input. Defaults to 5 seconds.
	ShutdownTimeout time.Duration

	io  *IOTransport
	cmd *exec.Cmd
}

// Stdio returns a transport for the given command.
func Stdio(command string, args ...string) *StdioTransport {
	return &StdioTransport{Command: command, Args: args}
}

// stdioWaitDelay bounds how long Close waits for the server's standard
// error to reach end of file after the server and the processes it started
// were killed, since a process that left the tree may hold it open, and
// then for the copy to the Stderr writer to return.
const stdioWaitDelay = 2 * time.Second

// Start implements Transport. The server runs in its own process tree (a
// process group on Unix, a Job Object on Windows). When the server exits,
// or Close stops it, the processes it started are killed too.
func (t *StdioTransport) Start(ctx context.Context) error {
	if t.cmd != nil {
		return errors.New("mcp: transport already started")
	}
	cmd := exec.Command(t.Command, t.Args...) //nolint:noctx // The process outlives ctx, which only bounds startup; Close stops it.
	cmd.Dir = t.Dir
	env := DefaultEnvironment()
	if t.InheritEnv {
		env = os.Environ()
	}
	env = append(env, t.Env...)
	cmd.Env = env
	// The pipes are created here rather than by exec. Wait closes the
	// read end of StdoutPipe, which would lose the messages a server
	// writes just before it exits, and it waits for exec's own stderr
	// pipe, which descendants such as the node process started by npx may
	// hold open.
	var files []*os.File // the child's ends, closed once it started
	var stdoutR, stdinW, stderrR *os.File
	closeAll := func() {
		for _, f := range append(files, stdoutR, stdinW, stderrR) {
			if f != nil {
				_ = f.Close()
			}
		}
	}
	pipe := func() (r, w *os.File, err error) {
		r, w, err = os.Pipe()
		if err != nil {
			closeAll()
		}
		return r, w, err
	}
	stdinR, stdinW, err := pipe()
	if err != nil {
		return err
	}
	files = append(files, stdinR)
	stdoutR, stdoutW, err := pipe()
	if err != nil {
		return err
	}
	files = append(files, stdoutW)
	cmd.Stdin, cmd.Stdout = stdinR, stdoutW
	switch w := t.Stderr.(type) {
	case nil:
	case *os.File:
		cmd.Stderr = w
	default:
		var stderrW *os.File
		if stderrR, stderrW, err = pipe(); err != nil {
			return err
		}
		files = append(files, stderrW)
		cmd.Stderr = stderrW
	}
	if err := ctx.Err(); err != nil {
		closeAll()
		return err
	}
	tree := process.NewTree(cmd)
	if err := tree.Start(); err != nil {
		closeAll()
		return fmt.Errorf("mcp: start %s: %w", t.Command, err)
	}
	for _, f := range files {
		_ = f.Close()
	}
	t.cmd = cmd
	stderrDone := make(chan struct{})
	if stderrR != nil {
		go func() {
			defer close(stderrDone)
			_, _ = io.Copy(t.Stderr, stderrR)
		}()
	} else {
		close(stderrDone)
	}
	exited := make(chan struct{})
	go func() {
		// Wait touches none of the pipes, so the reader still sees every
		// message up to end of file. It also kills the processes the server
		// left behind, which would keep its output open, hiding that the
		// connection ended.
		_ = tree.Wait()
		close(exited)
	}()
	t.io = NewIOTransport(stdoutR, stdinW, func() error {
		_ = stdinW.Close()
		timeout := t.ShutdownTimeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-exited:
			// The tree was killed when the server exited. Killing it again
			// could hit an unrelated process that reused its ID.
		case <-timer.C:
			_ = tree.Kill()
			<-exited
		}
		_ = tree.Close()
		// Unblock the readers if a process that left the tree still holds
		// the server's output. Closing a pipe interrupts a pending read,
		// on Windows too.
		_ = stdoutR.Close()
		if stderrR != nil {
			wait := time.NewTimer(stdioWaitDelay)
			defer wait.Stop()
			select {
			case <-stderrDone:
			case <-wait.C:
			}
			_ = stderrR.Close()
			// The copy may be blocked writing to Stderr rather than
			// reading; it then ends once that write returns.
			wait.Reset(stdioWaitDelay)
			select {
			case <-stderrDone:
			case <-wait.C:
			}
		}
		return nil
	})
	return t.io.Start(ctx)
}

// Send implements Transport.
func (t *StdioTransport) Send(ctx context.Context, msg []byte) error {
	if t.io == nil {
		return errors.New("mcp: transport not started")
	}
	return t.io.Send(ctx, msg)
}

// Receive implements Transport.
func (t *StdioTransport) Receive() <-chan []byte {
	if t.io == nil {
		return nil
	}
	return t.io.Receive()
}

// Err implements Transport.
func (t *StdioTransport) Err() error {
	if t.io == nil {
		return nil
	}
	return t.io.Err()
}

// Close implements Transport.
func (t *StdioTransport) Close() error {
	if t.io == nil {
		return nil
	}
	return t.io.Close()
}

// HTTPTransport implements the Streamable HTTP transport. Each message is a
// POST whose response, a JSON object or an SSE stream, belongs to that
// request alone; with ProtocolVersion, closing it cancels the request.
//
// With an initialization-based revision, the transport keeps the session ID
// the server assigns in its initialize response and sends it, with the
// negotiated MCP-Protocol-Version, on every later request, and Close ends
// the session with a DELETE. While a new session is being initialized,
// other messages wait for it and then carry its ID. An SSE stream that ends
// early, because the connection broke or because the server polls by
// closing it, is resumed: after the reconnection time the server set with
// a retry field (ReconnectDelay if none), the transport sends a GET with
// the ID of the last event received as Last-Event-ID. It gives up after
// MaxReconnectAttempts consecutive failed attempts, when the server
// refuses resumption, or when no event had an ID; a request whose stream
// is lost fails with ErrStreamLost. ProtocolVersion has no resumption, so
// its streams are never resumed.
type HTTPTransport struct {
	URL    string
	Client *http.Client
	Header http.Header
	// ReconnectDelay is how long to wait before resuming a stream whose
	// server set no reconnection time. Defaults to 1 second.
	ReconnectDelay time.Duration
	// MaxReconnectAttempts bounds the consecutive failed attempts to resume
	// a stream. Defaults to 5.
	MaxReconnectAttempts int

	in      *inbox
	mu      sync.Mutex
	streams map[string]context.CancelFunc
	lost    func(id json.RawMessage, err error)
	ctx     context.Context
	cancel  context.CancelFunc
	// legacy selects the requests of the initialization-based revisions,
	// with version as their MCP-Protocol-Version, if known, and session as
	// their Mcp-Session-Id, if any.
	legacy  bool
	version string
	session string
	// gate holds back messages while a session is being established.
	gate *sessionGate
}

// sessionGate is closed once a session is established or its
// establishment failed.
type sessionGate struct {
	done chan struct{}
	ok   bool
}

// open releases the messages held back by the gate.
func (g *sessionGate) open(ok bool) {
	select {
	case <-g.done:
	default:
		g.ok = ok
		close(g.done)
	}
}

func (t *HTTPTransport) beginSession() {
	t.mu.Lock()
	if t.gate != nil {
		t.gate.open(false)
	}
	t.legacy, t.version, t.session = true, "", ""
	t.gate = &sessionGate{done: make(chan struct{})}
	t.mu.Unlock()
}

func (t *HTTPTransport) setVersion(v string) {
	t.mu.Lock()
	t.version = v
	t.mu.Unlock()
}

func (t *HTTPTransport) endSession(ok bool) {
	t.mu.Lock()
	if t.gate != nil {
		t.gate.open(ok)
	}
	t.mu.Unlock()
}

// streamSession is what resuming a stream needs: whether it belongs to an
// initialization-based revision, and that revision and session.
type streamSession struct {
	legacy           bool
	version, session string
}

// protocolHeaders sets the protocol version and session headers of a
// message with the given method, first waiting for a session being
// established unless the message establishes it.
func (t *HTTPTransport) protocolHeaders(ctx context.Context, h http.Header, method string) (streamSession, error) {
	t.mu.Lock()
	gate := t.gate
	t.mu.Unlock()
	if gate != nil && method != "initialize" && method != "notifications/initialized" {
		select {
		case <-gate.done:
			if !gate.ok {
				return streamSession{}, fmt.Errorf("%w: no new session was established", ErrSessionExpired)
			}
		case <-ctx.Done():
			return streamSession{}, ctx.Err()
		case <-t.in.done:
			return streamSession{}, ErrClosed
		}
	}
	t.mu.Lock()
	ss := streamSession{legacy: t.legacy, version: t.version, session: t.session}
	t.mu.Unlock()
	version := ss.version
	if !ss.legacy {
		version = ProtocolVersion
	}
	// The initialize request itself goes without a version header.
	if version != "" {
		h.Set("MCP-Protocol-Version", version)
	}
	if ss.session != "" {
		h.Set("Mcp-Session-Id", ss.session)
	}
	return ss, nil
}

// expire forgets a session the server ended.
func (t *HTTPTransport) expire(session string) {
	t.mu.Lock()
	if t.session == session {
		t.session = ""
	}
	t.mu.Unlock()
}

// validSessionID reports whether id consists of visible ASCII characters
// only, as session IDs must.
func validSessionID(id string) bool {
	for i := range len(id) {
		if id[i] < 0x21 || id[i] > 0x7e {
			return false
		}
	}
	return id != ""
}

// HTTP returns a Streamable HTTP transport for the endpoint URL.
func HTTP(url string) *HTTPTransport {
	return &HTTPTransport{URL: url}
}

// Start implements Transport.
func (t *HTTPTransport) Start(context.Context) error {
	if t.in != nil {
		return errors.New("mcp: transport already started")
	}
	t.in = newInbox()
	t.streams = map[string]context.CancelFunc{}
	// Response streams outlive the Send call that opened them.
	t.ctx, t.cancel = context.WithCancel(context.Background())
	return nil
}

func (t *HTTPTransport) onLost(fn func(id json.RawMessage, err error)) {
	t.mu.Lock()
	t.lost = fn
	t.mu.Unlock()
}

func (t *HTTPTransport) cancelRequest(id json.RawMessage) {
	t.mu.Lock()
	cancel := t.streams[idKey(id)]
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Send implements Transport.
func (t *HTTPTransport) Send(ctx context.Context, msg []byte) error {
	if t.in == nil {
		return errors.New("mcp: transport not started")
	}
	select {
	case <-t.in.done:
		return ErrClosed
	default:
	}
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(msg, &m); err != nil {
		return fmt.Errorf("mcp: invalid message: %w", err)
	}
	// The request context must outlive Send when the server answers with a
	// stream, so cancellation of ctx only applies until headers arrive.
	reqCtx, cancel := context.WithCancel(t.ctx)
	stop := context.AfterFunc(ctx, cancel)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, t.URL, bytes.NewReader(msg))
	if err != nil {
		stop()
		cancel()
		return err
	}
	httpclient.AddHeaders(req.Header, t.Header)
	ss, err := t.protocolHeaders(ctx, req.Header, m.Method)
	if err != nil {
		stop()
		cancel()
		return err
	}
	legacy, session := ss.legacy, ss.session
	if !legacy {
		// The standard and parameter headers belong to ProtocolVersion.
		if h, ok := ctx.Value(paramHeadersKey{}).(http.Header); ok {
			httpclient.AddHeaders(req.Header, h)
		}
		req.Header.Set("Mcp-Method", m.Method)
		if name := requestName(m.Method, m.Params); name != "" {
			req.Header.Set("Mcp-Name", headerValue(name))
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	key := ""
	// A message with an ID and no method is a response to a server request.
	if len(m.ID) > 0 && m.Method != "" {
		key = idKey(m.ID)
		t.mu.Lock()
		t.streams[key] = cancel
		t.mu.Unlock()
	}
	release := func() {
		if key != "" {
			t.mu.Lock()
			delete(t.streams, key)
			t.mu.Unlock()
		}
		cancel()
	}
	// The body is closed below or by the goroutine that reads it.
	resp, err := httpclient.ClientOr(t.Client).Do(req) //nolint:bodyclose
	stop()
	if err != nil {
		release()
		if ctx.Err() != nil {
			// The server may have received the request before ctx ended
			// the exchange. Ending it cancels the request with
			// ProtocolVersion, but not with the earlier revisions.
			return fmt.Errorf("%w: %w", ErrMaybeSent, ctx.Err())
		}
		return fmt.Errorf("mcp: post: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		defer release()
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if resp.StatusCode == http.StatusNotFound && session != "" {
			t.expire(session)
			return fmt.Errorf("%w: http 404", ErrSessionExpired)
		}
		// Modern servers explain rejections, such as an unsupported
		// protocol version, with a JSON-RPC error.
		var e message
		if json.Unmarshal(body, &e) == nil && e.Error != nil {
			return e.Error
		}
		return &HTTPStatusError{StatusCode: resp.StatusCode, Body: string(bytes.TrimSpace(body))}
	}
	if legacy && m.Method == "initialize" {
		if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
			if !validSessionID(id) {
				resp.Body.Close()
				release()
				return fmt.Errorf("mcp: invalid session id %q", id)
			}
			t.mu.Lock()
			t.session = id
			t.mu.Unlock()
		}
	}
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		resp.Body.Close()
		release()
		if key != "" {
			return fmt.Errorf("mcp: http %d without a response", resp.StatusCode)
		}
		return nil
	}
	if !t.in.begin() {
		resp.Body.Close()
		release()
		return ErrClosed
	}
	go func() {
		defer t.in.wg.Done()
		defer release()
		defer resp.Body.Close()
		answered, err := t.read(reqCtx, resp, key, m.ID, ss)
		if key != "" && !answered && reqCtx.Err() == nil {
			t.mu.Lock()
			lost := t.lost
			t.mu.Unlock()
			if lost != nil {
				lost(m.ID, cmp.Or(err, ErrStreamLost))
			}
		}
	}()
	return nil
}

// read delivers the messages of a response and reports whether the
// response to the request with the given key and ID was among them. It
// stops once that response arrives.
func (t *HTTPTransport) read(ctx context.Context, resp *http.Response, key string, id json.RawMessage, ss streamSession) (answered bool, err error) {
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType == "text/event-stream" {
		return t.stream(ctx, resp.Body, key, ss)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrStreamLost, err)
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return false, nil
	}
	answered = isResponse(body, key)
	if !answered && key != "" {
		// A JSON body answers the request it was sent for, even if, like
		// the errors some servers return before initialize, it lacks the
		// request's ID.
		body, answered = adoptError(body, id)
	}
	t.in.push(body)
	return answered, nil
}

// defaultReconnectDelay and defaultReconnectAttempts are the defaults of
// HTTPTransport.ReconnectDelay and MaxReconnectAttempts.
const (
	defaultReconnectDelay    = time.Second
	defaultReconnectAttempts = 5
)

// stream delivers the messages of an SSE stream until the response to the
// request with the given key arrives, if key is not empty, and reports
// whether it did. When the stream ends first, a stream of an
// initialization-based revision whose events had IDs is resumed with GET
// and Last-Event-ID. It closes body.
func (t *HTTPTransport) stream(ctx context.Context, body io.ReadCloser, key string, ss streamSession) (answered bool, err error) {
	var cur sseCursor
	failures := 0
	for {
		stopped := false
		err = readEvents(body, &cur, func(event, data string) bool {
			msg := bytes.TrimSpace([]byte(data))
			if (event != "" && event != "message") || len(msg) == 0 {
				return true
			}
			answered = isResponse(msg, key)
			if !t.in.push(msg) {
				stopped = true
				return false
			}
			return !answered
		})
		_ = body.Close()
		if answered || stopped {
			return answered, nil
		}
		err = cmp.Or(err, io.EOF)
		if !ss.legacy || cur.lastID == "" || ctx.Err() != nil {
			return false, fmt.Errorf("%w: %w", ErrStreamLost, err)
		}
		for body = nil; body == nil; {
			delay := cmp.Or(cur.retry, t.ReconnectDelay, defaultReconnectDelay)
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return false, fmt.Errorf("%w: %w", ErrStreamLost, ctx.Err())
			case <-t.in.done:
				timer.Stop()
				return false, nil
			}
			body, err = t.resume(ctx, ss, cur.lastID)
			if err != nil {
				failures++
				if !resumable(ctx, err) || failures >= cmp.Or(t.MaxReconnectAttempts, defaultReconnectAttempts) {
					return false, fmt.Errorf("%w: resume: %w", ErrStreamLost, err)
				}
			}
		}
		failures = 0
	}
}

// resume reopens a stream of session ss after the event lastID.
func (t *HTTPTransport) resume(ctx context.Context, ss streamSession, lastID string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.URL, http.NoBody)
	if err != nil {
		return nil, err
	}
	httpclient.AddHeaders(req.Header, t.Header)
	if ss.version != "" {
		req.Header.Set("MCP-Protocol-Version", ss.version)
	}
	if ss.session != "" {
		req.Header.Set("Mcp-Session-Id", ss.session)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Last-Event-ID", lastID)
	// The body is closed below or by stream.
	resp, err := httpclient.ClientOr(t.Client).Do(req) //nolint:bodyclose
	if err != nil {
		return nil, err
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode == http.StatusOK && mediaType == "text/event-stream" {
		return resp.Body, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return nil, &HTTPStatusError{StatusCode: resp.StatusCode, Body: string(bytes.TrimSpace(body))}
}

// resumable reports whether a failed attempt to resume a stream may be
// repeated: after network errors and server errors, timeouts and rate
// limits, but not once the server refused resumption, for example with 404
// for an ended session or 405 for a server without GET streams.
func resumable(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if he, ok := errors.AsType[*HTTPStatusError](err); ok {
		return he.StatusCode >= http.StatusInternalServerError ||
			he.StatusCode == http.StatusRequestTimeout || he.StatusCode == http.StatusTooManyRequests
	}
	return true
}

// adoptError gives an error response without an ID the given ID.
func adoptError(body []byte, id json.RawMessage) ([]byte, bool) {
	var m message
	if json.Unmarshal(body, &m) != nil || m.Method != "" || m.Error == nil ||
		len(m.ID) > 0 && string(m.ID) != "null" {
		return body, false
	}
	b, err := json.Marshal(message{JSONRPC: "2.0", ID: id, Error: m.Error})
	if err != nil {
		return body, false
	}
	return b, true
}

// isResponse reports whether msg is the response to the request with the
// given key.
func isResponse(msg []byte, key string) bool {
	if key == "" {
		return false
	}
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	return json.Unmarshal(msg, &m) == nil && m.Method == "" && len(m.ID) > 0 && idKey(m.ID) == key
}

// requestName returns the Mcp-Name header source of a request: the tool or
// prompt name, or the resource URI.
func requestName(method string, params json.RawMessage) string {
	var p struct {
		Name string `json:"name"`
		URI  string `json:"uri"`
	}
	switch method {
	case "tools/call", "prompts/get":
		_ = json.Unmarshal(params, &p)
		return p.Name
	case "resources/read":
		_ = json.Unmarshal(params, &p)
		return p.URI
	}
	return ""
}

// Receive implements Transport.
func (t *HTTPTransport) Receive() <-chan []byte {
	if t.in == nil {
		return nil
	}
	return t.in.ch
}

// Err implements Transport.
func (t *HTTPTransport) Err() error {
	if t.in == nil {
		return nil
	}
	return t.in.Err()
}

// openStream opens the GET stream of a session, which carries the server's
// requests and notifications that belong to no request.
func (t *HTTPTransport) openStream(ctx context.Context) (<-chan error, error) {
	if t.in == nil {
		return nil, errors.New("mcp: transport not started")
	}
	reqCtx, cancel := context.WithCancel(t.ctx)
	stop := context.AfterFunc(ctx, cancel)
	end := func() {
		stop()
		cancel()
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, t.URL, http.NoBody)
	if err != nil {
		end()
		return nil, err
	}
	httpclient.AddHeaders(req.Header, t.Header)
	ss, err := t.protocolHeaders(ctx, req.Header, "")
	if err != nil {
		end()
		return nil, err
	}
	session := ss.session
	req.Header.Set("Accept", "text/event-stream")
	// The body is closed below or by the goroutine that reads it.
	resp, err := httpclient.ClientOr(t.Client).Do(req) //nolint:bodyclose
	if err != nil {
		end()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("mcp: get: %w", err)
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode != http.StatusOK || mediaType != "text/event-stream" {
		defer end()
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		switch {
		case resp.StatusCode == http.StatusMethodNotAllowed:
			return nil, fmt.Errorf("mcp: server offers no stream for notifications: %w", errors.ErrUnsupported)
		case resp.StatusCode == http.StatusNotFound && session != "":
			t.expire(session)
			return nil, fmt.Errorf("%w: http 404", ErrSessionExpired)
		case resp.StatusCode >= http.StatusBadRequest:
			return nil, &HTTPStatusError{StatusCode: resp.StatusCode, Body: string(bytes.TrimSpace(body))}
		}
		return nil, fmt.Errorf("mcp: unexpected stream response: http %d, %s", resp.StatusCode, mediaType)
	}
	if !t.in.begin() {
		resp.Body.Close()
		end()
		return nil, ErrClosed
	}
	ended := make(chan error, 1)
	go func() {
		defer t.in.wg.Done()
		defer end()
		_, err := t.stream(reqCtx, resp.Body, "", ss)
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		ended <- cmp.Or(err, ErrStreamLost)
	}()
	return ended, nil
}

// sessionDeleteTimeout bounds how long Close waits for the server to end a
// session.
const sessionDeleteTimeout = 5 * time.Second

// deleteSession asks the server to end the session, if there is one. The
// server may refuse with 405 Method Not Allowed; the session is forgotten
// either way.
func (t *HTTPTransport) deleteSession() {
	t.mu.Lock()
	legacy, version, session := t.legacy, t.version, t.session
	t.session = ""
	t.mu.Unlock()
	if !legacy || session == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionDeleteTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.URL, http.NoBody)
	if err != nil {
		return
	}
	httpclient.AddHeaders(req.Header, t.Header)
	if version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}
	req.Header.Set("Mcp-Session-Id", session)
	resp, err := httpclient.ClientOr(t.Client).Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// Close implements Transport. A session of an initialization-based
// revision is ended with an HTTP DELETE, waiting up to 5 seconds for the
// server. Open response streams are then closed, which with ProtocolVersion
// cancels their requests.
func (t *HTTPTransport) Close() error {
	if t.in == nil {
		return nil
	}
	t.in.shutdown.Do(func() {
		t.deleteSession()
		t.in.fail(ErrClosed)
		t.cancel()
		t.in.stop()
	})
	return nil
}

// inheritedEnv are the variables a stdio server inherits by default, as in
// the official MCP SDKs: what a shell, npx or uvx needs to find programs,
// the home directory and temporary files.
var inheritedEnv = map[string][]string{
	"windows": {"APPDATA", "HOMEDRIVE", "HOMEPATH", "LOCALAPPDATA", "PATH", "PATHEXT", "PROCESSOR_ARCHITECTURE",
		"PROGRAMFILES", "SYSTEMDRIVE", "SYSTEMROOT", "TEMP", "TMP", "USERNAME", "USERPROFILE"},
	"": {"HOME", "LOGNAME", "PATH", "SHELL", "TERM", "TMPDIR", "USER", "LANG", "LC_ALL"},
}

// DefaultEnvironment returns the KEY=value entries of the current
// environment a StdioTransport passes to its server unless InheritEnv is
// set: on Unix HOME, LOGNAME, PATH, SHELL, TERM, TMPDIR, USER, LANG and
// LC_ALL, on Windows the variables programs need to start. Values that
// are shell functions are left out.
func DefaultEnvironment() []string {
	names, ok := inheritedEnv[runtime.GOOS]
	if !ok {
		names = inheritedEnv[""]
	}
	var env []string
	for _, name := range names {
		v, ok := os.LookupEnv(name)
		if !ok || strings.HasPrefix(v, "()") {
			continue
		}
		env = append(env, name+"="+v)
	}
	return env
}
