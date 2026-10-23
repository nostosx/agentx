package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/session"
	"github.com/nostosx/agentx/tool"
)

var update = flag.Bool("update", false, "update golden files")

// golden compares got with testdata/name, or writes it with -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden file (run go test -update): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file; run go test ./server -update and review the diff\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// sseEvent is a parsed Server-Sent Event.
type sseEvent struct {
	ID, Event, Data string
}

// parseSSE parses a stream of Server-Sent Events, returning the events and
// the number of comment lines.
func parseSSE(t *testing.T, r io.Reader) (events []sseEvent, comments int) {
	t.Helper()
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1<<20)
	var cur sseEvent
	var data []string
	has := false
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if has {
				cur.Data = strings.Join(data, "\n")
				events = append(events, cur)
			}
			cur, data, has = sseEvent{}, nil, false
		case strings.HasPrefix(line, ":"):
			comments++
		default:
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			has = true
			switch field {
			case "id":
				cur.ID = value
			case "event":
				cur.Event = value
			case "data":
				data = append(data, value)
			default:
				t.Fatalf("unexpected SSE field %q", field)
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatalf("stream ended inside an event")
	}
	return events, comments
}

// fields decodes the data of each event as a JSON object.
func fields(t *testing.T, events []sseEvent) []map[string]any {
	t.Helper()
	out := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		if ev.Data == "[DONE]" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ev.Data), &m); err != nil {
			t.Fatalf("event data %q: %v", ev.Data, err)
		}
		out = append(out, m)
	}
	return out
}

func types(objs []map[string]any) []string {
	var out []string
	for _, o := range objs {
		out = append(out, o["type"].(string))
	}
	return out
}

// deployCalls counts the runs of the deploy tool.
type deployCalls struct{ n atomic.Int32 }

func (d *deployCalls) tool() tool.Tool {
	return tool.Func("deploy", "Deploy a build.", func(_ context.Context, in struct {
		Env string `json:"env"`
	}) (string, error) {
		d.n.Add(1)
		return "deployed to " + in.Env, nil
	}, tool.RequireApproval())
}

// fixture is a server around an agent with a scripted model.
type fixture struct {
	t      *testing.T
	model  *modeltest.Model
	cp     *loop.MemoryCheckpointer
	runner *agent.Runner
	server *Server
	// http serves the server on an in-memory network; send requests with
	// client.
	http   *httptest.Server
	client *http.Client
	deploy *deployCalls
}

func newFixture(t *testing.T, steps []modeltest.Step, agentOpts []agent.Option, opts ...Option) *fixture {
	t.Helper()
	f := &fixture{t: t, model: modeltest.New(steps...), cp: loop.NewMemoryCheckpointer(), deploy: &deployCalls{}}
	base := []agent.Option{
		agent.WithModel(f.model),
		agent.WithTools(f.deploy.tool()),
		agent.WithCheckpointer(f.cp),
		agent.WithStreaming(),
	}
	a, err := agent.New("chat", append(base, agentOpts...)...)
	if err != nil {
		t.Fatal(err)
	}
	f.runner = agent.NewRunner(a, nil)
	f.server = New(f.runner, append([]Option{WithHeartbeat(0)}, opts...)...)
	f.http = httptest.NewTestServer(t, f.server)
	f.client = f.http.Client()
	t.Cleanup(func() { _ = f.server.Close() })
	return f
}

// do sends a request with a JSON body.
func (f *fixture) do(method, path, accept string, body any) *response {
	f.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(f.t.Context(), method, f.http.URL+path, rd)
	if err != nil {
		f.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	return f.roundTrip(req)
}

// stream sends a request and parses its event stream.
func (f *fixture) stream(path string, body any) (http.Header, []map[string]any, []sseEvent) {
	f.t.Helper()
	resp := f.do(http.MethodPost, path, "text/event-stream", body)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		f.t.Fatalf("POST %s: %s: %s", path, resp.Status, b)
	}
	events, _ := parseSSE(f.t, resp.Body)
	return resp.Header, fields(f.t, events), events
}

// decodeJSON decodes a JSON response body.
func decodeJSON[T any](t *testing.T, resp *response) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func errorCode(t *testing.T, resp *response) string {
	t.Helper()
	body := decodeJSON[errorBody](t, resp)
	if body.Error == nil {
		t.Fatalf("no error in body")
	}
	return body.Error.Code
}

// deployStep asks to deploy to env.
func deployStep(id, env string) modeltest.Step {
	return modeltest.CallTools(modeltest.Call{ID: id, Name: "deploy", Args: map[string]string{"env": env}})
}

// blockingModel is a model whose calls wait for release or cancellation.
func blockingModel(started chan<- struct{}, release <-chan struct{}) model.Model {
	return model.Func(func(ctx context.Context, _ *model.Request) (*model.Response, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return &model.Response{Message: model.AssistantText("released"), FinishReason: model.FinishStop}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
}

// saveSession saves an empty session.
func saveSession(t *testing.T, f *fixture, id string) {
	t.Helper()
	sess, _, err := session.GetOrCreate(t.Context(), f.runner.Store(), id)
	if err == nil {
		err = f.runner.Store().Save(t.Context(), sess)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// response is a response whose body was read.
type response struct {
	StatusCode int
	Status     string
	Header     http.Header
	Body       *bytes.Reader
}

// roundTrip sends a request to the server and reads its response.
func (f *fixture) roundTrip(req *http.Request) *response {
	t := f.t
	t.Helper()
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return &response{StatusCode: resp.StatusCode, Status: resp.Status, Header: resp.Header, Body: bytes.NewReader(b)}
}
