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
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
)

// fields returns the params of a message by name.
func (m wireMessage) fields() map[string]json.RawMessage {
	var p map[string]json.RawMessage
	_ = json.Unmarshal(m.Params, &p)
	return p
}

// meta returns the _meta of a request's params.
func (m wireMessage) meta() map[string]json.RawMessage {
	var meta map[string]json.RawMessage
	_ = json.Unmarshal(m.fields()["_meta"], &meta)
	return meta
}

// modernHandler answers a request of ProtocolVersion other than
// server/discover with notifications and a response, or with nothing if
// resp is empty.
type modernHandler func(m wireMessage) (notes []string, resp string)

// modernClient connects to a fake server of ProtocolVersion over stdio or
// Streamable HTTP.
func modernClient(t *testing.T, transport string, handle modernHandler, opts ...ClientOption) *Client {
	t.Helper()
	var tr Transport
	switch transport {
	case "stdio":
		tr = pipeServer(t, func(m wireMessage, write func(string)) {
			switch {
			case m.Method == "server/discover":
				write(result(m.ID, discoverResult))
			case m.Method != "" && len(m.ID) > 0:
				go func() {
					notes, resp := handle(m)
					for _, n := range notes {
						write(n)
					}
					if resp != "" {
						write(resp)
					}
				}()
			}
		})
	case "http":
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var m wireMessage
			if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if got := r.Header.Get("Mcp-Method"); got != m.Method {
				t.Errorf("Mcp-Method = %q, want %q", got, m.Method)
			}
			if m.Method == "server/discover" {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, result(m.ID, discoverResult))
				return
			}
			notes, resp := handle(m)
			if resp == "" {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			for _, n := range append(notes, resp) {
				fmt.Fprintf(w, "data: %s\n\n", n)
			}
		}))
		t.Cleanup(s.Close)
		tr = HTTP(s.URL)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, err := Connect(ctx, tr, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

var transports = []string{"stdio", "http"}

const (
	samplingRequest = `{"method":"sampling/createMessage","params":{"messages":[{"role":"user","content":{"type":"text","text":"Capital of France?"}}],` +
		`"systemPrompt":"Be brief.","maxTokens":50,"tools":[{"name":"lookup","inputSchema":{"type":"object"}}],"toolChoice":{"mode":"auto"}}}`
	formRequest = `{"method":"elicitation/create","params":{"mode":"form","message":"Who are you?","requestedSchema":` +
		`{"type":"object","properties":{"name":{"type":"string","minLength":1}},"required":["name"]}}}`
	urlRequest   = `{"method":"elicitation/create","params":{"mode":"url","message":"Connect","url":"https://example.com/connect"}}`
	rootsRequest = `{"method":"roots/list"}`
)

func TestInputRequestsAreFulfilled(t *testing.T) {
	for _, transport := range transports {
		t.Run(transport, func(t *testing.T) {
			var round atomic.Int64
			var ids sync.Map
			handle := func(m wireMessage) ([]string, string) {
				if m.Method != "tools/call" {
					return nil, rpcError(m.ID, CodeMethodNotFound, m.Method)
				}
				if _, dup := ids.LoadOrStore(string(m.ID), true); dup {
					t.Errorf("request ID %s reused", m.ID)
				}
				f := m.fields()
				var responses map[string]json.RawMessage
				_ = json.Unmarshal(f["inputResponses"], &responses)
				state, hasState := f["requestState"]
				switch round.Add(1) {
				case 1:
					var caps map[string]json.RawMessage
					_ = json.Unmarshal(m.meta()[metaClientCapabilities], &caps)
					if string(caps["sampling"]) != `{"tools":{}}` || string(caps["elicitation"]) != `{"form":{},"url":{}}` ||
						string(caps["roots"]) != `{}` {
						t.Errorf("clientCapabilities = %s", m.meta()[metaClientCapabilities])
					}
					if responses != nil || hasState {
						t.Errorf("first request carries %s", m.Params)
					}
					return nil, result(m.ID, `{"resultType":"input_required","requestState":"st1","inputRequests":{`+
						`"s":`+samplingRequest+`,"f":`+formRequest+`,"u":`+urlRequest+`,"r":`+rootsRequest+`}}`)
				case 2:
					if string(state) != `"st1"` {
						t.Errorf("requestState = %s", state)
					}
					for key, want := range map[string]string{
						"s": `{"role":"assistant","content":{"type":"text","text":"Paris"},"model":"m1","stopReason":"endTurn"}`,
						"f": `{"action":"accept","content":{"name":"octocat"}}`,
						"u": `{"action":"accept"}`,
						"r": `{"roots":[{"uri":"file:///work","name":"work"}]}`,
					} {
						if got := string(responses[key]); got != want {
							t.Errorf("inputResponses[%s] = %s, want %s", key, got, want)
						}
					}
					return nil, result(m.ID, `{"resultType":"input_required","requestState":"st2"}`)
				case 3:
					// Responses and state apply to one retry only.
					if responses != nil || string(state) != `"st2"` {
						t.Errorf("third request params = %s", m.Params)
					}
					return nil, result(m.ID, `{"resultType":"input_required","inputRequests":{"f2":`+formRequest+`}}`)
				default:
					if hasState || len(responses) != 1 || responses["f2"] == nil {
						t.Errorf("last request params = %s", m.Params)
					}
					return nil, result(m.ID, `{"resultType":"complete","content":[{"type":"text","text":"done"}]}`)
				}
			}
			m := modeltest.New(modeltest.Respond(&model.Response{Model: "m1", Message: model.AssistantText("Paris"),
				FinishReason: model.FinishStop}))
			var approved, reviewed atomic.Int64
			var urls []string
			c := modernClient(t, transport, handle,
				WithSampling(ModelSampler(m), SamplingTools(),
					ApproveSampling(func(_ context.Context, req *SamplingRequest) error {
						approved.Add(1)
						if req.MaxTokens != 50 || len(req.Tools) != 1 {
							t.Errorf("sampling request = %+v", req)
						}
						return nil
					}),
					ReviewSampling(func(context.Context, *SamplingRequest, *SamplingResult) error {
						reviewed.Add(1)
						return nil
					})),
				WithElicitation(func(_ context.Context, req *ElicitRequest) (*ElicitResult, error) {
					if req.Mode == ElicitationURL {
						urls = append(urls, req.URL)
						return &ElicitResult{Action: ElicitAccept, Content: map[string]any{"ignored": true}}, nil
					}
					return &ElicitResult{Action: ElicitAccept, Content: map[string]any{"name": "octocat"}}, nil
				}, ElicitationURL, ElicitationForm),
				WithRoots(StaticRoots(Root{URI: "file:///work", Name: "work"})))
			res, err := c.CallTool(t.Context(), "deploy", nil)
			if err != nil {
				t.Fatal(err)
			}
			if FormatContent(res) != "done" || round.Load() != 4 {
				t.Errorf("res = %q after %d rounds", FormatContent(res), round.Load())
			}
			if approved.Load() != 1 || reviewed.Load() != 1 || !slices.Equal(urls, []string{"https://example.com/connect"}) {
				t.Errorf("approved %d, reviewed %d, urls %v", approved.Load(), reviewed.Load(), urls)
			}
			req := m.Requests()[0]
			if req.System != "Be brief." || req.MaxTokens != 50 || len(req.Tools) != 1 || req.Messages[0].Text() != "Capital of France?" {
				t.Errorf("model request = %+v", req)
			}
		})
	}
}

func TestInputRequestsWithoutHandler(t *testing.T) {
	for _, transport := range transports {
		t.Run(transport, func(t *testing.T) {
			sampled := false
			c := modernClient(t, transport, func(m wireMessage) ([]string, string) {
				return nil, result(m.ID, `{"resultType":"input_required","requestState":"x","inputRequests":{"s":`+
					samplingRequest+`,"f":`+formRequest+`}}`)
			}, WithSampling(func(context.Context, *SamplingRequest) (*SamplingResult, error) {
				sampled = true
				return &SamplingResult{}, nil
			}, SamplingTools()))
			var ie *InputRequiredError
			if _, err := c.CallTool(t.Context(), "x", nil); !errors.As(err, &ie) || ie.RequestState != "x" || len(ie.InputRequests) != 2 {
				t.Fatalf("err = %v", err)
			}
			if sampled {
				t.Error("handler ran although another input request cannot be fulfilled")
			}
		})
	}
}

func TestInputRequestFailures(t *testing.T) {
	inputRequired := func(requests string) modernHandler {
		return func(m wireMessage) ([]string, string) {
			if m.fields()["inputResponses"] != nil {
				t.Errorf("retried after a failed input request: %s", m.Params)
			}
			return nil, result(m.ID, `{"resultType":"input_required","inputRequests":{"a":`+requests+`}}`)
		}
	}
	accept := func(context.Context, *ElicitRequest) (*ElicitResult, error) {
		return &ElicitResult{Action: ElicitAccept, Content: map[string]any{"name": ""}}, nil
	}
	sampler := func(context.Context, *SamplingRequest) (*SamplingResult, error) {
		return &SamplingResult{Content: SamplingContents{{Type: "text", Text: "x"}}}, nil
	}
	for name, tc := range map[string]struct {
		requests string
		opts     []ClientOption
		check    func(error) bool
	}{
		"sampling rejected": {samplingRequest, []ClientOption{WithSampling(sampler, SamplingTools(),
			ApproveSampling(func(context.Context, *SamplingRequest) error { return errors.New("no") }))},
			func(err error) bool { return errors.Is(err, ErrSamplingRejected) }},
		"sampling tools not declared": {samplingRequest, []ClientOption{WithSampling(sampler)},
			func(err error) bool { var e *Error; return errors.As(err, &e) && e.Code == CodeInvalidParams }},
		"form content invalid": {formRequest, []ClientOption{WithElicitation(accept)},
			func(err error) bool { return err != nil && strings.Contains(err.Error(), "length out of range") }},
		"url mode not declared": {urlRequest, []ClientOption{WithElicitation(accept)},
			func(err error) bool { var e *Error; return errors.As(err, &e) && e.Code == CodeInvalidParams }},
		"root not a file URI": {rootsRequest, []ClientOption{WithRoots(StaticRoots(Root{URI: "https://x"}))},
			func(err error) bool { return err != nil && strings.Contains(err.Error(), "file://") }},
		"tool results mixed with text": {`{"method":"sampling/createMessage","params":{"maxTokens":1,"messages":[{"role":"user","content":[` +
			`{"type":"text","text":"x"},{"type":"tool_result","toolUseId":"c","content":[]}]}]}}`, []ClientOption{WithSampling(sampler)},
			func(err error) bool { var e *Error; return errors.As(err, &e) && e.Code == CodeInvalidParams }},
		"invalid elicitation action": {formRequest, []ClientOption{WithElicitation(func(context.Context, *ElicitRequest) (*ElicitResult, error) {
			return &ElicitResult{Action: "maybe"}, nil
		})}, func(err error) bool { return err != nil && strings.Contains(err.Error(), "invalid elicitation action") }},
	} {
		t.Run(name, func(t *testing.T) {
			c := modernClient(t, "stdio", inputRequired(tc.requests), tc.opts...)
			if _, err := c.CallTool(t.Context(), "x", nil); !tc.check(err) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestModernLoggingAndPing(t *testing.T) {
	for _, transport := range transports {
		t.Run(transport, func(t *testing.T) {
			var levels []string
			var mu sync.Mutex
			c := modernClient(t, transport, func(m wireMessage) ([]string, string) {
				mu.Lock()
				levels = append(levels, string(m.meta()[metaLogLevel]))
				mu.Unlock()
				note := `{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"warning","logger":"db","data":{"msg":"slow"}}}`
				return []string{note}, result(m.ID, `{"resultType":"complete","tools":[]}`)
			}, WithLogHandler(func(m LogMessage) {
				mu.Lock()
				defer mu.Unlock()
				if m.Level != LogWarning || m.Logger != "db" || string(m.Data) != `{"msg":"slow"}` {
					t.Errorf("log message = %+v", m)
				}
				levels = append(levels, "logged")
			}))
			if _, err := c.ListTools(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := c.SetLogLevel(t.Context(), LogInfo); err != nil {
				t.Fatal(err)
			}
			if _, err := c.ListTools(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := c.SetLogLevel(t.Context(), "verbose"); err == nil {
				t.Error("invalid level accepted")
			}
			mu.Lock()
			if !slices.Equal(levels, []string{"", "logged", `"info"`, "logged"}) {
				t.Errorf("levels = %q", levels)
			}
			mu.Unlock()
			if err := c.Ping(t.Context()); err != nil {
				t.Fatal(err)
			}
			// Roots need no notification with ProtocolVersion.
			if err := c.RootsChanged(t.Context()); !errors.Is(err, errors.ErrUnsupported) {
				t.Errorf("RootsChanged without roots = %v", err)
			}
		})
	}
}

func TestOptionValidation(t *testing.T) {
	for name, opt := range map[string]ClientOption{
		"log level":        WithLogLevel("loud"),
		"elicitation mode": WithElicitation(func(context.Context, *ElicitRequest) (*ElicitResult, error) { return nil, nil }, "popup"),
		"nil sampler":      WithSampling(nil),
		"nil elicitation":  WithElicitation(nil),
	} {
		if _, err := Connect(t.Context(), pipeServer(t, func(wireMessage, func(string)) {}), opt); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// TestLegacyClientFeatures checks the client features with a server of
// revision 2025-11-25, which sends requests to the client.
func TestLegacyClientFeatures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newLegacyStdio(t)
		s.caps = `{"tools":{},"logging":{}}`
		requests := map[string]string{
			"sample": samplingRequest, "form": formRequest, "url": urlRequest, "roots": rootsRequest,
			"badmode": `{"method":"elicitation/create","params":{"mode":"popup","message":"?"}}`,
			"unknown": `{"method":"tasks/list"}`,
		}
		s.handle = func(m wireMessage, write func(string)) {
			switch m.Method {
			case "tools/call":
				for id, r := range requests {
					write(`{"jsonrpc":"2.0","id":"` + id + `",` + r[1:])
				}
				write(result(m.ID, `{"content":[{"type":"text","text":"ok"}]}`))
			case "logging/setLevel":
				if m.param("level") != "debug" {
					t.Errorf("level = %q", m.param("level"))
				}
				write(result(m.ID, `{}`))
				write(`{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"debug","data":"hello"}}`)
			case "ping":
				write(result(m.ID, `{}`))
			}
		}
		logs := make(chan LogMessage, 1)
		var modes []string
		var mu sync.Mutex
		c, err := s.connect(t,
			WithSampling(func(context.Context, *SamplingRequest) (*SamplingResult, error) {
				return &SamplingResult{Content: SamplingContents{{Type: "text", Text: "Paris"}}, Model: "m", StopReason: StopEndTurn}, nil
			}, SamplingTools()),
			WithElicitation(func(_ context.Context, req *ElicitRequest) (*ElicitResult, error) {
				mu.Lock()
				modes = append(modes, req.Mode)
				mu.Unlock()
				return &ElicitResult{Action: ElicitDecline, Content: map[string]any{"dropped": 1}}, nil
			}, ElicitationForm, ElicitationURL),
			WithRoots(StaticRoots(Root{URI: "file:///a"})),
			WithLogHandler(func(m LogMessage) { logs <- m }))
		if err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		received := slices.Clone(s.received)
		s.mu.Unlock()
		for _, m := range received {
			if m.Method == "initialize" {
				caps := string(m.fields()["capabilities"])
				if caps != `{"elicitation":{"form":{},"url":{}},"roots":{"listChanged":true},"sampling":{"tools":{}}}` {
					t.Errorf("initialize capabilities = %s", caps)
				}
			}
		}
		if _, err := c.CallTool(t.Context(), "x", nil); err != nil {
			t.Fatal(err)
		}
		answers := map[string]wireMessage{}
		for range requests {
			select {
			case m := <-s.responses:
				answers[idKey(m.ID)] = m
			case <-time.After(5 * time.Second):
				t.Fatalf("answers = %v", answers)
			}
		}
		for id, want := range map[string]string{
			"sample": `{"role":"assistant","content":{"type":"text","text":"Paris"},"model":"m","stopReason":"endTurn"}`,
			"form":   `{"action":"decline"}`,
			"url":    `{"action":"decline"}`,
			"roots":  `{"roots":[{"uri":"file:///a"}]}`,
		} {
			if got := string(answers[id].Result); got != want {
				t.Errorf("%s answer = %s, want %s", id, got, want)
			}
		}
		if e := answers["badmode"].Error; e == nil || e.Code != CodeInvalidParams {
			t.Errorf("unsupported mode answer = %+v", answers["badmode"])
		}
		if e := answers["unknown"].Error; e == nil || e.Code != CodeMethodNotFound {
			t.Errorf("unknown method answer = %+v", answers["unknown"])
		}
		mu.Lock()
		slices.Sort(modes)
		if !slices.Equal(modes, []string{ElicitationForm, ElicitationURL}) {
			t.Errorf("elicitation modes = %v", modes)
		}
		mu.Unlock()

		if err := c.SetLogLevel(t.Context(), LogDebug); err != nil {
			t.Fatal(err)
		}
		if m := <-logs; m.Level != LogDebug || string(m.Data) != `"hello"` {
			t.Errorf("log = %+v", m)
		}
		if err := c.Ping(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := c.RootsChanged(t.Context()); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { return slices.Contains(s.methods(), "notifications/roots/list_changed") })
		if !slices.Contains(s.methods(), "ping") {
			t.Errorf("methods = %v", s.methods())
		}
	})
}

// waitFor polls cond for up to 5 seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestLegacyURLElicitationRequired(t *testing.T) {
	urlError := func(id json.RawMessage) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32042,"message":"Authorization required",`+
			`"data":{"elicitations":[{"mode":"url","elicitationId":"el-1","url":"https://example.com/auth","message":"Sign in"}]}}}`, id)
	}
	for name, action := range map[string]string{"accepted": ElicitAccept, "declined": ElicitDecline} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newLegacyStdio(t)
				var calls atomic.Int64
				var write func(string)
				var wmu sync.Mutex
				s.handle = func(m wireMessage, w func(string)) {
					if m.Method != "tools/call" {
						return
					}
					wmu.Lock()
					write = w
					wmu.Unlock()
					if calls.Add(1) == 1 {
						w(urlError(m.ID))
						return
					}
					w(result(m.ID, `{"content":[{"type":"text","text":"authorized"}]}`))
				}
				consented := make(chan string, 1)
				c, err := s.connect(t, WithElicitation(func(_ context.Context, req *ElicitRequest) (*ElicitResult, error) {
					consented <- req.ElicitationID + " " + req.URL
					return &ElicitResult{Action: action}, nil
				}, ElicitationURL))
				if err != nil {
					t.Fatal(err)
				}
				completed := make(chan struct{})
				go func() {
					defer close(completed)
					if got := <-consented; got != "el-1 https://example.com/auth" {
						t.Errorf("elicitation = %q", got)
					}
					wmu.Lock()
					defer wmu.Unlock()
					// Unknown IDs are ignored.
					write(`{"jsonrpc":"2.0","method":"notifications/elicitation/complete","params":{"elicitationId":"other"}}`)
					write(`{"jsonrpc":"2.0","method":"notifications/elicitation/complete","params":{"elicitationId":"el-1"}}`)
				}()
				res, err := c.CallTool(t.Context(), "x", nil)
				<-completed
				if action == ElicitDecline {
					var e *Error
					if !errors.As(err, &e) || e.Code != CodeURLElicitationRequired || calls.Load() != 1 {
						t.Fatalf("err = %v after %d calls", err, calls.Load())
					}
					return
				}
				if err != nil || FormatContent(res) != "authorized" || calls.Load() != 2 {
					t.Fatalf("res = %+v, err = %v after %d calls", res, err, calls.Load())
				}
			})
		})
	}
	t.Run("waiting honors the context", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := newLegacyStdio(t)
			s.handle = func(m wireMessage, w func(string)) {
				if m.Method == "tools/call" {
					w(urlError(m.ID))
				}
			}
			c, err := s.connect(t, WithElicitation(func(context.Context, *ElicitRequest) (*ElicitResult, error) {
				return &ElicitResult{Action: ElicitAccept}, nil
			}, ElicitationURL))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			if _, err := c.CallTool(ctx, "x", nil); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err = %v", err)
			}
		})
	})
}

func TestLegacyServerCancelsRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newLegacyStdio(t)
		s.handle = func(m wireMessage, w func(string)) {
			if m.Method == "tools/call" {
				w(`{"jsonrpc":"2.0","id":"e1","method":"elicitation/create","params":{"message":"?","requestedSchema":{"type":"object","properties":{}}}}`)
				w(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"e1","reason":"timeout"}}`)
				w(`{"jsonrpc":"2.0","id":"p1","method":"ping"}`)
				w(result(m.ID, `{"content":[]}`))
			}
		}
		cancelled := make(chan struct{})
		c, err := s.connect(t, WithElicitation(func(ctx context.Context, _ *ElicitRequest) (*ElicitResult, error) {
			<-ctx.Done()
			close(cancelled)
			return &ElicitResult{Action: ElicitCancel}, nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.CallTool(t.Context(), "x", nil); err != nil {
			t.Fatal(err)
		}
		select {
		case <-cancelled:
		case <-time.After(5 * time.Second):
			t.Fatal("handler not cancelled")
		}
		synctest.Wait()
		// Only the ping is answered; a cancelled request is not.
		if m := <-s.responses; idKey(m.ID) != "p1" {
			t.Errorf("answered %s", m.ID)
		}
		select {
		case m := <-s.responses:
			t.Errorf("cancelled request answered: %+v", m)
		default:
		}
	})
}

func TestLegacyLogLevel(t *testing.T) {
	t.Run("without the logging capability", func(t *testing.T) {
		s := newLegacyStdio(t)
		c, err := s.connect(t, WithLogLevel(LogError))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.SetLogLevel(t.Context(), LogInfo); !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("SetLogLevel = %v", err)
		}
		if slices.Contains(s.methods(), "logging/setLevel") {
			t.Errorf("methods = %v", s.methods())
		}
	})
	t.Run("renewed sessions get the level", func(t *testing.T) {
		s := newLegacyHTTP(t)
		s.caps = `{"tools":{},"logging":{}}`
		var levels []string
		var mu sync.Mutex
		s.handle = func(m wireMessage) string {
			if m.Method == "logging/setLevel" {
				mu.Lock()
				levels = append(levels, m.param("level"))
				mu.Unlock()
			}
			return result(m.ID, `{"tools":[]}`)
		}
		c := connectLegacyHTTP(t, s.start(), WithLogLevel(LogWarning))
		s.expireAll()
		if _, err := c.ListTools(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := c.SetLogLevel(t.Context(), LogDebug); err != nil {
			t.Fatal(err)
		}
		s.expireAll()
		if err := c.Ping(t.Context()); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		if !slices.Equal(levels, []string{"warning", "warning", "debug", "debug"}) {
			t.Errorf("levels = %v", levels)
		}
	})
}

func TestLegacyHTTPServerRequests(t *testing.T) {
	s := newLegacyHTTP(t)
	s.handle = func(m wireMessage) string {
		if m.Method != "tools/call" {
			return ""
		}
		// A sampling request on the response stream, before the response.
		return `{"jsonrpc":"2.0","id":"s1","method":"sampling/createMessage","params":{"messages":[],"maxTokens":5}}` +
			"\n\nid: 3\ndata: " + result(m.ID, `{"content":[]}`)
	}
	c := connectLegacyHTTP(t, s.start(), WithSampling(func(context.Context, *SamplingRequest) (*SamplingResult, error) {
		return &SamplingResult{Content: SamplingContents{{Type: "text", Text: "hi"}}, Model: "m"}, nil
	}))
	if _, err := c.CallTool(t.Context(), "x", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-s.responses:
		if idKey(m.ID) != "s1" || string(m.Result) != `{"role":"assistant","content":{"type":"text","text":"hi"},"model":"m"}` {
			t.Errorf("answer = %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sampling request not answered")
	}
}

func TestValidateForm(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{
		"name":{"type":"string","minLength":2,"maxLength":5},
		"color":{"type":"string","oneOf":[{"const":"#f00","title":"Red"}]},
		"size":{"type":"string","enum":["s","m"]},
		"age":{"type":"integer","minimum":18},
		"ratio":{"type":"number","maximum":1},
		"ok":{"type":"boolean"},
		"tags":{"type":"array","minItems":1,"items":{"anyOf":[{"const":"a"},{"const":"b"}]}}},
		"required":["name"]}`)
	valid := map[string]any{"name": "Ann", "color": "#f00", "size": "m", "age": 30.0, "ratio": 0.5, "ok": true, "tags": []any{"a", "b"}}
	if err := validateForm(schema, valid); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]map[string]any{
		"missing required": {"age": 20},
		"unknown field":    {"name": "Ann", "x": 1},
		"too short":        {"name": "A"},
		"not a string":     {"name": 5},
		"not in oneOf":     {"name": "Ann", "color": "#0f0"},
		"not in enum":      {"name": "Ann", "size": "xl"},
		"fractional int":   {"name": "Ann", "age": 20.5},
		"below minimum":    {"name": "Ann", "age": 17},
		"above maximum":    {"name": "Ann", "ratio": 2},
		"not a boolean":    {"name": "Ann", "ok": "yes"},
		"empty array":      {"name": "Ann", "tags": []string{}},
		"bad item":         {"name": "Ann", "tags": []any{"c"}},
		"array of numbers": {"name": "Ann", "tags": []any{1}},
	} {
		if err := validateForm(schema, content); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
