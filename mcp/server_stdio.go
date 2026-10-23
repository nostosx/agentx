package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// ServeStdio serves one client over newline-delimited JSON-RPC, reading
// from r and writing to w, as the stdio transport
// (https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/stdio)
// does with a server process's standard input and output; any reliable
// byte stream works the same way. Requests run concurrently. Requests with
// the protocol version in _meta are served statelessly; initialize starts a
// session of an initialization-based revision for the rest of the
// connection, in which handlers' Sample, Elicit and ListRoots send the
// client requests on w.
//
// The client cancels a request with notifications/cancelled. ServeStdio
// returns nil when r reaches end of file, and ctx.Err() when ctx is done,
// after ending subscriptions gracefully, cancelling the other requests and
// waiting for their handlers to return. A read blocked on r is abandoned
// then; close r to end it.
//
// Servers launched as a process serve with
//
//	err := srv.ServeStdio(ctx, os.Stdin, os.Stdout)
//
// and must write nothing else to standard output.
func (s *Server) ServeStdio(ctx context.Context, r io.Reader, w io.Writer) error {
	base, cancel := context.WithCancel(context.WithoutCancel(ctx))
	sc := &stdioConn{s: s, w: w, base: base, inflight: map[string]*inflight{}, shutdown: make(chan struct{})}
	lines := make(chan []byte)
	readErr := make(chan error, 1)
	stop := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) == 0 {
				continue
			}
			select {
			case lines <- bytes.Clone(line):
			case <-stop:
				return
			}
		}
		readErr <- sc.Err()
	}()
	var err error
loop:
	for {
		select {
		case line := <-lines:
			sc.handle(line)
		case err = <-readErr:
			if err != nil {
				err = fmt.Errorf("mcp: read: %w", err)
			}
			break loop
		case <-ctx.Done():
			err = ctx.Err()
			break loop
		}
	}
	close(stop)
	close(sc.shutdown)
	cancel()
	sc.wg.Wait()
	sc.wmu.Lock()
	sc.closed = true
	sc.wmu.Unlock()
	if ss := sc.session(); ss != nil {
		ss.close()
	}
	return err
}

// inflight is a request being served on a stdio connection.
type inflight struct {
	cancel context.CancelFunc
	// cancelled is set when the client cancelled the request, which then
	// gets no response.
	cancelled atomic.Bool
}

type stdioConn struct {
	s        *Server
	base     context.Context
	shutdown chan struct{}
	wg       sync.WaitGroup

	wmu    sync.Mutex
	w      io.Writer
	closed bool

	mu       sync.Mutex
	inflight map[string]*inflight
	sess     *session
}

// write sends one message.
func (sc *stdioConn) write(msg []byte) error {
	sc.wmu.Lock()
	defer sc.wmu.Unlock()
	if sc.closed {
		return ErrClosed
	}
	_, err := sc.w.Write(append(bytes.Clone(msg), '\n'))
	return err
}

func (sc *stdioConn) session() *session {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.sess
}

// handle dispatches one incoming message.
func (sc *stdioConn) handle(line []byte) {
	if line[0] == '[' {
		_ = sc.write(encode(json.RawMessage("null"), nil, &Error{Code: CodeInvalidRequest, Message: "batches are not supported"}))
		return
	}
	var m message
	if err := json.Unmarshal(line, &m); err != nil {
		_ = sc.write(encode(json.RawMessage("null"), nil, &Error{Code: CodeParseError, Message: "Parse error"}))
		return
	}
	switch {
	case m.Method == "" && len(m.ID) > 0:
		if ss := sc.session(); ss != nil {
			ss.resolve(m.ID, &m)
		}
	case m.Method != "" && len(m.ID) == 0:
		sc.notification(m.Method, m.Params)
	case m.Method != "":
		sc.request(&m)
	default:
		_ = sc.write(encode(json.RawMessage("null"), nil, &Error{Code: CodeInvalidRequest, Message: "Invalid request"}))
	}
}

func (sc *stdioConn) notification(method string, params json.RawMessage) {
	switch method {
	case "notifications/cancelled":
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(params, &p) != nil || len(p.RequestID) == 0 {
			return
		}
		sc.mu.Lock()
		in := sc.inflight[idKey(p.RequestID)]
		sc.mu.Unlock()
		if in != nil {
			in.cancelled.Store(true)
			in.cancel()
		}
	case "notifications/initialized":
		if ss := sc.session(); ss != nil {
			ss.markInitialized()
		}
	}
}

func (sc *stdioConn) request(m *message) {
	key := idKey(m.ID)
	ctx, cancel := context.WithCancel(sc.base)
	in := &inflight{cancel: cancel}
	sc.mu.Lock()
	if _, dup := sc.inflight[key]; dup {
		sc.mu.Unlock()
		cancel()
		_ = sc.write(encode(m.ID, nil, &Error{Code: CodeInvalidRequest, Message: "duplicate request id"}))
		return
	}
	sc.inflight[key] = in
	sc.mu.Unlock()
	c := &incoming{id: m.ID, method: m.Method, params: m.Params, send: sc.write, shutdown: sc.shutdown}
	sc.wg.Go(func() {
		defer func() {
			sc.mu.Lock()
			delete(sc.inflight, key)
			sc.mu.Unlock()
			cancel()
		}()
		res, err := sc.dispatch(ctx, c)
		if errors.Is(err, errNoResponse) || in.cancelled.Load() {
			return
		}
		_ = sc.write(encode(c.id, res, err))
	})
}

// dispatch selects the era of a request and serves it.
func (sc *stdioConn) dispatch(ctx context.Context, c *incoming) (any, error) {
	if isModern(c.params) {
		if e := sc.s.prepareModern(c); e != nil {
			return nil, e
		}
		return sc.s.serve(ctx, c)
	}
	if c.method == "initialize" {
		ss := newSession(sc.s, sc.write)
		res, err := sc.s.initialize(c, ss)
		if err != nil {
			return nil, err
		}
		sc.mu.Lock()
		old := sc.sess
		sc.sess = ss
		sc.mu.Unlock()
		if old != nil {
			old.close()
		}
		return res, nil
	}
	ss := sc.session()
	if ss == nil {
		return nil, invalidParams("missing required _meta field %s; send initialize first to use an initialization-based revision",
			metaProtocolVersion)
	}
	c.sess, c.caps, c.info = ss, ss.caps, ss.info
	return sc.s.serve(ctx, c)
}
