package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"
)

// Call is a single tool invocation passed through middleware.
type Call struct {
	ID        string
	Name      string
	Arguments json.RawMessage
	Tool      Tool
}

// Invoker executes a call.
type Invoker func(ctx context.Context, call *Call) (*Result, error)

// Middleware wraps an Invoker.
type Middleware func(next Invoker) Invoker

// Chain composes middleware so that the first one is outermost.
func Chain(final Invoker, mws ...Middleware) Invoker {
	for _, mw := range slices.Backward(mws) {
		final = mw(final)
	}
	return final
}

// Timeout limits each call to d.
func Timeout(d time.Duration) Middleware {
	return func(next Invoker) Invoker {
		return func(ctx context.Context, call *Call) (*Result, error) {
			ctx, cancel := context.WithTimeout(ctx, d)
			defer cancel()
			res, err := next(ctx, call)
			if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, fmt.Errorf("tool %s timed out after %s", call.Name, d)
			}
			return res, err
		}
	}
}

// TruncateOutput limits result content to max bytes, keeping the head and
// tail so the model still sees how the output ends.
func TruncateOutput(maxBytes int) Middleware {
	return func(next Invoker) Invoker {
		return func(ctx context.Context, call *Call) (*Result, error) {
			res, err := next(ctx, call)
			if err != nil || res == nil || len(res.Content) <= maxBytes {
				return res, err
			}
			out := *res
			out.Content = Truncate(res.Content, maxBytes)
			return &out, nil
		}
	}
}

// Truncate shortens s to about maxBytes, preserving its head and tail.
func Truncate(s string, maxBytes int) string {
	if len(s) <= maxBytes || maxBytes <= 0 {
		return s
	}
	head := maxBytes * 2 / 3
	tail := maxBytes - head
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	start := len(s) - tail
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	omitted := start - head
	return fmt.Sprintf("%s\n... [%d bytes truncated] ...\n%s", s[:head], omitted, s[start:])
}

// Allow rejects calls to tools that are not in names.
func Allow(names ...string) Middleware {
	allowed := make(map[string]bool, len(names))
	for _, n := range names {
		allowed[n] = true
	}
	return func(next Invoker) Invoker {
		return func(ctx context.Context, call *Call) (*Result, error) {
			if !allowed[call.Name] {
				return Errorf("tool %s is not allowed", call.Name), nil
			}
			return next(ctx, call)
		}
	}
}
