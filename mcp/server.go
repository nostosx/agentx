package mcp

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/nostosx/agentx"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/tool"
)

// This file holds the server: what it offers and how it answers requests
// of both protocol generations. ServeStdio and HTTPHandler carry the
// messages; see server_stdio.go and server_http.go.
//
// A request carrying the per-request _meta fields of ProtocolVersion is
// served statelessly, as
// https://modelcontextprotocol.io/specification/2026-07-28/basic/versioning
// describes for dual-era servers. An initialize request starts a session of
// an initialization-based revision
// (https://modelcontextprotocol.io/specification/2025-11-25/basic/lifecycle),
// scoped to the stdio connection or to the HTTP session it creates.

// CodeResourceNotFound is the error code with which revision 2025-11-25 and
// earlier report an unknown resource. ProtocolVersion uses
// CodeInvalidParams instead.
const CodeResourceNotFound = -32002

// ErrResourceNotFound is returned by resource handlers and sources for an
// unknown resource. The client receives the error code the request's
// protocol revision defines for it.
var ErrResourceNotFound = errors.New("mcp: resource not found")

// ErrPromptNotFound is returned by prompt sources for an unknown prompt.
var ErrPromptNotFound = errors.New("mcp: prompt not found")

// errNoResponse ends a request that must not be answered: one the client
// cancelled, or a subscription the client closed.
var errNoResponse = errors.New("mcp: no response")

// ResourceTemplate describes parameterized resources by an RFC 6570 URI
// template.
type ResourceTemplate struct {
	URITemplate string `json:"uriTemplate"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
	Icons       []Icon `json:"icons,omitempty"`
}

// ResourceHandler reads a resource. It returns an error wrapping
// ErrResourceNotFound for an unknown URI.
type ResourceHandler func(ctx context.Context, uri string) ([]ResourceContents, error)

// TemplateHandler reads a resource matching a resource template; vars holds
// the values of the template's variables.
type TemplateHandler func(ctx context.Context, uri string, vars map[string]string) ([]ResourceContents, error)

// PromptHandler renders a prompt with its arguments. The server checks
// that the required arguments are present first.
type PromptHandler func(ctx context.Context, args map[string]string) (*GetPromptResult, error)

// ResourceSource offers resources that change outside the server, such as
// the files of a directory. Sources are consulted after the resources added
// with Server.AddResource and AddResourceTemplate.
type ResourceSource interface {
	// Resources lists the resources, in a stable order.
	Resources(ctx context.Context) ([]Resource, error)
	// ReadResource reads one; it returns an error wrapping
	// ErrResourceNotFound for a URI it does not know.
	ReadResource(ctx context.Context, uri string) ([]ResourceContents, error)
}

// ResourceTemplateSource is implemented by resource sources that also list
// resource templates.
type ResourceTemplateSource interface {
	ResourceTemplates(ctx context.Context) ([]ResourceTemplate, error)
}

// PromptSource offers prompts that change outside the server.
type PromptSource interface {
	// Prompts lists the prompts, in a stable order.
	Prompts(ctx context.Context) ([]Prompt, error)
	// GetPrompt renders one; it returns an error wrapping
	// ErrPromptNotFound for a name it does not know.
	GetPrompt(ctx context.Context, name string, args map[string]string) (*GetPromptResult, error)
}

// Cache scopes of list and read results; see ServerOptions.CacheScope.
const (
	CachePublic  = "public"
	CachePrivate = "private"
)

// ServerOptions configures a Server. The zero value serves no tools,
// resources or prompts until they are added.
type ServerOptions struct {
	// Instructions tell models how to use the server.
	Instructions string
	// Registry is served live: tools registered or removed later are
	// listed too. Call Server.ToolsChanged after changing it directly.
	// Defaults to a new registry.
	Registry *tool.Registry
	// Tools are registered in the registry.
	Tools []tool.Tool
	// ToolMiddleware wraps every tool call, as tool.Executor does.
	ToolMiddleware []tool.Middleware
	// Resources and Prompts are consulted after the resources and prompts
	// added to the server.
	Resources ResourceSource
	Prompts   PromptSource
	// Logger receives the server's own warnings. Defaults to slog.Default.
	Logger *slog.Logger
	// PageSize, if positive, paginates list results.
	PageSize int
	// CacheTTL and CacheScope are the caching hints of list and read
	// results with ProtocolVersion
	// (https://modelcontextprotocol.io/specification/2026-07-28/server/utilities/caching).
	// They default to 0, immediately stale, and CachePrivate.
	CacheTTL   time.Duration
	CacheScope string
	// StateKey authenticates the requestState of input_required results.
	// Servers behind a load balancer need the same key on every instance;
	// it defaults to a random key.
	StateKey []byte
	// InputTimeout bounds how long a handler waiting for client input with
	// ProtocolVersion is kept for the client's retry. Defaults to 10
	// minutes.
	InputTimeout time.Duration
	// ModernOnly serves ProtocolVersion alone: initialize is rejected with
	// an error naming the supported version, and HTTP GET and DELETE are
	// refused with 405 Method Not Allowed, as
	// https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http#earlier-streamable-http-revisions
	// describes.
	ModernOnly bool
}

// Server is an MCP server. It serves tools, resources and prompts over
// stdio (ServeStdio) and Streamable HTTP (HTTPHandler), to clients of
// ProtocolVersion and of the initialization-based revisions alike.
type Server struct {
	info Implementation
	opts ServerOptions
	log  *slog.Logger
	reg  *tool.Registry
	exec *tool.Executor
	key  []byte

	mu        sync.Mutex
	resources []*staticResource
	templates []*staticTemplate
	prompts   []*staticPrompt
	subs      map[*subscription]struct{}
	sessions  map[*session]struct{}
	conts     map[string]*continuation
	queue     []serverEvent
	draining  bool
	closed    bool
	closing   chan struct{}
}

type staticResource struct {
	r Resource
	h ResourceHandler
}

type staticTemplate struct {
	t    ResourceTemplate
	h    TemplateHandler
	re   *regexp.Regexp
	vars []string
}

type staticPrompt struct {
	p Prompt
	h PromptHandler
}

// NewServer returns a server identifying itself with info; a zero info
// becomes agentx and its version. opts may be nil. It panics if the
// options' tools have invalid or duplicate names.
func NewServer(info Implementation, opts *ServerOptions) *Server {
	s := &Server{
		info:     info,
		subs:     map[*subscription]struct{}{},
		sessions: map[*session]struct{}{},
		conts:    map[string]*continuation{},
		closing:  make(chan struct{}),
	}
	if opts != nil {
		s.opts = *opts
	}
	if s.info.Name == "" {
		s.info.Name = "agentx"
	}
	if s.info.Version == "" {
		s.info.Version = agentx.Version
	}
	s.log = cmp.Or(s.opts.Logger, slog.Default())
	s.reg = s.opts.Registry
	if s.reg == nil {
		s.reg, _ = tool.NewRegistry()
	}
	if err := s.reg.Register(s.opts.Tools...); err != nil {
		panic("mcp: NewServer: " + err.Error())
	}
	s.exec = tool.NewExecutor(s.reg, s.opts.ToolMiddleware...)
	s.key = s.opts.StateKey
	if len(s.key) == 0 {
		s.key = make([]byte, 32)
		_, _ = rand.Read(s.key)
	}
	if s.opts.CacheScope == "" {
		s.opts.CacheScope = CachePrivate
	}
	if s.opts.InputTimeout <= 0 {
		s.opts.InputTimeout = 10 * time.Minute
	}
	return s
}

// Info returns the implementation the server reports.
func (s *Server) Info() Implementation { return s.info }

// Registry returns the registry of the served tools.
func (s *Server) Registry() *tool.Registry { return s.reg }

// AddTools registers tools and notifies clients that the tool list changed.
func (s *Server) AddTools(tools ...tool.Tool) error {
	if err := s.reg.Register(tools...); err != nil {
		return err
	}
	s.ToolsChanged()
	return nil
}

// RemoveTools removes tools by name and notifies clients.
func (s *Server) RemoveTools(names ...string) {
	s.reg.Remove(names...)
	s.ToolsChanged()
}

// AddResource adds a resource, replacing one with the same URI, and
// notifies clients that the resource list changed.
func (s *Server) AddResource(r Resource, h ResourceHandler) {
	s.mu.Lock()
	s.resources = slices.DeleteFunc(s.resources, func(x *staticResource) bool { return x.r.URI == r.URI })
	s.resources = append(s.resources, &staticResource{r: r, h: h})
	s.mu.Unlock()
	s.ResourcesChanged()
}

// AddResourceTemplate adds a resource template. Reading a URI that matches
// it calls h with the values of its variables. Simple ({var}) and reserved
// ({+var}) expansions are matched; a simple variable does not match "/",
// "?" or "#". It fails for other expressions.
func (s *Server) AddResourceTemplate(t ResourceTemplate, h TemplateHandler) error {
	re, vars, err := compileTemplate(t.URITemplate)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.templates = slices.DeleteFunc(s.templates, func(x *staticTemplate) bool { return x.t.URITemplate == t.URITemplate })
	s.templates = append(s.templates, &staticTemplate{t: t, h: h, re: re, vars: vars})
	s.mu.Unlock()
	s.ResourcesChanged()
	return nil
}

// RemoveResources removes resources and resource templates by URI or URI
// template and notifies clients.
func (s *Server) RemoveResources(uris ...string) {
	s.mu.Lock()
	s.resources = slices.DeleteFunc(s.resources, func(x *staticResource) bool { return slices.Contains(uris, x.r.URI) })
	s.templates = slices.DeleteFunc(s.templates, func(x *staticTemplate) bool { return slices.Contains(uris, x.t.URITemplate) })
	s.mu.Unlock()
	s.ResourcesChanged()
}

// AddPrompt adds a prompt, replacing one with the same name, and notifies
// clients that the prompt list changed.
func (s *Server) AddPrompt(p Prompt, h PromptHandler) {
	s.mu.Lock()
	s.prompts = slices.DeleteFunc(s.prompts, func(x *staticPrompt) bool { return x.p.Name == p.Name })
	s.prompts = append(s.prompts, &staticPrompt{p: p, h: h})
	s.mu.Unlock()
	s.PromptsChanged()
}

// RemovePrompts removes prompts by name and notifies clients.
func (s *Server) RemovePrompts(names ...string) {
	s.mu.Lock()
	s.prompts = slices.DeleteFunc(s.prompts, func(x *staticPrompt) bool { return slices.Contains(names, x.p.Name) })
	s.mu.Unlock()
	s.PromptsChanged()
}

// compileTemplate turns an RFC 6570 template of simple and reserved
// expansions into a regular expression.
func compileTemplate(tmpl string) (*regexp.Regexp, []string, error) {
	var b strings.Builder
	var vars []string
	b.WriteString("^")
	rest := tmpl
	for rest != "" {
		i := strings.IndexByte(rest, '{')
		if i < 0 {
			b.WriteString(regexp.QuoteMeta(rest))
			break
		}
		b.WriteString(regexp.QuoteMeta(rest[:i]))
		j := strings.IndexByte(rest[i:], '}')
		if j < 0 {
			return nil, nil, fmt.Errorf("mcp: resource template %q: unclosed expression", tmpl)
		}
		expr := rest[i+1 : i+j]
		rest = rest[i+j+1:]
		pattern := `([^/?#]+)`
		if strings.HasPrefix(expr, "+") {
			expr, pattern = expr[1:], `(.+)`
		}
		if expr == "" || strings.ContainsAny(expr, ",*:#./;?&=") {
			return nil, nil, fmt.Errorf("mcp: resource template %q: unsupported expression {%s}", tmpl, expr)
		}
		vars = append(vars, expr)
		b.WriteString(pattern)
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, nil, fmt.Errorf("mcp: resource template %q: %w", tmpl, err)
	}
	return re, vars, nil
}

// hasResources and hasPrompts report whether the server offers the
// feature, and so declares its capability.
func (s *Server) hasResources() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opts.Resources != nil || len(s.resources) > 0 || len(s.templates) > 0
}

func (s *Server) hasPrompts() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opts.Prompts != nil || len(s.prompts) > 0
}

// capabilities returns the server capabilities.
func (s *Server) capabilities() map[string]any {
	caps := map[string]any{
		"tools":   map[string]any{"listChanged": true},
		"logging": map[string]any{},
	}
	if s.hasResources() {
		caps["resources"] = map[string]any{"listChanged": true, "subscribe": true}
	}
	if s.hasPrompts() {
		caps["prompts"] = map[string]any{"listChanged": true}
	}
	return caps
}

// clientCaps are the client capabilities a request declares.
type clientCaps struct {
	Roots    *struct{} `json:"roots"`
	Sampling *struct {
		Tools json.RawMessage `json:"tools"`
	} `json:"sampling"`
	Elicitation *struct {
		Form json.RawMessage `json:"form"`
		URL  json.RawMessage `json:"url"`
	} `json:"elicitation"`
}

// missing returns the capability, in the form of ClientCapabilities, that
// an input request of method with params needs and caps lacks, or nil.
func (caps clientCaps) missing(method string, params any) map[string]any {
	switch method {
	case "sampling/createMessage":
		req, _ := params.(*SamplingRequest)
		withTools := req != nil && (len(req.Tools) > 0 || req.ToolChoice != nil)
		if caps.Sampling == nil {
			if withTools {
				return map[string]any{"sampling": map[string]any{"tools": map[string]any{}}}
			}
			return map[string]any{"sampling": map[string]any{}}
		}
		if withTools && len(caps.Sampling.Tools) == 0 {
			return map[string]any{"sampling": map[string]any{"tools": map[string]any{}}}
		}
	case "elicitation/create":
		req, _ := params.(*ElicitRequest)
		mode := ElicitationForm
		if req != nil && req.Mode != "" {
			mode = req.Mode
		}
		e := caps.Elicitation
		switch {
		case e == nil:
		case mode == ElicitationForm && (len(e.Form) > 0 || len(e.URL) == 0):
			// An empty elicitation object declares form mode.
			return nil
		case mode == ElicitationURL && len(e.URL) > 0:
			return nil
		}
		return map[string]any{"elicitation": map[string]any{mode: map[string]any{}}}
	case "roots/list":
		if caps.Roots == nil {
			return map[string]any{"roots": map[string]any{}}
		}
	}
	return nil
}

// incoming is one request being served.
type incoming struct {
	id     json.RawMessage
	method string
	params json.RawMessage
	// modern is set for a request of ProtocolVersion; sess is the session
	// of a request of an initialization-based revision.
	modern bool
	sess   *session
	caps   clientCaps
	info   Implementation
	// level is the log level of a modern request, if any.
	level    LogLevel
	progress json.RawMessage
	token    *TokenInfo
	// send delivers a message related to the request: a notification or,
	// in a session, a server request.
	send func(msg []byte) error
	// shutdown, when closed, ends subscriptions gracefully.
	shutdown <-chan struct{}
}

// version returns the protocol revision of the request.
func (c *incoming) version() string {
	if c.sess != nil {
		return c.sess.version
	}
	return ProtocolVersion
}

// requestMeta holds the per-request protocol fields of _meta.
type requestMeta struct {
	ProtocolVersion    *string         `json:"io.modelcontextprotocol/protocolVersion"`
	ClientInfo         json.RawMessage `json:"io.modelcontextprotocol/clientInfo"`
	ClientCapabilities json.RawMessage `json:"io.modelcontextprotocol/clientCapabilities"`
	LogLevel           *string         `json:"io.modelcontextprotocol/logLevel"`
	ProgressToken      json.RawMessage `json:"progressToken"`
}

// readMeta decodes the _meta of params.
func readMeta(params json.RawMessage) requestMeta {
	var p struct {
		Meta requestMeta `json:"_meta"`
	}
	_ = json.Unmarshal(params, &p)
	return p.Meta
}

// isModern reports whether params declare a protocol version in _meta.
func isModern(params json.RawMessage) bool { return readMeta(params).ProtocolVersion != nil }

// prepareModern checks the per-request fields of a modern request and
// fills in c. The error is a JSON-RPC error.
func (s *Server) prepareModern(c *incoming) *Error {
	m := readMeta(c.params)
	if m.ProtocolVersion == nil || len(m.ClientCapabilities) == 0 {
		return invalidParams("missing required _meta field %s or %s", metaProtocolVersion, metaClientCapabilities)
	}
	if v := *m.ProtocolVersion; v != ProtocolVersion {
		return s.unsupportedVersion(v)
	}
	if err := json.Unmarshal(m.ClientCapabilities, &c.caps); err != nil || m.ClientCapabilities[0] != '{' {
		return invalidParams("%s must be an object", metaClientCapabilities)
	}
	if len(m.ClientInfo) > 0 {
		_ = json.Unmarshal(m.ClientInfo, &c.info)
	}
	if m.LogLevel != nil {
		if err := validLogLevel(LogLevel(*m.LogLevel)); err != nil {
			return invalidParams("invalid log level %q", *m.LogLevel)
		}
		c.level = LogLevel(*m.LogLevel)
	}
	c.progress = m.ProgressToken
	c.modern = true
	return nil
}

// versions returns the protocol revisions the server serves, newest first.
func (s *Server) versions() []string {
	if s.opts.ModernOnly {
		return []string{ProtocolVersion}
	}
	return SupportedProtocolVersions()
}

func (s *Server) unsupportedVersion(requested string) *Error {
	data, _ := json.Marshal(map[string]any{"supported": s.versions(), "requested": requested})
	return &Error{Code: CodeUnsupportedProtocolVersion, Message: "Unsupported protocol version", Data: data}
}

func methodNotFound(method string) *Error {
	return &Error{Code: CodeMethodNotFound, Message: "Method not found: " + method}
}

// modernMethods are the methods served with ProtocolVersion, and
// legacyMethods those served in sessions, besides initialize.
var (
	modernMethods = []string{
		"server/discover", "subscriptions/listen", "tools/list", "tools/call",
		"resources/list", "resources/templates/list", "resources/read", "prompts/list", "prompts/get",
	}
	legacyMethods = []string{
		"ping", "logging/setLevel", "tools/list", "tools/call", "resources/list",
		"resources/templates/list", "resources/read", "resources/subscribe", "resources/unsubscribe",
		"prompts/list", "prompts/get",
	}
)

// knows reports whether the server serves method in c's revision, given
// the capabilities it declares.
func (s *Server) knows(c *incoming, method string) bool {
	methods := legacyMethods
	if c.modern {
		methods = modernMethods
	}
	if !slices.Contains(methods, method) {
		return false
	}
	switch {
	case strings.HasPrefix(method, "resources/"):
		return s.hasResources()
	case strings.HasPrefix(method, "prompts/"):
		return s.hasPrompts()
	}
	return true
}

// serve answers a request other than initialize. It returns the result,
// a JSON-RPC *Error, errNoResponse, or another error, which becomes an
// internal error.
func (s *Server) serve(ctx context.Context, c *incoming) (any, error) {
	if !s.knows(c, c.method) {
		return nil, methodNotFound(c.method)
	}
	if c.progress == nil {
		c.progress = readMeta(c.params).ProgressToken
	}
	switch c.method {
	case "ping":
		return struct{}{}, nil
	case "server/discover":
		fields := map[string]any{"supportedVersions": s.versions(), "capabilities": s.capabilities()}
		if s.opts.Instructions != "" {
			fields["instructions"] = s.opts.Instructions
		}
		return s.result(c, fields, true), nil
	case "logging/setLevel":
		var p struct {
			Level LogLevel `json:"level"`
		}
		if err := json.Unmarshal(c.params, &p); err != nil || validLogLevel(p.Level) != nil {
			return nil, invalidParams("invalid log level")
		}
		c.sess.setLevel(p.Level)
		return struct{}{}, nil
	case "subscriptions/listen":
		return s.listen(ctx, c)
	case "tools/list":
		return s.listTools(c)
	case "tools/call":
		return s.withInput(ctx, c, s.callTool)
	case "resources/list":
		return s.listResources(ctx, c)
	case "resources/templates/list":
		return s.listTemplates(ctx, c)
	case "resources/read":
		return s.withInput(ctx, c, s.readResource)
	case "resources/subscribe", "resources/unsubscribe":
		var p struct {
			URI string `json:"uri"`
		}
		if err := json.Unmarshal(c.params, &p); err != nil || p.URI == "" {
			return nil, invalidParams("missing uri")
		}
		c.sess.subscribe(p.URI, c.method == "resources/subscribe")
		return struct{}{}, nil
	case "prompts/list":
		return s.listPrompts(ctx, c)
	case "prompts/get":
		return s.withInput(ctx, c, s.getPrompt)
	}
	return nil, methodNotFound(c.method)
}

// result completes a result for c's revision: with ProtocolVersion it adds
// resultType, the server's identity in _meta and, if cacheable, the caching
// hints.
func (s *Server) result(c *incoming, fields map[string]any, cacheable bool) map[string]any {
	if !c.modern {
		return fields
	}
	fields["resultType"] = "complete"
	fields["_meta"] = map[string]any{metaServerInfo: s.info}
	if cacheable {
		fields["ttlMs"] = s.opts.CacheTTL.Milliseconds()
		fields["cacheScope"] = s.opts.CacheScope
	}
	return fields
}

// page returns the page of items a list request's cursor selects, and the
// next cursor.
func (s *Server) page[T any](c *incoming, items []T) ([]T, string, error) {
	var p struct {
		Cursor *string `json:"cursor"`
	}
	_ = json.Unmarshal(c.params, &p)
	start := 0
	if p.Cursor != nil {
		b, err := base64.RawURLEncoding.DecodeString(*p.Cursor)
		n, aerr := strconv.Atoi(strings.TrimPrefix(string(b), "o"))
		if err != nil || aerr != nil || !strings.HasPrefix(string(b), "o") || n < 0 || n > len(items) {
			return nil, "", invalidParams("invalid cursor")
		}
		start = n
	}
	size := s.opts.PageSize
	if size <= 0 || start+size >= len(items) {
		return items[start:], "", nil
	}
	return items[start : start+size], base64.RawURLEncoding.EncodeToString([]byte("o" + strconv.Itoa(start+size))), nil
}

// listResult builds a paginated list result.
func (s *Server) listResult[T any](c *incoming, field string, items []T) (any, error) {
	if items == nil {
		items = []T{}
	}
	pageItems, next, err := s.page(c, items)
	if err != nil {
		return nil, err
	}
	fields := map[string]any{field: pageItems}
	if next != "" {
		fields["nextCursor"] = next
	}
	return s.result(c, fields, true), nil
}

// MCPTool converts an agentx tool to its MCP definition. The input schema
// is the one sent to models. ReadOnly becomes readOnlyHint; other tools are
// hinted destructive exactly when they require approval, so that clients
// such as this package's adapter ask for approval for the same tools.
func MCPTool(t tool.Tool) Tool {
	info := t.Info()
	spec := tool.Spec(t)
	schema := spec.Parameters
	if len(schema) == 0 || string(schema) == "null" {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	ann := &ToolAnnotations{ReadOnlyHint: new(info.ReadOnly)}
	if !info.ReadOnly {
		ann.DestructiveHint = new(info.RequiresApproval)
	}
	return Tool{Name: info.Name, Description: info.Description, InputSchema: schema, Annotations: ann}
}

func (s *Server) listTools(c *incoming) (any, error) {
	list := s.reg.List()
	tools := make([]Tool, len(list))
	for i, t := range list {
		tools[i] = MCPTool(t)
	}
	return s.listResult(c, "tools", tools)
}

// toolResult is the result of tools/call.
type toolResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

// callTool runs a tool through the executor. A tool error that is an
// *Error answers the request with that JSON-RPC error; other failures,
// including invalid arguments, are tool execution errors.
func (s *Server) callTool(ctx context.Context, c *incoming) (any, error) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(c.params, &p); err != nil || p.Name == "" {
		return nil, invalidParams("invalid tools/call params")
	}
	if _, ok := s.reg.Get(p.Name); !ok {
		return nil, invalidParams("Unknown tool: %s", p.Name)
	}
	if len(p.Arguments) == 0 || string(p.Arguments) == "null" {
		p.Arguments = json.RawMessage(`{}`)
	}
	var cause error
	capture := func(next tool.Invoker) tool.Invoker {
		return func(ctx context.Context, tc *tool.Call) (*tool.Result, error) {
			res, err := next(ctx, tc)
			cause = err
			return res, err
		}
	}
	res := s.exec.ExecuteWith(ctx, &model.ToolCall{ID: "mcp", Name: p.Name, Arguments: p.Arguments}, capture)
	if e, ok := errors.AsType[*Error](cause); ok {
		return nil, e
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	out := toolResult{Content: toolContent(c.version(), res), IsError: res.IsError}
	if c.modern {
		return s.result(c, map[string]any{"content": out.Content, "isError": out.IsError}, false), nil
	}
	return out, nil
}

func (s *Server) listResources(ctx context.Context, c *incoming) (any, error) {
	s.mu.Lock()
	var list []Resource
	for _, r := range s.resources {
		list = append(list, r.r)
	}
	src := s.opts.Resources
	s.mu.Unlock()
	if src != nil {
		more, err := src.Resources(ctx)
		if err != nil {
			return nil, err
		}
		list = append(list, more...)
	}
	return s.listResult(c, "resources", list)
}

func (s *Server) listTemplates(ctx context.Context, c *incoming) (any, error) {
	s.mu.Lock()
	var list []ResourceTemplate
	for _, t := range s.templates {
		list = append(list, t.t)
	}
	src := s.opts.Resources
	s.mu.Unlock()
	if ts, ok := src.(ResourceTemplateSource); ok {
		more, err := ts.ResourceTemplates(ctx)
		if err != nil {
			return nil, err
		}
		list = append(list, more...)
	}
	return s.listResult(c, "resourceTemplates", list)
}

// notFound returns the error of an unknown resource in c's revision.
func notFound(c *incoming, uri string) *Error {
	data, _ := json.Marshal(map[string]string{"uri": uri})
	if c.modern {
		return &Error{Code: CodeInvalidParams, Message: "Resource not found", Data: data}
	}
	return &Error{Code: CodeResourceNotFound, Message: "Resource not found", Data: data}
}

func (s *Server) readResource(ctx context.Context, c *incoming) (any, error) {
	var p struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(c.params, &p); err != nil || p.URI == "" {
		return nil, invalidParams("missing uri")
	}
	contents, err := s.read(ctx, p.URI)
	if errors.Is(err, ErrResourceNotFound) {
		return nil, notFound(c, p.URI)
	}
	if err != nil {
		return nil, err
	}
	if contents == nil {
		contents = []ResourceContents{}
	}
	return s.result(c, map[string]any{"contents": contents}, true), nil
}

// read finds the handler of uri: a resource, then a template, then the
// source.
func (s *Server) read(ctx context.Context, uri string) ([]ResourceContents, error) {
	s.mu.Lock()
	var h ResourceHandler
	for _, r := range s.resources {
		if r.r.URI == uri {
			h = r.h
		}
	}
	var th TemplateHandler
	var vars map[string]string
	if h == nil {
		for _, t := range s.templates {
			if m := t.re.FindStringSubmatch(uri); m != nil {
				th, vars = t.h, map[string]string{}
				for i, v := range t.vars {
					vars[v] = m[i+1]
				}
				break
			}
		}
	}
	src := s.opts.Resources
	s.mu.Unlock()
	switch {
	case h != nil:
		return h(ctx, uri)
	case th != nil:
		return th(ctx, uri, vars)
	case src != nil:
		return src.ReadResource(ctx, uri)
	}
	return nil, ErrResourceNotFound
}

func (s *Server) listPrompts(ctx context.Context, c *incoming) (any, error) {
	s.mu.Lock()
	var list []Prompt
	for _, p := range s.prompts {
		list = append(list, p.p)
	}
	src := s.opts.Prompts
	s.mu.Unlock()
	if src != nil {
		more, err := src.Prompts(ctx)
		if err != nil {
			return nil, err
		}
		list = append(list, more...)
	}
	return s.listResult(c, "prompts", list)
}

func (s *Server) getPrompt(ctx context.Context, c *incoming) (any, error) {
	var p struct {
		Name      string            `json:"name"`
		Arguments map[string]string `json:"arguments"`
	}
	if err := json.Unmarshal(c.params, &p); err != nil || p.Name == "" {
		return nil, invalidParams("invalid prompts/get params")
	}
	s.mu.Lock()
	var sp *staticPrompt
	for _, x := range s.prompts {
		if x.p.Name == p.Name {
			sp = x
		}
	}
	src := s.opts.Prompts
	s.mu.Unlock()
	var res *GetPromptResult
	var err error
	switch {
	case sp != nil:
		for _, a := range sp.p.Arguments {
			if _, ok := p.Arguments[a.Name]; a.Required && !ok {
				return nil, invalidParams("missing required argument %q", a.Name)
			}
		}
		res, err = sp.h(ctx, p.Arguments)
	case src != nil:
		res, err = src.GetPrompt(ctx, p.Name, p.Arguments)
	default:
		err = ErrPromptNotFound
	}
	if errors.Is(err, ErrPromptNotFound) {
		return nil, invalidParams("Unknown prompt: %s", p.Name)
	}
	if err != nil {
		return nil, err
	}
	if res == nil {
		res = &GetPromptResult{}
	}
	if res.Messages == nil {
		res.Messages = []PromptMessage{}
	}
	fields := map[string]any{"messages": res.Messages}
	if res.Description != "" {
		fields["description"] = res.Description
	}
	return s.result(c, fields, false), nil
}

// toRPCError converts a handler error into the JSON-RPC error of a response.
func toRPCError(err error) *Error {
	if e, ok := errors.AsType[*Error](err); ok {
		return e
	}
	return &Error{Code: CodeInternalError, Message: err.Error()}
}

// encode returns the JSON-RPC response of a request with the given outcome.
func encode(id json.RawMessage, res any, err error) []byte {
	m := message{JSONRPC: "2.0", ID: id}
	if err == nil {
		b, merr := json.Marshal(res)
		if merr != nil {
			err = merr
		} else {
			m.Result = b
		}
	}
	if err != nil {
		m.Error = toRPCError(err)
	}
	b, _ := json.Marshal(m)
	return b
}

// notification encodes a notification.
func notification(method string, params any) []byte {
	m := message{JSONRPC: "2.0", Method: method}
	if params != nil {
		m.Params, _ = json.Marshal(params)
	}
	b, _ := json.Marshal(m)
	return b
}

// Close ends the server's subscriptions gracefully, ends its sessions and
// cancels the handlers waiting for client input. Serving continues for new
// requests; stop the transports to end it.
func (s *Server) Close() error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.closing)
	}
	sessions := make([]*session, 0, len(s.sessions))
	for ss := range s.sessions {
		sessions = append(sessions, ss)
	}
	conts := make([]*continuation, 0, len(s.conts))
	for _, k := range s.conts {
		conts = append(conts, k)
	}
	s.mu.Unlock()
	for _, ss := range sessions {
		ss.close()
	}
	for _, k := range conts {
		k.cancel()
		s.forget(k)
	}
	return nil
}

// newSessionID returns a cryptographically random session ID of visible
// ASCII characters: a version 4 UUID, as the Streamable HTTP transport
// suggests.
func newSessionID() string { return uuid.New().String() }
