package tool_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/tool"
)

func TestSchemaBuilders(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		schema *tool.Schema
		want   string
	}{
		{"number", tool.Number("ratio"), `{"type":"number","description":"ratio"}`},
		{"array", tool.Array(tool.Integer("id"), "ids"), `{"type":"array","description":"ids","items":{"type":"integer","description":"id"}}`},
		{"array of any", tool.Array(nil, ""), `{"type":"array"}`},
		{"enum", tool.Enum("unit", "c", "f"), `{"type":"string","description":"unit","enum":["c","f"]}`},
		// Without values Enum is a plain string.
		{"empty enum", tool.Enum("free"), `{"type":"string","description":"free"}`},
	} {
		if got := string(tc.schema.JSON()); got != tc.want {
			t.Errorf("%s: JSON = %s, want %s", tc.name, got, tc.want)
		}
	}

	schema := tool.Object(map[string]*tool.Schema{
		"ratio": tool.Number("ratio"),
		"ids":   tool.Array(tool.Integer("id"), "ids"),
		"unit":  tool.Enum("unit", "c", "f"),
		"tags":  tool.Array(tool.Enum("", "a", "b"), ""),
	}, "ratio")
	for _, tc := range []struct{ name, raw, want string }{
		{"valid", `{"ratio":1.5,"ids":[1,2],"unit":"c","tags":["a","b"]}`, ""},
		{"integral number", `{"ratio":2,"ids":[],"unit":"f"}`, ""},
		{"number as string", `{"ratio":"1.5"}`, "$.ratio: expected number, got string"},
		{"array element type", `{"ratio":1,"ids":[1,2.5]}`, "$.ids[1]: expected integer, got number"},
		{"array type", `{"ratio":1,"ids":"1"}`, "$.ids: expected array, got string"},
		{"enum value", `{"ratio":1,"unit":"k"}`, "$.unit: value must be one of [c f]"},
		{"enum type", `{"ratio":1,"unit":1}`, "$.unit: expected string, got number"},
		{"enum items", `{"ratio":1,"tags":["a","z"]}`, "$.tags[1]: value must be one of [a b]"},
		{"missing required", `{}`, `missing required property "ratio"`},
	} {
		err := schema.Validate(json.RawMessage(tc.raw))
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: Validate = %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestWithSchema(t *testing.T) {
	t.Parallel()
	type input struct {
		Unit string `json:"unit"`
	}
	calls := 0
	handler := func(_ context.Context, in input) (string, error) { calls++; return "unit " + in.Unit, nil }
	custom := tool.Object(map[string]*tool.Schema{"unit": tool.Enum("Temperature unit.", "c", "f")}, "unit")
	derived := tool.Func("convert", "Convert.", handler)
	overridden := tool.Func("convert", "Convert.", handler, tool.WithSchema(custom), tool.ReadOnly())

	if overridden.Info().Parameters != custom || !overridden.Info().ReadOnly {
		t.Fatalf("Info = %+v", overridden.Info())
	}
	if got, want := string(tool.Spec(overridden).Parameters), string(custom.JSON()); got != want {
		t.Fatalf("Spec parameters = %s, want %s", got, want)
	}
	if got := string(tool.Spec(derived).Parameters); got == string(custom.JSON()) {
		t.Fatal("derived schema unexpectedly equals the override")
	}

	execute := func(tl tool.Tool, args string) *model.ToolResult {
		reg, err := tool.NewRegistry(tl)
		if err != nil {
			t.Fatal(err)
		}
		return tool.NewExecutor(reg).Execute(t.Context(), &model.ToolCall{ID: "c", Name: "convert", Arguments: json.RawMessage(args)})
	}
	// The derived schema accepts any string; the override restricts it, and
	// the executor validates against the override before the handler runs.
	if res := execute(derived, `{"unit":"k"}`); res.IsError || res.Content != "unit k" || calls != 1 {
		t.Fatalf("derived = %+v, calls = %d", res, calls)
	}
	if res := execute(overridden, `{"unit":"k"}`); !res.IsError || !strings.Contains(res.Content, "value must be one of [c f]") || calls != 1 {
		t.Fatalf("override rejected = %+v, calls = %d", res, calls)
	}
	if res := execute(overridden, `{"unit":"f"}`); res.IsError || res.Content != "unit f" || calls != 2 {
		t.Fatalf("override accepted = %+v, calls = %d", res, calls)
	}

	// A nil schema disables validation and advertises an empty object.
	open := tool.Func("convert", "Convert.", handler, tool.WithSchema(nil))
	if open.Info().Parameters != nil || string(tool.Spec(open).Parameters) != `{"type":"object","properties":{}}` {
		t.Fatalf("nil schema Info = %+v, spec = %s", open.Info(), tool.Spec(open).Parameters)
	}
	if res := execute(open, `{"unit":7}`); !res.IsError || calls != 2 {
		// The handler's own decoding still rejects a number for a string.
		t.Fatalf("nil schema = %+v, calls = %d", res, calls)
	}
	if res := execute(open, `{"unit":"k","extra":true}`); res.IsError || res.Content != "unit k" || calls != 3 {
		t.Fatalf("nil schema extra = %+v, calls = %d", res, calls)
	}
}
