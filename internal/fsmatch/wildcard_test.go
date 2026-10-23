package fsmatch_test

import (
	"testing"

	"github.com/nostosx/agentx/internal/fsmatch"
)

func TestWildcard(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		pattern, s string
		want       bool
	}{
		{"a*b*c", "aXbYc", true},
		{"*", "", true},
		{"abc", "abc", true},
		{"abc", "abcd", false},
		{"a*c", "ab", false},
		{"a*b*b", "ab", false},
		{"git *", "git status", true},
		{"*.go", "x.go", true},
		{"?", "x", false},
		{"?", "?", true},
		{"[ab]", "a", false},
		{"[ab]", "[ab]", true},
		{"src/*", "src/nested/file.go", true},
		{"你好*界", "你好世界", true},
		{"a**b", "ab", true},
	} {
		if got := fsmatch.Wildcard(tc.pattern, tc.s); got != tc.want {
			t.Errorf("Wildcard(%q, %q) = %v, want %v", tc.pattern, tc.s, got, tc.want)
		}
	}
}
