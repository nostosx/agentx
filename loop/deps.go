package loop

import "context"

// depsKey keys the dependency of type T in a context.
type depsKey[T any] struct{}

// WithDeps returns a context carrying v as the dependency of type T, for
// example a database handle or the current user, in the style of the
// OpenAI Agents SDK's run context and Pydantic AI's deps. Tools, hooks,
// middleware and instruction functions of runs started with the context
// read it with Deps. A context holds one dependency per type; a later
// WithDeps of the same type replaces it for the derived context.
//
// Dependencies live only in the context: they are not sent to the model
// and not saved with checkpoints or sessions. Pass them again, with the
// context of Resume, when an interrupted run is resumed.
func WithDeps[T any](ctx context.Context, v T) context.Context {
	return context.WithValue(ctx, depsKey[T]{}, v)
}

// Deps returns the dependency of type T that ctx carries; see WithDeps.
// The type must match exactly: a dependency stored as a concrete type is
// not returned for an interface it implements.
func Deps[T any](ctx context.Context) (T, bool) {
	v, ok := ctx.Value(depsKey[T]{}).(T)
	return v, ok
}
