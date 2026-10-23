package agent

import (
	"context"

	"github.com/nostosx/agentx/loop"
)

// WithDeps gives the agent's runs v as their dependency of type T, which
// tools, hooks, middleware and instruction functions read with
// loop.Deps[T]. It is a default: a dependency of type T that the run's
// context already carries, from loop.WithDeps or RunDeps, takes
// precedence. Sub-agents and agents the run is handed off to see the
// dependencies of the run that started them, and their own defaults for
// other types. Dependencies are not saved with checkpoints or sessions;
// see loop.WithDeps.
func WithDeps[T any](v T) Option {
	return func(c *Config) {
		c.Deps = append(c.Deps, func(ctx context.Context) context.Context {
			if _, ok := loop.Deps[T](ctx); ok {
				return ctx
			}
			return loop.WithDeps(ctx, v)
		})
	}
}

// RunDeps gives one run v as its dependency of type T, overriding the
// agent's default from WithDeps; see loop.Deps. It is not saved:
// pass it again to Runner.Resume when resuming the run.
func RunDeps[T any](v T) RunOption {
	return func(in *Input) {
		in.Deps = append(in.Deps, func(ctx context.Context) context.Context { return loop.WithDeps(ctx, v) })
	}
}

// withDeps applies the dependencies of fs to ctx in order.
func withDeps(ctx context.Context, fs []func(context.Context) context.Context) context.Context {
	for _, f := range fs {
		ctx = f(ctx)
	}
	return ctx
}
