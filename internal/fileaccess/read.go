// Package fileaccess carries per-file permission checks into recursive tools
// without coupling built-in tools to a permission policy.
package fileaccess

import "context"

type readKey struct{}

// WithReadFilter adds a filter for files discovered by a recursive search.
// path is absolute. An enclosing filter, if any, must also allow the file.
func WithReadFilter(ctx context.Context, allow func(path string) bool) context.Context {
	previous, _ := ctx.Value(readKey{}).(func(string) bool)
	if previous != nil {
		next := allow
		allow = func(path string) bool { return previous(path) && next(path) }
	}
	return context.WithValue(ctx, readKey{}, allow)
}

// ReadAllowed reports whether every active search filter permits path.
// Without a policy, file tools retain their ordinary filesystem boundary.
func ReadAllowed(ctx context.Context, path string) bool {
	allow, _ := ctx.Value(readKey{}).(func(string) bool)
	return allow == nil || allow(path)
}
