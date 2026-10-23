package tool_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nostosx/agentx/tool"
)

func TestFuncDecodingAndResults(t *testing.T) {
	t.Parallel()
	type input struct {
		Name string `json:"name"`
	}
	typed := tool.Func("greet", "hello", func(_ context.Context, in input) (string, error) { return "Hello " + in.Name, nil }, tool.ReadOnly(), tool.RequireApproval(), tool.Strict())
	info := typed.Info()
	if !info.ReadOnly || !info.RequiresApproval || !info.Strict {
		t.Fatalf("options=%+v", info)
	}
	out, err := typed.Call(t.Context(), json.RawMessage(`{"name":"Ada"}`))
	if err != nil || out.Content != "Hello Ada" {
		t.Fatalf("Call=%v,%v", out, err)
	}
	out, err = typed.Call(t.Context(), json.RawMessage(`{"name":1}`))
	if err != nil || !out.IsError || !strings.Contains(out.Content, "invalid arguments") {
		t.Fatalf("bad arguments=%v,%v", out, err)
	}
	spec := tool.Spec(typed)
	if spec.Name != "greet" || !spec.Strict || !json.Valid(spec.Parameters) {
		t.Fatalf("Spec=%+v", spec)
	}
	sentinel := errors.New("failed")
	failing := tool.Func("fail", "", func(context.Context, struct{}) (string, error) { return "", sentinel })
	if _, err := failing.Call(t.Context(), nil); !errors.Is(err, sentinel) {
		t.Fatalf("Call=%v", err)
	}
}

func TestFuncOutputConversion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		value   any
		want    string
		isError bool
		wantErr bool
	}{
		{"nil", nil, "", false, false},
		{"string", "text", "text", false, false},
		{"bytes", []byte("bytes"), "bytes", false, false},
		{"result pointer", tool.Errorf("bad %d", 1), "bad 1", true, false},
		{"result value", tool.Result{Content: "value"}, "value", false, false},
		{"nil result", (*tool.Result)(nil), "", false, false},
		{"JSON", map[string]int{"x": 1}, `{"x":1}`, false, false},
		{"unsupported", make(chan int), "", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := tool.Func("test", "", func(context.Context, struct{}) (any, error) { return tt.value, nil })
			res, err := f.Call(t.Context(), nil)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected encoding error")
				}
				return
			}
			if err != nil || res.Content != tt.want || res.IsError != tt.isError {
				t.Fatalf("Call=%+v,%v", res, err)
			}
		})
	}
}

func TestFreeformSpec(t *testing.T) {
	t.Parallel()
	type input struct {
		Input string `json:"input"`
	}
	ff := tool.Func("shout", "", func(_ context.Context, in input) (string, error) { return strings.ToUpper(in.Input), nil },
		tool.Freeform("regex", `[a-z]+`))
	spec := tool.Spec(ff)
	if spec.Grammar == nil || spec.Grammar.Syntax != "regex" || spec.Grammar.Definition != "[a-z]+" {
		t.Fatalf("Grammar=%+v", spec.Grammar)
	}
	if !strings.Contains(string(spec.Parameters), `"input"`) {
		t.Fatalf("Parameters=%s", spec.Parameters)
	}
	if tool.Spec(tool.Func("plain", "", func(context.Context, input) (string, error) { return "", nil })).Grammar != nil {
		t.Fatal("plain tool has a grammar")
	}
}

type approvesRisky struct{ tool.Tool }

func (approvesRisky) RequiresApprovalFor(args json.RawMessage) bool { return string(args) == `"risky"` }

func TestNeedsApproval(t *testing.T) {
	t.Parallel()
	plain := tool.Func("plain", "plain", func(context.Context, struct{}) (string, error) { return "", nil }, tool.RequireApproval())
	if !tool.NeedsApproval(plain, nil) {
		t.Fatal("RequiresApproval ignored")
	}
	per := approvesRisky{plain}
	if tool.NeedsApproval(per, json.RawMessage(`"safe"`)) || !tool.NeedsApproval(per, json.RawMessage(`"risky"`)) {
		t.Fatal("CallApprover ignored")
	}
}
