package middleware_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/nostosx/agentx/actx"
	"github.com/nostosx/agentx/agent"
	"github.com/nostosx/agentx/guard"
	"github.com/nostosx/agentx/hook"
	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/middleware"
	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/model/modeltest"
	"github.com/nostosx/agentx/telemetry"
	"github.com/nostosx/agentx/tool"
)

func decodeJSON(t *testing.T, v any) any {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("content attribute %v is %T, want a JSON string", v, v)
	}
	var out any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatalf("content %q: %v", s, err)
	}
	return out
}

func echoTool() tool.Tool {
	return tool.Func("echo", "Echo text.", func(_ context.Context, in struct {
		Text string `json:"text"`
	}) (string, error) {
		return in.Text, nil
	})
}

func TestTracingRecordsContent(t *testing.T) {
	t.Parallel()
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "generate", true: "stream"}[stream], func(t *testing.T) {
			t.Parallel()
			tracer := &recordingTracer{}
			m := modeltest.New(modeltest.CallTools(modeltest.Call{ID: "c1", Name: "echo", Args: map[string]string{"text": "hi"}}), modeltest.Reply("done"))
			opts := []agent.Option{
				agent.WithModel(described{m, fixtureDescription}), agent.WithTools(echoTool()), agent.WithInstructions("Be brief."),
				agent.WithMiddleware(middleware.Tracing(tracer, middleware.RecordContent())),
			}
			if stream {
				opts = append(opts, agent.WithStreaming())
			}
			a := agent.MustNew("assistant", opts...)
			if _, err := a.Ask(t.Context(), "say hi"); err != nil {
				t.Fatal(err)
			}
			calls := tracer.all("chat gpt-fixture")
			if len(calls) != 2 {
				t.Fatalf("got %d model spans", len(calls))
			}
			first, second := calls[0], calls[1]
			wantSystem := []any{map[string]any{"type": "text", "content": "Be brief."}}
			if got := decodeJSON(t, first.attr("gen_ai.system_instructions")); !reflect.DeepEqual(got, wantSystem) {
				t.Errorf("system instructions = %v", got)
			}
			wantInput := []any{map[string]any{"role": "user", "parts": []any{map[string]any{"type": "text", "content": "say hi"}}}}
			if got := decodeJSON(t, first.attr("gen_ai.input.messages")); !reflect.DeepEqual(got, wantInput) {
				t.Errorf("input messages = %v", got)
			}
			wantTools := []any{map[string]any{"type": "function", "name": "echo"}}
			if got := decodeJSON(t, first.attr("gen_ai.tool.definitions")); !reflect.DeepEqual(got, wantTools) {
				t.Errorf("tool definitions = %v", got)
			}
			wantCall := []any{map[string]any{"role": "assistant", "parts": []any{map[string]any{
				"type": "tool_call", "id": "c1", "name": "echo", "arguments": map[string]any{"text": "hi"},
			}}}}
			if got := decodeJSON(t, first.attr("gen_ai.output.messages")); !reflect.DeepEqual(got, wantCall) {
				t.Errorf("output messages = %v", got)
			}
			// The second call sees the tool result.
			input := decodeJSON(t, second.attr("gen_ai.input.messages")).([]any)
			result := input[len(input)-1].(map[string]any)
			if result["role"] != "tool" || !reflect.DeepEqual(result["parts"], []any{map[string]any{"type": "tool_call_response", "id": "c1", "response": "hi"}}) {
				t.Errorf("tool result message = %v", result)
			}
			wantAnswer := []any{map[string]any{"role": "assistant", "parts": []any{map[string]any{"type": "text", "content": "done"}}}}
			if got := decodeJSON(t, second.attr("gen_ai.output.messages")); !reflect.DeepEqual(got, wantAnswer) {
				t.Errorf("answer = %v", got)
			}
			root := tracer.find(t, "invoke_agent assistant")
			if got := decodeJSON(t, root.attr("gen_ai.input.messages")); !reflect.DeepEqual(got, wantInput) {
				t.Errorf("run input = %v", got)
			}
			if got := decodeJSON(t, root.attr("gen_ai.output.messages")); !reflect.DeepEqual(got, wantAnswer) {
				t.Errorf("run output = %v", got)
			}
			if tool := tracer.find(t, "execute_tool echo"); tool.attr("gen_ai.tool.call.arguments") != `{"text":"hi"}` || tool.attr("gen_ai.tool.call.result") != "hi" {
				t.Errorf("tool span = %v", tool.attrs)
			}
		})
	}
}

func TestTracingContentIsOptIn(t *testing.T) {
	t.Parallel()
	tracer := &recordingTracer{}
	a := agent.MustNew("assistant", agent.WithModel(modeltest.New(modeltest.Reply("done"))), agent.WithInstructions("secret"),
		agent.WithMiddleware(middleware.Tracing(tracer)))
	if _, err := a.Ask(t.Context(), "private"); err != nil {
		t.Fatal(err)
	}
	tracer.mu.Lock()
	defer tracer.mu.Unlock()
	for _, s := range tracer.spans {
		for _, key := range []string{"gen_ai.system_instructions", "gen_ai.input.messages", "gen_ai.output.messages", "gen_ai.tool.definitions"} {
			if _, ok := s.attrs[key]; ok {
				t.Errorf("span %q records %s without RecordContent", s.name, key)
			}
		}
	}
}

func TestContentParts(t *testing.T) {
	t.Parallel()
	tracer := &recordingTracer{}
	m := middleware.Wrap(modeltest.New(modeltest.Respond(&model.Response{Message: &model.Message{Role: model.RoleAssistant, Parts: []model.Part{
		{Type: model.PartReasoning, Text: "thinking"},
		{Type: model.PartReasoning, Redacted: true, Data: []byte("secret")},
		{Type: model.PartRaw, Provider: "x", Raw: json.RawMessage(`{}`)},
		{Type: model.PartToolCall, ToolCall: &model.ToolCall{ID: "c", Name: "t", Arguments: json.RawMessage(`not json`)}},
	}}})), middleware.TraceModel(tracer, middleware.RecordContent()))
	req := &model.Request{Messages: []*model.Message{
		model.NewMessage(model.RoleUser, model.ImagePart("image/png", []byte{1, 2}), model.ImageURLPart("https://example.com/a.png"),
			model.AudioPart("audio/wav", []byte{3}), model.FilePart(model.PartDocument, "openai", "file-1", "application/pdf"), model.VideoURLPart("gs://b/v.mp4")),
	}}
	if _, err := m.Generate(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	span := tracer.find(t, "chat")
	in := decodeJSON(t, span.attr("gen_ai.input.messages"))
	wantIn := []any{map[string]any{"role": "user", "parts": []any{
		map[string]any{"type": "blob", "modality": "image", "mime_type": "image/png", "content": "AQI="},
		map[string]any{"type": "uri", "modality": "image", "uri": "https://example.com/a.png"},
		map[string]any{"type": "blob", "modality": "audio", "mime_type": "audio/wav", "content": "Aw=="},
		map[string]any{"type": "file", "modality": "document", "mime_type": "application/pdf", "file_id": "file-1"},
		map[string]any{"type": "uri", "modality": "video", "uri": "gs://b/v.mp4"},
	}}}
	if !reflect.DeepEqual(in, wantIn) {
		t.Errorf("input = %v", in)
	}
	out := decodeJSON(t, span.attr("gen_ai.output.messages"))
	wantOut := []any{map[string]any{"role": "assistant", "parts": []any{
		map[string]any{"type": "reasoning", "content": "thinking"},
		map[string]any{"type": "reasoning", "content": ""},
		map[string]any{"type": "tool_call", "id": "c", "name": "t", "arguments": "not json"},
	}}}
	if !reflect.DeepEqual(out, wantOut) {
		t.Errorf("output = %v", out)
	}
	if _, ok := span.attrs["gen_ai.system_instructions"]; ok {
		t.Error("empty system prompt recorded")
	}
}

// TestTracingNesting checks the span tree of a run with hooks, compaction,
// a tool call and a sub-agent traced by the same tracer.
func TestTracingNesting(t *testing.T) {
	t.Parallel()
	tracer := &recordingTracer{}
	trace := middleware.Tracing(tracer)
	researcher := agent.MustNew("researcher", agent.WithDescription("Researches."),
		agent.WithModel(described{modeltest.New(modeltest.CallTools(modeltest.Call{ID: "s1", Name: "echo", Args: map[string]string{"text": "fact"}}), modeltest.Reply("found it")), fixtureDescription}),
		agent.WithTools(echoTool()), agent.WithMiddleware(trace))

	parentModel := modeltest.New(
		modeltest.CallTools(modeltest.Call{ID: "p1", Name: "researcher", Args: map[string]string{"prompt": "look it up"}}),
		modeltest.Reply("done"),
	)
	var compacted bool
	shrink := actx.StrategyFunc(func(_ context.Context, in *actx.CompactInput) (*actx.CompactOutput, error) {
		compacted = true
		return &actx.CompactOutput{Messages: []*model.Message{model.UserText("short")}}, nil
	})
	allow := hook.Func(func(context.Context, *hook.Event) (*hook.Result, error) {
		return &hook.Result{Decision: hook.Allow}, nil
	})
	a := agent.MustNew("coordinator",
		agent.WithModel(described{parentModel, fixtureDescription}),
		agent.WithSubAgents(researcher),
		agent.WithContext(actx.Config{Window: actx.Window{Limit: 400, ReserveOutput: 10}, Strategy: shrink}),
		agent.WithHook(hook.PreToolUse, "", allow),
		agent.WithHook(hook.PreCompact, "", hook.Func(func(context.Context, *hook.Event) (*hook.Result, error) { return nil, nil })),
		agent.WithHook(hook.SubagentStart, "", hook.Func(func(context.Context, *hook.Event) (*hook.Result, error) { return nil, nil })),
		agent.WithMiddleware(trace),
	)
	res, err := agent.NewRunner(a, nil).RunText(t.Context(), "s", strings.Repeat("long input ", 300))
	if err != nil || res.Status != loop.StatusCompleted {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if !compacted {
		t.Fatal("the conversation was not compacted")
	}

	root := tracer.find(t, "invoke_agent coordinator")
	compaction := tracer.find(t, "compact_context")
	if compaction.parent != root || compaction.attr("agentx.compaction.applied") != true || compaction.attr("agentx.compaction.reason") != "auto" ||
		compaction.attr("agentx.compaction.tokens_after").(int) >= compaction.attr("agentx.compaction.tokens_before").(int) || compaction.ended != 1 {
		t.Errorf("compaction span = %+v", compaction.attrs)
	}
	if pre := tracer.find(t, "hook PreCompact"); pre.parent != compaction {
		t.Errorf("PreCompact hook parent = %v", pre.parent)
	}
	// Model calls after the compaction carry the attribute.
	for _, s := range tracer.all("chat gpt-fixture") {
		agentName := s.attr("gen_ai.agent.name")
		if want := agentName == "coordinator"; s.attr("gen_ai.conversation.compacted") == true != want {
			t.Errorf("model span of %v: compacted = %v", agentName, s.attr("gen_ai.conversation.compacted"))
		}
	}
	pre := tracer.find(t, "hook PreToolUse")
	if pre.parent != root || pre.attr("agentx.hook.decision") != "allow" || pre.attr("agentx.hook.tool") != "researcher" ||
		pre.attr("agentx.hook.count") != 1 || pre.ended != 1 {
		t.Errorf("PreToolUse hook span = %+v (parent %v)", pre.attrs, pre.parent)
	}

	call := tracer.find(t, "execute_tool researcher")
	if call.parent != root {
		t.Fatalf("tool span parent = %v", call.parent)
	}
	if start := tracer.find(t, "hook SubagentStart"); start.parent != call {
		t.Errorf("SubagentStart hook parent = %v", start.parent)
	}
	sub := tracer.find(t, "invoke_agent researcher")
	if sub.parent != call || sub.attr("agentx.run.status") != "completed" || sub.attr("gen_ai.agent.name") != "researcher" {
		t.Fatalf("sub-agent span = %+v (parent %v)", sub.attrs, sub.parent)
	}
	if subTool := tracer.find(t, "execute_tool echo"); subTool.parent != sub || subTool.attr("gen_ai.agent.name") != "researcher" {
		t.Errorf("sub-agent tool span parent = %v", subTool.parent)
	}
	for _, s := range tracer.all("chat gpt-fixture") {
		want := root
		if s.attr("gen_ai.agent.name") == "researcher" {
			want = sub
		}
		if s.parent != want {
			t.Errorf("model span of %v has parent %v", s.attr("gen_ai.agent.name"), s.parent.name)
		}
	}
	tracer.mu.Lock()
	defer tracer.mu.Unlock()
	for _, s := range tracer.spans {
		if s.ended != 1 {
			t.Errorf("span %q ended %d times", s.name, s.ended)
		}
	}
}

func TestSpanHelpersWithoutTracing(t *testing.T) {
	t.Parallel()
	ctx, span := telemetry.StartSpan(t.Context(), "work", telemetry.SpanKindInternal)
	if ctx != t.Context() {
		t.Error("StartSpan without a traced run changed the context")
	}
	span.SetAttributes(telemetry.Attr{Key: "k", Value: 1})
	telemetry.RecordSpanError(span, context.Canceled)
	span.End()
	telemetry.AddEvent(ctx, "nothing")
	if telemetry.SpanFromContext(ctx) == nil {
		t.Fatal("SpanFromContext returned nil")
	}
	// Spans that do not record events are skipped.
	tracer := &plainTracer{}
	m := middleware.Wrap(modeltest.New(modeltest.Reply("x")), middleware.TraceModel(tracer))
	if _, err := m.Generate(t.Context(), &model.Request{}); err != nil {
		t.Fatal(err)
	}
	telemetry.AddEvent(t.Context(), "nothing")
}

// plainTracer's spans do not implement EventSpan.
type plainTracer struct{}

func (plainTracer) Start(ctx context.Context, _ string, _ telemetry.SpanKind, _ ...telemetry.Attr) (context.Context, telemetry.Span) {
	return ctx, telemetry.SpanFromContext(context.Background())
}

// TestTracingHandoffAndGuardrail traces a run that is handed off to an
// agent whose output guardrail blocks the answer.
func TestTracingHandoffAndGuardrail(t *testing.T) {
	t.Parallel()
	for _, content := range []bool{false, true} {
		tracer := &recordingTracer{}
		var opts []middleware.TraceOption
		if content {
			opts = append(opts, middleware.RecordContent())
		}
		trace := middleware.Tracing(tracer, opts...)
		billing := agent.MustNew("billing", agent.WithModel(modeltest.New(modeltest.Reply("card 4242"))), agent.WithMiddleware(trace),
			agent.WithOutputGuardrail(guard.OutputGuardrail{Name: "pii", Check: func(context.Context, *loop.Run, *model.Message) (guard.Result, error) {
				return guard.Result{Tripwire: true, Message: "card number"}, nil
			}}))
		triage := agent.MustNew("triage", agent.WithModel(modeltest.New(modeltest.CallTools(modeltest.Call{ID: "h", Name: agent.HandoffToolPrefix + "billing"}))),
			agent.WithHandoffs(billing), agent.WithMiddleware(trace))
		res, err := loop.Collect(triage.Run(t.Context(), agent.Text("refund")), nil)
		if err != nil || res.Status != loop.StatusBlocked || res.Agent != "billing" {
			t.Fatalf("run = %+v, %v", res, err)
		}
		first, second := tracer.find(t, "invoke_agent triage"), tracer.find(t, "invoke_agent billing")
		if first.attr("agentx.handoff.from") != nil || first.attr("agentx.guardrail.name") != nil || len(first.events) != 0 {
			t.Errorf("triage span = %+v, events %+v", first.attrs, first.events)
		}
		if second.attr("agentx.handoff.from") != "triage" || second.attr("agentx.run.status") != "blocked" ||
			second.attr("agentx.guardrail.name") != "pii" || second.attr("agentx.guardrail.kind") != "output" {
			t.Errorf("billing span = %+v", second.attrs)
		}
		if len(second.events) != 1 || second.events[0].name != "agentx.guardrail" || second.events[0].attrs["agentx.guardrail.name"] != "pii" ||
			(second.events[0].attrs["agentx.guardrail.message"] == "card number") != content {
			t.Errorf("billing events = %+v", second.events)
		}
	}
}
