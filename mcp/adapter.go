package mcp

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/nostosx/agentx/model"
	"github.com/nostosx/agentx/tool"
)

var invalidName = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// maxToolName is the longest tool name providers accept.
const maxToolName = 64

// ToolName returns the agentx name of an MCP tool: mcp__<server>__<tool>,
// sanitized and limited to 64 characters. A name that is too long is cut
// and ends with a hash of the server and tool names, so that names sharing
// a long prefix stay distinct. Tools may use a different name: see Tools.
func ToolName(server, name string) string {
	n := "mcp__" + invalidName.ReplaceAllString(server, "_") + "__" + invalidName.ReplaceAllString(name, "_")
	if len(n) > maxToolName {
		n = hashedName(n, server, name)
	}
	return n
}

// hashedName ends n with a short hash of the original server and tool
// names, cutting n to fit.
func hashedName(n, server, name string) string {
	sum := sha256.Sum256([]byte(server + "\x00" + name))
	suffix := "_" + hex.EncodeToString(sum[:4])
	return n[:min(len(n), maxToolName-len(suffix))] + suffix
}

// AdapterOption configures adapted tools.
type AdapterOption func(*adapter)

type adapter struct {
	filter        func(Tool) bool
	approval      func(Tool) bool
	approvalSet   bool
	trustReadOnly bool
	prefix        bool
}

// Filter keeps only tools for which keep returns true.
func Filter(keep func(Tool) bool) AdapterOption { return func(a *adapter) { a.filter = keep } }

// RequireApprovalFor marks tools for which fn returns true as requiring
// approval, replacing the default: by default a tool requires approval
// unless the server hints it is not destructive, since the MCP default of
// destructiveHint is true. With TrustReadOnlyHint, tools hinted read-only
// do not require approval either. RequireApprovalFor(nil) requires approval
// for no tool at all.
func RequireApprovalFor(fn func(Tool) bool) AdapterOption {
	return func(a *adapter) { a.approval, a.approvalSet = fn, true }
}

// TrustReadOnlyHint trusts the server's readOnlyHint annotations: tools
// hinted read-only are marked tool.Info.ReadOnly, so adjacent calls run
// concurrently, and they skip the default approval. Annotations are
// self-reported by the server, so use this only with servers you trust.
//
// Without this option a hint that removes approval is still trusted: the
// default approval relies on destructiveHint, so a server that declares a
// tool destructiveHint: false, truthfully or not, gets it called without
// approval. Use RequireApprovalFor to decide approval without annotations.
func TrustReadOnlyHint() AdapterOption { return func(a *adapter) { a.trustReadOnly = true } }

// WithoutPrefix keeps the server's original tool names.
func WithoutPrefix() AdapterOption { return func(a *adapter) { a.prefix = false } }

func hint(h *bool) bool { return h != nil && *h }

func readOnly(t Tool) bool { return t.Annotations != nil && hint(t.Annotations.ReadOnlyHint) }

// destructive reports whether t may be destructive. A missing hint counts
// as destructive, as in the MCP specification.
func destructive(t Tool) bool {
	return t.Annotations == nil || t.Annotations.DestructiveHint == nil || *t.Annotations.DestructiveHint
}

func (a *adapter) readOnly(t Tool) bool { return a.trustReadOnly && readOnly(t) }

func (a *adapter) requiresApproval(t Tool) bool {
	switch {
	case a.approvalSet:
		return a.approval != nil && a.approval(t)
	case a.readOnly(t):
		return false
	}
	return destructive(t)
}

// Tools lists the server's tools and adapts them to tool.Tool. server names
// the server in tool names. Tools whose sanitized names collide, such as
// "a.b" and "a_b", are told apart by a hash suffix on the names that
// sanitization changed. A tool's name therefore depends on the other tools
// in the list and may change when the server's tool list changes, for
// example between a run and its resumption.
func Tools(ctx context.Context, c *Client, server string, opts ...AdapterOption) ([]tool.Tool, error) {
	a := adapter{prefix: true}
	for _, opt := range opts {
		opt(&a)
	}
	list, err := c.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	list = slices.DeleteFunc(list, func(t Tool) bool { return a.filter != nil && !a.filter(t) })
	names := make([]string, len(list))
	count := map[string]int{}
	for i, t := range list {
		names[i] = t.Name
		if a.prefix {
			names[i] = ToolName(server, t.Name)
		}
		count[names[i]]++
	}
	if a.prefix {
		for i, t := range list {
			if count[names[i]] > 1 && invalidName.MatchString(t.Name) {
				names[i] = hashedName(names[i], server, t.Name)
			}
		}
	}
	var out []tool.Tool
	for i, t := range list {
		name := names[i]
		if err := tool.ValidateName(name); err != nil {
			return nil, fmt.Errorf("mcp: server %s: %w", server, err)
		}
		schema := t.InputSchema
		if len(schema) == 0 || string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		info := tool.Info{
			Name:             name,
			Description:      cmp.Or(t.Description, t.Title),
			RawParameters:    schema,
			ReadOnly:         a.readOnly(t),
			RequiresApproval: a.requiresApproval(t),
		}
		out = append(out, &mcpTool{client: c, name: t.Name, info: info})
	}
	return out, nil
}

type mcpTool struct {
	client *Client
	name   string
	info   tool.Info
}

func (t *mcpTool) Info() tool.Info { return t.info }

func (t *mcpTool) Call(ctx context.Context, args json.RawMessage) (*tool.Result, error) {
	res, err := t.client.CallTool(ctx, t.name, args)
	if err != nil {
		return nil, err
	}
	return ToolResultFrom(res), nil
}

// ToolResultFrom converts the result of tools/call to a tool result. Text
// content and text resources become Content. Images, audio and embedded
// resource blobs become Parts, of the type their MIME type tells (see
// model.MediaPart), with the file name of the resource URI, so models
// that take media see them; a provider that cannot take one fails the
// request as for any other part. Resource links stay text, since models
// cannot fetch their URIs, as does media whose data is not valid base64.
// A result without text or media gives its structured content as text.
func ToolResultFrom(res *CallToolResult) *tool.Result {
	var texts []string
	var parts []model.Part
	for _, c := range res.Content {
		if p, ok := mediaPart(c); ok {
			parts = append(parts, p)
		} else if t, ok := formatItem(c); ok {
			texts = append(texts, t)
		}
	}
	out := &tool.Result{Content: strings.Join(texts, "\n"), Parts: parts, IsError: res.IsError}
	if len(texts) == 0 && len(parts) == 0 && len(res.StructuredContent) > 0 {
		out.Content = string(res.StructuredContent)
	}
	return out
}

// mediaPart returns the model part of image, audio and blob content.
func mediaPart(c Content) (model.Part, bool) {
	switch c.Type {
	case "image", "audio":
		data, err := base64.StdEncoding.DecodeString(c.Data)
		if err != nil || len(data) == 0 {
			return model.Part{}, false
		}
		mt := cmp.Or(c.MimeType, http.DetectContentType(data))
		if c.Type == "image" {
			return model.ImagePart(mt, data), true
		}
		return model.AudioPart(mt, data), true
	case "resource":
		r := c.Resource
		if r == nil || r.Blob == "" {
			return model.Part{}, false
		}
		data, err := base64.StdEncoding.DecodeString(r.Blob)
		if err != nil {
			return model.Part{}, false
		}
		p := model.MediaPart(cmp.Or(r.MimeType, http.DetectContentType(data)), data)
		if p.Type != model.PartImage {
			p.Filename = uriName(r.URI)
		}
		return p, true
	}
	return model.Part{}, false
}

// uriName returns the last element of a URI's path, or "".
func uriName(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	name := path.Base(u.Path)
	if name == "." || name == "/" {
		return ""
	}
	return name
}

// FormatContent renders a tool result as text, describing images, audio
// and blobs in brackets. ToolResultFrom keeps them as parts instead.
func FormatContent(res *CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if t, ok := formatItem(c); ok {
			parts = append(parts, t)
		}
	}
	if len(parts) == 0 && len(res.StructuredContent) > 0 {
		return string(res.StructuredContent)
	}
	return strings.Join(parts, "\n")
}

// formatItem renders one content item as text; ok is false for content
// that has none, such as unknown types.
func formatItem(c Content) (text string, ok bool) {
	switch c.Type {
	case "text":
		return c.Text, true
	case "image", "audio":
		return fmt.Sprintf("[%s: %s, %d bytes base64]", c.Type, c.MimeType, len(c.Data)), true
	case "resource_link":
		return fmt.Sprintf("[resource: %s %s]", c.Name, c.URI), true
	case "resource":
		if r := c.Resource; r != nil {
			if r.Text != "" {
				return r.Text, true
			}
			return fmt.Sprintf("[resource: %s, %s]", r.URI, r.MimeType), true
		}
	}
	return "", false
}
