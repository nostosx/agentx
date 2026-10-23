package tool_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/tool"
)

func TestExecutorConvertsFailures(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		handler tool.Handler
		args    string
		want    string
		isError bool
	}{
		{"success", func(context.Context, json.RawMessage) (*tool.Result, error) { return tool.Text("done"), nil }, `{}`, "done", false},
		{"handler error", func(context.Context, json.RawMessage) (*tool.Result, error) { return nil, errors.New("failure") }, `{}`, "failure", true},
		{"panic", func(context.Context, json.RawMessage) (*tool.Result, error) { panic("boom") }, `{}`, "panicked: boom", true},
		{"nil result", func(context.Context, json.RawMessage) (*tool.Result, error) { return nil, nil }, `{}`, "", false},
		{"model error", func(context.Context, json.RawMessage) (*tool.Result, error) { return tool.Errorf("bad input"), nil }, `{}`, "bad input", true},
		{"validation", func(context.Context, json.RawMessage) (*tool.Result, error) {
			return nil, errors.New("invalid arguments reached handler")
		}, `[]`, "expected object", true},
		{"empty arguments", func(_ context.Context, args json.RawMessage) (*tool.Result, error) {
			if string(args) != "{}" {
				return nil, errors.New("unexpected arguments")
			}
			return tool.Text("empty"), nil
		}, "", "empty", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r, _ := tool.NewRegistry(tool.New("test", "", tool.Object(nil), tt.handler))
			res := tool.NewExecutor(r).Execute(t.Context(), &model.ToolCall{ID: "id", Name: "test", Arguments: json.RawMessage(tt.args)})
			contentOK := res.Content == tt.want
			if tt.isError {
				contentOK = strings.Contains(res.Content, tt.want)
			}
			if res.CallID != "id" || res.Name != "test" || res.IsError != tt.isError || !contentOK {
				t.Fatalf("Execute=%+v", res)
			}
		})
	}
	if res := tool.NewExecutor(nil).Execute(t.Context(), &model.ToolCall{Name: "missing"}); !res.IsError || !strings.Contains(res.Content, "does not exist") {
		t.Fatalf("missing=%+v", res)
	}
}

func TestExecutorMiddlewareObservesPanic(t *testing.T) {
	t.Parallel()
	var caught *tool.PanicError
	mw := func(next tool.Invoker) tool.Invoker {
		return func(ctx context.Context, c *tool.Call) (*tool.Result, error) {
			res, err := next(ctx, c)
			errors.As(err, &caught)
			return res, err
		}
	}
	r, _ := tool.NewRegistry(tool.New("panic", "", nil, func(context.Context, json.RawMessage) (*tool.Result, error) { panic("secret") }))
	res := tool.NewExecutor(r, mw).Execute(t.Context(), &model.ToolCall{Name: "panic"})
	if caught == nil || caught.Tool != "panic" || caught.Value != "secret" || len(caught.Stack) == 0 || strings.Contains(res.Content, "goroutine") {
		t.Fatalf("panic=%+v result=%+v", caught, res)
	}
}

func TestExecutorRawSchemaBypassesSubsetValidation(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"type":"array","prefixItems":[{"type":"string"}]}`)
	custom := tool.New("custom", "", tool.Object(nil), func(_ context.Context, args json.RawMessage) (*tool.Result, error) {
		return tool.Text(string(args)), nil
	}, func(info *tool.Info) { info.RawParameters = raw })
	if spec := tool.Spec(custom); string(spec.Parameters) != string(raw) {
		t.Fatalf("raw schema=%s", spec.Parameters)
	}
	registry, err := tool.NewRegistry(custom)
	if err != nil {
		t.Fatal(err)
	}
	result := tool.NewExecutor(registry).Execute(t.Context(), &model.ToolCall{Name: "custom", Arguments: json.RawMessage(`["value"]`)})
	if result.IsError || result.Content != `["value"]` {
		t.Fatalf("raw schema arguments rejected: %+v", result)
	}
}

func TestExecutorRecoversMiddlewarePanic(t *testing.T) {
	t.Parallel()
	mw := func(tool.Invoker) tool.Invoker {
		return func(context.Context, *tool.Call) (*tool.Result, error) { panic("middleware bug") }
	}
	r, _ := tool.NewRegistry(tool.New("ok", "", nil, func(context.Context, json.RawMessage) (*tool.Result, error) { return tool.Text("ok"), nil }))
	res := tool.NewExecutor(r, mw).Execute(t.Context(), &model.ToolCall{ID: "1", Name: "ok"})
	if !res.IsError || !strings.Contains(res.Content, "middleware bug") {
		t.Fatalf("result = %+v", res)
	}
}

func TestExecutorKeepsPartsAndMetadata(t *testing.T) {
	t.Parallel()
	image := model.ImagePart("image/png", []byte{1, 2})
	r, err := tool.NewRegistry(tool.New("shot", "", nil, func(context.Context, json.RawMessage) (*tool.Result, error) {
		res := tool.Media(strings.Repeat("x", 100), image)
		res.Metadata = map[string]any{"width": 1}
		return res, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	res := tool.NewExecutor(r, tool.TruncateOutput(50)).Execute(t.Context(), &model.ToolCall{ID: "c", Name: "shot"})
	if res.IsError || len(res.Content) > 100 || len(res.Parts) != 1 || res.Parts[0].MediaType != "image/png" || res.Metadata["width"] != 1 {
		t.Fatalf("result = %+v", res)
	}
}

// encoding/json decodes "Command" into a field named command, so a call
// whose arguments spell a declared property in another case must not reach
// the tool: hooks and permission rules read the exact key and would see
// another value.
func TestExecutorRejectsCaseVariantKeys(t *testing.T) {
	t.Parallel()
	type input struct {
		Command string `json:"command"`
	}
	var ran []string
	run := tool.Func("run", "Run a command.", func(_ context.Context, in input) (string, error) {
		ran = append(ran, in.Command)
		return "ran", nil
	})
	r, _ := tool.NewRegistry(run)
	for _, args := range []string{`{"command":"ls","Command":"rm -rf ~"}`, `{"COMMAND":"rm -rf ~"}`} {
		res := tool.NewExecutor(r).Execute(t.Context(), &model.ToolCall{Name: "run", Arguments: json.RawMessage(args)})
		if !res.IsError || !strings.Contains(res.Content, "only in case") {
			t.Errorf("%s: Execute = %+v", args, res)
		}
	}
	if len(ran) != 0 {
		t.Fatalf("the tool ran with %q", ran)
	}
}
