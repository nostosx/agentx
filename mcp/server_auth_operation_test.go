package mcp

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOperationScopesUseMethodIdentifier(t *testing.T) {
	t.Parallel()
	for _, op := range []struct {
		method, name, field, extra, output string
		params                             map[string]any
	}{
		{"tools/call", "delete", "name", "uri", "deleted target", map[string]any{"arguments": map[string]any{"text": "target"}}},
		{"prompts/get", "greet", "name", "uri", "Hello, reader", map[string]any{"arguments": map[string]any{"who": "reader"}}},
		{"resources/read", "file:///readme.md", "uri", "name", "# Hello", nil},
	} {
		for _, extra := range []bool{false, true} {
			for _, granted := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/extra=%v/granted=%v", op.method, extra, granted), func(t *testing.T) {
					f := newFixture(t, nil)
					var got string
					h := f.s.HTTPHandler(&HTTPOptions{Auth: &AuthOptions{
						Scopes: []string{"files:read"},
						Verify: func(context.Context, string, *http.Request) (*TokenInfo, error) {
							scopes := []string{"files:read"}
							if granted {
								scopes = append(scopes, "files:write")
							}
							return &TokenInfo{Subject: "reader", Scopes: scopes}, nil
						},
						OperationScopes: func(method, name string) []string {
							got = name
							if method == op.method && name == op.name {
								return []string{"files:write"}
							}
							return nil
						},
					}})
					defer h.Close()
					params := map[string]any{op.field: op.name}
					maps.Copy(params, op.params)
					if extra {
						params[op.extra] = "irrelevant"
					}
					body, headers := modernRequest(1, op.method, params)
					headers["Mcp-Name"] = op.name
					r := httptest.NewRequest(http.MethodPost, "http://localhost/mcp", strings.NewReader(body))
					r.Header.Set("Content-Type", "application/json")
					r.Header.Set("Authorization", "Bearer fixture")
					for k, v := range headers {
						r.Header.Set(k, v)
					}
					w := httptest.NewRecorder()
					h.ServeHTTP(w, r)
					if got != op.name {
						t.Errorf("OperationScopes identifier = %q, want %q", got, op.name)
					}
					if !granted {
						if w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), op.output) {
							t.Fatalf("insufficient scopes: status=%d body=%s", w.Code, w.Body.String())
						}
					} else if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), op.output) {
						t.Fatalf("granted scopes: status=%d body=%s", w.Code, w.Body.String())
					}
				})
			}
		}
	}
}
