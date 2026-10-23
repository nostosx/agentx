package hook

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/nostosx/agentx/model"
)

// Default timeouts of Prompt and Agent hooks, as in Claude Code.
const (
	defaultPromptTimeout = 30 * time.Second
	defaultAgentTimeout  = 60 * time.Second
)

// promptSystem instructs the model of a Prompt hook.
const promptSystem = `You evaluate a hook in the lifecycle of an AI agent. The user message holds the condition or rule to check and the hook input as JSON.

Respond with a single JSON object and nothing else:
- {"ok": true} if the condition holds or the action may proceed;
- {"ok": false, "reason": "..."} if it does not, with the reason addressed to the agent;
- add "impossible": true to an ok false response only if the condition can never be satisfied.`

// agentInstructions are appended to the prompt of an Agent hook.
const agentInstructions = `Investigate as needed with your tools, then end with a single JSON object and nothing else: {"ok": true} if the condition holds or the action may proceed, or {"ok": false, "reason": "..."} if it does not, with the reason addressed to the agent.`

// Prompt is a hook that asks a model to evaluate the event, like Claude
// Code's prompt hooks.
//
// The model receives Prompt with "$ARGUMENTS" replaced by the event's JSON
// input, the input a Command receives; without "$ARGUMENTS" the input is
// appended. "\$" stands for a literal "$". It must answer with a JSON
// object: {"ok": true} lets the action proceed, and {"ok": false,
// "reason": "..."} blocks it, which depends on the event as in Claude
// Code:
//
//   - Stop and SubagentStop: the agent continues with the reason, unless
//     the answer also has "impossible": true, which lets it stop.
//   - PreToolUse: the call is denied with the reason; unless
//     ContinueOnBlock is set, the run also halts after the step, with the
//     reason as a warning.
//   - PostToolUse: unless ContinueOnBlock is set, the run halts with the
//     reason as a warning; with it, the reason is added to the result.
//   - PostToolUseFailure: the reason is added to the result.
//   - UserPromptSubmit and other events: Deny with the reason.
//
// An answer that is not such an object is a hook error.
type Prompt struct {
	Model  model.Model
	Prompt string
	// ModelName, if set, overrides the model name of the request.
	ModelName string
	// Timeout defaults to 30 seconds. A negative Timeout disables it.
	Timeout         time.Duration
	ContinueOnBlock bool
}

func (p *Prompt) usesModel() bool { return true }

func (p *Prompt) hookTimeout() time.Duration { return p.Timeout }

func (p *Prompt) hookKey() string {
	id := identity(p.Model)
	if id == "" {
		return ""
	}
	key, _ := json.Marshal([]any{"prompt", id, p.Prompt, p.ModelName, p.Timeout, p.ContinueOnBlock})
	return string(key)
}

// Handle asks the model.
func (p *Prompt) Handle(ctx context.Context, ev *Event) (*Result, error) {
	if p.Model == nil {
		return nil, errors.New("prompt hook: no model")
	}
	ctx, cancel := withDefaultTimeout(ctx, p.Timeout, defaultPromptTimeout)
	defer cancel()
	input, err := eventJSON(ev)
	if err != nil {
		return nil, err
	}
	resp, err := p.Model.Generate(ctx, &model.Request{
		Model:    p.ModelName,
		System:   promptSystem,
		Messages: []*model.Message{model.UserText(expandArguments(p.Prompt, input))},
	})
	if err != nil {
		return nil, fmt.Errorf("prompt hook: %w", err)
	}
	v, err := parseVerdict(resp.Text())
	if err != nil {
		return nil, fmt.Errorf("prompt hook: %w", err)
	}
	return v.result(ev.Type, p.ContinueOnBlock, true), nil
}

// Verifier runs the agentic check of an Agent hook: it works on prompt
// with tools, over as many turns as it needs, and returns its final
// answer. model is the model name the hook configures, or "" for the
// verifier's default. agent.HookVerifier returns one backed by an agent.
type Verifier interface {
	Verify(ctx context.Context, model, prompt string) (string, error)
}

// VerifierFunc adapts a function to Verifier.
type VerifierFunc func(ctx context.Context, model, prompt string) (string, error)

// Verify calls f.
func (f VerifierFunc) Verify(ctx context.Context, model, prompt string) (string, error) {
	return f(ctx, model, prompt)
}

// Agent is a hook that has an agent verify the event with tools, like
// Claude Code's agent hooks, which are experimental. Prompt is expanded as
// for a Prompt hook, followed by instructions to end with {"ok": true} or
// {"ok": false, "reason": "..."}. A block is handled like that of a Prompt
// hook with ContinueOnBlock set, and "impossible" is ignored.
type Agent struct {
	Verifier Verifier
	Prompt   string
	// Model is passed to the verifier; "" selects its default.
	Model string
	// Timeout defaults to 60 seconds. A negative Timeout disables it.
	Timeout time.Duration
}

func (a *Agent) usesModel() bool { return true }

func (a *Agent) hookTimeout() time.Duration { return a.Timeout }

func (a *Agent) hookKey() string {
	id := identity(a.Verifier)
	if id == "" {
		return ""
	}
	key, _ := json.Marshal([]any{"agent", id, a.Prompt, a.Model, a.Timeout})
	return string(key)
}

// Handle runs the verifier.
func (a *Agent) Handle(ctx context.Context, ev *Event) (*Result, error) {
	if a.Verifier == nil {
		return nil, errors.New("agent hook: no verifier")
	}
	ctx, cancel := withDefaultTimeout(ctx, a.Timeout, defaultAgentTimeout)
	defer cancel()
	input, err := eventJSON(ev)
	if err != nil {
		return nil, err
	}
	out, err := a.Verifier.Verify(ctx, a.Model, expandArguments(a.Prompt, input)+"\n\n"+agentInstructions)
	if err != nil {
		return nil, fmt.Errorf("agent hook: %w", err)
	}
	v, err := parseVerdict(out)
	if err != nil {
		return nil, fmt.Errorf("agent hook: %w", err)
	}
	return v.result(ev.Type, true, false), nil
}

func withDefaultTimeout(ctx context.Context, timeout, def time.Duration) (context.Context, context.CancelFunc) {
	switch {
	case timeout < 0:
		return context.WithCancel(ctx)
	case timeout == 0:
		timeout = def
	}
	return context.WithTimeout(ctx, timeout)
}

// eventJSON returns the JSON input of ev, as a Command receives it.
func eventJSON(ev *Event) ([]byte, error) {
	payload := commandInput(ev)
	payload.Cwd, _ = os.Getwd()
	return json.Marshal(payload)
}

// expandArguments replaces "$ARGUMENTS" in prompt by input, or appends
// input if there is none, and "\$" by "$".
func expandArguments(prompt string, input []byte) string {
	var b strings.Builder
	found := false
	for i := 0; i < len(prompt); {
		switch {
		case strings.HasPrefix(prompt[i:], `\$`):
			b.WriteByte('$')
			i += 2
		case strings.HasPrefix(prompt[i:], "$ARGUMENTS"):
			b.Write(input)
			found = true
			i += len("$ARGUMENTS")
		default:
			b.WriteByte(prompt[i])
			i++
		}
	}
	if !found {
		b.WriteString("\n\n")
		b.Write(input)
	}
	return b.String()
}

// verdict is the answer of a Prompt or Agent hook.
type verdict struct {
	OK         *bool  `json:"ok"`
	Reason     string `json:"reason"`
	Impossible bool   `json:"impossible"`
}

// parseVerdict reads the JSON object in a model's answer, which may be
// wrapped in prose or a code fence.
func parseVerdict(text string) (*verdict, error) {
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return nil, fmt.Errorf("answer is not a JSON object: %q", truncate(text))
	}
	var v verdict
	if err := json.Unmarshal([]byte(text[start:end+1]), &v); err != nil {
		return nil, fmt.Errorf("parse answer %q: %w", truncate(text), err)
	}
	if v.OK == nil {
		return nil, fmt.Errorf(`answer has no "ok" field: %q`, truncate(text))
	}
	return &v, nil
}

func truncate(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

// result maps a verdict to a hook result for an event.
func (v *verdict) result(event EventType, continueOnBlock, honorImpossible bool) *Result {
	if *v.OK {
		return nil
	}
	reason := cmp.Or(v.Reason, "blocked by hook")
	switch event {
	case Stop, SubagentStop:
		if honorImpossible && v.Impossible {
			return nil
		}
	case PreToolUse:
		if !continueOnBlock {
			return &Result{Decision: Deny, Reason: reason, Halt: true, HaltReason: reason, SystemMessage: reason}
		}
	case PostToolUse:
		if !continueOnBlock {
			return &Result{Halt: true, HaltReason: reason, SystemMessage: reason}
		}
	}
	return &Result{Decision: Deny, Reason: reason}
}

// identity identifies a pointer-like value, so hooks that share it can be
// recognized as identical. Other values, including functions, whose
// pointers do not identify them, have none.
func identity(v any) string {
	if v == nil {
		return "nil"
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Chan, reflect.UnsafePointer:
		return fmt.Sprintf("%T@%x", v, rv.Pointer())
	}
	return ""
}
