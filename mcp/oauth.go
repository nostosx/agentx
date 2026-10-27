package mcp

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/nostosx/kitx/boundedio"
	"github.com/nostosx/kitx/utf8text"

	"github.com/nostosx/agentx/internal/httpclient"
)

// This file is the client side of MCP authorization
// (https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization,
// and the same flow in
// https://modelcontextprotocol.io/specification/2025-11-25/basic/authorization).
// An HTTP round tripper adds the access token to every request. When the
// server answers 401, it reads the WWW-Authenticate challenge, fetches the
// Protected Resource Metadata (RFC 9728), discovers the authorization
// server (RFC 8414 or OpenID Connect Discovery), obtains a client ID
// (pre-registered, Client ID Metadata Document, or Dynamic Client
// Registration, RFC 7591), runs the authorization code flow with PKCE S256
// or the client credentials grant, both with the resource parameter of RFC
// 8707, and repeats the request with the new token. A 403
// insufficient_scope challenge steps the authorization up to the union of
// the scopes requested before and the scopes challenged.

// OAuth grants.
const (
	GrantAuthorizationCode = "authorization_code"
	GrantClientCredentials = "client_credentials"
)

// Client authentication methods at the token endpoint.
const (
	AuthMethodNone              = "none"
	AuthMethodClientSecretBasic = "client_secret_basic"
	AuthMethodClientSecretPost  = "client_secret_post"
	AuthMethodPrivateKeyJWT     = "private_key_jwt"
)

// ErrAuthorization is wrapped by the errors of a failed authorization.
var ErrAuthorization = errors.New("mcp: authorization failed")

// OAuthConfig configures the authorization of MCP requests over Streamable
// HTTP; see WithOAuth.
type OAuthConfig struct {
	// Grant is GrantAuthorizationCode, the default, or
	// GrantClientCredentials for machine-to-machine access without a user,
	// as the OAuth client credentials extension of MCP
	// (io.modelcontextprotocol/oauth-client-credentials) describes. The
	// latter needs a pre-registered ClientID with ClientSecret or
	// ClientAssertion.
	Grant string

	// ClientID and ClientSecret are pre-registered client credentials,
	// which take precedence over the other ways to obtain a client ID.
	ClientID     string
	ClientSecret string
	// ClientIssuer, if set, is the issuer of the authorization server the
	// pre-registered credentials belong to. Authorization fails if the
	// server names another one, rather than sending it the credentials.
	ClientIssuer string
	// TokenEndpointAuthMethod is how the pre-registered client
	// authenticates at the token endpoint. Defaults to
	// AuthMethodClientSecretBasic, or AuthMethodClientSecretPost when the
	// server supports only that, with a secret; to AuthMethodPrivateKeyJWT
	// with ClientAssertion; and to AuthMethodNone otherwise.
	TokenEndpointAuthMethod string
	// ClientAssertion returns a signed JWT for AuthMethodPrivateKeyJWT
	// (RFC 7523); see PrivateKeyJWT.
	ClientAssertion func(ctx context.Context, issuer, tokenEndpoint string) (string, error)

	// ClientMetadataURL is the HTTPS URL of the client's Client ID Metadata
	// Document, used as the client ID with authorization servers that
	// declare client_id_metadata_document_supported. The document must list
	// RedirectURL.
	ClientMetadataURL string

	// ClientName, ClientURI and ApplicationType describe the client in
	// Dynamic Client Registration, the fallback for servers that support
	// neither of the above. ApplicationType defaults to "native" for a
	// loopback RedirectURL and "web" otherwise.
	ClientName      string
	ClientURI       string
	ApplicationType string

	// RedirectURL receives the authorization response; it must be a
	// loopback or HTTPS URL. See LoopbackRedirect.
	RedirectURL string
	// Authorize lets the user authorize the client: it shows authURL to
	// the user, usually by opening a browser, and returns the authorization
	// response delivered to RedirectURL. It is required for
	// GrantAuthorizationCode. LoopbackRedirect.Authorize implements it.
	Authorize func(ctx context.Context, authURL string) (*AuthorizationResponse, error)

	// Scopes are requested when neither the challenge nor the Protected
	// Resource Metadata names any.
	Scopes []string
	// Store keeps tokens and dynamic client registrations. Defaults to a
	// MemoryTokenStore.
	Store TokenStore
	// HTTPClient sends the requests to the authorization server and for the
	// metadata. Defaults to an http.Client with the wrapped transport.
	HTTPClient *http.Client
	// SelectAuthorizationServer chooses among the authorization servers of
	// the Protected Resource Metadata. Defaults to the first.
	SelectAuthorizationServer func(issuers []string) string
}

// AuthorizationResponse is the authorization server's response at the
// redirect URL (RFC 6749, section 4.1.2), with the issuer of RFC 9207.
type AuthorizationResponse struct {
	Code             string
	State            string
	Issuer           string
	Error            string
	ErrorDescription string
}

// ParseAuthorizationResponse reads an authorization response from the
// query of the redirect.
func ParseAuthorizationResponse(q url.Values) *AuthorizationResponse {
	return &AuthorizationResponse{
		Code: q.Get("code"), State: q.Get("state"), Issuer: q.Get("iss"),
		Error: q.Get("error"), ErrorDescription: q.Get("error_description"),
	}
}

// Token is an OAuth access token with its refresh token.
type Token struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry,omitzero"`
	// Scope is the space-separated scope granted, or requested if the
	// server did not say.
	Scope string `json:"scope,omitempty"`
	// Issuer is the authorization server that issued the token, and
	// Resource the resource it was requested for.
	Issuer   string `json:"issuer,omitempty"`
	Resource string `json:"resource,omitempty"`
}

// valid reports whether the token can still be used, with a margin for
// clock skew and latency.
func (t *Token) valid() bool {
	return t != nil && t.AccessToken != "" && (t.Expiry.IsZero() || time.Until(t.Expiry) > 30*time.Second)
}

// ClientRegistration is a client registered with an authorization server
// (RFC 7591, section 3.2.1).
type ClientRegistration struct {
	ClientID                string   `json:"client_id"`
	ClientSecret            string   `json:"client_secret,omitempty"`
	ClientIDIssuedAt        int64    `json:"client_id_issued_at,omitempty"`
	ClientSecretExpiresAt   int64    `json:"client_secret_expires_at,omitempty"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method,omitempty"`
	RedirectURIs            []string `json:"redirect_uris,omitempty"`
}

// AuthorizationServerMetadata is the metadata of an authorization server
// (RFC 8414, section 2, and OpenID Connect Discovery).
type AuthorizationServerMetadata struct {
	Issuer                                     string   `json:"issuer"`
	AuthorizationEndpoint                      string   `json:"authorization_endpoint,omitempty"`
	TokenEndpoint                              string   `json:"token_endpoint,omitempty"`
	RegistrationEndpoint                       string   `json:"registration_endpoint,omitempty"`
	JWKSURI                                    string   `json:"jwks_uri,omitempty"`
	ScopesSupported                            []string `json:"scopes_supported,omitempty"`
	ResponseTypesSupported                     []string `json:"response_types_supported,omitempty"`
	GrantTypesSupported                        []string `json:"grant_types_supported,omitempty"`
	TokenEndpointAuthMethodsSupported          []string `json:"token_endpoint_auth_methods_supported,omitempty"`
	TokenEndpointAuthSigningAlgValuesSupported []string `json:"token_endpoint_auth_signing_alg_values_supported,omitempty"`
	CodeChallengeMethodsSupported              []string `json:"code_challenge_methods_supported,omitempty"`
	ClientIDMetadataDocumentSupported          bool     `json:"client_id_metadata_document_supported,omitempty"`
	AuthorizationResponseISSParameterSupported bool     `json:"authorization_response_iss_parameter_supported,omitempty"`
}

// OAuthError is an error response of an authorization server (RFC 6749,
// section 5.2). It wraps ErrAuthorization.
type OAuthError struct {
	StatusCode  int    `json:"-"`
	Code        string `json:"error"`
	Description string `json:"error_description,omitempty"`
	URI         string `json:"error_uri,omitempty"`
}

func (e *OAuthError) Error() string {
	msg := "mcp: oauth: " + e.Code
	if e.Description != "" {
		msg += ": " + e.Description
	}
	return msg
}

// Unwrap returns ErrAuthorization.
func (e *OAuthError) Unwrap() error { return ErrAuthorization }

func authError(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrAuthorization}, args...)...)
}

// WithOAuth authorizes the requests of a Streamable HTTP transport as cfg
// describes. Connect fails for other transports. It wraps the transport of
// the HTTPTransport's Client; see OAuthConfig.Transport to authorize other
// HTTP clients.
// The client keeps a copy of cfg.
func WithOAuth(cfg OAuthConfig) ClientOption {
	return func(c *Client) { c.oauth = &cfg }
}

// applyOAuth wraps the transport's HTTP client for WithOAuth.
func (c *Client) applyOAuth() error {
	if c.oauth == nil {
		return nil
	}
	ht, ok := c.t.(*HTTPTransport)
	if !ok {
		return errors.New("mcp: WithOAuth needs an HTTPTransport")
	}
	if c.oauth.Grant != "" && c.oauth.Grant != GrantAuthorizationCode && c.oauth.Grant != GrantClientCredentials {
		return fmt.Errorf("mcp: WithOAuth: unknown grant %q", c.oauth.Grant)
	}
	hc := *httpclient.ClientOr(ht.Client)
	hc.Transport = c.oauth.Transport(hc.Transport)
	hc.CheckRedirect = sameOriginRedirects(hc.CheckRedirect)
	ht.Client = &hc
	return nil
}

// sameOriginRedirects returns a CheckRedirect function that refuses
// redirects to another origin, then defers to next, or to the default
// limit of 10 redirects if next is nil. An MCP endpoint has no reason to
// send its client elsewhere, and the authorized client must not follow it
// with the server's requests.
func sameOriginRedirects(next func(*http.Request, []*http.Request) error) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if from := via[0].URL; !strings.EqualFold(req.URL.Scheme, from.Scheme) || !strings.EqualFold(req.URL.Host, from.Host) {
			return fmt.Errorf("mcp: refusing redirect from %s to another origin, %s://%s", from.Host, req.URL.Scheme, req.URL.Host)
		}
		if next != nil {
			return next(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
}

// Transport returns a round tripper that authorizes the requests it sends
// with base (http.DefaultTransport if nil) to MCP servers, as the package
// documentation of the OAuth flow describes. Each call returns a round
// tripper with state of its own; tokens persist in the Store. Tokens are
// kept by server URL and sent only to the server they were obtained for,
// so one round tripper may serve several servers. The client using it
// should not follow redirects to other origins; WithOAuth refuses them.
func (cfg *OAuthConfig) Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	t := &oauthTransport{cfg: cfg, base: base, store: cfg.Store, servers: map[string]*serverAuth{}, meta: map[string]*AuthorizationServerMetadata{}}
	if t.store == nil {
		t.store = NewMemoryTokenStore()
	}
	t.http = cfg.HTTPClient
	if t.http == nil {
		t.http = &http.Client{Transport: base, Timeout: 30 * time.Second}
	}
	return t
}

type oauthTransport struct {
	cfg   *OAuthConfig
	base  http.RoundTripper
	http  *http.Client
	store TokenStore

	// mu serializes authorizations.
	mu sync.Mutex
	// servers holds the authorization state of each MCP server, by
	// canonical URL. A token is sent only to the server it was obtained
	// for: one round tripper may serve several servers, and a redirect
	// may lead to another host.
	servers map[string]*serverAuth
	meta    map[string]*AuthorizationServerMetadata
}

// serverAuth is the authorization state of one MCP server.
type serverAuth struct {
	// tok is the current token, loaded is set once the store was read.
	tok    *Token
	loaded bool
	// scopes are the scopes requested last.
	scopes []string
}

// server returns the state of server, loading its token from the store
// the first time. t.mu must be held.
func (t *oauthTransport) server(ctx context.Context, server string) (*serverAuth, error) {
	st := t.servers[server]
	if st == nil {
		st = &serverAuth{}
		t.servers[server] = st
	}
	if !st.loaded {
		tok, err := t.store.Token(ctx, server)
		if err != nil {
			return nil, fmt.Errorf("mcp: load token: %w", err)
		}
		st.tok, st.loaded = tok, true
		if tok != nil {
			st.scopes = strings.Fields(tok.Scope)
		}
	}
	return st, nil
}

// RoundTrip implements http.RoundTripper.
func (t *oauthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		defer req.Body.Close()
	}
	ctx := req.Context()
	server := canonicalURL(req.URL)
	tok, err := t.current(ctx, server)
	if err != nil {
		return nil, err
	}
	resp, err := t.send(req, tok)
	// Retry after at most two authorizations: a step-up that still lacks
	// scope is a permanent failure.
	for range 2 {
		if err != nil {
			return nil, err
		}
		ch, ok := challengeOf(resp)
		if !ok {
			return resp, nil
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		if tok, err = t.authorize(ctx, req.URL, server, ch, tok); err != nil {
			return nil, err
		}
		resp, err = t.send(req, tok)
	}
	return resp, err
}

// send sends a copy of req with the token.
func (t *oauthTransport) send(req *http.Request, tok *Token) (*http.Response, error) {
	r := req.Clone(req.Context())
	if req.GetBody != nil && req.Body != nil && req.Body != http.NoBody {
		// A retry needs the body again.
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		r.Body = body
	}
	if tok != nil {
		r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	}
	return t.base.RoundTrip(r)
}

// challenge is a Bearer challenge that calls for authorization.
type challenge struct {
	status           int
	resourceMetadata string
	scope            []string
	err              string
}

// challengeOf returns the challenge of a 401 response, or of a 403
// response with insufficient_scope.
func challengeOf(resp *http.Response) (challenge, bool) {
	ch := challenge{status: resp.StatusCode}
	for _, c := range parseChallenges(resp.Header.Values("WWW-Authenticate")) {
		if strings.EqualFold(c.scheme, "Bearer") {
			ch.resourceMetadata = c.params["resource_metadata"]
			ch.scope = strings.Fields(c.params["scope"])
			ch.err = c.params["error"]
			break
		}
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return ch, true
	case http.StatusForbidden:
		return ch, ch.err == "insufficient_scope"
	}
	return ch, false
}

// current returns the token to send to server, refreshing an expired one.
func (t *oauthTransport) current(ctx context.Context, server string) (*Token, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, err := t.server(ctx, server)
	if err != nil {
		return nil, err
	}
	if st.tok == nil || st.tok.valid() {
		return st.tok, nil
	}
	if st.tok.RefreshToken != "" && st.tok.Issuer != "" {
		md, err := t.serverMetadata(ctx, st.tok.Issuer)
		if err == nil {
			var tok *Token
			if tok, err = t.refresh(ctx, md, st.tok); err == nil {
				t.save(ctx, st, server, tok)
				return tok, nil
			}
		}
	}
	// The expired token is of no use; the server's challenge starts a new
	// authorization.
	st.tok = nil
	return nil, nil
}

// save keeps a new token for server.
func (t *oauthTransport) save(ctx context.Context, st *serverAuth, server string, tok *Token) {
	st.tok = tok
	_ = t.store.SetToken(ctx, server, tok)
}

// authorize obtains a new token after the server challenged the token old.
func (t *oauthTransport) authorize(ctx context.Context, u *url.URL, server string, ch challenge, old *Token) (*Token, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, err := t.server(ctx, server)
	if err != nil {
		return nil, err
	}
	if st.tok != nil && (old == nil || st.tok.AccessToken != old.AccessToken) && ch.status == http.StatusUnauthorized {
		// Another request already replaced the rejected token.
		return st.tok, nil
	}
	prm, err := t.resourceMetadata(ctx, u, ch.resourceMetadata)
	if err != nil {
		return nil, err
	}
	issuer := prm.AuthorizationServers[0]
	if t.cfg.SelectAuthorizationServer != nil {
		issuer = t.cfg.SelectAuthorizationServer(prm.AuthorizationServers)
		if !slices.Contains(prm.AuthorizationServers, issuer) {
			return nil, authError("selected authorization server %q is not listed by the resource", issuer)
		}
	}
	md, err := t.serverMetadata(ctx, issuer)
	if err != nil {
		return nil, err
	}
	scopes := t.scopesFor(st, ch, prm, old)
	if ch.status == http.StatusUnauthorized && old != nil && old.RefreshToken != "" && old.Issuer == md.Issuer &&
		!missing(strings.Fields(old.Scope), scopes) {
		// An expired or revoked token may still be refreshed.
		if tok, err := t.refresh(ctx, md, old); err == nil {
			t.save(ctx, st, server, tok)
			return tok, nil
		}
	}
	var tok *Token
	if cmp.Or(t.cfg.Grant, GrantAuthorizationCode) == GrantClientCredentials {
		tok, err = t.clientCredentials(ctx, md, prm.Resource, scopes)
	} else {
		tok, err = t.authorizationCode(ctx, md, prm.Resource, scopes)
	}
	if err != nil {
		return nil, err
	}
	st.scopes = scopes
	t.save(ctx, st, server, tok)
	return tok, nil
}

// scopesFor selects the scopes to request: those of the challenge, with a
// step-up adding those requested before for the server st; otherwise those
// the resource supports, or OAuthConfig.Scopes.
func (t *oauthTransport) scopesFor(st *serverAuth, ch challenge, prm *ProtectedResourceMetadata, old *Token) []string {
	var scopes []string
	switch {
	case len(ch.scope) > 0:
		scopes = slices.Clone(ch.scope)
		if ch.status == http.StatusForbidden || old != nil {
			scopes = append(scopes, st.scopes...)
			if old != nil {
				scopes = append(scopes, strings.Fields(old.Scope)...)
			}
		}
	case len(prm.ScopesSupported) > 0:
		scopes = slices.Clone(prm.ScopesSupported)
	default:
		scopes = slices.Clone(t.cfg.Scopes)
	}
	slices.Sort(scopes)
	return slices.Compact(scopes)
}

// resourceMetadata fetches the Protected Resource Metadata of the server
// at u: from the challenge's URL if it named one, otherwise from the
// well-known URLs, path-specific first.
func (t *oauthTransport) resourceMetadata(ctx context.Context, u *url.URL, fromChallenge string) (*ProtectedResourceMetadata, error) {
	var candidates []string
	if fromChallenge != "" {
		candidates = []string{fromChallenge}
	} else {
		root := (&url.URL{Scheme: u.Scheme, Host: u.Host}).String() + "/.well-known/oauth-protected-resource"
		if p := strings.TrimSuffix(u.EscapedPath(), "/"); p != "" {
			candidates = append(candidates, root+p)
		}
		candidates = append(candidates, root)
	}
	var errs []error
	for _, c := range candidates {
		var md ProtectedResourceMetadata
		if err := t.getJSON(ctx, c, &md); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := checkResource(u, md.Resource); err != nil {
			return nil, err
		}
		if len(md.AuthorizationServers) == 0 {
			return nil, authError("protected resource metadata %s lists no authorization servers", c)
		}
		return &md, nil
	}
	return nil, authError("no protected resource metadata: %w", errors.Join(errs...))
}

// checkResource checks that the resource of the metadata identifies the
// server at u: it has the same origin and a path that u's path is within.
func checkResource(u *url.URL, resource string) error {
	r, err := url.Parse(resource)
	if err != nil || resource == "" || r.Fragment != "" || r.Scheme == "" {
		return authError("invalid resource %q in protected resource metadata", resource)
	}
	rp, up := strings.TrimSuffix(r.Path, "/"), strings.TrimSuffix(u.Path, "/")
	if !strings.EqualFold(r.Scheme, u.Scheme) || !strings.EqualFold(r.Host, u.Host) ||
		(up != rp && !strings.HasPrefix(up, rp+"/")) {
		return authError("protected resource metadata is for %q, not %q", resource, canonicalURL(u))
	}
	return nil
}

// canonicalURL returns the canonical URI of the MCP server at u: lowercase
// scheme and host, no query, fragment or trailing slash.
func canonicalURL(u *url.URL) string {
	c := url.URL{Scheme: strings.ToLower(u.Scheme), Host: strings.ToLower(u.Host), Path: strings.TrimSuffix(u.Path, "/")}
	return c.String()
}

// secureEndpoint checks that an authorization server URL uses HTTPS, as
// MCP requires, or a loopback host.
func secureEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return authError("invalid authorization server URL %q", raw)
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !isLoopback(u.Hostname())) {
		return authError("authorization server URL %q must use https", raw)
	}
	return nil
}

// serverMetadata discovers the metadata of the authorization server with
// the given issuer, trying RFC 8414 and OpenID Connect Discovery URLs in the
// order MCP prescribes, and checks the issuer and PKCE support.
func (t *oauthTransport) serverMetadata(ctx context.Context, issuer string) (*AuthorizationServerMetadata, error) {
	if md := t.meta[issuer]; md != nil {
		return md, nil
	}
	if err := secureEndpoint(issuer); err != nil {
		return nil, err
	}
	u, _ := url.Parse(issuer)
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, authError("invalid issuer %q", issuer)
	}
	origin := (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
	var candidates []string
	if p := strings.TrimSuffix(u.EscapedPath(), "/"); p != "" {
		candidates = []string{
			origin + "/.well-known/oauth-authorization-server" + p,
			origin + "/.well-known/openid-configuration" + p,
			origin + p + "/.well-known/openid-configuration",
		}
	} else {
		candidates = []string{origin + "/.well-known/oauth-authorization-server", origin + "/.well-known/openid-configuration"}
	}
	var errs []error
	for _, c := range candidates {
		var md AuthorizationServerMetadata
		if err := t.getJSON(ctx, c, &md); err != nil {
			errs = append(errs, err)
			continue
		}
		if md.Issuer != issuer {
			// RFC 8414, section 3.3: the metadata must not be used.
			errs = append(errs, fmt.Errorf("%s: issuer %q is not %q", c, md.Issuer, issuer))
			continue
		}
		for _, e := range []string{md.AuthorizationEndpoint, md.TokenEndpoint, md.RegistrationEndpoint} {
			if e != "" {
				if err := secureEndpoint(e); err != nil {
					return nil, err
				}
			}
		}
		if md.TokenEndpoint == "" {
			return nil, authError("authorization server %s has no token endpoint", issuer)
		}
		t.meta[issuer] = &md
		return &md, nil
	}
	return nil, authError("no authorization server metadata for %s: %w", issuer, errors.Join(errs...))
}

// getJSON fetches a JSON document.
func (t *oauthTransport) getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := t.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := readOAuthBody(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: http %d", u, resp.StatusCode)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", u, err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: %w", u, err)
	}
	return nil
}

// client returns the client registration to use with the authorization
// server md, in the order of priority MCP recommends: pre-registered
// credentials, a Client ID Metadata Document, then Dynamic Client
// Registration.
func (t *oauthTransport) client(ctx context.Context, md *AuthorizationServerMetadata, scopes []string) (*ClientRegistration, error) {
	cfg := t.cfg
	if cfg.ClientID != "" {
		if cfg.ClientIssuer != "" && cfg.ClientIssuer != md.Issuer {
			return nil, authError("the pre-registered client belongs to %s, but the server uses %s", cfg.ClientIssuer, md.Issuer)
		}
		method := cfg.TokenEndpointAuthMethod
		if method == "" {
			switch {
			case cfg.ClientAssertion != nil:
				method = AuthMethodPrivateKeyJWT
			case cfg.ClientSecret != "":
				method = AuthMethodClientSecretBasic
				if len(md.TokenEndpointAuthMethodsSupported) > 0 &&
					!slices.Contains(md.TokenEndpointAuthMethodsSupported, AuthMethodClientSecretBasic) &&
					slices.Contains(md.TokenEndpointAuthMethodsSupported, AuthMethodClientSecretPost) {
					method = AuthMethodClientSecretPost
				}
			default:
				method = AuthMethodNone
			}
		}
		return &ClientRegistration{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, TokenEndpointAuthMethod: method}, nil
	}
	if cmp.Or(cfg.Grant, GrantAuthorizationCode) == GrantClientCredentials {
		return nil, authError("the client credentials grant needs a pre-registered ClientID")
	}
	if cfg.ClientMetadataURL != "" && md.ClientIDMetadataDocumentSupported {
		if u, err := url.Parse(cfg.ClientMetadataURL); err != nil || u.Scheme != "https" || strings.Trim(u.Path, "/") == "" {
			return nil, authError("client ID metadata document URL %q must be an https URL with a path", cfg.ClientMetadataURL)
		}
		return &ClientRegistration{ClientID: cfg.ClientMetadataURL, TokenEndpointAuthMethod: AuthMethodNone}, nil
	}
	reg, err := t.store.Client(ctx, md.Issuer)
	if err != nil {
		return nil, fmt.Errorf("mcp: load client registration: %w", err)
	}
	if reg != nil && reg.ClientID != "" && (reg.ClientSecretExpiresAt == 0 || time.Now().Unix() < reg.ClientSecretExpiresAt) &&
		(len(reg.RedirectURIs) == 0 || slices.ContainsFunc(reg.RedirectURIs, func(u string) bool { return sameRedirect(u, cfg.RedirectURL) })) {
		return reg, nil
	}
	if md.RegistrationEndpoint == "" {
		return nil, authError("authorization server %s supports neither Client ID Metadata Documents nor Dynamic Client Registration; "+
			"set OAuthConfig.ClientID", md.Issuer)
	}
	if reg, err = t.register(ctx, md, scopes); err != nil {
		return nil, err
	}
	if err := t.store.SetClient(ctx, md.Issuer, reg); err != nil {
		return nil, fmt.Errorf("mcp: save client registration: %w", err)
	}
	return reg, nil
}

// register registers a public client dynamically (RFC 7591).
func (t *oauthTransport) register(ctx context.Context, md *AuthorizationServerMetadata, scopes []string) (*ClientRegistration, error) {
	cfg := t.cfg
	appType := cfg.ApplicationType
	if appType == "" {
		appType = "web"
		if u, err := url.Parse(cfg.RedirectURL); err == nil && isLoopback(u.Hostname()) {
			appType = "native"
		}
	}
	body := map[string]any{
		"client_name":                cmp.Or(cfg.ClientName, "agentx MCP client"),
		"redirect_uris":              []string{cfg.RedirectURL},
		"grant_types":                []string{GrantAuthorizationCode, "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": AuthMethodNone,
		"application_type":           appType,
	}
	if cfg.ClientURI != "" {
		body["client_uri"] = cfg.ClientURI
	}
	if len(scopes) > 0 {
		body["scope"] = strings.Join(scopes, " ")
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, md.RegistrationEndpoint, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := t.http.Do(req)
	if err != nil {
		return nil, authError("register client: %w", err)
	}
	defer resp.Body.Close()
	raw, err := readOAuthBody(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, oauthFailure(resp.StatusCode, raw, "register client")
	}
	if err != nil {
		return nil, authError("register client: %w", err)
	}
	var reg ClientRegistration
	if err := json.Unmarshal(raw, &reg); err != nil || reg.ClientID == "" {
		return nil, authError("register client: invalid response %s", utf8text.Truncate(string(bytes.TrimSpace(raw)), 300, "..."))
	}
	if len(reg.RedirectURIs) == 0 {
		reg.RedirectURIs = []string{cfg.RedirectURL}
	}
	return &reg, nil
}

// maxOAuthResponse bounds the metadata, registration and token responses
// read from authorization servers.
const maxOAuthResponse = 1 << 20

// readOAuthBody reads an authorization server response. A body larger than
// maxOAuthResponse is an error rather than truncated, which would otherwise
// surface as malformed JSON. Callers check the status first: the body of an
// error response is only used to describe it.
func readOAuthBody(r io.Reader) ([]byte, error) {
	body, err := boundedio.ReadAll(r, maxOAuthResponse)
	if errors.Is(err, boundedio.ErrLimitExceeded) {
		return nil, fmt.Errorf("response larger than %d bytes: %w", maxOAuthResponse, err)
	}
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return body, nil
}

// oauthFailure converts an error response of the authorization server.
func oauthFailure(status int, body []byte, op string) error {
	e := &OAuthError{StatusCode: status}
	if json.Unmarshal(body, e) == nil && e.Code != "" {
		return e
	}
	return authError("%s: http %d: %s", op, status, utf8text.Truncate(string(bytes.TrimSpace(body)), 300, "..."))
}

// authorizationCode runs the authorization code flow with PKCE.
func (t *oauthTransport) authorizationCode(ctx context.Context, md *AuthorizationServerMetadata, resource string, scopes []string) (*Token, error) {
	cfg := t.cfg
	if cfg.Authorize == nil || cfg.RedirectURL == "" {
		return nil, authError("the server requires authorization; set OAuthConfig.RedirectURL and Authorize, " +
			"for example with a LoopbackRedirect")
	}
	if err := checkRedirect(cfg.RedirectURL); err != nil {
		return nil, err
	}
	if !slices.Contains(md.CodeChallengeMethodsSupported, "S256") {
		// MCP clients must refuse servers that do not declare PKCE support.
		return nil, authError("authorization server %s does not declare PKCE (S256) support", md.Issuer)
	}
	if md.AuthorizationEndpoint == "" {
		return nil, authError("authorization server %s has no authorization endpoint", md.Issuer)
	}
	if slices.Contains(md.ScopesSupported, "offline_access") && len(scopes) > 0 && !slices.Contains(scopes, "offline_access") {
		scopes = append(slices.Clone(scopes), "offline_access")
	}
	reg, err := t.client(ctx, md, scopes)
	if err != nil {
		return nil, err
	}
	verifier := rand.Text() + rand.Text()
	sum := sha256.Sum256([]byte(verifier))
	state := uuid.New().String()
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {reg.ClientID},
		"redirect_uri":          {cfg.RedirectURL},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"state":                 {state},
		"resource":              {resource},
	}
	if len(scopes) > 0 {
		q.Set("scope", strings.Join(scopes, " "))
	}
	authURL := md.AuthorizationEndpoint
	if strings.Contains(authURL, "?") {
		authURL += "&" + q.Encode()
	} else {
		authURL += "?" + q.Encode()
	}
	res, err := cfg.Authorize(ctx, authURL)
	if err != nil {
		return nil, authError("authorize: %w", err)
	}
	if res == nil {
		return nil, authError("authorize: no response")
	}
	// RFC 9207: the issuer is checked before anything else of the
	// response is used, including its error.
	switch {
	case res.Issuer != "" && res.Issuer != md.Issuer:
		return nil, authError("authorization response from issuer %q, expected %q", res.Issuer, md.Issuer)
	case res.Issuer == "" && md.AuthorizationResponseISSParameterSupported:
		return nil, authError("authorization response without the iss parameter the server declares")
	case res.State != state:
		return nil, authError("authorization response with a state that does not match the request")
	case res.Error != "":
		return nil, &OAuthError{Code: res.Error, Description: res.ErrorDescription}
	case res.Code == "":
		return nil, authError("authorization response without a code")
	}
	form := url.Values{
		"grant_type":    {GrantAuthorizationCode},
		"code":          {res.Code},
		"redirect_uri":  {cfg.RedirectURL},
		"code_verifier": {verifier},
		"resource":      {resource},
	}
	return t.tokenRequest(ctx, md, reg, form, resource, scopes)
}

// sameRedirect reports whether two redirect URLs are the same, ignoring
// the port of loopback IP addresses, which RFC 8252, section 7.3, lets
// native clients choose for each request.
func sameRedirect(a, b string) bool {
	if a == b {
		return true
	}
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	if err1 != nil || err2 != nil || ua.Scheme != "http" || ub.Scheme != "http" {
		return false
	}
	ip := net.ParseIP(ua.Hostname())
	return ip != nil && ip.IsLoopback() && ua.Hostname() == ub.Hostname() && ua.Path == ub.Path && ua.RawQuery == ub.RawQuery
}

// checkRedirect checks that a redirect URL is a loopback or HTTPS URL.
func checkRedirect(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Fragment != "" {
		return authError("invalid redirect URL %q", raw)
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !isLoopback(u.Hostname())) {
		return authError("redirect URL %q must be a loopback or https URL", raw)
	}
	return nil
}

// clientCredentials obtains a token with the client credentials grant.
func (t *oauthTransport) clientCredentials(ctx context.Context, md *AuthorizationServerMetadata, resource string, scopes []string) (*Token, error) {
	reg, err := t.client(ctx, md, scopes)
	if err != nil {
		return nil, err
	}
	if reg.TokenEndpointAuthMethod == AuthMethodNone {
		return nil, authError("the client credentials grant needs a ClientSecret or ClientAssertion")
	}
	form := url.Values{"grant_type": {GrantClientCredentials}, "resource": {resource}}
	if len(scopes) > 0 {
		form.Set("scope", strings.Join(scopes, " "))
	}
	return t.tokenRequest(ctx, md, reg, form, resource, scopes)
}

// refresh exchanges the refresh token of old.
func (t *oauthTransport) refresh(ctx context.Context, md *AuthorizationServerMetadata, old *Token) (*Token, error) {
	reg, err := t.client(ctx, md, strings.Fields(old.Scope))
	if err != nil {
		return nil, err
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {old.RefreshToken}}
	if old.Resource != "" {
		form.Set("resource", old.Resource)
	}
	tok, err := t.tokenRequest(ctx, md, reg, form, old.Resource, strings.Fields(old.Scope))
	if err != nil {
		return nil, err
	}
	if tok.RefreshToken == "" {
		// The server keeps the refresh token unless it rotates it.
		tok.RefreshToken = old.RefreshToken
	}
	return tok, nil
}

// tokenRequest sends a token request with the client's authentication.
func (t *oauthTransport) tokenRequest(ctx context.Context, md *AuthorizationServerMetadata, reg *ClientRegistration,
	form url.Values, resource string, scopes []string,
) (*Token, error) {
	var basic bool
	switch reg.TokenEndpointAuthMethod {
	case AuthMethodClientSecretBasic:
		basic = true
	case AuthMethodClientSecretPost:
		form.Set("client_id", reg.ClientID)
		form.Set("client_secret", reg.ClientSecret)
	case AuthMethodPrivateKeyJWT:
		if t.cfg.ClientAssertion == nil {
			return nil, authError("private_key_jwt needs OAuthConfig.ClientAssertion")
		}
		assertion, err := t.cfg.ClientAssertion(ctx, md.Issuer, md.TokenEndpoint)
		if err != nil {
			return nil, authError("client assertion: %w", err)
		}
		form.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
		form.Set("client_assertion", assertion)
		if form.Get("grant_type") != GrantClientCredentials {
			form.Set("client_id", reg.ClientID)
		}
	default:
		form.Set("client_id", reg.ClientID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, md.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if basic {
		// RFC 6749, section 2.3.1: the credentials are form-encoded first.
		req.SetBasicAuth(url.QueryEscape(reg.ClientID), url.QueryEscape(reg.ClientSecret))
	}
	resp, err := t.http.Do(req)
	if err != nil {
		return nil, authError("token request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := readOAuthBody(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, oauthFailure(resp.StatusCode, raw, "token request")
	}
	if err != nil {
		return nil, authError("token request: %w", err)
	}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "" && mt != "application/json" {
		return nil, authError("token response of type %s", mt)
	}
	var body struct {
		AccessToken  string          `json:"access_token"`
		TokenType    string          `json:"token_type"`
		ExpiresIn    json.RawMessage `json:"expires_in"`
		RefreshToken string          `json:"refresh_token"`
		Scope        string          `json:"scope"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.AccessToken == "" {
		return nil, authError("invalid token response")
	}
	if !strings.EqualFold(body.TokenType, "Bearer") {
		return nil, authError("unsupported token type %q", body.TokenType)
	}
	tok := &Token{
		AccessToken: body.AccessToken, TokenType: "Bearer", RefreshToken: body.RefreshToken,
		Scope: cmp.Or(body.Scope, strings.Join(scopes, " ")), Issuer: md.Issuer, Resource: resource,
	}
	// expires_in is a number, sent as a string by some servers.
	if n, err := strconv.ParseFloat(strings.Trim(string(body.ExpiresIn), `"`), 64); err == nil && n > 0 {
		tok.Expiry = time.Now().Add(time.Duration(n * float64(time.Second)))
	}
	return tok, nil
}

// authChallenge is a challenge of a WWW-Authenticate header (RFC 9110,
// section 11.6.1).
type authChallenge struct {
	scheme string
	params map[string]string
}

// parseChallenges parses WWW-Authenticate header values. Parameter names
// are lowercased; token68 credentials are not supported.
func parseChallenges(values []string) []authChallenge {
	var out []authChallenge
	for _, v := range values {
		p := &challengeParser{s: v}
		for {
			p.skip(" \t,")
			scheme := p.token()
			if scheme == "" {
				break
			}
			ch := authChallenge{scheme: scheme, params: map[string]string{}}
			for {
				p.skip(" \t")
				mark := p.i
				name := p.token()
				p.skip(" \t")
				if name == "" || !p.consume('=') {
					// The scheme of the next challenge.
					p.i = mark
					break
				}
				p.skip(" \t")
				var val string
				if p.i < len(p.s) && p.s[p.i] == '"' {
					val = p.quoted()
				} else {
					val = p.token()
				}
				ch.params[strings.ToLower(name)] = val
				p.skip(" \t")
				if !p.consume(',') {
					break
				}
			}
			out = append(out, ch)
		}
	}
	return out
}

type challengeParser struct {
	s string
	i int
}

func (p *challengeParser) skip(chars string) {
	for p.i < len(p.s) && strings.IndexByte(chars, p.s[p.i]) >= 0 {
		p.i++
	}
}

func (p *challengeParser) consume(c byte) bool {
	if p.i < len(p.s) && p.s[p.i] == c {
		p.i++
		return true
	}
	return false
}

func (p *challengeParser) token() string {
	start := p.i
	for p.i < len(p.s) && isToken(p.s[p.i:p.i+1]) {
		p.i++
	}
	return p.s[start:p.i]
}

func (p *challengeParser) quoted() string {
	p.i++ // the opening quote
	var b strings.Builder
	for p.i < len(p.s) {
		c := p.s[p.i]
		p.i++
		switch {
		case c == '\\' && p.i < len(p.s):
			b.WriteByte(p.s[p.i])
			p.i++
		case c == '"':
			return b.String()
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
