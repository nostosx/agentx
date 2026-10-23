package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"

	"github.com/nostosx/agentx/model"
)

// Executor runs tool calls from a registry through middleware.
type Executor struct {
	Registry    *Registry
	Middlewares []Middleware
}

// NewExecutor returns an executor for registry.
func NewExecutor(registry *Registry, mws ...Middleware) *Executor {
	return &Executor{Registry: registry, Middlewares: mws}
}

// Execute runs a model tool call. It never returns an error: unknown tools,
// invalid arguments, failures and panics become error results that are
// reported to the model so it can recover.
func (e *Executor) Execute(ctx context.Context, tc *model.ToolCall) *model.ToolResult {
	return e.ExecuteWith(ctx, tc)
}

// ExecuteWith is like Execute but wraps the call in mws outside the
// executor's own middleware; the first is outermost.
func (e *Executor) ExecuteWith(ctx context.Context, tc *model.ToolCall, mws ...Middleware) *model.ToolResult {
	res := &model.ToolResult{CallID: tc.ID, Name: tc.Name}
	t, ok := e.Registry.Get(tc.Name)
	if !ok {
		res.Content = fmt.Sprintf("Error: tool %q does not exist", tc.Name)
		res.IsError = true
		return res
	}
	args := tc.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	info := t.Info()
	if info.RawParameters == nil {
		if err := info.Parameters.Validate(args); err != nil {
			res.Content = "Error: " + err.Error()
			res.IsError = true
			return res
		}
	}
	out, err := e.invoke(ctx, &Call{ID: tc.ID, Name: tc.Name, Arguments: args, Tool: t}, mws)
	switch {
	case err != nil:
		res.Content = "Error: " + err.Error()
		res.IsError = true
	case out == nil:
		res.Content = ""
	default:
		res.Content = out.Content
		res.Parts = out.Parts
		res.IsError = out.IsError
		res.Metadata = out.Metadata
	}
	return res
}

// PanicError reports a panic recovered from a tool. Only the panic value is
// shown to the model; Stack is available to middleware for logging.
type PanicError struct {
	Tool  string
	Value any
	Stack []byte
}

func (e *PanicError) Error() string { return fmt.Sprintf("tool %s panicked: %v", e.Tool, e.Value) }

// invoke runs call through outer and then the executor's middleware. Panics
// raised by middleware are recovered too, so a faulty middleware cannot
// crash the process.
func (e *Executor) invoke(ctx context.Context, call *Call, outer []Middleware) (res *Result, err error) {
	defer recoverPanic(call, &err)
	return Chain(Chain(callTool, e.Middlewares...), outer...)(ctx, call)
}

func callTool(ctx context.Context, call *Call) (res *Result, err error) {
	defer recoverPanic(call, &err)
	return call.Tool.Call(ctx, call.Arguments)
}

func recoverPanic(call *Call, err *error) {
	if r := recover(); r != nil {
		*err = &PanicError{Tool: call.Name, Value: r, Stack: debug.Stack()}
	}
}
