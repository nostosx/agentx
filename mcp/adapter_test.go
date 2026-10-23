package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nostosx/agentx/tool"
)

// listServer serves tools/list with the given pages, keyed by cursor.
func listServer(t *testing.T, pages map[string]string) *Client {
	t.Helper()
	return stdioServer(t, func(req rpcRequest, write func(string)) {
		if req.Method != "tools/list" {
			return
		}
		write(result(req.ID, pages[req.Params.Cursor]))
	})
}

func TestToolNameTruncationKeepsNamesDistinct(t *testing.T) {
	long := strings.Repeat("x", 70)
	a, b := ToolName("srv", long+"a"), ToolName("srv", long+"b")
	if a == b || len(a) > 64 || len(b) > 64 {
		t.Fatalf("names %q and %q", a, b)
	}
	if a != ToolName("srv", long+"a") {
		t.Error("ToolName is not deterministic")
	}
	if err := tool.ValidateName(a); err != nil {
		t.Error(err)
	}
	if got := ToolName("srv", "read_file"); got != "mcp__srv__read_file" {
		t.Errorf("short name changed: %q", got)
	}
}

func TestToolsAdapterNamesAndApproval(t *testing.T) {
	long := strings.Repeat("y", 70)
	list := fmt.Sprintf(`{"resultType":"complete","tools":[
		{"name":"a.b","inputSchema":{"type":"object"}},
		{"name":"a_b","inputSchema":{"type":"object"}},
		{"name":"%[1]s1","inputSchema":{"type":"object"}},
		{"name":"%[1]s2","inputSchema":{"type":"object"}},
		{"name":"bare","inputSchema":{"type":"object"}},
		{"name":"ro","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":true}},
		{"name":"safe","inputSchema":{"type":"object"},"annotations":{"destructiveHint":false}},
		{"name":"rm","inputSchema":{"type":"object"},"annotations":{"destructiveHint":true}}
	]}`, long)
	c := listServer(t, map[string]string{"": strings.ReplaceAll(list, "\n", "")})

	tools, err := Tools(t.Context(), c, "s")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.NewRegistry(tools...); err != nil {
		t.Fatalf("names collide: %v", err)
	}
	info := map[string]tool.Info{}
	for _, tl := range tools {
		i := tl.Info()
		info[tl.(*mcpTool).name] = i
	}
	if got := info["a_b"].Name; got != "mcp__s__a_b" {
		t.Errorf("a_b renamed to %q", got)
	}
	if got := info["a.b"].Name; !strings.HasPrefix(got, "mcp__s__a_b_") {
		t.Errorf("a.b named %q", got)
	}
	for name, want := range map[string]bool{"bare": true, "ro": true, "safe": false, "rm": true} {
		if i := info[name]; i.RequiresApproval != want || i.ReadOnly {
			t.Errorf("%s: approval = %v, read-only = %v; want approval %v, not read-only", name, i.RequiresApproval, i.ReadOnly, want)
		}
	}

	tools, err = Tools(t.Context(), c, "s", TrustReadOnlyHint())
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools {
		i := tl.Info()
		if n := tl.(*mcpTool).name; n == "ro" && (!i.ReadOnly || i.RequiresApproval) {
			t.Errorf("trusted ro: %+v", i)
		} else if n == "bare" && (i.ReadOnly || !i.RequiresApproval) {
			t.Errorf("trusted bare: %+v", i)
		}
	}
}

func TestPaginateStopsOnCursorCycle(t *testing.T) {
	c := listServer(t, map[string]string{
		"":  `{"tools":[{"name":"t1","inputSchema":{}}],"nextCursor":"a"}`,
		"a": `{"tools":[{"name":"t2","inputSchema":{}}],"nextCursor":"b"}`,
		"b": `{"tools":[{"name":"t3","inputSchema":{}}],"nextCursor":"a"}`,
	})
	tools, err := c.ListTools(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 3 {
		t.Fatalf("got %d tools, want 3", len(tools))
	}
}

// toolOf returns the adapted tool whose server name is name.
func toolOf(t *testing.T, tools []tool.Tool, name string) tool.Tool {
	t.Helper()
	for _, tl := range tools {
		if tl.(*mcpTool).name == name {
			return tl
		}
	}
	t.Fatalf("no tool %q", name)
	return nil
}

func TestToolCallThroughAdapter(t *testing.T) {
	const list = `{"tools":[{"name":"read.file","inputSchema":{"type":"object"}}]}`
	args := make(chan rpcRequest, 4)
	c := stdioServer(t, func(req rpcRequest, write func(string)) {
		switch req.Method {
		case "tools/list":
			write(result(req.ID, list))
		case "tools/call":
			args <- req
			switch string(req.Params.Arguments) {
			case `{"path":"missing"}`:
				write(result(req.ID, `{"content":[{"type":"text","text":"no such file"}],"isError":true}`))
			case `{"path":"rpc"}`:
				write(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":%d,"message":"bad path"}}`, req.ID, CodeInvalidParams))
			default:
				write(result(req.ID, `{"content":[{"type":"text","text":"hello"}]}`))
			}
		}
	})
	tools, err := Tools(t.Context(), c, "fs")
	if err != nil {
		t.Fatal(err)
	}
	tl := toolOf(t, tools, "read.file")

	res, err := tl.Call(t.Context(), json.RawMessage(`{"path":"a.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "hello" || res.IsError {
		t.Errorf("result = %+v", res)
	}
	if req := <-args; req.Params.Name != "read.file" || string(req.Params.Arguments) != `{"path":"a.txt"}` {
		t.Errorf("forwarded name %q, arguments %s", req.Params.Name, req.Params.Arguments)
	}

	if _, err := tl.Call(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if req := <-args; string(req.Params.Arguments) != `{}` {
		t.Errorf("nil arguments forwarded as %s, want {}", req.Params.Arguments)
	}

	res, err = tl.Call(t.Context(), json.RawMessage(`{"path":"missing"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || res.Content != "no such file" {
		t.Errorf("isError result = %+v", res)
	}
	<-args

	var e *Error
	if _, err := tl.Call(t.Context(), json.RawMessage(`{"path":"rpc"}`)); !errors.As(err, &e) || e.Code != CodeInvalidParams {
		t.Errorf("err = %v, want JSON-RPC error %d", err, CodeInvalidParams)
	}
	<-args

	_ = c.Close()
	<-c.Done()
	if _, err := tl.Call(t.Context(), nil); !errors.Is(err, ErrClosed) {
		t.Errorf("after close: err = %v, want ErrClosed", err)
	}
}

func TestFormatContent(t *testing.T) {
	for name, tc := range map[string]struct {
		res  CallToolResult
		want string
	}{
		"text":          {CallToolResult{Content: []Content{{Type: "text", Text: "a"}, {Type: "text", Text: "b"}}}, "a\nb"},
		"image":         {CallToolResult{Content: []Content{{Type: "image", MimeType: "image/png", Data: "AAAA"}}}, "[image: image/png, 4 bytes base64]"},
		"audio":         {CallToolResult{Content: []Content{{Type: "audio", MimeType: "audio/wav", Data: "AA"}}}, "[audio: audio/wav, 2 bytes base64]"},
		"resource link": {CallToolResult{Content: []Content{{Type: "resource_link", Name: "readme", URI: "file:///README"}}}, "[resource: readme file:///README]"},
		"resource text": {CallToolResult{Content: []Content{{Type: "resource", Resource: &ResourceContents{URI: "file:///a", Text: "body"}}}}, "body"},
		"resource blob": {CallToolResult{Content: []Content{{Type: "resource", Resource: &ResourceContents{URI: "file:///b", MimeType: "application/pdf", Blob: "AA"}}}}, "[resource: file:///b, application/pdf]"},
		"nil resource":  {CallToolResult{Content: []Content{{Type: "resource"}}}, ""},
		"unknown type":  {CallToolResult{Content: []Content{{Type: "video", Data: "AA"}}}, ""},
		"structured":    {CallToolResult{StructuredContent: json.RawMessage(`{"n":1}`)}, `{"n":1}`},
		"text over structured": {
			CallToolResult{Content: []Content{{Type: "text", Text: "1"}}, StructuredContent: json.RawMessage(`{"n":1}`)}, "1",
		},
		"mixed": {
			CallToolResult{Content: []Content{{Type: "text", Text: "see"}, {Type: "image", MimeType: "image/jpeg", Data: "AAA"}}},
			"see\n[image: image/jpeg, 3 bytes base64]",
		},
	} {
		if got := FormatContent(&tc.res); got != tc.want {
			t.Errorf("%s: FormatContent = %q, want %q", name, got, tc.want)
		}
	}
}

func TestToolsAdapterOptions(t *testing.T) {
	list := `{"tools":[
		{"name":"keep","title":"Keep title","inputSchema":{"type":"object"}},
		{"name":"ro","description":"reads","annotations":{"readOnlyHint":true}},
		{"name":"drop","inputSchema":{"type":"object"}}]}`
	c := listServer(t, map[string]string{"": strings.ReplaceAll(list, "\n", "")})

	tools, err := Tools(t.Context(), c, "s", Filter(func(tl Tool) bool { return tl.Name != "drop" }), WithoutPrefix())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2 after Filter", len(tools))
	}
	keep, ro := toolOf(t, tools, "keep").Info(), toolOf(t, tools, "ro").Info()
	if keep.Name != "keep" || ro.Name != "ro" {
		t.Errorf("WithoutPrefix names = %q, %q", keep.Name, ro.Name)
	}
	if keep.Description != "Keep title" || ro.Description != "reads" {
		t.Errorf("descriptions = %q, %q", keep.Description, ro.Description)
	}
	if got := string(ro.RawParameters); got != `{"type":"object","properties":{}}` {
		t.Errorf("missing schema became %s", got)
	}

	// RequireApprovalFor replaces the default, even over TrustReadOnlyHint.
	tools, err = Tools(t.Context(), c, "s", TrustReadOnlyHint(), RequireApprovalFor(func(tl Tool) bool { return tl.Name == "ro" }))
	if err != nil {
		t.Fatal(err)
	}
	if i := toolOf(t, tools, "ro").Info(); !i.RequiresApproval || !i.ReadOnly {
		t.Errorf("ro: %+v, want approval and read-only", i)
	}
	if i := toolOf(t, tools, "keep").Info(); i.RequiresApproval {
		t.Errorf("keep requires approval despite RequireApprovalFor")
	}

	// A nil function requires approval for no tool.
	tools, err = Tools(t.Context(), c, "s", RequireApprovalFor(nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools {
		if tl.Info().RequiresApproval {
			t.Errorf("%s requires approval with RequireApprovalFor(nil)", tl.Info().Name)
		}
	}
}

func TestToolsAdapterErrors(t *testing.T) {
	c := listServer(t, map[string]string{"": `{"tools":[{"name":"a.b","inputSchema":{}}]}`})
	if _, err := Tools(t.Context(), c, "s", WithoutPrefix()); err == nil || !strings.Contains(err.Error(), "mcp: server s") {
		t.Errorf("invalid unprefixed name: err = %v", err)
	}

	c = stdioServer(t, func(req rpcRequest, write func(string)) {
		write(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":%d,"message":"no tools"}}`, req.ID, CodeMethodNotFound))
	})
	var e *Error
	if tools, err := Tools(t.Context(), c, "s"); !errors.As(err, &e) || e.Code != CodeMethodNotFound || tools != nil {
		t.Errorf("tools = %v, err = %v", tools, err)
	}
}
