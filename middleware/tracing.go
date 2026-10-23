package middleware

import (
	"cmp"
	"context"
	"errors"
	"iter"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/tool"
)

// Attr is a span attribute.
type Attr struct {
	Key   string
	Value any
}

// SpanKind is the kind of a span, as in OpenTelemetry.
type SpanKind int

const (
	// SpanKindInternal marks work done in the process, such as agent runs
	// and tool calls.
	SpanKindInternal SpanKind = iota
	// SpanKindClient marks calls to a remote service, such as model calls.
	SpanKindClient
)

// Span is an in-progress trace span.
type Span interface {
	SetAttributes(attrs ...Attr)
	// RecordError records err and marks the span as failed: adapters should
	// set the span status to error.
	RecordError(err error)
	End()
}

// EventSpan is a Span that records events. Spans that do not implement it
// drop the events of this package, such as the retries of a model call.
type EventSpan interface {
	Span
	AddEvent(name string, attrs ...Attr)
}

// Tracer starts spans. Adapt OpenTelemetry or another tracing library to
// it; agentx itself has no tracing dependency.
//
// Spans follow the OpenTelemetry GenAI semantic conventions
// (https://github.com/open-telemetry/semantic-conventions-genai):
//
//   - "invoke_agent {agent}" (internal) covers a run of the agent loop;
//   - "{operation} {model}" (client), such as "chat gpt-6-astra", covers a
//     model call;
//   - "execute_tool {tool}" (internal) covers a tool call. A sub-agent run
//     by agent.AsTool is a child of its execute_tool span.
//
// Work the conventions do not cover gets agentx spans: "hook {event}" for
// the hooks of a lifecycle event and "compact_context" for an automatic
// compaction. Retries of a model call are "agentx.retry" events.
//
// Attributes use the gen_ai.* names of the conventions, plus agentx.run_id,
// agentx.turn and agentx.run.status. The span of an agent a run was handed
// off to has agentx.handoff.from, the agent that handed it off; a run that
// a guardrail blocked has agentx.guardrail.name and agentx.guardrail.kind
// and an "agentx.guardrail" event, which also holds the guardrail's
// message when content is recorded. A failed operation sets error.type to
// the provider's error code, "timeout", "canceled", "panic", "tool_error"
// for a tool that returned an error result, or "_OTHER".
type Tracer interface {
	Start(ctx context.Context, name string, kind SpanKind, attrs ...Attr) (context.Context, Span)
}

// NoopTracer discards spans.
type NoopTracer struct{}

// Start implements Tracer.
func (NoopTracer) Start(ctx context.Context, _ string, _ SpanKind, _ ...Attr) (context.Context, Span) {
	return ctx, noopSpan{}
}

type noopSpan struct{}

func (noopSpan) SetAttributes(...Attr) {}
func (noopSpan) RecordError(error)     {}
func (noopSpan) End()                  {}

// TraceOption configures tracing.
type TraceOption func(*traceConfig)

type traceConfig struct {
	tracer  Tracer
	content bool
}

// RecordContent records the content of model and tool calls: the system
// prompt, messages and tool names of model calls and runs
// (gen_ai.system_instructions, gen_ai.input.messages,
// gen_ai.output.messages and gen_ai.tool.definitions, as JSON strings), and
// the arguments and results of tool calls (gen_ai.tool.call.arguments and
// gen_ai.tool.call.result). They may contain sensitive data and can be
// large, so the conventions make them opt-in.
func RecordContent() TraceOption { return func(c *traceConfig) { c.content = true } }

func newTraceConfig(t Tracer, opts []TraceOption) *traceConfig {
	c := &traceConfig{tracer: t}
	if c.tracer == nil {
		c.tracer = NoopTracer{}
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Tracing returns runtime middleware that records an invoke_agent span for
// each run, with the spans of TraceModel and TraceTools as its children.
// The spans carry the agent name, the session ID as
// gen_ai.conversation.id, the run ID and the turn.
//
// The run's context carries the tracer, so hooks and compaction add their
// spans with StartSpan, and a sub-agent traced the same way nests under
// the execute_tool span that runs it.
func Tracing(t Tracer, opts ...TraceOption) loop.Middleware {
	return tracing{newTraceConfig(t, opts)}
}

type tracing struct{ cfg *traceConfig }

func (tracing) Name() string { return "tracing" }

// runTrace is the tracing state of a run, carried in its context.
type runTrace struct {
	cfg  *traceConfig
	run  *loop.Run
	span Span
	// compacted is set once the run compacted its conversation.
	compacted atomic.Bool
}

type (
	runTraceKey struct{}
	spanKey     struct{}
)

func runTraceFrom(ctx context.Context) *runTrace {
	rt, _ := ctx.Value(runTraceKey{}).(*runTrace)
	return rt
}

func withSpan(ctx context.Context, span Span) context.Context {
	return context.WithValue(ctx, spanKey{}, span)
}

// SpanFromContext returns the innermost span the tracing of this package
// started in ctx, or a span that discards everything.
func SpanFromContext(ctx context.Context) Span {
	if span, ok := ctx.Value(spanKey{}).(Span); ok {
		return span
	}
	return noopSpan{}
}

// AddEvent records an event on the span of ctx, if it is an EventSpan.
func AddEvent(ctx context.Context, name string, attrs ...Attr) {
	if span, ok := SpanFromContext(ctx).(EventSpan); ok {
		span.AddEvent(name, attrs...)
	}
}

// StartSpan starts a span with the tracer of the run ctx belongs to, as
// installed by Tracing, for work done inside the run, such as hooks. Without
// such a tracer it returns ctx and a span that discards everything.
func StartSpan(ctx context.Context, name string, kind SpanKind, attrs ...Attr) (context.Context, Span) {
	rt := runTraceFrom(ctx)
	if rt == nil {
		return ctx, noopSpan{}
	}
	ctx, span := rt.cfg.tracer.Start(ctx, name, kind, attrs...)
	return withSpan(ctx, span), span
}

// RecordSpanError records err on span with a low-cardinality error.type,
// as the spans of this package do.
func RecordSpanError(span Span, err error) { recordError(span, err) }

// MarkCompacted records that run compacted its conversation; ctx must
// belong to the run. Its later model spans carry
// gen_ai.conversation.compacted.
func MarkCompacted(ctx context.Context, run *loop.Run) {
	if rt := runTraceFrom(ctx); rt != nil && rt.run == run {
		rt.compacted.Store(true)
	}
}

func (tr tracing) WrapContext(ctx context.Context, run *loop.Run) (context.Context, func()) {
	name := "invoke_agent"
	if run.Agent() != "" {
		name += " " + run.Agent()
	}
	attrs := append([]Attr{{"gen_ai.operation.name", "invoke_agent"}}, agentAttrs(run)...)
	if from, _ := run.Value(loop.HandoffFromKey); from != "" {
		attrs = append(attrs, Attr{"agentx.handoff.from", from})
	}
	ctx, span := tr.cfg.tracer.Start(ctx, name, SpanKindInternal, attrs...)
	ctx = context.WithValue(ctx, runTraceKey{}, &runTrace{cfg: tr.cfg, run: run, span: span})
	return withSpan(ctx, span), span.End
}

func (tr tracing) AfterRun(ctx context.Context, run *loop.Run, res *loop.Result) {
	rt := runTraceFrom(ctx)
	if rt == nil || rt.run != run || res == nil {
		return
	}
	span := rt.span
	// The conventions put only the input and output totals on agent spans:
	// cache and reasoning breakdowns aggregated over several models
	// mislead.
	attrs := []Attr{
		{"gen_ai.usage.input_tokens", res.Usage.PromptTokens()},
		{"gen_ai.usage.output_tokens", res.Usage.OutputTokens},
		{"agentx.run.status", string(res.Status)},
	}
	if res.FinishReason != "" {
		attrs = append(attrs, Attr{"gen_ai.response.finish_reasons", []string{string(res.FinishReason)}})
	}
	if g := res.Guardrail; g != nil {
		ga := []Attr{{"agentx.guardrail.name", g.Name}}
		if g.Kind != "" {
			ga = append(ga, Attr{"agentx.guardrail.kind", g.Kind})
		}
		attrs = append(attrs, ga...)
		if tr.cfg.content && g.Message != "" {
			ga = append(ga, Attr{"agentx.guardrail.message", g.Message})
		}
		if es, ok := span.(EventSpan); ok {
			es.AddEvent("agentx.guardrail", ga...)
		}
	}
	if tr.cfg.content {
		attrs = append(attrs, runContent(run, res)...)
	}
	span.SetAttributes(attrs...)
	if res.Err != nil {
		recordError(span, res.Err)
	}
}

// runContent returns the content attributes of a run: its input, the
// messages before the model's first answer, and its last answer.
func runContent(run *loop.Run, res *loop.Result) []Attr {
	var input []*model.Message
	var output *model.Message
	for i, msg := range res.NewMessages {
		if msg != nil && msg.Role == model.RoleAssistant {
			if input == nil {
				input = res.NewMessages[:i:i]
			}
			output = msg
		}
	}
	if input == nil && output == nil {
		input = res.NewMessages
	}
	attrs := requestContent(run.System(), input, run.Tools())
	return append(attrs, outputContent(output)...)
}

func (tr tracing) WrapModel(run *loop.Run, next model.Model) model.Model {
	compacted := func(ctx context.Context) bool {
		rt := runTraceFrom(ctx)
		return rt != nil && rt.run == run && rt.compacted.Load()
	}
	return traceModel(tr.cfg, runAttrs(run), compacted)(next)
}

func (tr tracing) WrapTool(run *loop.Run, next tool.Invoker) tool.Invoker {
	return traceTools(tr.cfg, runAttrs(run))(next)
}

// agentAttrs identify the agent and conversation of a run.
func agentAttrs(run *loop.Run) []Attr {
	var attrs []Attr
	if run.Agent() != "" {
		attrs = append(attrs, Attr{"gen_ai.agent.name", run.Agent()})
	}
	if run.SessionID() != "" {
		attrs = append(attrs, Attr{"gen_ai.conversation.id", run.SessionID()})
	}
	return append(attrs, Attr{"agentx.run_id", run.ID()})
}

func runAttrs(run *loop.Run) []Attr {
	return append(agentAttrs(run), Attr{"agentx.turn", run.Turn()})
}

// TraceModel records a span for each model call. The provider, operation
// and default model come from the model's model.Describer.
func TraceModel(t Tracer, opts ...TraceOption) Model {
	return traceModel(newTraceConfig(t, opts), nil, nil)
}

// traceModel traces model calls; compacted, if non-nil, reports whether the
// run of a call's context compacted its conversation.
func traceModel(cfg *traceConfig, extra []Attr, compacted func(context.Context) bool) Model {
	return func(next model.Model) model.Model {
		start := func(ctx context.Context, req *model.Request, stream bool) (context.Context, Span) {
			name, attrs := requestAttrs(model.Describe(next), req, stream)
			if compacted != nil && compacted(ctx) {
				// The conventions set it only when compaction is certain.
				attrs = append(attrs, Attr{"gen_ai.conversation.compacted", true})
			}
			if cfg.content {
				attrs = append(attrs, requestContent(req.System, req.Messages, req.Tools)...)
			}
			ctx, span := cfg.tracer.Start(ctx, name, SpanKindClient, append(attrs, extra...)...)
			return withSpan(ctx, span), span
		}
		return &Funcs{
			Next: next,
			GenerateFunc: func(ctx context.Context, req *model.Request) (*model.Response, error) {
				ctx, span := start(ctx, req, false)
				defer span.End()
				resp, err := next.Generate(ctx, req)
				if err != nil {
					recordError(span, err)
					return resp, err
				}
				span.SetAttributes(responseAttrs(resp)...)
				if cfg.content && resp != nil {
					span.SetAttributes(outputContent(resp.Message)...)
				}
				return resp, nil
			},
			StreamFunc: func(ctx context.Context, req *model.Request) iter.Seq2[*model.StreamEvent, error] {
				return func(yield func(*model.StreamEvent, error) bool) {
					began := time.Now()
					ctx, span := start(ctx, req, true)
					defer span.End()
					first := true
					var acc model.Accumulator
					for ev, err := range next.Stream(ctx, req) {
						switch {
						case err != nil:
							recordError(span, err)
						case ev != nil && ev.Type == model.StreamDelta:
							if first {
								first = false
								span.SetAttributes(Attr{"gen_ai.response.time_to_first_chunk", time.Since(began).Seconds()})
							}
							if cfg.content {
								acc.Add(ev)
							}
						case ev != nil && ev.Type == model.StreamDone:
							span.SetAttributes(responseAttrs(ev.Response)...)
							if cfg.content {
								msg := acc.Message()
								if ev.Response != nil && ev.Response.Message != nil {
									msg = ev.Response.Message
								}
								span.SetAttributes(outputContent(msg)...)
							}
						}
						if !yield(ev, err) {
							return
						}
					}
				}
			},
		}
	}
}

// requestAttrs returns the span name and attributes of a model call.
func requestAttrs(d model.Description, req *model.Request, stream bool) (string, []Attr) {
	op := cmp.Or(d.Operation, "chat")
	name := op
	attrs := []Attr{{"gen_ai.operation.name", op}}
	if d.Provider != "" {
		attrs = append(attrs, Attr{"gen_ai.provider.name", d.Provider})
	}
	if m := cmp.Or(req.Model, d.Model); m != "" {
		name += " " + m
		attrs = append(attrs, Attr{"gen_ai.request.model", m})
	}
	if d.ServerAddress != "" {
		attrs = append(attrs, Attr{"server.address", d.ServerAddress}, Attr{"server.port", d.ServerPort})
	}
	if req.MaxTokens > 0 {
		attrs = append(attrs, Attr{"gen_ai.request.max_tokens", req.MaxTokens})
	}
	if req.Temperature != nil {
		attrs = append(attrs, Attr{"gen_ai.request.temperature", *req.Temperature})
	}
	if req.TopP != nil {
		attrs = append(attrs, Attr{"gen_ai.request.top_p", *req.TopP})
	}
	if len(req.Stop) > 0 {
		attrs = append(attrs, Attr{"gen_ai.request.stop_sequences", req.Stop})
	}
	if r := req.Reasoning; r != nil && r.Effort != "" {
		attrs = append(attrs, Attr{"gen_ai.request.reasoning.level", r.Effort})
	}
	if req.ResponseFormat != nil {
		attrs = append(attrs, Attr{"gen_ai.output.type", "json"})
	}
	if stream {
		// The conventions set this attribute only for streaming calls.
		attrs = append(attrs, Attr{"gen_ai.request.stream", true})
	}
	return name, attrs
}

func responseAttrs(resp *model.Response) []Attr {
	if resp == nil {
		return nil
	}
	var attrs []Attr
	if resp.ID != "" {
		attrs = append(attrs, Attr{"gen_ai.response.id", resp.ID})
	}
	if resp.Model != "" {
		attrs = append(attrs, Attr{"gen_ai.response.model", resp.Model})
	}
	reason := string(resp.FinishReason)
	if resp.FinishReason == "" || resp.FinishReason == model.FinishUnknown {
		// The conventions report a missing finish reason as "error".
		reason = "error"
	}
	attrs = append(attrs, Attr{"gen_ai.response.finish_reasons", []string{reason}})
	return append(attrs, usageAttrs(resp.Usage)...)
}

// usageAttrs reports usage as the conventions define it: input tokens
// include cached ones, and output tokens include reasoning.
func usageAttrs(u model.Usage) []Attr {
	attrs := []Attr{
		{"gen_ai.usage.input_tokens", u.PromptTokens()},
		{"gen_ai.usage.output_tokens", u.OutputTokens},
	}
	if u.CacheReadTokens > 0 {
		attrs = append(attrs, Attr{"gen_ai.usage.cache_read.input_tokens", u.CacheReadTokens})
	}
	if u.CacheWriteTokens > 0 {
		attrs = append(attrs, Attr{"gen_ai.usage.cache_write.input_tokens", u.CacheWriteTokens})
	}
	if u.ReasoningTokens > 0 {
		attrs = append(attrs, Attr{"gen_ai.usage.reasoning.output_tokens", u.ReasoningTokens})
	}
	return attrs
}

// TraceTools records a span for each tool call.
func TraceTools(t Tracer, opts ...TraceOption) tool.Middleware {
	return traceTools(newTraceConfig(t, opts), nil)
}

// errToolResult marks the span of a tool that returned an error result.
var errToolResult = errors.New("tool returned an error result")

func traceTools(cfg *traceConfig, extra []Attr) tool.Middleware {
	return func(next tool.Invoker) tool.Invoker {
		return func(ctx context.Context, call *tool.Call) (*tool.Result, error) {
			attrs := []Attr{
				{"gen_ai.operation.name", "execute_tool"},
				{"gen_ai.tool.name", call.Name},
				{"gen_ai.tool.type", "function"},
			}
			if call.ID != "" {
				attrs = append(attrs, Attr{"gen_ai.tool.call.id", call.ID})
			}
			if call.Tool != nil {
				if d := call.Tool.Info().Description; d != "" {
					attrs = append(attrs, Attr{"gen_ai.tool.description", d})
				}
			}
			if cfg.content {
				attrs = append(attrs, Attr{"gen_ai.tool.call.arguments", string(call.Arguments)})
			}
			ctx, span := cfg.tracer.Start(ctx, "execute_tool "+call.Name, SpanKindInternal, append(attrs, extra...)...)
			defer span.End()
			ctx = withSpan(ctx, span)
			res, err := next(ctx, call)
			switch {
			case err != nil:
				recordError(span, err)
			case res != nil && res.IsError:
				span.SetAttributes(Attr{"error.type", "tool_error"})
				span.RecordError(errToolResult)
			case res != nil && cfg.content:
				span.SetAttributes(Attr{"gen_ai.tool.call.result", res.Content})
			}
			return res, err
		}
	}
}

func recordError(span Span, err error) {
	span.SetAttributes(Attr{"error.type", ErrorType(err)})
	span.RecordError(err)
}

// ErrorType returns the low-cardinality error.type the spans and metrics
// of this package record for err: "timeout", "canceled", the provider's
// error type or HTTP status code, "panic" or "_OTHER". Middleware that
// records errors, such as retries, uses it to stay consistent.
func ErrorType(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	if e, ok := errors.AsType[*model.APIError](err); ok {
		if e.Type != "" {
			return e.Type
		}
		return strconv.Itoa(e.StatusCode)
	}
	if _, ok := errors.AsType[*tool.PanicError](err); ok {
		return "panic"
	}
	return "_OTHER"
}
