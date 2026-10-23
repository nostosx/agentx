// Package middleware provides the tracing and metrics of agent runs, which
// plug into an agent with agent.WithMiddleware, and the types other
// middleware builds on. Tracing and metrics follow the OpenTelemetry GenAI
// semantic conventions without depending on OpenTelemetry; the
// github.com/nostosx/agentx-contrib/otel module adapts them, and
// github.com/nostosx/agentx-contrib/middleware provides logging and retries.
//
// For model calls outside the agent loop, such as the model of a
// actx.Summarize strategy, the package also provides plain decorators:
// TraceModel and MeterModel wrap a model.Model, and TraceTools and
// MeterTools are tool.Middleware.
package middleware

import (
	"context"
	"iter"
	"slices"

	"github.com/nostosx/agentx/model"
)

// Model wraps a model.Model.
type Model func(model.Model) model.Model

// Wrap applies middlewares to m. The first middleware is the outermost.
func Wrap(m model.Model, mws ...Model) model.Model {
	for _, mw := range slices.Backward(mws) {
		m = mw(m)
	}
	return m
}

// Funcs builds a model from functions. A nil function delegates to Next,
// and Describe reports Next's description.
type Funcs struct {
	Next         model.Model
	GenerateFunc func(ctx context.Context, req *model.Request) (*model.Response, error)
	StreamFunc   func(ctx context.Context, req *model.Request) iter.Seq2[*model.StreamEvent, error]
}

// Generate implements model.Model.
func (f *Funcs) Generate(ctx context.Context, req *model.Request) (*model.Response, error) {
	if f.GenerateFunc != nil {
		return f.GenerateFunc(ctx, req)
	}
	return f.Next.Generate(ctx, req)
}

// Stream implements model.Model.
func (f *Funcs) Stream(ctx context.Context, req *model.Request) iter.Seq2[*model.StreamEvent, error] {
	if f.StreamFunc != nil {
		return f.StreamFunc(ctx, req)
	}
	return f.Next.Stream(ctx, req)
}

// Describe implements model.Describer.
func (f *Funcs) Describe() model.Description { return model.Describe(f.Next) }
