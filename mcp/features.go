package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"unicode/utf8"
)

// This file holds the client features: sampling, elicitation and roots,
// which servers request from the client, and logging.
//
// With an initialization-based revision the client declares them in
// initialize and answers the server's sampling/createMessage,
// elicitation/create and roots/list requests. With ProtocolVersion the
// client declares them in the _meta of every request, and a server asks for
// them in the inputRequests of an input_required result; Call then runs the
// same handlers and retries the request with their inputResponses. See
// https://modelcontextprotocol.io/specification/2026-07-28/basic/patterns/mrtr.

// CodeURLElicitationRequired is the error code of a request that an
// initialization-based server cannot process until the user completes URL
// mode elicitations.
const CodeURLElicitationRequired = -32042

// codeUserRejected is the error code of a sampling request the user
// rejected.
const codeUserRejected = -1

// ErrSamplingRejected is wrapped by the error returned for a sampling
// request that the approval or review hook of WithSampling rejected, or
// that a SamplingHandler rejected by returning an error wrapping it.
var ErrSamplingRejected = errors.New("mcp: sampling request rejected")

// SamplingContent is a content block of a sampling message: text, image or
// audio, and with tool use a tool_use block in assistant messages or a
// tool_result block in user messages.
type SamplingContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// Data is the base64 data of an image or audio block.
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`

	// ID, Name and Input describe a tool_use block.
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// ToolUseID, Content, StructuredContent and IsError describe a
	// tool_result block.
	ToolUseID         string          `json:"toolUseId,omitempty"`
	Content           []Content       `json:"content,omitempty"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

// MarshalJSON implements json.Marshaler. It keeps the fields that text and
// tool_result blocks require even when they are empty.
func (b SamplingContent) MarshalJSON() ([]byte, error) {
	type plain SamplingContent
	switch {
	case b.Type == "text" && b.Text == "":
		return json.Marshal(struct {
			plain
			Text string `json:"text"`
		}{plain: plain(b)})
	case b.Type == "tool_result" && b.Content == nil:
		return json.Marshal(struct {
			plain
			Content []Content `json:"content"`
		}{plain: plain(b), Content: []Content{}})
	}
	return json.Marshal(plain(b))
}

// SamplingContents is the content of a sampling message. It decodes from a
// single block or an array of blocks, and encodes one block as an object
// and several as an array.
type SamplingContents []SamplingContent

// MarshalJSON implements json.Marshaler.
func (c SamplingContents) MarshalJSON() ([]byte, error) {
	if len(c) == 1 {
		return json.Marshal(c[0])
	}
	return json.Marshal([]SamplingContent(c))
}

// UnmarshalJSON implements json.Unmarshaler.
func (c *SamplingContents) UnmarshalJSON(b []byte) error {
	var one SamplingContent
	if json.Unmarshal(b, &one) == nil {
		*c = SamplingContents{one}
		return nil
	}
	var many []SamplingContent
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*c = many
	return nil
}

// SamplingMessage is a message of a sampling request.
type SamplingMessage struct {
	// Role is "user" or "assistant".
	Role    string           `json:"role"`
	Content SamplingContents `json:"content"`
}

// ModelHint suggests a model by name or name substring.
type ModelHint struct {
	Name string `json:"name,omitempty"`
}

// ModelPreferences are the server's advisory model preferences. The
// priorities range from 0 to 1.
type ModelPreferences struct {
	Hints                []ModelHint `json:"hints,omitempty"`
	CostPriority         *float64    `json:"costPriority,omitempty"`
	SpeedPriority        *float64    `json:"speedPriority,omitempty"`
	IntelligencePriority *float64    `json:"intelligencePriority,omitempty"`
}

// SamplingToolChoice controls tool use during sampling: Mode is "auto"
// (the default), "required" or "none".
type SamplingToolChoice struct {
	Mode string `json:"mode,omitempty"`
}

// SamplingRequest holds the parameters of sampling/createMessage.
type SamplingRequest struct {
	Messages         []SamplingMessage `json:"messages"`
	ModelPreferences *ModelPreferences `json:"modelPreferences,omitempty"`
	SystemPrompt     string            `json:"systemPrompt,omitempty"`
	// IncludeContext is "none", or the deprecated "thisServer" or
	// "allServers"; the client may ignore it.
	IncludeContext string   `json:"includeContext,omitempty"`
	Temperature    *float64 `json:"temperature,omitempty"`
	// MaxTokens is the most tokens to sample; the client must respect it.
	MaxTokens     int             `json:"maxTokens"`
	StopSequences []string        `json:"stopSequences,omitempty"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
	// Tools and ToolChoice are set only for a client that declared tool
	// use with SamplingTools.
	Tools      []Tool              `json:"tools,omitempty"`
	ToolChoice *SamplingToolChoice `json:"toolChoice,omitempty"`
}

// Stop reasons of a SamplingResult. Implementations may report others.
const (
	StopEndTurn      = "endTurn"
	StopSequence     = "stopSequence"
	StopMaxTokens    = "maxTokens"
	StopToolUse      = "toolUse"
	samplingRoleUser = "user"
)

// SamplingResult is the result of sampling/createMessage.
type SamplingResult struct {
	// Role is "assistant"; it defaults to that when empty.
	Role    string           `json:"role"`
	Content SamplingContents `json:"content"`
	// Model names the model that generated the message.
	Model      string `json:"model"`
	StopReason string `json:"stopReason,omitempty"`
}

// SamplingHandler generates a message for a sampling request.
type SamplingHandler func(ctx context.Context, req *SamplingRequest) (*SamplingResult, error)

// SamplingOption configures sampling.
type SamplingOption func(*samplingConfig)

type samplingConfig struct {
	handler SamplingHandler
	tools   bool
	approve func(context.Context, *SamplingRequest) error
	review  func(context.Context, *SamplingRequest, *SamplingResult) error
}

// SamplingTools declares the sampling.tools capability, so that servers may
// include tools and toolChoice in sampling requests. Without it, such
// requests are rejected.
func SamplingTools() SamplingOption { return func(s *samplingConfig) { s.tools = true } }

// ApproveSampling sets a hook that reviews every sampling request before
// the handler runs, as the specification asks clients to let a human do.
// An error rejects the request: an initialization-based server receives
// the error code -1, and with ProtocolVersion the request that asked for
// the sample fails with an error wrapping ErrSamplingRejected.
func ApproveSampling(fn func(ctx context.Context, req *SamplingRequest) error) SamplingOption {
	return func(s *samplingConfig) { s.approve = fn }
}

// ReviewSampling sets a hook that reviews every generated message before it
// is returned to the server. An error rejects it, as with ApproveSampling.
func ReviewSampling(fn func(ctx context.Context, req *SamplingRequest, res *SamplingResult) error) SamplingOption {
	return func(s *samplingConfig) { s.review = fn }
}

// WithSampling declares the sampling capability and answers the server's
// sampling/createMessage requests with h. See ModelSampler for a handler
// backed by an agentx model. Sampling is deprecated as of ProtocolVersion
// but still part of it.
func WithSampling(h SamplingHandler, opts ...SamplingOption) ClientOption {
	return func(c *Client) {
		s := &samplingConfig{handler: h}
		for _, opt := range opts {
			opt(s)
		}
		c.sampling = s
	}
}

// Elicitation modes.
const (
	ElicitationForm = "form"
	ElicitationURL  = "url"
)

// Elicitation actions.
const (
	ElicitAccept  = "accept"
	ElicitDecline = "decline"
	ElicitCancel  = "cancel"
)

// ElicitRequest holds the parameters of elicitation/create.
type ElicitRequest struct {
	// Mode is ElicitationForm or ElicitationURL. Requests without a mode
	// are form requests; the client fills in the mode.
	Mode    string `json:"mode,omitempty"`
	Message string `json:"message"`
	// RequestedSchema describes the form: a flat object of primitive
	// properties.
	RequestedSchema json.RawMessage `json:"requestedSchema,omitempty"`
	// URL is the page a URL mode elicitation asks the user to open. The
	// client must show it in full, must not open or prefetch it without the
	// user's consent, and must open it so that neither it nor the model can
	// see what the user enters.
	URL string `json:"url,omitempty"`
	// ElicitationID identifies a URL mode elicitation of revision
	// 2025-11-25. Later revisions have none.
	ElicitationID string `json:"elicitationId,omitempty"`
}

// ElicitResult is the user's answer to an elicitation.
type ElicitResult struct {
	// Action is ElicitAccept, ElicitDecline or ElicitCancel.
	Action string `json:"action"`
	// Content holds the submitted form data of an accepted form mode
	// elicitation: strings, numbers, booleans and string arrays.
	Content map[string]any `json:"content,omitempty"`
}

// ElicitationHandler asks the user for the information an elicitation
// requests. For a URL mode elicitation, accepting means that the user
// consented to open the URL, not that the interaction is complete.
type ElicitationHandler func(ctx context.Context, req *ElicitRequest) (*ElicitResult, error)

// WithElicitation declares the elicitation capability for the given modes,
// ElicitationForm if none are given, and answers the server's
// elicitation/create requests with h. Requests for other modes are
// rejected. The content of accepted form elicitations is checked against
// the requested schema before it is sent.
//
// With URL mode and an initialization-based revision, a request that fails
// with CodeURLElicitationRequired runs h for each elicitation in the
// error. If the user accepts all of them, the client waits, bounded by the
// request's context, until the server reports them complete with
// notifications/elicitation/complete and then issues the request again
// once. Otherwise the request fails with the server's error.
func WithElicitation(h ElicitationHandler, modes ...string) ClientOption {
	return func(c *Client) {
		c.elicit = h
		c.elicitModes = slices.Compact(slices.Sorted(slices.Values(modes)))
		if len(c.elicitModes) == 0 {
			c.elicitModes = []string{ElicitationForm}
		}
	}
}

// Root is a directory or file the client exposes to servers.
type Root struct {
	// URI must be a file:// URI.
	URI  string `json:"uri"`
	Name string `json:"name,omitempty"`
}

// RootsFunc returns the current roots.
type RootsFunc func(ctx context.Context) ([]Root, error)

// StaticRoots returns a RootsFunc that always returns roots.
func StaticRoots(roots ...Root) RootsFunc {
	roots = slices.Clone(roots)
	return func(context.Context) ([]Root, error) { return slices.Clone(roots), nil }
}

// WithRoots declares the roots capability and answers the server's
// roots/list requests with fn. Call Client.RootsChanged when the roots
// change. Roots are deprecated as of ProtocolVersion but still part of it.
func WithRoots(fn RootsFunc) ClientOption {
	return func(c *Client) { c.roots = fn }
}

// LogLevel is a syslog severity level of server log messages.
type LogLevel string

// Log levels, from the least to the most severe.
const (
	LogDebug     LogLevel = "debug"
	LogInfo      LogLevel = "info"
	LogNotice    LogLevel = "notice"
	LogWarning   LogLevel = "warning"
	LogError     LogLevel = "error"
	LogCritical  LogLevel = "critical"
	LogAlert     LogLevel = "alert"
	LogEmergency LogLevel = "emergency"
)

var logLevels = []LogLevel{LogDebug, LogInfo, LogNotice, LogWarning, LogError, LogCritical, LogAlert, LogEmergency}

// LogMessage is a server log message from notifications/message.
type LogMessage struct {
	Level  LogLevel        `json:"level"`
	Logger string          `json:"logger,omitempty"`
	Data   json.RawMessage `json:"data"`
}

// WithLogHandler delivers the server's log messages
// (notifications/message) to fn. It runs on the receive goroutine and must
// not block. Servers send log messages only once a level is set: see
// WithLogLevel and Client.SetLogLevel.
func WithLogHandler(fn func(LogMessage)) ClientOption {
	return func(c *Client) { c.onLog = fn }
}

// WithLogLevel sets the minimum level of the log messages the server
// sends, as Client.SetLogLevel does, from the start of the connection and,
// with an initialization-based revision, of every renewed session. An
// invalid level makes Connect fail.
func WithLogLevel(level LogLevel) ClientOption {
	return func(c *Client) { c.logLevel = level }
}

func validLogLevel(level LogLevel) error {
	if !slices.Contains(logLevels, level) {
		return fmt.Errorf("mcp: invalid log level %q", level)
	}
	return nil
}

// capabilities returns the client capabilities, in the form of an
// initialization-based revision if legacy is set.
func (c *Client) capabilities(legacy bool) map[string]any {
	caps := map[string]any{}
	if c.roots != nil {
		if legacy {
			caps["roots"] = map[string]any{"listChanged": true}
		} else {
			caps["roots"] = map[string]any{}
		}
	}
	if c.sampling != nil {
		s := map[string]any{}
		if c.sampling.tools {
			s["tools"] = map[string]any{}
		}
		caps["sampling"] = s
	}
	if c.elicit != nil {
		e := map[string]any{}
		for _, m := range c.elicitModes {
			e[m] = map[string]any{}
		}
		caps["elicitation"] = e
	}
	return caps
}

// errNoHandler reports a server request the client declares no capability
// for.
var errNoHandler = errors.New("mcp: no handler")

// handles reports whether the client answers method.
func (c *Client) handles(method string) bool {
	switch method {
	case "sampling/createMessage":
		return c.sampling != nil
	case "elicitation/create":
		return c.elicit != nil
	case "roots/list":
		return c.roots != nil
	}
	return false
}

// serve answers a sampling, elicitation or roots request from the server.
// It returns errNoHandler for methods the client declares no capability
// for, and an *Error for invalid requests.
func (c *Client) serve(ctx context.Context, method string, params json.RawMessage) (any, error) {
	if !c.handles(method) {
		return nil, errNoHandler
	}
	switch method {
	case "sampling/createMessage":
		return c.sample(ctx, params)
	case "elicitation/create":
		return c.elicitation(ctx, params)
	default:
		return c.listRoots(ctx)
	}
}

func invalidParams(format string, args ...any) *Error {
	return &Error{Code: CodeInvalidParams, Message: fmt.Sprintf(format, args...)}
}

func (c *Client) sample(ctx context.Context, params json.RawMessage) (*SamplingResult, error) {
	var req SamplingRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, invalidParams("invalid sampling request: %v", err)
	}
	if (len(req.Tools) > 0 || req.ToolChoice != nil) && !c.sampling.tools {
		return nil, invalidParams("sampling with tools requires the sampling.tools capability")
	}
	if err := checkToolResults(req.Messages); err != nil {
		return nil, err
	}
	s := c.sampling
	if s.approve != nil {
		if err := s.approve(ctx, &req); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrSamplingRejected, err)
		}
	}
	res, err := s.handler(ctx, &req)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, errors.New("mcp: sampling handler returned no result")
	}
	if res.Role == "" {
		res.Role = "assistant"
	}
	if s.review != nil {
		if err := s.review(ctx, &req, res); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrSamplingRejected, err)
		}
	}
	return res, nil
}

// checkToolResults enforces that a message with tool results holds nothing
// else.
func checkToolResults(msgs []SamplingMessage) error {
	for i, m := range msgs {
		results := 0
		for _, b := range m.Content {
			if b.Type == "tool_result" {
				results++
			}
		}
		if results > 0 && results != len(m.Content) {
			return invalidParams("sampling message %d mixes tool results with other content", i)
		}
	}
	return nil
}

func (c *Client) elicitation(ctx context.Context, params json.RawMessage) (*ElicitResult, error) {
	var req ElicitRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, invalidParams("invalid elicitation request: %v", err)
	}
	if req.Mode == "" {
		req.Mode = ElicitationForm
	}
	if !slices.Contains(c.elicitModes, req.Mode) {
		return nil, invalidParams("elicitation mode %q is not supported", req.Mode)
	}
	if req.Mode == ElicitationURL && req.URL == "" {
		return nil, invalidParams("url mode elicitation without a url")
	}
	res, err := c.elicit(ctx, &req)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, errors.New("mcp: elicitation handler returned no result")
	}
	switch res.Action {
	case ElicitAccept:
		if req.Mode == ElicitationURL {
			res.Content = nil
		} else if err := validateForm(req.RequestedSchema, res.Content); err != nil {
			return nil, fmt.Errorf("mcp: elicitation content: %w", err)
		}
	case ElicitDecline, ElicitCancel:
		res.Content = nil
	default:
		return nil, fmt.Errorf("mcp: invalid elicitation action %q", res.Action)
	}
	return res, nil
}

func (c *Client) listRoots(ctx context.Context) (any, error) {
	roots, err := c.roots(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range roots {
		if !strings.HasPrefix(r.URI, "file://") {
			return nil, fmt.Errorf("mcp: root %q is not a file:// URI", r.URI)
		}
	}
	if roots == nil {
		roots = []Root{}
	}
	return map[string]any{"roots": roots}, nil
}

// propertySchema is the subset of JSON Schema elicitation forms use.
type propertySchema struct {
	Type      string   `json:"type"`
	Enum      []any    `json:"enum"`
	OneOf     []option `json:"oneOf"`
	MinLength *int     `json:"minLength"`
	MaxLength *int     `json:"maxLength"`
	Minimum   *float64 `json:"minimum"`
	Maximum   *float64 `json:"maximum"`
	MinItems  *int     `json:"minItems"`
	MaxItems  *int     `json:"maxItems"`
	Items     *struct {
		Enum  []any    `json:"enum"`
		AnyOf []option `json:"anyOf"`
	} `json:"items"`
}

type option struct {
	Const any `json:"const"`
}

// validateForm checks submitted form content against the requested schema,
// a flat object of primitive properties.
func validateForm(schema json.RawMessage, content map[string]any) error {
	var s struct {
		Properties map[string]propertySchema `json:"properties"`
		Required   []string                  `json:"required"`
	}
	if len(schema) > 0 {
		if err := json.Unmarshal(schema, &s); err != nil {
			return fmt.Errorf("invalid requested schema: %w", err)
		}
	}
	for _, name := range s.Required {
		if _, ok := content[name]; !ok {
			return fmt.Errorf("missing required field %q", name)
		}
	}
	for name, v := range content {
		p, ok := s.Properties[name]
		if !ok {
			return fmt.Errorf("unknown field %q", name)
		}
		if err := p.check(v); err != nil {
			return fmt.Errorf("field %q: %w", name, err)
		}
	}
	return nil
}

func (p propertySchema) check(v any) error {
	switch p.Type {
	case "string":
		s, ok := v.(string)
		if !ok {
			return errors.New("not a string")
		}
		n := utf8.RuneCountInString(s)
		if p.MinLength != nil && n < *p.MinLength || p.MaxLength != nil && n > *p.MaxLength {
			return errors.New("length out of range")
		}
		if !allowed(s, p.Enum, p.OneOf) {
			return fmt.Errorf("%q is not an allowed value", s)
		}
	case "number", "integer":
		f, ok := number(v)
		if !ok || p.Type == "integer" && f != math.Trunc(f) {
			return fmt.Errorf("not of type %s", p.Type)
		}
		if p.Minimum != nil && f < *p.Minimum || p.Maximum != nil && f > *p.Maximum {
			return errors.New("out of range")
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return errors.New("not a boolean")
		}
	case "array":
		items, ok := stringArray(v)
		if !ok {
			return errors.New("not an array of strings")
		}
		if p.MinItems != nil && len(items) < *p.MinItems || p.MaxItems != nil && len(items) > *p.MaxItems {
			return errors.New("item count out of range")
		}
		if p.Items != nil {
			for _, s := range items {
				if !allowed(s, p.Items.Enum, p.Items.AnyOf) {
					return fmt.Errorf("%q is not an allowed value", s)
				}
			}
		}
	default:
		return fmt.Errorf("unsupported schema type %q", p.Type)
	}
	return nil
}

func allowed(s string, enum []any, options []option) bool {
	if len(enum) == 0 && len(options) == 0 {
		return true
	}
	for _, e := range enum {
		if e == s {
			return true
		}
	}
	for _, o := range options {
		if o.Const == s {
			return true
		}
	}
	return false
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func stringArray(v any) ([]string, bool) {
	switch a := v.(type) {
	case []string:
		return a, true
	case []any:
		out := make([]string, len(a))
		for i, e := range a {
			s, ok := e.(string)
			if !ok {
				return nil, false
			}
			out[i] = s
		}
		return out, true
	}
	return nil, false
}

// inputRequest is an entry of the inputRequests of an input_required
// result.
type inputRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// fulfill runs the handlers for the inputRequests of an input_required
// result and returns the inputResponses. It returns an
// *InputRequiredError, without running any handler, if the client does not
// handle every request.
func (c *Client) fulfill(ctx context.Context, requests map[string]json.RawMessage, state *string) (map[string]any, error) {
	parsed := make(map[string]inputRequest, len(requests))
	for key, raw := range requests {
		var r inputRequest
		if err := json.Unmarshal(raw, &r); err != nil || !c.handles(r.Method) {
			return nil, &InputRequiredError{InputRequests: requests, RequestState: deref(state)}
		}
		parsed[key] = r
	}
	responses := make(map[string]any, len(parsed))
	for _, key := range slices.Sorted(maps.Keys(parsed)) {
		r := parsed[key]
		res, err := c.serve(ctx, r.Method, r.Params)
		if err != nil {
			return nil, fmt.Errorf("mcp: %s: %w", r.Method, err)
		}
		responses[key] = res
	}
	return responses, nil
}
