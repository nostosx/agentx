package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"
)

// This file is the resource server side of MCP authorization
// (https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization):
// the HTTP handler validates bearer tokens with a verifier the application
// supplies, challenges requests without valid tokens with the location of
// its Protected Resource Metadata (RFC 9728), and asks for more scopes with
// insufficient_scope.

// ProtectedResourceMetadata is the OAuth 2.0 Protected Resource Metadata of
// an MCP server (RFC 9728, section 2).
type ProtectedResourceMetadata struct {
	// Resource is the server's resource identifier: its canonical URI,
	// such as https://mcp.example.com/mcp.
	Resource string `json:"resource"`
	// AuthorizationServers lists the issuer identifiers of the
	// authorization servers; MCP requires at least one.
	AuthorizationServers              []string `json:"authorization_servers,omitempty"`
	JWKSURI                           string   `json:"jwks_uri,omitempty"`
	ScopesSupported                   []string `json:"scopes_supported,omitempty"`
	BearerMethodsSupported            []string `json:"bearer_methods_supported,omitempty"`
	ResourceSigningAlgValuesSupported []string `json:"resource_signing_alg_values_supported,omitempty"`
	ResourceName                      string   `json:"resource_name,omitempty"`
	ResourceDocumentation             string   `json:"resource_documentation,omitempty"`
	ResourcePolicyURI                 string   `json:"resource_policy_uri,omitempty"`
	ResourceTOSURI                    string   `json:"resource_tos_uri,omitempty"`
}

// ProtectedResourceMetadataHandler serves md as JSON, to any origin. Mount
// it at the path of ProtectedResourceMetadataURL(md.Resource) when the MCP
// handler does not receive that path itself.
func ProtectedResourceMetadataHandler(md *ProtectedResourceMetadata) http.Handler {
	body, _ := json.Marshal(md)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		switch r.Method {
		case http.MethodGet, http.MethodHead:
		case http.MethodOptions:
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
			return
		default:
			w.Header().Set("Allow", "GET, OPTIONS")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "max-age=3600")
		_, _ = w.Write(body)
	})
}

// TokenInfo describes a verified access token.
type TokenInfo struct {
	// Subject identifies the principal. Sessions and the requestState of
	// input_required results are bound to it.
	Subject string
	// Scopes are the scopes the token grants.
	Scopes []string
	// Audience, if set, must include the resource of the Protected
	// Resource Metadata.
	Audience []string
	// ExpiresAt, if set, is checked on every request.
	ExpiresAt time.Time
	// Extra holds verifier-specific claims.
	Extra map[string]any
}

// TokenVerifier validates an access token and describes it. It must reject
// tokens not issued for this server (RFC 8707 audience) and return an error
// for invalid or expired tokens, which the client gets as 401
// invalid_token.
type TokenVerifier func(ctx context.Context, token string, r *http.Request) (*TokenInfo, error)

// AuthOptions makes the HTTP handler a protected resource.
type AuthOptions struct {
	// Verify validates bearer tokens. It is required.
	Verify TokenVerifier
	// Metadata is the server's Protected Resource Metadata. The handler
	// serves it at MetadataURL when that request reaches it, and names
	// that URL in its challenges.
	Metadata *ProtectedResourceMetadata
	// MetadataURL is where the metadata is served. Defaults to
	// ProtectedResourceMetadataURL(Metadata.Resource).
	MetadataURL string
	// Scopes are required for every request.
	Scopes []string
	// OperationScopes, if set, returns the scopes an operation needs
	// beyond Scopes: name is the tool or prompt name or resource URI of
	// tools/call, prompts/get and resources/read. A token without them is
	// refused with 403 insufficient_scope naming every scope the operation
	// needs, so that the client can step up its authorization.
	OperationScopes func(method, name string) []string
}

// metadataURL returns where the metadata is served, or "".
func (a *AuthOptions) metadataURL() string {
	if a.MetadataURL != "" {
		return a.MetadataURL
	}
	if a.Metadata != nil && a.Metadata.Resource != "" {
		return ProtectedResourceMetadataURL(a.Metadata.Resource)
	}
	return ""
}

// challenge writes a WWW-Authenticate challenge (RFC 6750, section 3).
func (h *HTTPHandler) challenge(w http.ResponseWriter, status int, errCode, desc string, scopes []string) {
	a := h.opts.Auth
	var params []string
	if u := a.metadataURL(); u != "" {
		params = append(params, `resource_metadata="`+quote(u)+`"`)
	}
	if len(scopes) > 0 {
		params = append(params, `scope="`+quote(strings.Join(scopes, " "))+`"`)
	}
	if errCode != "" {
		params = append(params, `error="`+errCode+`"`)
	}
	if desc != "" {
		params = append(params, `error_description="`+quote(desc)+`"`)
	}
	v := "Bearer"
	if len(params) > 0 {
		v += " " + strings.Join(params, ", ")
	}
	w.Header().Set("WWW-Authenticate", v)
	code := CodeInvalidRequest
	writeError(w, status, nil, &Error{Code: code, Message: http.StatusText(status) + cond(desc != "", ": "+desc, "")})
}

func cond[T any](ok bool, a, b T) T {
	if ok {
		return a
	}
	return b
}

// quote escapes a quoted-string value.
func quote(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}

// authenticate checks the bearer token of a request. It writes the
// challenge and returns false when the request may not proceed.
func (h *HTTPHandler) authenticate(w http.ResponseWriter, r *http.Request) (*TokenInfo, bool) {
	a := h.opts.Auth
	if a == nil {
		return nil, true
	}
	scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	token = strings.TrimSpace(token)
	if !strings.EqualFold(scheme, "Bearer") || token == "" {
		// A request without a token gets no error code (RFC 6750, 3.1).
		h.challenge(w, http.StatusUnauthorized, "", "", a.Scopes)
		return nil, false
	}
	if a.Verify == nil {
		h.challenge(w, http.StatusUnauthorized, "invalid_token", "no token verifier", a.Scopes)
		return nil, false
	}
	info, err := a.Verify(r.Context(), token, r)
	if err != nil || info == nil {
		h.challenge(w, http.StatusUnauthorized, "invalid_token", "The access token is invalid", a.Scopes)
		return nil, false
	}
	if !info.ExpiresAt.IsZero() && time.Now().After(info.ExpiresAt) {
		h.challenge(w, http.StatusUnauthorized, "invalid_token", "The access token expired", a.Scopes)
		return nil, false
	}
	if len(info.Audience) > 0 && a.Metadata != nil && a.Metadata.Resource != "" &&
		!slices.Contains(info.Audience, a.Metadata.Resource) {
		h.challenge(w, http.StatusUnauthorized, "invalid_token", "The access token was issued for another resource", a.Scopes)
		return nil, false
	}
	if missing(info.Scopes, a.Scopes) {
		h.challenge(w, http.StatusForbidden, "insufficient_scope", "Additional scopes required", a.Scopes)
		return nil, false
	}
	return info, true
}

// authorizeOperation checks the scopes an operation needs. It writes the
// challenge and returns false when the token lacks them.
func (h *HTTPHandler) authorizeOperation(w http.ResponseWriter, c *incoming) bool {
	a := h.opts.Auth
	if a == nil || a.OperationScopes == nil || c.token == nil {
		return true
	}
	var p struct {
		Name string `json:"name"`
		URI  string `json:"uri"`
	}
	_ = json.Unmarshal(c.params, &p)
	var name string
	switch c.method {
	case "tools/call", "prompts/get":
		name = p.Name
	case "resources/read":
		name = p.URI
	}
	extra := a.OperationScopes(c.method, name)
	if len(extra) == 0 {
		return true
	}
	need := slices.Compact(slices.Sorted(slices.Values(append(slices.Clone(a.Scopes), extra...))))
	if !missing(c.token.Scopes, need) {
		return true
	}
	h.challenge(w, http.StatusForbidden, "insufficient_scope", "Additional scopes required for "+c.method, need)
	return false
}

// missing reports whether granted lacks any of need.
func missing(granted, need []string) bool {
	for _, s := range need {
		if !slices.Contains(granted, s) {
			return true
		}
	}
	return false
}
