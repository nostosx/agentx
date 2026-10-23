package tool_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/nostosx/agentx/tool"
)

func TestChainAndAllow(t *testing.T) {
	t.Parallel()
	var order []string
	mw := func(name string) tool.Middleware {
		return func(next tool.Invoker) tool.Invoker {
			return func(ctx context.Context, c *tool.Call) (*tool.Result, error) {
				order = append(order, name+" before")
				r, e := next(ctx, c)
				order = append(order, name+" after")
				return r, e
			}
		}
	}
	final := func(context.Context, *tool.Call) (*tool.Result, error) {
		order = append(order, "call")
		return tool.Text("ok"), nil
	}
	invoke := tool.Chain(final, mw("a"), mw("b"), tool.Allow("allowed"))
	res, err := invoke(t.Context(), &tool.Call{Name: "allowed"})
	if err != nil || res.Content != "ok" || !reflect.DeepEqual(order, []string{"a before", "b before", "call", "b after", "a after"}) {
		t.Fatalf("chain=%v,%v,%v", res, err, order)
	}
	order = nil
	res, err = invoke(t.Context(), &tool.Call{Name: "denied"})
	if err != nil || !res.IsError || !reflect.DeepEqual(order, []string{"a before", "b before", "b after", "a after"}) {
		t.Fatalf("allow=%v,%v,%v", res, err, order)
	}
}

func TestTimeout(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		invoke := tool.Timeout(time.Second)(func(ctx context.Context, _ *tool.Call) (*tool.Result, error) { <-ctx.Done(); return nil, ctx.Err() })
		res, err := invoke(t.Context(), &tool.Call{Name: "slow"})
		if res != nil || err == nil || !strings.Contains(err.Error(), "tool slow timed out after 1s") {
			t.Fatalf("Timeout=%v,%v", res, err)
		}
	})
}

func TestTruncatePreservesUTF8AndInput(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, text string
		limit      int
	}{{"ASCII", "abcdefghijklmnopqrstuvwxyz", 9}, {"unicode", "你好世界你好世界你好世界", 10}, {"one byte", "你好世界", 1}} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tool.Truncate(tt.text, tt.limit)
			if !utf8.ValidString(got) || !strings.Contains(got, "bytes truncated") {
				t.Fatalf("Truncate=%q", got)
			}
		})
	}
	for _, n := range []int{0, -1, 100} {
		if got := tool.Truncate("short", n); got != "short" {
			t.Fatalf("Truncate(%d)=%q", n, got)
		}
	}
	original := &tool.Result{Content: strings.Repeat("x", 50), IsError: true, Metadata: map[string]any{"key": "value"}}
	invoke := tool.TruncateOutput(10)(func(context.Context, *tool.Call) (*tool.Result, error) { return original, nil })
	result, err := invoke(t.Context(), &tool.Call{})
	if err != nil || result == original || original.Content != strings.Repeat("x", 50) || !result.IsError || result.Metadata["key"] != "value" {
		t.Fatalf("truncated=%+v err=%v original=%+v", result, err, original)
	}
}
