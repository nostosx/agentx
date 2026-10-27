package mcp

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nostosx/agentx"
)

// ProtocolVersion is the MCP revision this client prefers. The revision is
// stateless: every request declares the version in its _meta and there is no
// initialization handshake. Connect falls back to the initialization-based
// revisions listed by SupportedProtocolVersions for servers that do not
// implement it.
const ProtocolVersion = "2026-07-28"

// LegacyProtocolVersion is the initialization-based revision Connect offers
// in its initialize request when a server does not speak ProtocolVersion.
const LegacyProtocolVersion = "2025-11-25"

// legacyVersions are the initialization-based revisions the client accepts,
// newest first. They share the tool, resource and prompt methods the client
// uses with ProtocolVersion.
var legacyVersions = []string{LegacyProtocolVersion, "2025-06-18", "2025-03-26", "2024-11-05"}

// SupportedProtocolVersions returns the revisions the client speaks, newest
// first: ProtocolVersion, then the initialization-based revisions it accepts
// from a server's initialize response.
func SupportedProtocolVersions() []string {
	return append([]string{ProtocolVersion}, legacyVersions...)
}

// defaultProbeTimeout bounds how long Connect waits for server/discover on
// transports other than Streamable HTTP before it sends initialize as well,
// assuming a server that implements an initialization-based revision.
const defaultProbeTimeout = 10 * time.Second

// JSON-RPC and MCP error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603

	CodeHeaderMismatch                  = -32020
	CodeMissingRequiredClientCapability = -32021
	CodeUnsupportedProtocolVersion      = -32022
)

// modernError reports whether code is an error that only servers
// implementing ProtocolVersion or later return.
func modernError(code int) bool {
	return code == CodeHeaderMismatch || code == CodeMissingRequiredClientCapability ||
		code == CodeUnsupportedProtocolVersion
}

// ErrSessionExpired is returned when a server ended the session of an
// initialization-based revision. The client then starts a new session and
// issues the request again once.
var ErrSessionExpired = errors.New("mcp: session expired")

// Reserved _meta keys.
const (
	metaProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	metaClientInfo         = "io.modelcontextprotocol/clientInfo"
	metaClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
	metaLogLevel           = "io.modelcontextprotocol/logLevel"
)

// maxStateRetries bounds how often a request is retried with the
// requestState of an input_required result.
const maxStateRetries = 8

// Error is a JSON-RPC error returned by the server.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("mcp: %s (code %d)", e.Message, e.Code) }

// InputRequiredError reports an input_required result asking for client
// input, such as elicitation or sampling, that this client has no handler
// for: see WithSampling, WithElicitation and WithRoots.
type InputRequiredError struct {
	InputRequests map[string]json.RawMessage
	RequestState  string
}

func (e *InputRequiredError) Error() string { return "mcp: server requires client input" }

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`

	// err is a local failure, such as a lost response stream.
	err error
}

// Icon is an icon for an implementation, tool, resource or prompt.
type Icon struct {
	Src      string   `json:"src"`
	MimeType string   `json:"mimeType,omitempty"`
	Sizes    []string `json:"sizes,omitempty"`
	Theme    string   `json:"theme,omitempty"`
}

// Implementation identifies a client or server.
type Implementation struct {
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	WebsiteURL  string `json:"websiteUrl,omitempty"`
	Icons       []Icon `json:"icons,omitempty"`
}

// DiscoverResult is the server's answer to server/discover.
type DiscoverResult struct {
	SupportedVersions []string        `json:"supportedVersions"`
	Capabilities      json.RawMessage `json:"capabilities"`
	Instructions      string          `json:"instructions,omitempty"`
	// ServerInfo is self-reported by the server and is only meant for
	// display and logging.
	ServerInfo Implementation `json:"-"`
}

// ToolAnnotations are hints about tool behavior. Clients must not rely on
// them for security decisions.
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// Tool is a tool offered by a server.
type Tool struct {
	Name         string           `json:"name"`
	Title        string           `json:"title,omitempty"`
	Description  string           `json:"description,omitempty"`
	InputSchema  json.RawMessage  `json:"inputSchema"`
	OutputSchema json.RawMessage  `json:"outputSchema,omitempty"`
	Annotations  *ToolAnnotations `json:"annotations,omitempty"`
	Icons        []Icon           `json:"icons,omitempty"`
}

// Content is a content block in tool results and prompts.
type Content struct {
	Type     string            `json:"type"`
	Text     string            `json:"text,omitempty"`
	Data     string            `json:"data,omitempty"`
	MimeType string            `json:"mimeType,omitempty"`
	URI      string            `json:"uri,omitempty"`
	Name     string            `json:"name,omitempty"`
	Resource *ResourceContents `json:"resource,omitempty"`
}

// CallToolResult is the result of tools/call.
type CallToolResult struct {
	Content           []Content       `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

// Resource describes a resource offered by a server.
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
	Icons       []Icon `json:"icons,omitempty"`
}

// ResourceContents holds the text or base64 blob of a resource.
type ResourceContents struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

// PromptArgument describes a prompt parameter.
type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// Prompt is a prompt template offered by a server.
type Prompt struct {
	Name        string           `json:"name"`
	Title       string           `json:"title,omitempty"`
	Description string           `json:"description,omitempty"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
	Icons       []Icon           `json:"icons,omitempty"`
}

// PromptMessage is a message produced by a prompt.
type PromptMessage struct {
	Role    string  `json:"role"`
	Content Content `json:"content"`
}

// GetPromptResult is the result of prompts/get.
type GetPromptResult struct {
	Description string          `json:"description,omitempty"`
	Messages    []PromptMessage `json:"messages"`
}

// Subscription selects the change notifications delivered by Listen.
type Subscription struct {
	ToolsListChanged      bool     `json:"toolsListChanged,omitempty"`
	PromptsListChanged    bool     `json:"promptsListChanged,omitempty"`
	ResourcesListChanged  bool     `json:"resourcesListChanged,omitempty"`
	ResourceSubscriptions []string `json:"resourceSubscriptions,omitempty"`
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithClientInfo sets the implementation reported to servers.
func WithClientInfo(name, version string) ClientOption {
	return func(c *Client) { c.info = Implementation{Name: name, Version: version} }
}

// OnNotification registers a handler for server notifications, such as
// notifications/progress or the change notifications delivered by Listen.
// It runs on the receive goroutine and must not block.
func OnNotification(fn func(method string, params json.RawMessage)) ClientOption {
	return func(c *Client) { c.notify = fn }
}

// WithLogger sets the logger for warnings, such as tools rejected for
// invalid x-mcp-header annotations. Defaults to slog.Default.
func WithLogger(l *slog.Logger) ClientOption {
	return func(c *Client) { c.log = l }
}

// WithProtocolVersion pins the protocol revision, which must be one of
// SupportedProtocolVersions. Connect then neither probes nor falls back:
// with ProtocolVersion it requires server/discover to succeed, and with an
// initialization-based revision it sends initialize right away and fails
// unless the server agrees to exactly that revision.
func WithProtocolVersion(v string) ClientOption {
	return func(c *Client) { c.pin = v }
}

// WithProbeTimeout sets how long Connect waits for the answer to its
// server/discover probe over transports other than Streamable HTTP, such as
// stdio, before it assumes the server implements an initialization-based
// revision and sends initialize. Defaults to 10 seconds; zero or less waits
// for the answer as long as Connect's context allows.
//
// The timeout only decides when initialize is sent, not which revision is
// used: Connect keeps waiting for the probe's answer while it waits for the
// answer to initialize, and whichever identifies the server first wins. A
// server that starts slowly and implements ProtocolVersion, which answers
// the probe first and rejects initialize, is therefore still used with
// ProtocolVersion. A shorter timeout makes Connect faster with servers of
// earlier revisions that ignore requests before initialize; it then sends
// initialize to more modern servers, which they reject or, if they serve
// both eras, answer for a session the client abandons.
func WithProbeTimeout(d time.Duration) ClientOption {
	return func(c *Client) { c.probeTimeout = d }
}

// Client is an MCP client connected to one server.
type Client struct {
	t            Transport
	info         Implementation
	notify       func(string, json.RawMessage)
	log          *slog.Logger
	pin          string
	probeTimeout time.Duration
	// offer is the revision sent in initialize.
	offer string
	// legacy is set once the client speaks an initialization-based
	// revision.
	legacy atomic.Bool
	// notifyMu serializes calls of the notification handler.
	notifyMu sync.Mutex
	// initMu serializes the renewal of expired sessions.
	initMu sync.Mutex

	sampling    *samplingConfig
	elicit      ElicitationHandler
	elicitModes []string
	roots       RootsFunc
	onLog       func(LogMessage)
	oauth       *OAuthConfig
	// base is the context of the handlers answering server requests; it
	// ends with the connection.
	base context.Context
	stop context.CancelFunc

	mu      sync.Mutex
	server  DiscoverResult
	version string
	// gen counts the sessions started with initialize.
	gen     int64
	nextID  int64
	pending map[string]chan *message
	headers map[string][]paramHeader
	done    chan struct{}
	err     error
	closed  bool
	// logLevel is the minimum level of server log messages, if set.
	logLevel LogLevel
	// inbound cancels the handlers of server requests, by request ID.
	inbound map[string]context.CancelFunc
	// elicited wakes the requests waiting for URL mode elicitations to
	// complete, by elicitation ID.
	elicited map[string]chan struct{}
}

// Connect starts the transport and finds a protocol revision the server
// speaks, as the backward compatibility rules of revision 2026-07-28
// describe. It first sends server/discover with ProtocolVersion. A
// DiscoverResult or an error only servers implementing that revision
// return, such as an unsupported protocol version, identifies a modern
// server. Any other JSON-RPC error, an HTTP 4xx response without such an
// error (other than 401, 403, 407, 408 and 429) or, on transports other than
// Streamable HTTP, no answer within the probe timeout identifies a server
// implementing an earlier revision: Connect then sends initialize offering
// LegacyProtocolVersion, accepts any revision of SupportedProtocolVersions
// the server answers with, and sends notifications/initialized. ctx bounds
// these requests. See WithProtocolVersion to pin a revision, and
// WithProbeTimeout for servers that start slowly.
//
// The client features configured with WithSampling, WithElicitation and
// WithRoots are declared in initialize or, with ProtocolVersion, in the
// _meta of every request.
func Connect(ctx context.Context, t Transport, opts ...ClientOption) (*Client, error) {
	c := &Client{
		t:            t,
		info:         Implementation{Name: "agentx", Version: agentx.Version},
		log:          slog.Default(),
		probeTimeout: defaultProbeTimeout,
		pending:      map[string]chan *message{},
		done:         make(chan struct{}),
		inbound:      map[string]context.CancelFunc{},
		elicited:     map[string]chan struct{}{},
	}
	for _, opt := range opts {
		opt(c)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	if err := c.applyOAuth(); err != nil {
		return nil, err
	}
	c.base, c.stop = context.WithCancel(context.Background())
	if s, ok := t.(streamTransport); ok {
		s.onLost(c.fail)
	}
	if err := t.Start(ctx); err != nil {
		c.stop()
		return nil, err
	}
	go c.receive()
	if err := c.negotiate(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// validate checks the options.
func (c *Client) validate() error {
	if c.pin != "" && !slices.Contains(SupportedProtocolVersions(), c.pin) {
		return fmt.Errorf("mcp: unsupported protocol version %q (supported: %s)",
			c.pin, strings.Join(SupportedProtocolVersions(), ", "))
	}
	if c.logLevel != "" {
		if err := validLogLevel(c.logLevel); err != nil {
			return err
		}
	}
	if c.sampling != nil && c.sampling.handler == nil {
		return errors.New("mcp: WithSampling: nil handler")
	}
	if c.elicitModes != nil && c.elicit == nil {
		return errors.New("mcp: WithElicitation: nil handler")
	}
	for _, m := range c.elicitModes {
		if m != ElicitationForm && m != ElicitationURL {
			return fmt.Errorf("mcp: WithElicitation: unknown mode %q", m)
		}
	}
	return nil
}

// negotiate selects the protocol revision.
func (c *Client) negotiate(ctx context.Context) error {
	switch c.pin {
	case "":
	case ProtocolVersion:
		_, err := c.discover(ctx, false)
		return err
	default:
		c.offer = c.pin
		return c.initialize(ctx, nil)
	}
	if _, http := c.t.(streamTransport); !http && c.probeTimeout > 0 {
		return c.probe(ctx)
	}
	legacy, err := c.discover(ctx, true)
	if err != nil || legacy == "" {
		return err
	}
	c.offer = legacy
	return c.initialize(ctx, nil)
}

// probe sends server/discover over a transport other than Streamable HTTP.
// If no answer arrives within the probe timeout, which happens with servers
// of earlier revisions that ignore requests before initialize, it sends
// initialize too and uses whichever answer identifies the server first: a
// server that starts slowly may still answer server/discover.
func (c *Client) probe(ctx context.Context) error {
	p, err := c.withMeta(nil)
	if err != nil {
		return err
	}
	// A server that does not know server/discover is not asked to cancel
	// it before initialize.
	d, err := c.issue(ctx, "server/discover", p, false)
	if err != nil {
		legacy, err := c.probeFailed(err, true)
		if err != nil || legacy == "" {
			return err
		}
		c.offer = legacy
		return c.initialize(ctx, nil)
	}
	timer := time.NewTimer(c.probeTimeout)
	defer timer.Stop()
	select {
	case resp, ok := <-d.ch:
		raw, err := c.outcome(resp, ok)
		legacy, err := c.discovered(raw, err, true)
		if err != nil || legacy == "" {
			return err
		}
		c.offer = legacy
		return c.initialize(ctx, nil)
	case <-ctx.Done():
		c.forget(d.key)
		return fmt.Errorf("mcp: server/discover: %w", ctx.Err())
	case <-timer.C:
	}
	// Some servers do not answer requests before initialize at all.
	c.offer = LegacyProtocolVersion
	st := c.beginInitialize()
	ip, err := c.initializeParams(c.offer)
	if err == nil {
		var ic *call
		if ic, err = c.issue(ctx, "initialize", ip, false); err == nil {
			return c.race(ctx, d, ic, st)
		}
	}
	c.forget(d.key)
	c.abortInitialize(st)
	return fmt.Errorf("mcp: initialize: %w", err)
}

// race waits for the answers to a probe d that timed out and to the
// initialize request ic sent after it, and completes the negotiation with
// the first answer that identifies the server.
func (c *Client) race(ctx context.Context, d, ic *call, st sessionTransport) error {
	dch, ich := d.ch, ic.ch
	// failed holds an initialize error that arrived while the probe was
	// still pending: an error does not identify the server, so a modern
	// server that started slowly may still answer the probe.
	var failed *reply
	for {
		select {
		case resp, ok := <-dch:
			dch = nil
			raw, err := c.outcome(resp, ok)
			legacy, err := c.discovered(raw, err, true)
			if legacy != "" {
				// Initialize decides.
				if failed != nil {
					return c.initialize(ctx, failed)
				}
				continue
			}
			// A modern server, or a failure.
			c.forget(ic.key)
			c.abortInitialize(st)
			c.legacy.Store(false)
			return err
		case resp, ok := <-ich:
			ich = nil
			raw, err := c.outcome(resp, ok)
			var e *Error
			if errors.As(err, &e) && slices.Contains(supportedVersions(e), ProtocolVersion) {
				// A modern server that started slowly and rejected
				// initialize, naming the revisions it supports.
				c.forget(d.key)
				c.abortInitialize(st)
				c.legacy.Store(false)
				_, err := c.discover(ctx, false)
				return err
			}
			if err != nil && dch != nil {
				failed = &reply{raw: raw, err: err}
				continue
			}
			c.forget(d.key)
			return c.initialize(ctx, &reply{raw: raw, err: err})
		case <-ctx.Done():
			c.forget(d.key)
			c.forget(ic.key)
			c.abortInitialize(st)
			c.legacy.Store(false)
			return fmt.Errorf("mcp: initialize: %w", ctx.Err())
		}
	}
}

// discover calls server/discover. As a probe, it returns the revision to
// offer in initialize when the answer identifies a server implementing an
// initialization-based revision.
func (c *Client) discover(ctx context.Context, probe bool) (legacy string, err error) {
	p, err := c.withMeta(nil)
	if err != nil {
		return "", err
	}
	// A server that does not know server/discover is not asked to cancel
	// it before initialize.
	raw, err := c.roundTrip(ctx, "server/discover", p, !probe)
	return c.discovered(raw, err, probe)
}

// discovered interprets the answer to server/discover, as discover
// describes.
func (c *Client) discovered(raw json.RawMessage, err error, probe bool) (legacy string, _ error) {
	if err != nil {
		return c.probeFailed(err, probe)
	}
	var res struct {
		DiscoverResult
		Meta struct {
			ServerInfo Implementation `json:"io.modelcontextprotocol/serverInfo"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || len(res.SupportedVersions) == 0 {
		if probe {
			// Not a DiscoverResult.
			return LegacyProtocolVersion, nil
		}
		return "", fmt.Errorf("mcp: server/discover: invalid result %s", raw)
	}
	if !slices.Contains(res.SupportedVersions, ProtocolVersion) {
		if v := newestLegacy(res.SupportedVersions); probe && v != "" {
			return v, nil
		}
		return "", fmt.Errorf("mcp: server does not support protocol %s (supported: %s)",
			ProtocolVersion, strings.Join(res.SupportedVersions, ", "))
	}
	c.mu.Lock()
	c.server = res.DiscoverResult
	c.server.ServerInfo = res.Meta.ServerInfo
	c.version = ProtocolVersion
	c.mu.Unlock()
	return "", nil
}

// probeFailed classifies a failed server/discover request.
func (c *Client) probeFailed(err error, probe bool) (string, error) {
	var e *Error
	if errors.As(err, &e) && e.Code == CodeUnsupportedProtocolVersion {
		supported := supportedVersions(e)
		if v := newestLegacy(supported); probe && v != "" {
			return v, nil
		}
		return "", fmt.Errorf("mcp: server does not support protocol %s (supported: %s)",
			ProtocolVersion, strings.Join(supported, ", "))
	}
	if probe {
		var he *HTTPStatusError
		switch {
		case errors.As(err, &e):
			// The fallback must not be keyed to one error code: servers
			// implementing earlier revisions reject unknown requests before
			// initialize with codes of their own.
			if !modernError(e.Code) {
				return LegacyProtocolVersion, nil
			}
		case errors.As(err, &he):
			if fallbackStatus(he.StatusCode) {
				return LegacyProtocolVersion, nil
			}
		}
		return "", fmt.Errorf("mcp: server/discover: %w", err)
	}
	return "", fmt.Errorf("mcp: server/discover: %w (a server implementing only an initialization-based "+
		"revision needs Connect without WithProtocolVersion(ProtocolVersion))", err)
}

// fallbackStatus reports whether an HTTP status answering server/discover
// identifies a server implementing an initialization-based revision.
// Authentication, timeout and rate-limit statuses do not.
func fallbackStatus(code int) bool {
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusProxyAuthRequired,
		http.StatusRequestTimeout, http.StatusTooManyRequests:
		return false
	}
	return code >= 400 && code < 500
}

// supportedVersions returns the versions listed by an unsupported protocol
// version error.
func supportedVersions(e *Error) []string {
	var data struct {
		Supported []string `json:"supported"`
	}
	_ = json.Unmarshal(e.Data, &data)
	return data.Supported
}

// newestLegacy returns the newest initialization-based revision in
// versions that the client accepts.
func newestLegacy(versions []string) string {
	for _, v := range legacyVersions {
		if slices.Contains(versions, v) {
			return v
		}
	}
	return ""
}

// reply is the outcome of a request.
type reply struct {
	raw json.RawMessage
	err error
}

// beginInitialize switches to the initialization-based revisions and, over
// Streamable HTTP, starts a new session, holding back other requests until
// it is established.
func (c *Client) beginInitialize() sessionTransport {
	c.legacy.Store(true)
	st, _ := c.t.(sessionTransport)
	if st != nil {
		st.beginSession()
	}
	return st
}

// abortInitialize releases the requests held back by beginInitialize when
// no session was established.
func (c *Client) abortInitialize(st sessionTransport) {
	if st != nil {
		st.endSession(false)
	}
}

func (c *Client) initializeParams(offer string) (json.RawMessage, error) {
	return json.Marshal(map[string]any{
		"protocolVersion": offer,
		"capabilities":    c.capabilities(true),
		"clientInfo":      c.info,
	})
}

// initialize starts a session of an initialization-based revision,
// offering c.offer. If first is not nil, it is the outcome of an initialize
// request already sent after beginInitialize.
func (c *Client) initialize(ctx context.Context, first *reply) (err error) {
	st, _ := c.t.(sessionTransport)
	if first == nil {
		st = c.beginInitialize()
	}
	established := false
	defer func() {
		if !established {
			c.abortInitialize(st)
		}
	}()
	offer := c.offer
	var res struct {
		ProtocolVersion string          `json:"protocolVersion"`
		Capabilities    json.RawMessage `json:"capabilities"`
		ServerInfo      Implementation  `json:"serverInfo"`
		Instructions    string          `json:"instructions"`
	}
	for retried := false; ; retried = true {
		var raw json.RawMessage
		if first != nil {
			raw, err = first.raw, first.err
			first = nil
		} else {
			p, perr := c.initializeParams(offer)
			if perr != nil {
				return perr
			}
			// Clients must not cancel initialize.
			raw, err = c.roundTrip(ctx, "initialize", p, false)
		}
		if err != nil {
			// Some servers reject an unsupported version with an error
			// listing the versions they support instead of answering with
			// one of them.
			var e *Error
			if errors.As(err, &e) && !retried && c.pin == "" {
				if v := newestLegacy(supportedVersions(e)); v != "" && v != offer {
					offer = v
					continue
				}
			}
			return fmt.Errorf("mcp: initialize: %w", err)
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return fmt.Errorf("mcp: decode initialize result: %w", err)
		}
		break
	}
	v := res.ProtocolVersion
	switch {
	case c.pin != "" && v != c.pin:
		return fmt.Errorf("mcp: server negotiated protocol %q, not the pinned %s", v, c.pin)
	case !slices.Contains(legacyVersions, v):
		return fmt.Errorf("mcp: server negotiated unsupported protocol version %q (supported: %s)",
			v, strings.Join(SupportedProtocolVersions(), ", "))
	}
	c.mu.Lock()
	c.version = v
	c.server = DiscoverResult{
		SupportedVersions: []string{v},
		Capabilities:      res.Capabilities,
		Instructions:      res.Instructions,
		ServerInfo:        res.ServerInfo,
	}
	c.gen++
	level := c.logLevel
	c.mu.Unlock()
	if st != nil {
		st.setVersion(v)
	}
	if err := c.Notify(ctx, "notifications/initialized", nil); err != nil {
		return fmt.Errorf("mcp: notifications/initialized: %w", err)
	}
	established = true
	if st != nil {
		st.endSession(true)
	}
	if level != "" && c.capable("logging", "") {
		// A new session starts without the level of the previous one.
		if err := c.setLevel(ctx, level); err != nil {
			c.log.Warn("mcp: setting the log level failed", "level", level, "error", err)
		}
	}
	return nil
}

// renew starts a new session after the server ended session gen.
func (c *Client) renew(ctx context.Context, gen int64) error {
	c.initMu.Lock()
	defer c.initMu.Unlock()
	if c.generation() != gen {
		// Another request already started a new session.
		return nil
	}
	if err := c.initialize(ctx, nil); err != nil {
		return fmt.Errorf("mcp: renew expired session: %w", err)
	}
	return nil
}

func (c *Client) generation() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

// Server returns what the server reported about itself: the discovery
// result, or for an initialization-based revision the initialize result,
// whose SupportedVersions holds the negotiated revision alone.
func (c *Client) Server() DiscoverResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.server
}

// ProtocolVersion returns the revision in use: the package constant
// ProtocolVersion, or the initialization-based revision negotiated with
// initialize.
func (c *Client) ProtocolVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.version
}

// capable reports whether the server advertised capability name and, if
// flag is not empty, that capability's boolean flag.
func (c *Client) capable(name, flag string) bool {
	c.mu.Lock()
	raw := c.server.Capabilities
	c.mu.Unlock()
	var caps map[string]json.RawMessage
	if json.Unmarshal(raw, &caps) != nil {
		return false
	}
	sub, ok := caps[name]
	if !ok || string(sub) == "null" {
		return false
	}
	if flag == "" {
		return true
	}
	var flags map[string]any
	_ = json.Unmarshal(sub, &flags)
	b, _ := flags[flag].(bool)
	return b
}

// lacks reports whether a server implementing an initialization-based
// revision did not advertise capability name, without which it does not
// serve the methods of that feature.
func (c *Client) lacks(name string) bool { return c.legacy.Load() && !c.capable(name, "") }

func unsupported(method, capability string) error {
	return fmt.Errorf("mcp: %s: server did not advertise the %s capability: %w", method, capability, errors.ErrUnsupported)
}

// Done is closed when the connection ends.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err returns why the connection ended.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Client) receive() {
	for raw := range c.t.Receive() {
		raw = bytes.TrimSpace(raw)
		if len(raw) > 0 && raw[0] == '[' {
			var batch []json.RawMessage
			if json.Unmarshal(raw, &batch) == nil {
				for _, m := range batch {
					c.handle(m)
				}
			}
			continue
		}
		c.handle(raw)
	}
	c.mu.Lock()
	c.err = cmp.Or(c.t.Err(), ErrClosed)
	pending := c.pending
	c.pending = map[string]chan *message{}
	c.closed = true
	c.mu.Unlock()
	c.stop()
	for _, ch := range pending {
		close(ch)
	}
	close(c.done)
}

func (c *Client) handle(raw []byte) {
	var m message
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	switch {
	case m.Method != "" && len(m.ID) > 0:
		// Servers implementing ProtocolVersion must not send requests;
		// input is requested through input_required results instead.
		// Earlier revisions allow them and expect an answer.
		if c.legacy.Load() {
			c.answer(m.ID, m.Method, m.Params)
		}
	case m.Method != "":
		c.deliver(m.Method, m.Params)
	case len(m.ID) > 0:
		c.resolve(m.ID, &m)
	}
}

// deliver handles a notification: it cancels the server requests that
// notifications/cancelled names, wakes requests waiting for the URL mode
// elicitations notifications/elicitation/complete names, passes
// notifications/message to the WithLogHandler handler and then every
// notification to the OnNotification handler.
func (c *Client) deliver(method string, params json.RawMessage) {
	switch method {
	case "notifications/cancelled":
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(params, &p) == nil && len(p.RequestID) > 0 {
			c.mu.Lock()
			cancel := c.inbound[idKey(p.RequestID)]
			c.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		}
	case "notifications/elicitation/complete":
		var p struct {
			ElicitationID string `json:"elicitationId"`
		}
		if json.Unmarshal(params, &p) == nil {
			c.mu.Lock()
			// Unknown and already completed IDs are ignored.
			if ch, ok := c.elicited[p.ElicitationID]; ok {
				close(ch)
				delete(c.elicited, p.ElicitationID)
			}
			c.mu.Unlock()
		}
	}
	if c.notify == nil && (c.onLog == nil || method != "notifications/message") {
		return
	}
	c.notifyMu.Lock()
	defer c.notifyMu.Unlock()
	if method == "notifications/message" && c.onLog != nil {
		var m LogMessage
		if json.Unmarshal(params, &m) == nil {
			c.onLog(m)
		}
	}
	if c.notify != nil {
		c.notify(method, params)
	}
}

// answer responds to a request from a server implementing an
// initialization-based revision. ping succeeds; sampling/createMessage,
// elicitation/create and roots/list run the handlers configured with
// WithSampling, WithElicitation and WithRoots, and fail with method not
// found without them, as does any other method. The handler runs in a
// goroutine of its own, until the server cancels the request with
// notifications/cancelled or the connection ends.
func (c *Client) answer(id json.RawMessage, method string, params json.RawMessage) {
	key := idKey(id)
	ctx, cancel := context.WithCancel(c.base)
	c.mu.Lock()
	c.inbound[key] = cancel
	c.mu.Unlock()
	go func() {
		defer func() {
			c.mu.Lock()
			delete(c.inbound, key)
			c.mu.Unlock()
			cancel()
		}()
		resp := message{JSONRPC: "2.0", ID: id}
		res, err := c.serveLegacy(ctx, method, params)
		if ctx.Err() != nil && c.base.Err() == nil {
			// A cancelled request is not answered.
			return
		}
		if err != nil {
			resp.Error = rpcErrorFor(method, err)
		} else if resp.Result, err = json.Marshal(res); err != nil {
			resp.Result = nil
			resp.Error = &Error{Code: CodeInternalError, Message: err.Error()}
		}
		b, err := json.Marshal(resp)
		if err != nil {
			return
		}
		sctx, stop := context.WithTimeout(context.WithoutCancel(ctx), cancelTimeout)
		defer stop()
		_ = c.t.Send(sctx, b)
	}()
}

func (c *Client) serveLegacy(ctx context.Context, method string, params json.RawMessage) (any, error) {
	if method == "ping" {
		return struct{}{}, nil
	}
	return c.serve(ctx, method, params)
}

// rpcErrorFor returns the JSON-RPC error answering a server request that
// failed with err.
func rpcErrorFor(method string, err error) *Error {
	var e *Error
	switch {
	case errors.Is(err, errNoHandler):
		return &Error{Code: CodeMethodNotFound, Message: "Method not found: " + method}
	case errors.As(err, &e):
		return e
	case errors.Is(err, ErrSamplingRejected):
		return &Error{Code: codeUserRejected, Message: "User rejected sampling request"}
	}
	return &Error{Code: CodeInternalError, Message: err.Error()}
}

func (c *Client) resolve(id json.RawMessage, m *message) {
	key := idKey(id)
	c.mu.Lock()
	ch, ok := c.pending[key]
	delete(c.pending, key)
	c.mu.Unlock()
	if ok {
		ch <- m
	}
}

// fail ends a pending request with a local error.
func (c *Client) fail(id json.RawMessage, err error) { c.resolve(id, &message{err: err}) }

func idKey(id json.RawMessage) string {
	var n json.Number
	if json.Unmarshal(id, &n) == nil {
		return n.String()
	}
	var s string
	if json.Unmarshal(id, &s) == nil {
		return s
	}
	return string(id)
}

// Call sends a request and decodes its result into out, which may be nil.
// params must encode as a JSON object or null. With ProtocolVersion, the
// protocol fields of _meta are added to it; with an initialization-based
// revision it is sent as given.
//
// With ProtocolVersion, a server may answer with an input_required result.
// Call then runs the handlers for its inputRequests (see WithSampling,
// WithElicitation and WithRoots) and issues the request again with their
// inputResponses and the result's requestState, up to 8 times. If the
// client has no handler for one of the requests, Call returns an
// *InputRequiredError; if a handler fails, Call returns its error.
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	p, err := c.withMeta(params)
	if err != nil {
		return err
	}
	for range maxStateRetries {
		res, err := c.request(ctx, method, p)
		var e *Error
		if errors.As(err, &e) && e.Code == CodeURLElicitationRequired && c.legacy.Load() &&
			slices.Contains(c.elicitModes, ElicitationURL) {
			if err := c.elicitURLs(ctx, e); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		var head struct {
			ResultType    string                     `json:"resultType"`
			InputRequests map[string]json.RawMessage `json:"inputRequests"`
			RequestState  *string                    `json:"requestState"`
		}
		if len(res) > 0 {
			if err := json.Unmarshal(res, &head); err != nil {
				return fmt.Errorf("mcp: decode %s result: %w", method, err)
			}
		}
		switch head.ResultType {
		case "", "complete":
			// Servers implementing earlier revisions omit resultType.
			if out != nil && len(res) > 0 {
				if err := json.Unmarshal(res, out); err != nil {
					return fmt.Errorf("mcp: decode %s result: %w", method, err)
				}
			}
			return nil
		case "input_required":
			if len(head.InputRequests) == 0 && head.RequestState == nil {
				return &InputRequiredError{}
			}
			var responses any // nil removes the previous responses
			if len(head.InputRequests) > 0 {
				r, err := c.fulfill(ctx, head.InputRequests, head.RequestState)
				if err != nil {
					return err
				}
				responses = r
			}
			if p, err = setParam(p, "inputResponses", responses); err != nil {
				return err
			}
			// The retry echoes the requestState exactly, or has none.
			var state any
			if head.RequestState != nil {
				state = *head.RequestState
			}
			if p, err = setParam(p, "requestState", state); err != nil {
				return err
			}
		default:
			return fmt.Errorf("mcp: %s: unknown result type %q", method, head.ResultType)
		}
	}
	return fmt.Errorf("mcp: %s: server kept requesting input", method)
}

// elicitURLs handles a URLElicitationRequiredError of revision 2025-11-25:
// it runs the elicitation handler for each elicitation and, if the user
// accepts them all, waits until the server reports them complete. It
// returns e if the user does not accept one of them.
func (c *Client) elicitURLs(ctx context.Context, e *Error) error {
	var data struct {
		Elicitations []json.RawMessage `json:"elicitations"`
	}
	if json.Unmarshal(e.Data, &data) != nil || len(data.Elicitations) == 0 {
		return e
	}
	var waits []chan struct{}
	own := map[string]chan struct{}{} // the waits this request registered
	defer func() {
		c.mu.Lock()
		for id, ch := range own {
			if c.elicited[id] == ch {
				delete(c.elicited, id)
			}
		}
		c.mu.Unlock()
	}()
	for _, raw := range data.Elicitations {
		var req ElicitRequest
		if json.Unmarshal(raw, &req) != nil || req.Mode != ElicitationURL || req.ElicitationID == "" {
			return e
		}
		// Register before the user acts, so that a completion arriving
		// early is not missed.
		c.mu.Lock()
		ch, ok := c.elicited[req.ElicitationID]
		if !ok {
			// Another request may be waiting for the same elicitation.
			ch = make(chan struct{})
			c.elicited[req.ElicitationID] = ch
			own[req.ElicitationID] = ch
		}
		waits = append(waits, ch)
		c.mu.Unlock()
		res, err := c.elicitation(ctx, raw)
		if err != nil {
			return fmt.Errorf("mcp: elicitation/create: %w", err)
		}
		if res.Action != ElicitAccept {
			return e
		}
	}
	for _, ch := range waits {
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return c.Err()
		}
	}
	return nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// request sends one request. If the server ended the session of an
// initialization-based revision, which it then did not process the request
// in, request starts a new session and sends the request again.
func (c *Client) request(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	gen := c.generation()
	res, err := c.roundTrip(ctx, method, params, true)
	if errors.Is(err, ErrSessionExpired) && c.legacy.Load() {
		if err := c.renew(ctx, gen); err != nil {
			return nil, err
		}
		res, err = c.roundTrip(ctx, method, params, true)
	}
	return res, err
}

// roundTrip sends one request and returns its raw result. If ctx is done
// first, the server is asked to cancel the request unless cancel is false.
func (c *Client) roundTrip(ctx context.Context, method string, params json.RawMessage, cancel bool) (json.RawMessage, error) {
	call, err := c.issue(ctx, method, params, cancel)
	if err != nil {
		return nil, err
	}
	select {
	case resp, ok := <-call.ch:
		return c.outcome(resp, ok)
	case <-ctx.Done():
		c.forget(call.key)
		c.cancel(ctx, call.id, cancel)
		return nil, ctx.Err()
	}
}

// call is a request waiting for its response.
type call struct {
	key string
	id  json.RawMessage
	ch  chan *message
}

// outcome returns the result or error of a response received from a
// call's channel.
func (c *Client) outcome(resp *message, ok bool) (json.RawMessage, error) {
	switch {
	case !ok:
		return nil, c.Err()
	case resp.err != nil:
		return nil, resp.err
	case resp.Error != nil:
		return nil, resp.Error
	}
	return resp.Result, nil
}

// issue sends a request and registers for its response. If ctx is done
// while the request may already have been sent, the server is asked to
// cancel it unless cancel is false.
func (c *Client) issue(ctx context.Context, method string, params json.RawMessage, cancel bool) (*call, error) {
	ch := make(chan *message, 1)
	c.mu.Lock()
	if c.closed {
		err := c.err
		c.mu.Unlock()
		return nil, err
	}
	c.nextID++
	key := strconv.FormatInt(c.nextID, 10)
	c.pending[key] = ch
	c.mu.Unlock()
	m := message{JSONRPC: "2.0", ID: json.RawMessage(key), Method: method, Params: params}
	b, err := json.Marshal(m)
	if err != nil {
		c.forget(key)
		return nil, err
	}
	if err := c.t.Send(ctx, b); err != nil {
		c.forget(key)
		if errors.Is(err, ErrMaybeSent) && ctx.Err() != nil {
			// The server may receive the request and work on it. The
			// cancellation waits for the write still in progress, so send
			// it without delaying the caller.
			go c.cancel(ctx, m.ID, cancel)
			return nil, ctx.Err()
		}
		return nil, err
	}
	return &call{key: key, id: m.ID, ch: ch}, nil
}

// cancelTimeout bounds how long sending notifications/cancelled may wait,
// for example behind a write the server does not read.
const cancelTimeout = 5 * time.Second

// cancel stops waiting for a request and, if notify is set, tells the
// server to stop working on it. Where the transport has a response stream
// per request, cancel closes it, which with ProtocolVersion is the
// cancellation; otherwise, and with the initialization-based revisions,
// where a closed stream does not cancel a request, the client sends
// notifications/cancelled.
func (c *Client) cancel(ctx context.Context, id json.RawMessage, notify bool) {
	s, stream := c.t.(streamTransport)
	if stream {
		s.cancelRequest(id)
	}
	if !notify || stream && !c.legacy.Load() {
		return
	}
	nctx, stop := context.WithTimeout(context.WithoutCancel(ctx), cancelTimeout)
	defer stop()
	_ = c.Notify(nctx, "notifications/cancelled", map[string]any{
		"requestId": id,
		"reason":    ctx.Err().Error(),
	})
}

func (c *Client) forget(key string) {
	c.mu.Lock()
	delete(c.pending, key)
	c.mu.Unlock()
}

// withMeta encodes params and adds the per-request protocol fields to its
// _meta, keeping any other _meta keys. The initialization-based revisions
// have no per-request protocol fields, so params is then only encoded.
func (c *Client) withMeta(params any) (json.RawMessage, error) {
	if c.legacy.Load() {
		if params == nil {
			return nil, nil
		}
		b, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		if b = bytes.TrimSpace(b); string(b) != "null" && (len(b) == 0 || b[0] != '{') {
			return nil, fmt.Errorf("mcp: params must be a JSON object, not %s", b)
		}
		if string(b) == "null" {
			return nil, nil
		}
		return b, nil
	}
	var fields map[string]json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &fields); err != nil {
			return nil, fmt.Errorf("mcp: params must be a JSON object: %w", err)
		}
	}
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	var meta map[string]any
	if raw, ok := fields["_meta"]; ok {
		if err := json.Unmarshal(raw, &meta); err != nil {
			return nil, fmt.Errorf("mcp: _meta must be a JSON object: %w", err)
		}
	}
	if meta == nil {
		meta = map[string]any{}
	}
	meta[metaProtocolVersion] = ProtocolVersion
	meta[metaClientInfo] = c.info
	meta[metaClientCapabilities] = c.capabilities(false)
	c.mu.Lock()
	level := c.logLevel
	c.mu.Unlock()
	if level != "" {
		meta[metaLogLevel] = level
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	fields["_meta"] = b
	return json.Marshal(fields)
}

// setParam sets params[key] to value, or removes it if value is nil.
func setParam(params json.RawMessage, key string, value any) (json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &fields); err != nil {
			return nil, err
		}
	}
	if value == nil {
		delete(fields, key)
		return json.Marshal(fields)
	}
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	fields[key] = b
	return json.Marshal(fields)
}

// Ping checks that the server is responsive. With an initialization-based
// revision it sends ping. ProtocolVersion has no ping, so Ping sends
// server/discover, which every such server answers, and checks that the
// answer is a discovery result.
func (c *Client) Ping(ctx context.Context) error {
	if c.legacy.Load() {
		_, err := c.request(ctx, "ping", nil)
		return err
	}
	p, err := c.withMeta(nil)
	if err != nil {
		return err
	}
	raw, err := c.roundTrip(ctx, "server/discover", p, true)
	if err != nil {
		return err
	}
	var res DiscoverResult
	if err := json.Unmarshal(raw, &res); err != nil || len(res.SupportedVersions) == 0 {
		return fmt.Errorf("mcp: server/discover: invalid result %s", raw)
	}
	return nil
}

// SetLogLevel sets the minimum level of the log messages the server sends
// as notifications/message, which WithLogHandler and OnNotification
// receive. With an initialization-based revision it sends
// logging/setLevel, which requires the server's logging capability, and
// sends it again for every renewed session. ProtocolVersion has no
// logging/setLevel: the level is sent in the _meta of every later request
// instead, and the server sends the log messages of a request before its
// response. Logging is deprecated as of ProtocolVersion but still part of
// it.
func (c *Client) SetLogLevel(ctx context.Context, level LogLevel) error {
	if err := validLogLevel(level); err != nil {
		return err
	}
	if c.legacy.Load() {
		if !c.capable("logging", "") {
			return unsupported("logging/setLevel", "logging")
		}
		p, err := json.Marshal(map[string]any{"level": level})
		if err != nil {
			return err
		}
		if _, err := c.request(ctx, "logging/setLevel", p); err != nil {
			return err
		}
	}
	c.mu.Lock()
	c.logLevel = level
	c.mu.Unlock()
	return nil
}

// setLevel sends logging/setLevel in the current session.
func (c *Client) setLevel(ctx context.Context, level LogLevel) error {
	p, err := json.Marshal(map[string]any{"level": level})
	if err != nil {
		return err
	}
	_, err = c.roundTrip(ctx, "logging/setLevel", p, true)
	return err
}

// RootsChanged tells the server that the roots WithRoots returns changed.
// With an initialization-based revision it sends
// notifications/roots/list_changed, after which the server lists the roots
// again. ProtocolVersion has no such notification, since the server asks
// for the roots with each request that needs them, so RootsChanged then
// does nothing.
func (c *Client) RootsChanged(ctx context.Context) error {
	if c.roots == nil {
		return fmt.Errorf("mcp: RootsChanged: no roots configured with WithRoots: %w", errors.ErrUnsupported)
	}
	if !c.legacy.Load() {
		return nil
	}
	return c.Notify(ctx, "notifications/roots/list_changed", nil)
}

// Notify sends a notification.
func (c *Client) Notify(ctx context.Context, method string, params any) error {
	m := message{JSONRPC: "2.0", Method: method}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		m.Params = b
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return c.t.Send(ctx, b)
}

// Listen opens a subscriptions/listen stream and blocks until ctx is done or
// the stream ends. Notifications, starting with
// notifications/subscriptions/acknowledged, go to the OnNotification
// handler. It returns nil when the server ends the subscription gracefully,
// ctx.Err() when ctx is done, and ErrStreamLost when the stream drops; a
// caller may then listen again. ProtocolVersion does not resume streams.
//
// The initialization-based revisions have no subscriptions/listen, so with
// them Listen emulates it. It sends resources/subscribe for each resource
// subscription, which requires the server's resources.subscribe capability,
// and resources/unsubscribe when it returns. Over Streamable HTTP it opens
// the session's GET stream, which carries the notifications that belong to
// no request, resumes it with Last-Event-ID when it ends early, as
// HTTPTransport describes, and returns ErrStreamLost when it cannot; over other
// transports notifications arrive on the connection itself, and Listen
// returns once ctx is done or the connection ends. Listen then reports
// notifications/subscriptions/acknowledged itself, without a subscription
// ID in _meta and with the list-change notifications limited to those the
// server advertises listChanged for. The server's notifications carry no
// subscription ID either and are not filtered: such a server sends every
// change notification it supports, and over transports other than
// Streamable HTTP it sends them while no Listen runs too.
func (c *Client) Listen(ctx context.Context, s Subscription) error {
	if c.legacy.Load() {
		return c.listenLegacy(ctx, s)
	}
	return c.Call(ctx, "subscriptions/listen", map[string]any{"notifications": s}, nil)
}

func (c *Client) listenLegacy(ctx context.Context, s Subscription) error {
	if len(s.ResourceSubscriptions) > 0 && !c.capable("resources", "subscribe") {
		return unsupported("resources/subscribe", "resources.subscribe")
	}
	var ended <-chan error // nil, which blocks, without a stream
	if st, ok := c.t.(sessionTransport); ok {
		gen := c.generation()
		ch, err := st.openStream(ctx)
		if errors.Is(err, ErrSessionExpired) {
			if err = c.renew(ctx, gen); err == nil {
				ch, err = st.openStream(ctx)
			}
		}
		if err != nil {
			return err
		}
		ended = ch
	}
	var subscribed []string
	defer func() {
		if len(subscribed) == 0 {
			return
		}
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cancelTimeout)
		defer cancel()
		for _, uri := range subscribed {
			_ = c.Call(uctx, "resources/unsubscribe", map[string]any{"uri": uri}, nil)
		}
	}()
	for _, uri := range s.ResourceSubscriptions {
		if err := c.Call(ctx, "resources/subscribe", map[string]any{"uri": uri}, nil); err != nil {
			return err
		}
		subscribed = append(subscribed, uri)
	}
	ack, err := json.Marshal(map[string]any{"notifications": Subscription{
		ToolsListChanged:      s.ToolsListChanged && c.capable("tools", "listChanged"),
		PromptsListChanged:    s.PromptsListChanged && c.capable("prompts", "listChanged"),
		ResourcesListChanged:  s.ResourcesListChanged && c.capable("resources", "listChanged"),
		ResourceSubscriptions: s.ResourceSubscriptions,
	}})
	if err != nil {
		return err
	}
	c.deliver("notifications/subscriptions/acknowledged", ack)
	select {
	case err := <-ended:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.Err()
	}
}

// paginate collects every page of a list method. It stops when the server
// repeats any earlier cursor, which would otherwise loop forever.
func (c *Client) paginate[T any](ctx context.Context, method, field string) ([]T, error) {
	var all []T
	cursor := ""
	seen := map[string]bool{}
	for {
		var params map[string]any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}
		var page map[string]json.RawMessage
		if err := c.Call(ctx, method, params, &page); err != nil {
			return nil, err
		}
		var items []T
		if raw, ok := page[field]; ok {
			if err := json.Unmarshal(raw, &items); err != nil {
				return nil, fmt.Errorf("mcp: decode %s: %w", method, err)
			}
		}
		all = append(all, items...)
		next := ""
		if raw, ok := page["nextCursor"]; ok {
			_ = json.Unmarshal(raw, &next)
		}
		if next == "" || seen[next] {
			return all, nil
		}
		seen[next] = true
		cursor = next
	}
}

// ListTools returns every tool the server offers. Over Streamable HTTP with
// ProtocolVersion, tools with invalid x-mcp-header annotations are left out.
// A server implementing an initialization-based revision that does not
// advertise the tools capability offers no tools.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	if c.lacks("tools") {
		return nil, nil
	}
	tools, err := c.paginate[Tool](ctx, "tools/list", "tools")
	if err != nil {
		return nil, err
	}
	// x-mcp-header annotations belong to ProtocolVersion.
	if _, ok := c.t.(streamTransport); !ok || c.legacy.Load() {
		return tools, nil
	}
	headers := map[string][]paramHeader{}
	valid := tools[:0]
	for _, t := range tools {
		h, err := paramHeaders(t.InputSchema)
		if err != nil {
			c.log.Warn("mcp: rejecting tool", "tool", t.Name, "error", err)
			continue
		}
		if len(h) > 0 {
			headers[t.Name] = h
		}
		valid = append(valid, t)
	}
	c.mu.Lock()
	c.headers = headers
	c.mu.Unlock()
	return valid, nil
}

// CallTool invokes a tool. Tool failures are reported in the result's
// IsError field; the error return is for protocol failures.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (*CallToolResult, error) {
	if c.lacks("tools") {
		return nil, unsupported("tools/call", "tools")
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	params := map[string]any{"name": name, "arguments": args}
	var out CallToolResult
	err := c.Call(c.toolContext(ctx, name, args), "tools/call", params, &out)
	var e *Error
	if errors.As(err, &e) && e.Code == CodeHeaderMismatch {
		// The tool's x-mcp-header annotations may have changed.
		if _, lerr := c.ListTools(ctx); lerr == nil {
			out = CallToolResult{}
			err = c.Call(c.toolContext(ctx, name, args), "tools/call", params, &out)
		}
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// toolContext attaches the Mcp-Param headers of a tool call to ctx.
func (c *Client) toolContext(ctx context.Context, name string, args json.RawMessage) context.Context {
	c.mu.Lock()
	h := c.headers[name]
	c.mu.Unlock()
	if len(h) == 0 {
		return ctx
	}
	return context.WithValue(ctx, paramHeadersKey{}, paramValues(h, args))
}

// ListResources returns every resource the server offers. A server
// implementing an initialization-based revision that does not advertise the
// resources capability offers none.
func (c *Client) ListResources(ctx context.Context) ([]Resource, error) {
	if c.lacks("resources") {
		return nil, nil
	}
	return c.paginate[Resource](ctx, "resources/list", "resources")
}

// ListResourceTemplates returns every resource template the server offers.
// A server implementing an initialization-based revision that does not
// advertise the resources capability offers none.
func (c *Client) ListResourceTemplates(ctx context.Context) ([]ResourceTemplate, error) {
	if c.lacks("resources") {
		return nil, nil
	}
	return c.paginate[ResourceTemplate](ctx, "resources/templates/list", "resourceTemplates")
}

// ReadResource reads a resource.
func (c *Client) ReadResource(ctx context.Context, uri string) ([]ResourceContents, error) {
	if c.lacks("resources") {
		return nil, unsupported("resources/read", "resources")
	}
	var out struct {
		Contents []ResourceContents `json:"contents"`
	}
	if err := c.Call(ctx, "resources/read", map[string]any{"uri": uri}, &out); err != nil {
		return nil, err
	}
	return out.Contents, nil
}

// ListPrompts returns every prompt the server offers. A server implementing
// an initialization-based revision that does not advertise the prompts
// capability offers none.
func (c *Client) ListPrompts(ctx context.Context) ([]Prompt, error) {
	if c.lacks("prompts") {
		return nil, nil
	}
	return c.paginate[Prompt](ctx, "prompts/list", "prompts")
}

// GetPrompt renders a prompt.
func (c *Client) GetPrompt(ctx context.Context, name string, args map[string]string) (*GetPromptResult, error) {
	if c.lacks("prompts") {
		return nil, unsupported("prompts/get", "prompts")
	}
	params := map[string]any{"name": name}
	if len(args) > 0 {
		params["arguments"] = args
	}
	var out GetPromptResult
	if err := c.Call(ctx, "prompts/get", params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Close ends the connection. Over Streamable HTTP, a session of an
// initialization-based revision is ended with an HTTP DELETE first.
func (c *Client) Close() error {
	err := c.t.Close()
	if errors.Is(err, ErrClosed) {
		return nil
	}
	return err
}
