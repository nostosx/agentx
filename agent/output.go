package agent

import (
	"context"
	"reflect"

	"github.com/nostosx/agentx/loop"
	"github.com/nostosx/agentx/tool"
)

// OutputOption configures the output of WithOutput.
type OutputOption func(*loop.Output)

// OutputRetries sets how often an answer that does not match the output
// schema is sent back to the model; see loop.Output.Retries.
func OutputRetries(n int) OutputOption { return func(o *loop.Output) { o.Retries = n } }

// OutputName names the output schema for providers. It defaults to the name
// of T if that is a valid name, otherwise to "output".
func OutputName(name string) OutputOption { return func(o *loop.Output) { o.Name = name } }

// WithOutput makes the agent answer with JSON matching the schema of T,
// which must be a struct type or a pointer to one; New fails otherwise. The
// schema, derived with tool.SchemaOf, is requested as the response format
// unless WithResponseFormat sets one, and the final answer is validated
// against it. Decode the answer with loop.Result.DecodeAs or use AskAs. See
// loop.Output for retries and failures.
func WithOutput[T any](opts ...OutputOption) Option {
	return func(c *Config) {
		o := &loop.Output{Name: schemaName(reflect.TypeFor[T]()), Schema: tool.SchemaOf[T]()}
		for _, opt := range opts {
			opt(o)
		}
		c.Output = o
	}
}

// schemaName returns the name of t, or of the type it points to, if it is
// a valid schema name.
func schemaName(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	name := t.Name()
	if name == "" || len(name) > 64 {
		return ""
	}
	for _, r := range name {
		valid := r == '_' || r == '-' || '0' <= r && r <= '9' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z'
		if !valid {
			return ""
		}
	}
	return name
}

// AskAs runs a on a single prompt without history and decodes its final
// answer as T. Configure a with WithOutput[T], so the model answers in
// JSON matching T.
func AskAs[T any](ctx context.Context, a Agent, prompt string) (T, error) {
	res, err := loop.Collect(a.Run(ctx, Text(prompt)), nil)
	if err != nil {
		var zero T
		return zero, err
	}
	return res.DecodeAs[T]()
}
