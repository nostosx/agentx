package replay_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nostosx/agentx/eval/replay"
)

// rawHeader returns the first value of a header whose key may not be
// canonical.
func rawHeader(h http.Header, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

// doErr sends req and returns its error, closing the body of a response.
func doErr(c *http.Client, req *http.Request) error {
	resp, err := c.Do(req)
	if err == nil {
		resp.Body.Close()
	}
	return err
}

// reply is a response read by do.
type reply struct {
	StatusCode    int
	Status        string
	Header        http.Header
	ContentLength int64
}

func do(t *testing.T, c *http.Client, method, u, body string, header http.Header) (reply, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, u, r)
	if err != nil {
		t.Fatal(err)
	}
	maps.Copy(req.Header, header)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return reply{resp.StatusCode, resp.Status, resp.Header, resp.ContentLength}, string(b)
}

func TestRecordAndReplay(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "session=secret-cookie")
		w.Header().Set("X-Request-Id", "req-1")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"method":%q,"auth":%t,"got":%s}`, r.Method, r.Header.Get("Authorization") != "", body)
	}))
	t.Cleanup(srv.Close)
	path := filepath.Join(t.TempDir(), "cassette.json")

	rec, err := replay.New(path, replay.WithMode(replay.ModeRecord), replay.WithTransport(srv.Client().Transport))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Mode() != replay.ModeRecord {
		t.Fatalf("mode = %s", rec.Mode())
	}
	header := http.Header{"Authorization": {"Bearer secret-token"}, "x-api-key": {"secret-key"}, "Content-Type": {"application/json"}}
	resp, body := do(t, rec.Client(), http.MethodPost, srv.URL+"/v1/items?key=secret-query&b=2&a=1", `{"b":1,"a":[1,2]}`, header)
	if resp.StatusCode != http.StatusCreated || body != `{"method":"POST","auth":true,"got":{"b":1,"a":[1,2]}}` {
		t.Fatalf("recorded response = %d %s", resp.StatusCode, body)
	}
	if _, body = do(t, rec.Client(), http.MethodGet, srv.URL+"/v1/items", "", nil); !strings.Contains(body, `"got":}`) {
		t.Fatalf("GET = %s", body)
	}
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"secret-token", "secret-key", "secret-query", "secret-cookie"} {
		if strings.Contains(string(data), leak) {
			t.Errorf("cassette leaks %s:\n%s", leak, data)
		}
	}
	c, err := replay.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	in := c.Interactions[0]
	if c.Version != replay.CassetteVersion || len(c.Interactions) != 2 || in.Request.Method != http.MethodPost ||
		in.Request.Header.Get("Authorization") != replay.Redacted || rawHeader(in.Request.Header, "x-api-key") != replay.Redacted ||
		in.Request.Header.Get("Content-Type") != "application/json" || in.Response.Header.Get("Set-Cookie") != replay.Redacted ||
		in.Request.Body != `{"b":1,"a":[1,2]}` || in.Response.StatusCode != http.StatusCreated {
		t.Fatalf("interaction = %+v", in)
	}
	if u, _ := url.Parse(in.Request.URL); u.Query().Get("key") != replay.Redacted || u.Query().Get("a") != "1" {
		t.Fatalf("URL = %s", in.Request.URL)
	}
	srv.Close()
	served := hits.Load()

	rp, err := replay.New(path, replay.WithMode(replay.ModeReplay))
	if err != nil {
		t.Fatal(err)
	}
	// Other credentials, query order and JSON formatting still match.
	header = http.Header{"Authorization": {"Bearer other"}}
	resp, body = do(t, rp.Client(), http.MethodPost, srv.URL+"/v1/items?a=1&b=2&key=other", "{\n \"a\": [1, 2], \"b\": 1}", header)
	if resp.StatusCode != http.StatusCreated || resp.Status != "201 Created" || resp.Header.Get("X-Request-Id") != "req-1" ||
		!strings.Contains(body, `"got":{"b":1,"a":[1,2]}`) || resp.ContentLength != int64(len(body)) {
		t.Fatalf("replayed response = %+v %s", resp, body)
	}
	if len(rp.Unused()) != 1 || len(rp.Interactions()) != 2 {
		t.Fatalf("unused = %d", len(rp.Unused()))
	}
	// Each interaction answers once.
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/v1/items?a=1&b=2&key=x", strings.NewReader(`{"b":1,"a":[1,2]}`))
	err = doErr(rp.Client(), req)
	var nm *replay.NoMatchError
	if !errors.As(err, &nm) || nm.Method != http.MethodPost || !strings.Contains(nm.URL, "key=REDACTED") || !strings.Contains(err.Error(), "AGENTX_REPLAY=record") {
		t.Fatalf("second request = %v", err)
	}
	if _, body = do(t, rp.Client(), http.MethodGet, srv.URL+"/v1/items", "", nil); !strings.Contains(body, `"method":"GET"`) {
		t.Fatalf("replayed GET = %s", body)
	}
	if len(rp.Unused()) != 0 || hits.Load() != served {
		t.Fatalf("unused = %d, hits %d", len(rp.Unused()), hits.Load())
	}
	// A different body does not match.
	req, _ = http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/v1/items", strings.NewReader(`{"b":2}`))
	if err := doErr(rp.Client(), req); !errors.As(err, &nm) {
		t.Fatalf("different body = %v", err)
	}
	if err := rp.Save(); err != nil {
		t.Fatalf("Save in replay mode = %v", err)
	}
}

func sseServer(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	events := []string{
		`{"id":"r1","model":"gpt-test","choices":[{"delta":{"content":"Hel"}}]}`,
		`{"choices":[{"delta":{"content":"lo"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`,
		`[DONE]`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range events {
			fmt.Fprintf(w, "data: %s\n\n", ev)
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEventStream(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := sseServer(t, &hits)
	path := filepath.Join(t.TempDir(), "stream.json")
	stream := func(rec *replay.Recorder) string {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"stream":true}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer sk-secret")
		resp, err := rec.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
			t.Fatalf("Content-Type = %q", ct)
		}
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	rec := replay.NewTest(t, path, replay.WithMode(replay.ModeRecord), replay.WithTransport(srv.Client().Transport))
	recorded := stream(rec)
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	c, err := replay.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	rs := c.Interactions[0].Response
	if len(rs.Events) != 4 || rs.Body != "" || rs.Events[0] != "data: {\"id\":\"r1\",\"model\":\"gpt-test\",\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n" ||
		c.Interactions[0].Request.Header.Get("Authorization") != replay.Redacted {
		t.Fatalf("recorded stream = %+v", c.Interactions[0])
	}

	served := hits.Load()
	rp := replay.NewTest(t, path, replay.WithMode(replay.ModeReplay))
	replayed := stream(rp)
	if hits.Load() != served {
		t.Fatal("replay reached the server")
	}
	if !strings.HasPrefix(recorded, "data: {\"id\":\"r1\"") || !strings.HasSuffix(recorded, "data: [DONE]\n\n") || replayed != recorded {
		t.Fatalf("replayed = %q, recorded %q", replayed, recorded)
	}
}

func TestEventBodyReads(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "events.json")
	c := replay.Cassette{Version: replay.CassetteVersion, Interactions: []*replay.Interaction{{
		Request:  replay.Request{Method: http.MethodGet, URL: "http://api.test/stream"},
		Response: replay.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, "Content-Length": {"99"}}, Events: []string{"data: one\n\n", "data: two\n\n"}},
	}, {
		Request:  replay.Request{Method: http.MethodGet, URL: "http://api.test/stream"},
		Response: replay.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Events: []string{"data: three\n\n"}},
	}}}
	writeCassette(t, path, &c)
	rp, err := replay.New(path, replay.WithMode(replay.ModeReplay))
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://api.test/stream", nil)
	resp, err := rp.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.ContentLength != -1 || resp.Header.Get("Content-Length") != "" {
		t.Fatalf("length = %d %q", resp.ContentLength, resp.Header.Get("Content-Length"))
	}
	buf := make([]byte, 64)
	n, err := resp.Body.Read(buf)
	if err != nil || string(buf[:n]) != "data: one\n\n" {
		t.Fatalf("first read = %q, %v", buf[:n], err)
	}
	small := make([]byte, 4)
	n, _ = resp.Body.Read(small)
	if string(small[:n]) != "data" {
		t.Fatalf("partial read = %q", small[:n])
	}
	rest, err := io.ReadAll(resp.Body)
	if err != nil || string(rest) != ": two\n\n" {
		t.Fatalf("rest = %q, %v", rest, err)
	}
	resp.Body.Close()

	// A canceled request stops the stream.
	ctx, cancel := context.WithCancel(t.Context())
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, "http://api.test/stream", nil)
	resp, err = rp.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := resp.Body.Read(buf); !errors.Is(err, context.Canceled) {
		t.Fatalf("read after cancel = %v", err)
	}
	if err := doErr(&http.Client{Transport: rp}, req); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled request = %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeCassette(t *testing.T, path string, c *replay.Cassette) {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestModes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(replay.EnvMode, "")
	if replay.ModeFromEnv() != replay.ModeReplay {
		t.Fatal("default mode")
	}
	t.Setenv(replay.EnvMode, " Record ")
	if replay.ModeFromEnv() != replay.ModeRecord {
		t.Fatal("mode from env")
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, "live")
	}))
	defer srv.Close()

	// The environment sets the mode.
	path := filepath.Join(dir, "env.json")
	rec, err := replay.New(path, replay.WithTransport(srv.Client().Transport))
	if err != nil || rec.Mode() != replay.ModeRecord {
		t.Fatalf("env mode = %v, %v", rec, err)
	}

	// Auto records a missing cassette and replays an existing one.
	path = filepath.Join(dir, "auto.json")
	rec, err = replay.New(path, replay.WithMode(replay.ModeAuto), replay.WithTransport(srv.Client().Transport))
	if err != nil || rec.Mode() != replay.ModeRecord {
		t.Fatalf("auto without cassette = %v, %v", rec, err)
	}
	if _, body := do(t, rec.Client(), http.MethodGet, srv.URL, "", nil); body != "live" {
		t.Fatalf("body = %q", body)
	}
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	rec, err = replay.New(path, replay.WithMode(replay.ModeAuto))
	if err != nil || rec.Mode() != replay.ModeReplay {
		t.Fatalf("auto with cassette = %v, %v", rec, err)
	}
	if _, body := do(t, rec.Client(), http.MethodGet, srv.URL, "", nil); body != "live" || hits.Load() != 1 {
		t.Fatalf("auto replay = %q, hits %d", body, hits.Load())
	}

	// Passthrough sends requests without recording.
	rec, err = replay.New(filepath.Join(dir, "none.json"), replay.WithMode(replay.ModePassthrough), replay.WithTransport(srv.Client().Transport))
	if err != nil {
		t.Fatal(err)
	}
	if _, body := do(t, rec.Client(), http.MethodGet, srv.URL, "", nil); body != "live" || hits.Load() != 2 || len(rec.Interactions()) != 0 {
		t.Fatalf("passthrough = %q", body)
	}
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "none.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("passthrough saved a cassette: %v", err)
	}

	if _, err := replay.New(path, replay.WithMode("rewind")); err == nil || !strings.Contains(err.Error(), `unknown mode "rewind"`) {
		t.Fatalf("unknown mode = %v", err)
	}
	if _, err := replay.New(filepath.Join(dir, "missing.json"), replay.WithMode(replay.ModeReplay)); err == nil || !strings.Contains(err.Error(), "AGENTX_REPLAY=record") {
		t.Fatalf("missing cassette = %v", err)
	}
	bad := filepath.Join(dir, "bad.json")
	writeCassette(t, bad, &replay.Cassette{Version: 99})
	if _, err := replay.New(bad, replay.WithMode(replay.ModeReplay)); err == nil || !strings.Contains(err.Error(), "version 99") {
		t.Fatalf("bad version = %v", err)
	}
	if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := replay.New(bad, replay.WithMode(replay.ModeReplay)); err == nil {
		t.Fatal("invalid cassette loaded")
	}
	rec, err = replay.New(filepath.Join(dir, "no", "such", "dir.json"), replay.WithMode(replay.ModeRecord))
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.Save(); err == nil {
		t.Fatal("Save into a missing directory succeeded")
	}
}

func TestFiltersAndMatchers(t *testing.T) {
	t.Parallel()
	binary := []byte{0x1f, 0x8b, 0xff, 0x00}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bin" {
			w.Header().Set("Content-Length", "4")
			_, _ = w.Write(binary)
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Custom", "private")
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "filters.json")
	// The filter drops a volatile field from request bodies.
	filter := func(in *replay.Interaction) {
		var m map[string]any
		if json.Unmarshal(in.Request.BodyBytes(), &m) == nil {
			delete(m, "nonce")
			b, _ := json.Marshal(m)
			in.Request.SetBody(b)
		}
	}
	opts := []replay.Option{replay.WithFilter(filter), replay.WithRedactHeaders("x-custom"), replay.WithRedactQuery("token")}
	rec, err := replay.New(path, append(opts, replay.WithMode(replay.ModeRecord), replay.WithTransport(srv.Client().Transport))...)
	if err != nil {
		t.Fatal(err)
	}
	_, got := do(t, rec.Client(), http.MethodPost, srv.URL+"/echo?token=t1", `{"q":"x","nonce":1}`, nil)
	if got != `{"q":"x","nonce":1}` {
		t.Fatalf("echo = %s", got)
	}
	_, got = do(t, rec.Client(), http.MethodGet, srv.URL+"/bin", "", nil)
	if got != string(binary) {
		t.Fatalf("binary = %q", got)
	}
	ins := rec.Interactions()
	if len(ins) != 2 || ins[0].Request.Body != `{"q":"x"}` || ins[0].Response.Header.Get("X-Custom") != replay.Redacted ||
		!strings.Contains(ins[0].Request.URL, "token=REDACTED") || ins[1].Response.BodyBase64 == nil || ins[1].Response.Body != "" {
		t.Fatalf("interactions = %+v %+v", ins[0], ins[1])
	}
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}

	rp, err := replay.New(path, append(opts, replay.WithMode(replay.ModeReplay))...)
	if err != nil {
		t.Fatal(err)
	}
	if _, got = do(t, rp.Client(), http.MethodPost, srv.URL+"/echo?token=t2", `{"q":"x","nonce":2}`, nil); got != `{"q":"x","nonce":1}` {
		t.Fatalf("filtered replay = %s", got)
	}
	resp, got := do(t, rp.Client(), http.MethodGet, srv.URL+"/bin", "", nil)
	if got != string(binary) || resp.Header.Get("Content-Length") != "4" {
		t.Fatalf("binary replay = %q", got)
	}

	// MatchMethodURL ignores bodies.
	rp, err = replay.New(path, replay.WithMode(replay.ModeReplay), replay.WithMatcher(replay.MatchMethodURL), replay.WithRedactQuery("token"))
	if err != nil {
		t.Fatal(err)
	}
	if _, got = do(t, rp.Client(), http.MethodPost, srv.URL+"/echo?token=t3", `anything`, nil); got != `{"q":"x","nonce":1}` {
		t.Fatalf("MatchMethodURL = %s", got)
	}
}

func TestMatchers(t *testing.T) {
	t.Parallel()
	r := func(method, u, body string) *replay.Request {
		return &replay.Request{Method: method, URL: u, Body: body}
	}
	for i, tc := range []struct {
		a, b *replay.Request
		want bool
	}{
		{r("GET", "http://x/a?b=1&a=2", ""), r("GET", "http://x/a?a=2&b=1", ""), true},
		{r("GET", "http://x/a?a=1", ""), r("GET", "http://x/a?a=2", ""), false},
		{r("GET", "http://x/a", ""), r("POST", "http://x/a", ""), false},
		{r("GET", "http://x/a", ""), r("GET", "http://x/b", ""), false},
		{r("GET", "%zz", ""), r("GET", "http://x/b", ""), false},
		{r("POST", "http://x", `{"a":1}`), r("POST", "http://x", `{ "a" : 1 }`), true},
		{r("POST", "http://x", `text`), r("POST", "http://x", `text `), false},
	} {
		if got := replay.DefaultMatcher(tc.a, tc.b); got != tc.want {
			t.Errorf("%d: DefaultMatcher = %v", i, got)
		}
	}
	req := replay.Request{}
	req.SetBody([]byte{0xff})
	if req.Body != "" || string(req.BodyBytes()) != "\xff" {
		t.Fatalf("binary request body = %+v", req)
	}
	var resp replay.Response
	resp.Header = http.Header{"Content-Type": {"text/event-stream; charset=utf-8"}}
	resp.SetBody([]byte("data: a\r\n\r\ndata: b\r\n\r\npartial"))
	if len(resp.Events) != 3 || resp.Events[2] != "partial" || string(resp.BodyBytes()) != "data: a\r\n\r\ndata: b\r\n\r\npartial" {
		t.Fatalf("CRLF events = %q", resp.Events)
	}
	resp.SetBody(nil)
	if resp.Events != nil || resp.Body != "" {
		t.Fatalf("empty stream = %+v", resp)
	}
}

func TestPartialAndConcurrent(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.Repeat("x", 1000)+r.URL.Query().Get("n"))
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "partial.json")
	rec, err := replay.New(path, replay.WithMode(replay.ModeRecord), replay.WithTransport(srv.Client().Transport))
	if err != nil {
		t.Fatal(err)
	}
	// A body closed early is saved as far as it was read.
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"?n=partial", nil)
	resp, err := rec.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(resp.Body, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if ins := rec.Interactions(); len(ins) != 1 || ins[0].Response.Body != strings.Repeat("x", 10) {
		t.Fatalf("partial = %+v", ins)
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("%s?n=%d", srv.URL, i), nil)
			resp, err := rec.Client().Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			_, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	rp, err := replay.New(path, replay.WithMode(replay.ModeReplay))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		wg.Go(func() {
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("%s?n=%d", srv.URL, i), nil)
			resp, err := rp.Client().Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			b, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Error(err)
				return
			}
			if want := strings.Repeat("x", 1000) + fmt.Sprint(i); string(b) != want {
				t.Errorf("request %d: body = %q, want %q", i, b, want)
			}
		})
	}
	wg.Wait()
	if n := len(rp.Unused()); n != 1 {
		t.Fatalf("unused = %d", n)
	}
	// The transport's errors are returned.
	failing, err := replay.New(path, replay.WithMode(replay.ModeRecord), replay.WithTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed")
	})))
	if err != nil {
		t.Fatal(err)
	}
	req, _ = http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("x"))
	if err := doErr(&http.Client{Transport: failing}, req); err == nil || len(failing.Interactions()) != 0 {
		t.Fatalf("failing transport = %v", err)
	}
	req, _ = http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, errReader{})
	if err := doErr(&http.Client{Transport: failing}, req); err == nil || !strings.Contains(err.Error(), "read the request body") {
		t.Fatalf("unreadable body = %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("broken body") }

// fakeTB runs the cleanups of NewTest and records its failures.
type fakeTB struct {
	testing.TB
	cleanups []func()
	errors   []string
	failed   bool
}

func (f *fakeTB) Helper()           {}
func (f *fakeTB) Cleanup(fn func()) { f.cleanups = append(f.cleanups, fn) }
func (f *fakeTB) Failed() bool      { return f.failed }
func (f *fakeTB) Error(args ...any) {
	f.errors = append(f.errors, fmt.Sprint(args...))
}
func (f *fakeTB) Errorf(format string, args ...any) {
	f.errors = append(f.errors, fmt.Sprintf(format, args...))
}
func (f *fakeTB) Fatal(args ...any) { f.Error(args...); f.failed = true }

func (f *fakeTB) finish() {
	for _, fn := range slices.Backward(f.cleanups) {
		fn()
	}
}

func TestNewTest(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") }))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "test.json")

	// A failed recording keeps no cassette.
	tb := &fakeTB{}
	rec := replay.NewTest(tb, path, replay.WithMode(replay.ModeRecord), replay.WithTransport(srv.Client().Transport))
	do(t, rec.Client(), http.MethodGet, srv.URL+"/a", "", nil)
	tb.failed = true
	tb.finish()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed recording saved: %v", err)
	}

	tb = &fakeTB{}
	rec = replay.NewTest(tb, path, replay.WithMode(replay.ModeRecord), replay.WithTransport(srv.Client().Transport))
	do(t, rec.Client(), http.MethodGet, srv.URL+"/a", "", nil)
	do(t, rec.Client(), http.MethodGet, srv.URL+"/b", "", nil)
	tb.finish()
	if len(tb.errors) != 0 {
		t.Fatalf("record = %v", tb.errors)
	}

	tb = &fakeTB{}
	rp := replay.NewTest(tb, path, replay.WithMode(replay.ModeReplay))
	do(t, rp.Client(), http.MethodGet, srv.URL+"/a", "", nil)
	tb.finish()
	if len(tb.errors) != 1 || !strings.Contains(tb.errors[0], "1 interactions of") {
		t.Fatalf("unused = %v", tb.errors)
	}

	tb = &fakeTB{}
	if replay.NewTest(tb, filepath.Join(t.TempDir(), "missing.json"), replay.WithMode(replay.ModeReplay)); !tb.failed {
		t.Fatal("missing cassette did not fail the test")
	}
}
