// Package tool defines tools that models can call, their JSON Schemas, a
// registry and an executor with middleware support.
package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nostosx/agentx/model"
)

// Info describes a tool to the model and to the runtime.
type Info struct {
	Name        string
	Description string
	// Parameters describes the arguments. Arguments are validated against it
	// before the tool runs.
	Parameters *Schema
	// RawParameters, when set, is sent to the model instead of Parameters and
	// disables argument validation. It is used for schemas that Schema
	// cannot represent, such as those of MCP tools.
	RawParameters json.RawMessage
	// ReadOnly marks a tool without side effects. The runtime runs adjacent
	// read-only calls concurrently.
	ReadOnly bool
	// Concurrent marks a tool whose calls may run in parallel with adjacent
	// concurrent or read-only calls even though they may have side effects,
	// such as sub-agents. Unlike ReadOnly it does not make a call safe to
	// repeat: a concurrent call with side effects that a stopped run cut off
	// is not run again when the run resumes.
	Concurrent bool
	// RequiresApproval asks the runtime to obtain approval before each call.
	RequiresApproval bool
	// Strict asks providers to guarantee schema-valid arguments. See
	// Strict for how the schema is converted.
	Strict bool
	// Grammar makes the tool freeform where the provider supports it; see
	// Freeform.
	Grammar *model.ToolGrammar
}

// Tool is a capability the model can invoke.
type Tool interface {
	Info() Info
	// Call runs the tool. Returning an error produces an error result that
	// the model can see; it does not stop the agent.
	Call(ctx context.Context, args json.RawMessage) (*Result, error)
}

// CallApprover is implemented by tools whose need for approval depends on
// the arguments of a call, such as a shell that runs sandboxed commands
// without approval but asks before running one outside the sandbox. The
// runtime asks it instead of reading Info.RequiresApproval, which should
// then report whether some calls need approval.
type CallApprover interface {
	RequiresApprovalFor(args json.RawMessage) bool
}

// NeedsApproval reports whether a call of t with args needs approval: the
// answer of t's CallApprover, or else Info.RequiresApproval.
func NeedsApproval(t Tool, args json.RawMessage) bool {
	if a, ok := t.(CallApprover); ok {
		return a.RequiresApprovalFor(args)
	}
	return t.Info().RequiresApproval
}

// Result is the output of a tool call.
type Result struct {
	// Content is the text output.
	Content string
	// Parts holds output beyond text, such as images and documents, that
	// follows Content. See model.ToolResult.Parts for how providers send it.
	Parts   []model.Part
	IsError bool
	// Metadata is information for the application, such as for middleware
	// or event consumers; it is not sent to the model.
	Metadata map[string]any
}

// Text returns a successful result.
func Text(content string) *Result { return &Result{Content: content} }

// Media returns a successful result with text and parts, such as images or
// documents: model.ImagePart, model.DocumentPart or model.MediaPart.
func Media(content string, parts ...model.Part) *Result {
	return &Result{Content: content, Parts: parts}
}

// Errorf returns an error result that is reported to the model.
func Errorf(format string, args ...any) *Result {
	return &Result{Content: fmt.Sprintf(format, args...), IsError: true}
}

// Spec converts a tool to the specification sent to models.
func Spec(t Tool) *model.ToolSpec {
	info := t.Info()
	params := info.RawParameters
	switch {
	case params != nil:
	case info.Strict:
		params = info.Parameters.StrictJSON()
	default:
		params = info.Parameters.JSON()
	}
	return &model.ToolSpec{
		Name:        info.Name,
		Description: info.Description,
		Parameters:  params,
		Strict:      info.Strict,
		Grammar:     info.Grammar,
	}
}

// Option configures a tool's Info.
type Option func(*Info)

// ReadOnly marks the tool as free of side effects.
func ReadOnly() Option { return func(i *Info) { i.ReadOnly = true } }

// Concurrent lets calls of the tool run in parallel with adjacent
// concurrent or read-only calls; see Info.Concurrent.
func Concurrent() Option { return func(i *Info) { i.Concurrent = true } }

// RequireApproval requires approval before each call.
func RequireApproval() Option { return func(i *Info) { i.RequiresApproval = true } }

// Strict enables strict argument generation where supported. The schema
// sent to the model is converted with Schema.StrictJSON; RawParameters are
// sent unchanged and must already satisfy the provider's strict rules.
func Strict() Option { return func(i *Info) { i.Strict = true } }

// Freeform lets providers that support freeform tools, such as the OpenAI
// Responses API, have the model write the tool's input as plain text
// matching a grammar instead of JSON. syntax is "lark" or "regex"; an empty
// definition accepts any text. The text arrives as the string argument
// model.FreeformInput ("input"), which the parameters must declare and
// which carries the same text from providers without freeform tools.
func Freeform(syntax, definition string) Option {
	return func(i *Info) { i.Grammar = &model.ToolGrammar{Syntax: syntax, Definition: definition} }
}

// WithSchema overrides the derived parameter schema.
func WithSchema(s *Schema) Option { return func(i *Info) { i.Parameters = s } }

// Handler is the raw implementation of a tool.
type Handler func(ctx context.Context, args json.RawMessage) (*Result, error)

type funcTool struct {
	info Info
	fn   Handler
}

func (t *funcTool) Info() Info { return t.info }

func (t *funcTool) Call(ctx context.Context, args json.RawMessage) (*Result, error) {
	return t.fn(ctx, args)
}

// New returns a tool backed by a raw handler.
func New(name, description string, params *Schema, fn Handler, opts ...Option) Tool {
	info := Info{Name: name, Description: description, Parameters: params}
	for _, opt := range opts {
		opt(&info)
	}
	return &funcTool{info: info, fn: fn}
}

// Func returns a tool whose arguments are decoded into In. The parameter
// schema is derived from In with SchemaOf.
//
// A string result becomes the tool output, a *Result is used as-is and any
// other value is encoded as JSON.
func Func[In, Out any](name, description string, fn func(ctx context.Context, in In) (Out, error), opts ...Option) Tool {
	handler := func(ctx context.Context, args json.RawMessage) (*Result, error) {
		var in In
		if len(args) > 0 && string(args) != "null" {
			if err := json.Unmarshal(args, &in); err != nil {
				return Errorf("invalid arguments: %v", err), nil
			}
		}
		out, err := fn(ctx, in)
		if err != nil {
			return nil, err
		}
		return toResult(out)
	}
	return New(name, description, SchemaOf[In](), handler, opts...)
}

func toResult(v any) (*Result, error) {
	switch out := v.(type) {
	case nil:
		return Text(""), nil
	case string:
		return Text(out), nil
	case *Result:
		if out == nil {
			return Text(""), nil
		}
		return out, nil
	case Result:
		return &out, nil
	case []byte:
		return Text(string(out)), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode tool output: %w", err)
	}
	return Text(string(b)), nil
}
