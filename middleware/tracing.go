package middleware

import (
	"cmp"
	"context"
	"errors"
	"iter"
	"time"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/telemetry"
	"github.com/nostosx/agentx/tool"
)

// TraceOption configures tracing.
type TraceOption func(*traceConfig)

type traceConfig struct {
	tracer  telemetry.Tracer
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

func newTraceConfig(t telemetry.Tracer, opts []TraceOption) *traceConfig {
	c := &traceConfig{tracer: t}
	if c.tracer == nil {
		c.tracer = telemetry.NoopTracer{}
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
// spans with telemetry.StartSpan, and a sub-agent traced the same way nests
// under the execute_tool span that runs it. telemetry.Tracer lists the spans
// and their attributes.
func Tracing(t telemetry.Tracer, opts ...TraceOption) loop.Middleware {
	return tracing{newTraceConfig(t, opts)}
}

type tracing struct{ cfg *traceConfig }

func (tracing) Name() string { return "tracing" }

// runTrace is the tracing state of a run, carried in its context.
type runTrace struct {
	run  *loop.Run
	span telemetry.Span
}

type runTraceKey struct{}

func runTraceFrom(ctx context.Context) *runTrace {
	rt, _ := ctx.Value(runTraceKey{}).(*runTrace)
	return rt
}

func (tr tracing) WrapContext(ctx context.Context, run *loop.Run) (context.Context, func()) {
	name := "invoke_agent"
	if run.Agent() != "" {
		name += " " + run.Agent()
	}
	attrs := append([]telemetry.Attr{{Key: "gen_ai.operation.name", Value: "invoke_agent"}}, agentAttrs(run)...)
	if from, _ := run.Value(loop.HandoffFromKey); from != "" {
		attrs = append(attrs, telemetry.Attr{Key: "agentx.handoff.from", Value: from})
	}
	ctx, span := tr.cfg.tracer.Start(ctx, name, telemetry.SpanKindInternal, attrs...)
	ctx = telemetry.ContextWithRun(ctx, tr.cfg.tracer, run.ID())
	ctx = context.WithValue(ctx, runTraceKey{}, &runTrace{run: run, span: span})
	return telemetry.ContextWithSpan(ctx, span), span.End
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
	attrs := []telemetry.Attr{
		{Key: "gen_ai.usage.input_tokens", Value: res.Usage.PromptTokens()},
		{Key: "gen_ai.usage.output_tokens", Value: res.Usage.OutputTokens},
		{Key: "agentx.run.status", Value: string(res.Status)},
	}
	if res.FinishReason != "" {
		attrs = append(attrs, telemetry.Attr{Key: "gen_ai.response.finish_reasons", Value: []string{string(res.FinishReason)}})
	}
	if g := res.Guardrail; g != nil {
		ga := []telemetry.Attr{{Key: "agentx.guardrail.name", Value: g.Name}}
		if g.Kind != "" {
			ga = append(ga, telemetry.Attr{Key: "agentx.guardrail.kind", Value: g.Kind})
		}
		attrs = append(attrs, ga...)
		if tr.cfg.content && g.Message != "" {
			ga = append(ga, telemetry.Attr{Key: "agentx.guardrail.message", Value: g.Message})
		}
		if es, ok := span.(telemetry.EventSpan); ok {
			es.AddEvent("agentx.guardrail", ga...)
		}
	}
	if tr.cfg.content {
		attrs = append(attrs, runContent(run, res)...)
	}
	span.SetAttributes(attrs...)
	if res.Err != nil {
		telemetry.RecordSpanError(span, res.Err)
	}
}

// runContent returns the content attributes of a run: its input, the
// messages before the model's first answer, and its last answer.
func runContent(run *loop.Run, res *loop.Result) []telemetry.Attr {
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
	compacted := func(ctx context.Context) bool { return telemetry.Compacted(ctx, run.ID()) }
	return traceModel(tr.cfg, runAttrs(run), compacted)(next)
}

func (tr tracing) WrapTool(run *loop.Run, next tool.Invoker) tool.Invoker {
	return traceTools(tr.cfg, runAttrs(run))(next)
}

// agentAttrs identify the agent and conversation of a run.
func agentAttrs(run *loop.Run) []telemetry.Attr {
	var attrs []telemetry.Attr
	if run.Agent() != "" {
		attrs = append(attrs, telemetry.Attr{Key: "gen_ai.agent.name", Value: run.Agent()})
	}
	if run.SessionID() != "" {
		attrs = append(attrs, telemetry.Attr{Key: "gen_ai.conversation.id", Value: run.SessionID()})
	}
	return append(attrs, telemetry.Attr{Key: "agentx.run_id", Value: run.ID()})
}

func runAttrs(run *loop.Run) []telemetry.Attr {
	return append(agentAttrs(run), telemetry.Attr{Key: "agentx.turn", Value: run.Turn()})
}

// TraceModel records a span for each model call. The provider, operation
// and default model come from the model's model.Describer.
func TraceModel(t telemetry.Tracer, opts ...TraceOption) Model {
	return traceModel(newTraceConfig(t, opts), nil, nil)
}

// traceModel traces model calls; compacted, if non-nil, reports whether the
// run of a call's context compacted its conversation.
func traceModel(cfg *traceConfig, extra []telemetry.Attr, compacted func(context.Context) bool) Model {
	return func(next model.Model) model.Model {
		start := func(ctx context.Context, req *model.Request, stream bool) (context.Context, telemetry.Span) {
			name, attrs := requestAttrs(model.Describe(next), req, stream)
			if compacted != nil && compacted(ctx) {
				// The conventions set it only when compaction is certain.
				attrs = append(attrs, telemetry.Attr{Key: "gen_ai.conversation.compacted", Value: true})
			}
			if cfg.content {
				attrs = append(attrs, requestContent(req.System, req.Messages, req.Tools)...)
			}
			ctx, span := cfg.tracer.Start(ctx, name, telemetry.SpanKindClient, append(attrs, extra...)...)
			return telemetry.ContextWithSpan(ctx, span), span
		}
		return &Funcs{
			Next: next,
			GenerateFunc: func(ctx context.Context, req *model.Request) (*model.Response, error) {
				ctx, span := start(ctx, req, false)
				defer span.End()
				resp, err := next.Generate(ctx, req)
				if err != nil {
					telemetry.RecordSpanError(span, err)
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
							telemetry.RecordSpanError(span, err)
						case ev != nil && ev.Type == model.StreamDelta:
							if first {
								first = false
								span.SetAttributes(telemetry.Attr{Key: "gen_ai.response.time_to_first_chunk", Value: time.Since(began).Seconds()})
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
func requestAttrs(d model.Description, req *model.Request, stream bool) (string, []telemetry.Attr) {
	op := cmp.Or(d.Operation, "chat")
	name := op
	attrs := []telemetry.Attr{{Key: "gen_ai.operation.name", Value: op}}
	if d.Provider != "" {
		attrs = append(attrs, telemetry.Attr{Key: "gen_ai.provider.name", Value: d.Provider})
	}
	if m := cmp.Or(req.Model, d.Model); m != "" {
		name += " " + m
		attrs = append(attrs, telemetry.Attr{Key: "gen_ai.request.model", Value: m})
	}
	if d.ServerAddress != "" {
		attrs = append(attrs, telemetry.Attr{Key: "server.address", Value: d.ServerAddress}, telemetry.Attr{Key: "server.port", Value: d.ServerPort})
	}
	if req.MaxTokens > 0 {
		attrs = append(attrs, telemetry.Attr{Key: "gen_ai.request.max_tokens", Value: req.MaxTokens})
	}
	if req.Temperature != nil {
		attrs = append(attrs, telemetry.Attr{Key: "gen_ai.request.temperature", Value: *req.Temperature})
	}
	if req.TopP != nil {
		attrs = append(attrs, telemetry.Attr{Key: "gen_ai.request.top_p", Value: *req.TopP})
	}
	if len(req.Stop) > 0 {
		attrs = append(attrs, telemetry.Attr{Key: "gen_ai.request.stop_sequences", Value: req.Stop})
	}
	if r := req.Reasoning; r != nil && r.Effort != "" {
		attrs = append(attrs, telemetry.Attr{Key: "gen_ai.request.reasoning.level", Value: r.Effort})
	}
	if req.ResponseFormat != nil {
		attrs = append(attrs, telemetry.Attr{Key: "gen_ai.output.type", Value: "json"})
	}
	if stream {
		// The conventions set this attribute only for streaming calls.
		attrs = append(attrs, telemetry.Attr{Key: "gen_ai.request.stream", Value: true})
	}
	return name, attrs
}

func responseAttrs(resp *model.Response) []telemetry.Attr {
	if resp == nil {
		return nil
	}
	var attrs []telemetry.Attr
	if resp.ID != "" {
		attrs = append(attrs, telemetry.Attr{Key: "gen_ai.response.id", Value: resp.ID})
	}
	if resp.Model != "" {
		attrs = append(attrs, telemetry.Attr{Key: "gen_ai.response.model", Value: resp.Model})
	}
	reason := string(resp.FinishReason)
	if resp.FinishReason == "" || resp.FinishReason == model.FinishUnknown {
		// The conventions report a missing finish reason as "error".
		reason = "error"
	}
	attrs = append(attrs, telemetry.Attr{Key: "gen_ai.response.finish_reasons", Value: []string{reason}})
	return append(attrs, usageAttrs(resp.Usage)...)
}

// usageAttrs reports usage as the conventions define it: input tokens
// include cached ones, and output tokens include reasoning.
func usageAttrs(u model.Usage) []telemetry.Attr {
	attrs := []telemetry.Attr{
		{Key: "gen_ai.usage.input_tokens", Value: u.PromptTokens()},
		{Key: "gen_ai.usage.output_tokens", Value: u.OutputTokens},
	}
	if u.CacheReadTokens > 0 {
		attrs = append(attrs, telemetry.Attr{Key: "gen_ai.usage.cache_read.input_tokens", Value: u.CacheReadTokens})
	}
	if u.CacheWriteTokens > 0 {
		attrs = append(attrs, telemetry.Attr{Key: "gen_ai.usage.cache_write.input_tokens", Value: u.CacheWriteTokens})
	}
	if u.ReasoningTokens > 0 {
		attrs = append(attrs, telemetry.Attr{Key: "gen_ai.usage.reasoning.output_tokens", Value: u.ReasoningTokens})
	}
	return attrs
}

// TraceTools records a span for each tool call.
func TraceTools(t telemetry.Tracer, opts ...TraceOption) tool.Middleware {
	return traceTools(newTraceConfig(t, opts), nil)
}

// errToolResult marks the span of a tool that returned an error result.
var errToolResult = errors.New("tool returned an error result")

func traceTools(cfg *traceConfig, extra []telemetry.Attr) tool.Middleware {
	return func(next tool.Invoker) tool.Invoker {
		return func(ctx context.Context, call *tool.Call) (*tool.Result, error) {
			attrs := []telemetry.Attr{
				{Key: "gen_ai.operation.name", Value: "execute_tool"},
				{Key: "gen_ai.tool.name", Value: call.Name},
				{Key: "gen_ai.tool.type", Value: "function"},
			}
			if call.ID != "" {
				attrs = append(attrs, telemetry.Attr{Key: "gen_ai.tool.call.id", Value: call.ID})
			}
			if call.Tool != nil {
				if d := call.Tool.Info().Description; d != "" {
					attrs = append(attrs, telemetry.Attr{Key: "gen_ai.tool.description", Value: d})
				}
			}
			if cfg.content {
				attrs = append(attrs, telemetry.Attr{Key: "gen_ai.tool.call.arguments", Value: string(call.Arguments)})
			}
			ctx, span := cfg.tracer.Start(ctx, "execute_tool "+call.Name, telemetry.SpanKindInternal, append(attrs, extra...)...)
			defer span.End()
			ctx = telemetry.ContextWithSpan(ctx, span)
			res, err := next(ctx, call)
			switch {
			case err != nil:
				telemetry.RecordSpanError(span, err)
			case res != nil && res.IsError:
				span.SetAttributes(telemetry.Attr{Key: "error.type", Value: "tool_error"})
				span.RecordError(errToolResult)
			case res != nil && cfg.content:
				span.SetAttributes(telemetry.Attr{Key: "gen_ai.tool.call.result", Value: res.Content})
			}
			return res, err
		}
	}
}
