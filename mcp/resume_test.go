package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestReadEvents(t *testing.T) {
	for _, sep := range []string{"\n", "\r\n", "\r"} {
		input := strings.Join([]string{
			": comment", "retry: 25", "id: 1", "data:", "",
			"event: message", "data: a", "data: b", "",
			// An ID without data is still recorded when the event is
			// dispatched, and the ID carries over to later events.
			"id: 2", "", "data: c", "",
			"retry: x", "id: bad\x00", "data: d", "",
			"id: 3", "data: cut",
		}, sep)
		var cur sseCursor
		var got []string
		if err := readEvents(strings.NewReader(input), &cur, func(event, data string) bool {
			got = append(got, event+"|"+data)
			return true
		}); err != nil {
			t.Fatal(err)
		}
		if want := []string{"|", "message|a\nb", "|c", "|d"}; !slices.Equal(got, want) {
			t.Errorf("events = %q, want %q", got, want)
		}
		if cur.lastID != "2" || cur.retry != 25*time.Millisecond || cur.events != 5 {
			t.Errorf("cursor = %+v", cur)
		}
	}
	var cur sseCursor
	n := 0
	_ = readEvents(strings.NewReader("data: 1\n\ndata: 2\n\n"), &cur, func(string, string) bool { n++; return false })
	if n != 1 {
		t.Errorf("stopped after %d events", n)
	}
}

// resumeServer is a Streamable HTTP server of revision 2025-11-25 whose
// streams break: post answers a POST in the session, get a resumption.
type resumeServer struct {
	t    *testing.T
	post func(w http.ResponseWriter, m wireMessage)
	get  func(w http.ResponseWriter, lastID string)

	mu      sync.Mutex
	lastIDs []string
	gets    int
}

func (s *resumeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodDelete:
		return
	case http.MethodGet:
		if r.Header.Get("Mcp-Session-Id") != "sess" || r.Header.Get("MCP-Protocol-Version") != LegacyProtocolVersion {
			s.t.Errorf("GET headers = %v", r.Header)
		}
		id := r.Header.Get("Last-Event-ID")
		s.mu.Lock()
		s.gets++
		if id != "" {
			s.lastIDs = append(s.lastIDs, id)
		}
		s.mu.Unlock()
		s.get(w, id)
		return
	}
	var m wireMessage
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch {
	case m.Method == "server/discover":
		http.Error(w, "no session", http.StatusBadRequest)
	case m.Method == "initialize":
		w.Header().Set("Mcp-Session-Id", "sess")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, initResult(m.ID, LegacyProtocolVersion, `{"tools":{}}`))
	case len(m.ID) == 0 || m.Method == "":
		w.WriteHeader(http.StatusAccepted)
	default:
		s.post(w, m)
	}
}

func (s *resumeServer) resumed() (ids []string, gets int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.lastIDs), s.gets
}

func sse(w http.ResponseWriter, events string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, events)
}

func connectResume(t *testing.T, s *resumeServer, tr *HTTPTransport, opts ...ClientOption) *Client {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	tr.URL = srv.URL
	c, err := Connect(t.Context(), tr, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestHTTPResumePolling(t *testing.T) {
	var callID json.RawMessage
	var mu sync.Mutex
	s := &resumeServer{t: t}
	s.post = func(w http.ResponseWriter, m wireMessage) {
		mu.Lock()
		callID = m.ID
		mu.Unlock()
		// Prime the client and close the connection: the 2025-11-25
		// polling pattern.
		sse(w, "retry: 20\nid: e1\ndata:\n\n")
	}
	broken := false
	s.get = func(w http.ResponseWriter, lastID string) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case lastID == "e1":
			sse(w, "id: e2\ndata: "+`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progress":1}}`+"\n\n")
		case lastID == "e2" && !broken:
			// The connection breaks in the middle of an event.
			broken = true
			sse(w, "id: e3\ndata: {\"jsonrpc\"")
		case lastID == "e2":
			sse(w, "id: e3\ndata: "+result(callID, `{"content":[{"type":"text","text":"done"}]}`)+"\n\n")
		default:
			http.Error(w, "unknown event "+lastID, http.StatusBadRequest)
		}
	}
	notes := make(chan string, 4)
	c := connectResume(t, s, &HTTPTransport{}, OnNotification(func(method string, _ json.RawMessage) { notes <- method }))
	start := time.Now()
	res, err := c.CallTool(t.Context(), "slow", nil)
	if err != nil || FormatContent(res) != "done" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	// The retry field set the delay; the incomplete event set no ID.
	if ids, _ := s.resumed(); !slices.Equal(ids, []string{"e1", "e2", "e2"}) {
		t.Errorf("Last-Event-IDs = %v", ids)
	}
	if d := time.Since(start); d < 60*time.Millisecond {
		t.Errorf("resumed after %v, before the retry delays", d)
	}
	if m := <-notes; m != "notifications/progress" {
		t.Errorf("notification = %q", m)
	}
}

func TestHTTPResumeAbortedConnection(t *testing.T) {
	var mu sync.Mutex
	var callID json.RawMessage
	s := &resumeServer{t: t}
	s.post = func(w http.ResponseWriter, m wireMessage) {
		mu.Lock()
		callID = m.ID
		mu.Unlock()
		sse(w, "id: a1\ndata:\n\n")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // a network failure
	}
	s.get = func(w http.ResponseWriter, _ string) {
		mu.Lock()
		defer mu.Unlock()
		sse(w, "data: "+result(callID, `{"content":[]}`)+"\n\n")
	}
	c := connectResume(t, s, &HTTPTransport{ReconnectDelay: 10 * time.Millisecond})
	if _, err := c.CallTool(t.Context(), "x", nil); err != nil {
		t.Fatal(err)
	}
	if ids, _ := s.resumed(); !slices.Equal(ids, []string{"a1"}) {
		t.Errorf("Last-Event-IDs = %v", ids)
	}
}

func TestHTTPResumeGivesUp(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		gets   int
	}{
		"server error is retried": {status: http.StatusBadGateway, gets: 3},
		"405 refuses resumption":  {status: http.StatusMethodNotAllowed, gets: 1},
		"404 ended the session":   {status: http.StatusNotFound, gets: 1},
	} {
		t.Run(name, func(t *testing.T) {
			s := &resumeServer{t: t}
			s.post = func(w http.ResponseWriter, _ wireMessage) { sse(w, "id: p\ndata:\n\n") }
			s.get = func(w http.ResponseWriter, _ string) { http.Error(w, "no", tc.status) }
			c := connectResume(t, s, &HTTPTransport{ReconnectDelay: time.Millisecond, MaxReconnectAttempts: 3})
			_, err := c.CallTool(t.Context(), "x", nil)
			var he *HTTPStatusError
			if !errors.Is(err, ErrStreamLost) || !errors.As(err, &he) || he.StatusCode != tc.status {
				t.Fatalf("err = %v", err)
			}
			if errors.Is(err, ErrSessionExpired) {
				t.Error("a request the server may have processed must not be reissued")
			}
			if _, gets := s.resumed(); gets != tc.gets {
				t.Errorf("GETs = %d, want %d", gets, tc.gets)
			}
		})
	}
	t.Run("no event ID", func(t *testing.T) {
		s := &resumeServer{t: t}
		s.post = func(w http.ResponseWriter, _ wireMessage) { sse(w, "data:\n\n") }
		s.get = func(http.ResponseWriter, string) { t.Error("resumed without an event ID") }
		c := connectResume(t, s, &HTTPTransport{ReconnectDelay: time.Millisecond})
		if _, err := c.CallTool(t.Context(), "x", nil); !errors.Is(err, ErrStreamLost) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestHTTPResumeHonorsContext(t *testing.T) {
	s := &resumeServer{t: t}
	s.post = func(w http.ResponseWriter, _ wireMessage) { sse(w, "retry: 60000\nid: p\ndata:\n\n") }
	s.get = func(http.ResponseWriter, string) { t.Error("resumed before the retry delay") }
	c := connectResume(t, s, &HTTPTransport{})
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.CallTool(ctx, "x", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("CallTool took %v", d)
	}
}

func TestModernHTTPDoesNotResume(t *testing.T) {
	var gets int
	var mu sync.Mutex
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			mu.Lock()
			gets++
			mu.Unlock()
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "server/discover" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, result(req.ID, discoverResult))
			return
		}
		sse(w, "retry: 1\nid: 1\ndata:\n\n")
	}))
	defer s.Close()
	c := connectHTTP(t, s.URL)
	if _, err := c.CallTool(t.Context(), "x", nil); !errors.Is(err, ErrStreamLost) {
		t.Fatalf("err = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gets != 0 {
		t.Errorf("GETs = %d", gets)
	}
}

func TestLegacyHTTPListenResumeRefused(t *testing.T) {
	s := newLegacyHTTP(t)
	s.refuseResume = true
	c := connectLegacyHTTP(t, s.start())
	done := make(chan error, 1)
	go func() { done <- c.Listen(t.Context(), Subscription{}) }()
	<-s.streams
	s.push <- ""
	if id := <-s.resumes; id != "prime" {
		t.Errorf("Last-Event-ID = %q", id)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrStreamLost) {
			t.Errorf("Listen = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Listen did not return")
	}
}

// TestLegacyHTTPRequestsWaitForRenewal checks that requests issued while an
// expired session is being renewed wait for the new session instead of
// going out without a session ID.
func TestLegacyHTTPRequestsWaitForRenewal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newLegacyHTTP(t)
		s.initGate = make(chan struct{})
		s.initStarted = make(chan string, 1)
		s.handle = func(m wireMessage) string { return result(m.ID, `{"tools":[]}`) }
		c := connectLegacyHTTP(t, s.start())
		release := sync.OnceFunc(func() { close(s.initGate) })
		t.Cleanup(release) // before the server closes
		s.expireAll()
		first := make(chan error, 1)
		go func() {
			_, err := c.ListTools(t.Context())
			first <- err
		}()
		var renewed string
		select {
		case renewed = <-s.initStarted:
		case <-time.After(5 * time.Second):
			t.Fatal("session not renewed")
		}
		second := make(chan error, 1)
		go func() {
			_, err := c.ListResources(t.Context())
			second <- err
		}()
		synctest.Wait()
		if slices.Contains(s.methods(), "resources/list") {
			t.Fatal("request sent while the session was being renewed")
		}
		release()
		for _, ch := range []chan error{first, second} {
			if err := <-ch; err != nil {
				t.Fatal(err)
			}
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		for i, m := range s.posts {
			if m.Method == "resources/list" && s.postedIn[i] != renewed {
				t.Errorf("resources/list sent in session %q, want %q", s.postedIn[i], renewed)
			}
			if m.Method != "initialize" && m.Method != "server/discover" && s.postedIn[i] == "" {
				t.Errorf("%s sent without a session", m.Method)
			}
		}
	})
}
