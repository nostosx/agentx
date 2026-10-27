package mcp

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nostosx/kitx/boundedio"
)

// fakeAS is an OAuth 2.1 authorization server for the tests.
type fakeAS struct {
	t  *testing.T
	ts *httptest.Server
	// path is the issuer's path, such as "/tenant".
	path string
	// Behavior switches.
	noPKCE, iss, cimd, noRegistration, openIDOnly, wrongIssuer, omitIss, tls bool
	authMethods                                                              []string
	expiresIn                                                                int

	mu       sync.Mutex
	clients  map[string]*asClient
	codes    map[string]*asCode
	tokens   map[string]*asToken
	refresh  map[string]*asToken
	log      []url.Values // authorize, token and registration requests
	counts   map[string]int
	jwtKeys  map[string]crypto.PublicKey
	registry []map[string]any
}

type asClient struct {
	id, secret string
	redirects  []string
}

type asCode struct {
	client, redirect, challenge, resource, scope string
}

type asToken struct {
	subject, resource, scope, client string
	revoked                          bool
}

func newFakeAS(t *testing.T, configure func(*fakeAS)) *fakeAS {
	a := &fakeAS{t: t, clients: map[string]*asClient{}, codes: map[string]*asCode{}, tokens: map[string]*asToken{},
		refresh: map[string]*asToken{}, counts: map[string]int{}, jwtKeys: map[string]crypto.PublicKey{}, iss: true, expiresIn: 3600}
	if configure != nil {
		configure(a)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{path...}", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/.well-known/") || strings.HasSuffix(r.URL.Path, "/.well-known/openid-configuration") {
			a.metadata(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/authorize") {
			a.authorize(w, r)
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("POST "+a.path+"/token", a.token)
	mux.HandleFunc("POST "+a.path+"/register", a.register)
	if a.tls {
		a.ts = httptest.NewTLSServer(mux)
	} else {
		a.ts = httptest.NewServer(mux)
	}
	t.Cleanup(a.ts.Close)
	return a
}

func (a *fakeAS) issuer() string { return a.ts.URL + a.path }

func (a *fakeAS) count(k string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.counts[k]
}

func (a *fakeAS) record(kind string, v url.Values) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.counts[kind]++
	v = maps2(v)
	v.Set("kind", kind)
	a.log = append(a.log, v)
}

func maps2(v url.Values) url.Values {
	c := url.Values{}
	for k, vs := range v {
		c[k] = slices.Clone(vs)
	}
	return c
}

// last returns the last logged request of a kind.
func (a *fakeAS) last(kind string) url.Values {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, v := range slices.Backward(a.log) {
		if v.Get("kind") == kind {
			return v
		}
	}
	return nil
}

func (a *fakeAS) metadata(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	oauthPath := "/.well-known/oauth-authorization-server" + a.path
	openidInsert := "/.well-known/openid-configuration" + a.path
	openidAppend := a.path + "/.well-known/openid-configuration"
	switch {
	case p == oauthPath && !a.openIDOnly:
	case a.openIDOnly && (a.path == "" && p == openidInsert || a.path != "" && p == openidAppend):
	default:
		http.NotFound(w, r)
		return
	}
	a.record("metadata", url.Values{"path": {p}})
	md := AuthorizationServerMetadata{
		Issuer:                                     a.issuer(),
		AuthorizationEndpoint:                      a.issuer() + "/authorize",
		TokenEndpoint:                              a.issuer() + "/token",
		ScopesSupported:                            []string{"files:read", "files:write", "offline_access"},
		TokenEndpointAuthMethodsSupported:          a.authMethods,
		ClientIDMetadataDocumentSupported:          a.cimd,
		AuthorizationResponseISSParameterSupported: a.iss,
	}
	if !a.noRegistration {
		md.RegistrationEndpoint = a.issuer() + "/register"
	}
	if !a.noPKCE {
		md.CodeChallengeMethodsSupported = []string{"S256"}
	}
	if a.wrongIssuer {
		md.Issuer = "https://honest.example"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(md)
}

func (a *fakeAS) register(w http.ResponseWriter, r *http.Request) {
	var req map[string]any
	_ = json.NewDecoder(r.Body).Decode(&req)
	a.mu.Lock()
	a.registry = append(a.registry, req)
	id := "client-" + rand.Text()[:6]
	var redirects []string
	for _, u := range req["redirect_uris"].([]any) {
		redirects = append(redirects, u.(string))
	}
	a.clients[id] = &asClient{id: id, redirects: redirects}
	a.counts["register"]++
	a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"client_id": id, "redirect_uris": redirects, "token_endpoint_auth_method": "none"})
}

func (a *fakeAS) addClient(id, secret string) {
	a.mu.Lock()
	a.clients[id] = &asClient{id: id, secret: secret}
	a.mu.Unlock()
}

func (a *fakeAS) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	a.record("authorize", q)
	fail := func(msg string) {
		a.t.Errorf("authorize: %s (%v)", msg, q)
		http.Error(w, msg, http.StatusBadRequest)
	}
	clientID, redirect := q.Get("client_id"), q.Get("redirect_uri")
	a.mu.Lock()
	cl := a.clients[clientID]
	a.mu.Unlock()
	switch {
	case cl == nil && (!a.cimd || !strings.HasPrefix(clientID, "https://")):
		fail("unknown client")
		return
	case cl != nil && cl.redirects != nil && !slices.Contains(cl.redirects, redirect):
		fail("unregistered redirect")
		return
	case q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "":
		fail("not a PKCE code request")
		return
	case q.Get("resource") == "" || q.Get("state") == "":
		fail("missing resource or state")
		return
	}
	code := rand.Text()
	a.mu.Lock()
	a.codes[code] = &asCode{client: clientID, redirect: redirect, challenge: q.Get("code_challenge"),
		resource: q.Get("resource"), scope: q.Get("scope")}
	a.mu.Unlock()
	back := url.Values{"code": {code}, "state": {q.Get("state")}}
	if a.iss && !a.omitIss {
		back.Set("iss", a.issuer())
	}
	http.Redirect(w, r, redirect+"?"+back.Encode(), http.StatusFound)
}

func (a *fakeAS) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	form := r.PostForm
	a.record("token", form)
	oauthErr := func(status int, code string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": code + " test"})
	}
	clientID := form.Get("client_id")
	if id, secret, ok := r.BasicAuth(); ok {
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
		a.mu.Lock()
		cl := a.clients[id]
		a.mu.Unlock()
		if cl == nil || cl.secret != secret {
			oauthErr(401, "invalid_client")
			return
		}
		clientID = id
	} else if form.Get("client_secret") != "" {
		a.mu.Lock()
		cl := a.clients[clientID]
		a.mu.Unlock()
		if cl == nil || cl.secret != form.Get("client_secret") {
			oauthErr(401, "invalid_client")
			return
		}
	} else if assertion := form.Get("client_assertion"); assertion != "" {
		sub, err := a.verifyJWT(assertion)
		if err != nil {
			a.t.Errorf("client assertion: %v", err)
			oauthErr(401, "invalid_client")
			return
		}
		clientID = sub
	}
	var tok asToken
	switch form.Get("grant_type") {
	case GrantAuthorizationCode:
		a.mu.Lock()
		c := a.codes[form.Get("code")]
		delete(a.codes, form.Get("code"))
		a.mu.Unlock()
		sum := sha256.Sum256([]byte(form.Get("code_verifier")))
		switch {
		case c == nil:
			oauthErr(400, "invalid_grant")
			return
		case base64.RawURLEncoding.EncodeToString(sum[:]) != c.challenge:
			a.t.Error("PKCE verification failed")
			oauthErr(400, "invalid_grant")
			return
		case c.client != clientID || c.redirect != form.Get("redirect_uri") || c.resource != form.Get("resource"):
			a.t.Errorf("token request does not match the code: %+v %v", c, form)
			oauthErr(400, "invalid_grant")
			return
		}
		tok = asToken{subject: "user-1", resource: c.resource, scope: c.scope, client: clientID}
	case "refresh_token":
		a.mu.Lock()
		old := a.refresh[form.Get("refresh_token")]
		delete(a.refresh, form.Get("refresh_token"))
		a.mu.Unlock()
		if old == nil {
			oauthErr(400, "invalid_grant")
			return
		}
		tok = *old
		tok.revoked = false
	case GrantClientCredentials:
		a.mu.Lock()
		cl := a.clients[clientID]
		a.mu.Unlock()
		if cl == nil {
			oauthErr(401, "invalid_client")
			return
		}
		tok = asToken{subject: "machine-" + clientID, resource: form.Get("resource"), scope: form.Get("scope"), client: clientID}
	default:
		oauthErr(400, "unsupported_grant_type")
		return
	}
	access, refresh := "at-"+rand.Text(), "rt-"+rand.Text()
	a.mu.Lock()
	a.tokens[access] = &tok
	a.refresh[refresh] = &tok
	a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": access, "token_type": "bearer",
		"expires_in": a.expiresIn, "refresh_token": refresh, "scope": tok.scope})
}

// verifyJWT checks a client assertion signed with a registered key.
func (a *fakeAS) verifyJWT(jwt string) (string, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return "", errors.New("not a JWT")
	}
	var header struct{ Alg, Kid string }
	var claims struct {
		Iss, Sub, Aud, Jti string
		Exp                int64
	}
	hb, _ := base64.RawURLEncoding.DecodeString(parts[0])
	cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	_ = json.Unmarshal(hb, &header)
	_ = json.Unmarshal(cb, &claims)
	a.mu.Lock()
	key := a.jwtKeys[claims.Sub]
	a.mu.Unlock()
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	switch k := key.(type) {
	case *rsa.PublicKey:
		if header.Alg != "RS256" || rsa.VerifyPKCS1v15(k, crypto.SHA256, sum[:], sig) != nil {
			return "", errors.New("bad RS256 signature")
		}
	case *ecdsa.PublicKey:
		if header.Alg != "ES256" || len(sig) != 64 ||
			!ecdsa.Verify(k, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
			return "", errors.New("bad ES256 signature")
		}
	default:
		return "", errors.New("unknown client")
	}
	if claims.Iss != claims.Sub || claims.Aud != a.issuer()+"/token" || claims.Jti == "" || claims.Exp < time.Now().Unix() {
		return "", fmt.Errorf("bad claims %+v", claims)
	}
	return claims.Sub, nil
}

// lookup returns the token record of an access token.
func (a *fakeAS) lookup(token string) *asToken {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t := a.tokens[token]; t != nil && !t.revoked {
		c := *t
		return &c
	}
	return nil
}

func (a *fakeAS) revokeAll() {
	a.mu.Lock()
	for _, t := range a.tokens {
		t.revoked = true
	}
	a.mu.Unlock()
}

// protectedServer serves the fixture at /mcp, protected by the fake
// authorization server: every request needs files:read, and the delete
// tool files:write.
type protectedServer struct {
	f        *srvFixture
	ts       *httptest.Server
	url      string
	resource string
	h        *HTTPHandler
}

func newProtectedServer(t *testing.T, as *fakeAS, wrap func(http.Handler) http.Handler) *protectedServer {
	t.Helper()
	p := &protectedServer{f: newFixture(t, ServerOptions{})}
	mux := http.NewServeMux()
	p.ts = httptest.NewServer(mux)
	t.Cleanup(p.ts.Close)
	p.url = p.ts.URL + "/mcp"
	p.resource = p.url
	md := &ProtectedResourceMetadata{Resource: p.resource, AuthorizationServers: []string{as.issuer()},
		ScopesSupported: []string{"files:read", "files:write"}, BearerMethodsSupported: []string{"header"}}
	p.h = p.f.s.HTTPHandler(HTTPOptions{Auth: &AuthOptions{
		Metadata: md,
		Scopes:   []string{"files:read"},
		Verify: func(_ context.Context, token string, _ *http.Request) (*TokenInfo, error) {
			at := as.lookup(token)
			if at == nil {
				return nil, errors.New("unknown token")
			}
			return &TokenInfo{Subject: at.subject, Scopes: strings.Fields(at.scope), Audience: []string{at.resource}}, nil
		},
		OperationScopes: func(method, name string) []string {
			if method == "tools/call" && name == "delete" {
				return []string{"files:write"}
			}
			return nil
		},
	}})
	t.Cleanup(func() { _ = p.h.Close() })
	var h http.Handler = p.h
	if wrap != nil {
		h = wrap(h)
	}
	mux.Handle("/mcp", h)
	mux.Handle(strings.TrimPrefix(ProtectedResourceMetadataURL(p.resource), p.ts.URL), ProtectedResourceMetadataHandler(md))
	return p
}

// browser follows the authorization request like a user agent that
// approves it, and returns the response it is redirected with.
func browser(*testing.T) func(ctx context.Context, authURL string) (*AuthorizationResponse, error) {
	return browserWith(http.DefaultClient)
}

func browserWith(hc *http.Client) func(ctx context.Context, authURL string) (*AuthorizationResponse, error) {
	return func(ctx context.Context, authURL string) (*AuthorizationResponse, error) {
		c := &http.Client{Transport: hc.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, authURL, http.NoBody)
		resp, err := c.Do(req)
		if err != nil {
			return nil, err
		}
		resp.Body.Close()
		loc, err := url.Parse(resp.Header.Get("Location"))
		if err != nil || resp.StatusCode != http.StatusFound {
			return nil, fmt.Errorf("authorize: %d", resp.StatusCode)
		}
		return ParseAuthorizationResponse(loc.Query()), nil
	}
}

func TestOAuthAuthorizationCodeFlow(t *testing.T) {
	for _, g := range generations {
		t.Run(g.name, func(t *testing.T) {
			t.Parallel()
			as := newFakeAS(t, nil)
			p := newProtectedServer(t, as, nil)
			cfg := OAuthConfig{RedirectURL: "http://127.0.0.1:9/callback", Authorize: browser(t), ClientName: "test client"}
			c, err := Connect(t.Context(), HTTP(p.url), append(slices.Clone(g.opts), WithOAuth(cfg))...)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if c.ProtocolVersion() != g.version {
				t.Fatalf("version %s", c.ProtocolVersion())
			}
			reg := as.registry[0]
			if reg["application_type"] != "native" || reg["client_name"] != "test client" || reg["token_endpoint_auth_method"] != "none" {
				t.Errorf("registration = %v", reg)
			}
			authz := as.last("authorize")
			if authz.Get("resource") != p.resource || authz.Get("scope") != "files:read offline_access" {
				t.Errorf("authorize request = %v", authz)
			}
			tok := as.last("token")
			if tok.Get("resource") != p.resource || tok.Get("code_verifier") == "" || tok.Get("client_id") == "" {
				t.Errorf("token request = %v", tok)
			}
			if out, _ := callText(t, c, "echo", `{"text":"authorized"}`); out != "echo: authorized" {
				t.Errorf("echo = %q", out)
			}
			// The delete tool needs files:write: the client steps up,
			// keeping the scope it had.
			if out, isErr := callText(t, c, "delete", `{"text":"x"}`); out != "deleted x" || isErr {
				t.Errorf("delete = %q", out)
			}
			if got := as.last("authorize").Get("scope"); got != "files:read files:write offline_access" {
				t.Errorf("step-up scope = %q", got)
			}
			if n := as.count("authorize"); n != 2 {
				t.Errorf("%d authorizations", n)
			}
			if n := as.count("register"); n != 1 {
				t.Errorf("%d registrations", n)
			}
			// A revoked token is refreshed without user interaction.
			as.revokeAll()
			if out, _ := callText(t, c, "echo", `{"text":"again"}`); out != "echo: again" {
				t.Errorf("echo after revocation = %q", out)
			}
			if got := as.last("token").Get("grant_type"); got != "refresh_token" || as.count("authorize") != 2 {
				t.Errorf("after revocation: grant %q, %d authorizations", got, as.count("authorize"))
			}
		})
	}
}

func TestOAuthPersistentStore(t *testing.T) {
	t.Parallel()
	as := newFakeAS(t, nil)
	p := newProtectedServer(t, as, nil)
	// The store outlives the clients, as a persistent store outlives a
	// process.
	store := NewMemoryTokenStore()
	cfg := OAuthConfig{RedirectURL: "http://localhost:9/callback", Authorize: browser(t), Store: store}
	for i := range 2 {
		c, err := Connect(t.Context(), HTTP(p.url), WithOAuth(cfg))
		if err != nil {
			t.Fatal(err)
		}
		if out, _ := callText(t, c, "echo", fmt.Sprintf(`{"text":"%d"}`, i)); out != fmt.Sprintf("echo: %d", i) {
			t.Errorf("echo = %q", out)
		}
		_ = c.Close()
	}
	if n := as.count("authorize"); n != 1 {
		t.Errorf("%d authorizations, want the stored token reused", n)
	}
	if tok, err := store.Token(t.Context(), p.url); err != nil || tok == nil || tok.Issuer != as.issuer() || tok.RefreshToken == "" || tok.Resource != p.resource {
		t.Errorf("stored token = %+v, %v", tok, err)
	}
	if reg, err := store.Client(t.Context(), as.issuer()); err != nil || reg == nil {
		t.Errorf("stored client = %+v, %v", reg, err)
	}
}

func TestOAuthExpiredTokenIsRefreshed(t *testing.T) {
	t.Parallel()
	as := newFakeAS(t, func(a *fakeAS) { a.expiresIn = 10 })
	p := newProtectedServer(t, as, nil)
	cfg := OAuthConfig{RedirectURL: "http://127.0.0.1:9/callback", Authorize: browser(t)}
	c, err := Connect(t.Context(), HTTP(p.url), WithOAuth(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Tokens expiring within 30 seconds count as expired, so the next
	// request refreshes first.
	before := as.count("token")
	callText(t, c, "echo", `{"text":"x"}`)
	if as.count("token") <= before || as.last("token").Get("grant_type") != "refresh_token" {
		t.Errorf("no refresh: %v", as.last("token"))
	}
	if as.count("authorize") != 1 {
		t.Errorf("%d authorizations", as.count("authorize"))
	}
}

func TestOAuthClientCredentials(t *testing.T) {
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	for _, tt := range []struct {
		name   string
		cfg    OAuthConfig
		as     func(*fakeAS)
		method string
	}{
		{"basic", OAuthConfig{ClientID: "m2m", ClientSecret: "s3cr:t"}, nil, "basic"},
		{"post", OAuthConfig{ClientID: "m2m", ClientSecret: "s3cr:t"},
			func(a *fakeAS) { a.authMethods = []string{AuthMethodClientSecretPost} }, "post"},
		{"es256", OAuthConfig{ClientID: "m2m", ClientAssertion: PrivateKeyJWT("m2m", ecKey, "k1")}, nil, "jwt"},
		{"rs256", OAuthConfig{ClientID: "m2m", ClientAssertion: PrivateKeyJWT("m2m", rsaKey, "")}, nil, "jwt"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			as := newFakeAS(t, tt.as)
			as.addClient("m2m", "s3cr:t")
			as.jwtKeys["m2m"] = ecKey.Public()
			if tt.name == "rs256" {
				as.jwtKeys["m2m"] = rsaKey.Public()
			}
			p := newProtectedServer(t, as, nil)
			cfg := tt.cfg
			cfg.Grant = GrantClientCredentials
			c, err := Connect(t.Context(), HTTP(p.url), WithOAuth(cfg))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			tok := as.last("token")
			if tok.Get("grant_type") != GrantClientCredentials || tok.Get("resource") != p.resource || tok.Get("scope") != "files:read" {
				t.Errorf("token request = %v", tok)
			}
			switch tt.method {
			case "post":
				if tok.Get("client_secret") != "s3cr:t" {
					t.Errorf("client_secret_post = %v", tok)
				}
			case "jwt":
				if tok.Get("client_assertion_type") == "" || tok.Has("client_id") {
					t.Errorf("private_key_jwt = %v", tok)
				}
			default:
				if tok.Has("client_secret") || tok.Has("client_id") {
					t.Errorf("client_secret_basic = %v", tok)
				}
			}
			// Step-up without a user.
			if out, _ := callText(t, c, "delete", `{"text":"y"}`); out != "deleted y" {
				t.Errorf("delete = %q", out)
			}
			if as.last("token").Get("scope") != "files:read files:write" || as.count("authorize") != 0 || as.count("register") != 0 {
				t.Errorf("step-up token request = %v", as.last("token"))
			}
		})
	}
}

func TestOAuthRegistrationChoices(t *testing.T) {
	t.Parallel()
	t.Run("client ID metadata document", func(t *testing.T) {
		t.Parallel()
		as := newFakeAS(t, func(a *fakeAS) { a.cimd = true })
		p := newProtectedServer(t, as, nil)
		cfg := OAuthConfig{ClientMetadataURL: "https://client.example/oauth/meta.json",
			RedirectURL: "http://127.0.0.1:9/callback", Authorize: browser(t)}
		c, err := Connect(t.Context(), HTTP(p.url), WithOAuth(cfg))
		if err != nil {
			t.Fatal(err)
		}
		_ = c.Close()
		if as.last("authorize").Get("client_id") != cfg.ClientMetadataURL || as.count("register") != 0 {
			t.Errorf("authorize = %v, %d registrations", as.last("authorize"), as.count("register"))
		}
	})
	t.Run("pre-registered", func(t *testing.T) {
		t.Parallel()
		as := newFakeAS(t, nil)
		as.addClient("pre", "")
		as.clients["pre"].redirects = []string{"http://127.0.0.1:9/callback"}
		p := newProtectedServer(t, as, nil)
		cfg := OAuthConfig{ClientID: "pre", ClientIssuer: as.issuer(), RedirectURL: "http://127.0.0.1:9/callback", Authorize: browser(t)}
		c, err := Connect(t.Context(), HTTP(p.url), WithOAuth(cfg))
		if err != nil {
			t.Fatal(err)
		}
		_ = c.Close()
		if as.count("register") != 0 || as.last("token").Get("client_id") != "pre" {
			t.Errorf("token request = %v", as.last("token"))
		}
		cfg.ClientIssuer = "https://elsewhere.example"
		_, err = Connect(t.Context(), HTTP(p.url), WithOAuth(OAuthConfig{ClientID: "pre", ClientIssuer: "https://elsewhere.example",
			RedirectURL: cfg.RedirectURL, Authorize: browser(t)}))
		if !errors.Is(err, ErrAuthorization) || !strings.Contains(err.Error(), "belongs to") {
			t.Errorf("credentials of another issuer: %v", err)
		}
	})
}

func TestOAuthDiscovery(t *testing.T) {
	t.Parallel()
	ok := func(configure func(*fakeAS)) func(t *testing.T) {
		return func(t *testing.T) {
			t.Parallel()
			as := newFakeAS(t, configure)
			p := newProtectedServer(t, as, nil)
			c, err := Connect(t.Context(), HTTP(p.url), WithOAuth(OAuthConfig{RedirectURL: "http://127.0.0.1:9/cb", Authorize: browser(t)}))
			if err != nil {
				t.Fatal(err)
			}
			_ = c.Close()
		}
	}
	t.Run("issuer with path, OpenID path appending", ok(func(a *fakeAS) { a.path, a.openIDOnly = "/tenant1", true }))
	t.Run("issuer with path, RFC 8414", ok(func(a *fakeAS) { a.path = "/tenant1" }))
	t.Run("OpenID without path", ok(func(a *fakeAS) { a.openIDOnly = true }))
	t.Run("no iss support", ok(func(a *fakeAS) { a.iss = false }))
	t.Run("https", func(t *testing.T) {
		t.Parallel()
		as := newFakeAS(t, func(a *fakeAS) { a.tls = true })
		p := newProtectedServer(t, as, nil)
		c, err := Connect(t.Context(), HTTP(p.url), WithOAuth(OAuthConfig{RedirectURL: "https://app.example/cb",
			Authorize: browserWith(as.ts.Client()), HTTPClient: as.ts.Client()}))
		if err != nil {
			t.Fatal(err)
		}
		_ = c.Close()
		if !strings.HasPrefix(as.issuer(), "https://") || as.count("authorize") != 1 {
			t.Errorf("issuer %s, %d authorizations", as.issuer(), as.count("authorize"))
		}
		if reg := as.registry[0]; reg["application_type"] != "web" {
			t.Errorf("registration = %v", reg)
		}
	})
	fails := func(configure func(*fakeAS), want string) func(t *testing.T) {
		return func(t *testing.T) {
			t.Parallel()
			as := newFakeAS(t, configure)
			p := newProtectedServer(t, as, nil)
			_, err := Connect(t.Context(), HTTP(p.url), WithOAuth(OAuthConfig{RedirectURL: "http://127.0.0.1:9/cb", Authorize: browser(t)}))
			if !errors.Is(err, ErrAuthorization) || !strings.Contains(err.Error(), want) {
				t.Fatalf("Connect = %v, want %q", err, want)
			}
		}
	}
	t.Run("issuer mismatch", fails(func(a *fakeAS) { a.wrongIssuer = true }, "is not"))
	t.Run("no PKCE", fails(func(a *fakeAS) { a.noPKCE = true }, "PKCE"))
	t.Run("no registration", fails(func(a *fakeAS) { a.noRegistration = true }, "ClientID"))
	t.Run("missing iss", fails(func(a *fakeAS) { a.omitIss = true }, "iss"))
}

func TestOAuthResourceMetadataFallbackAndChecks(t *testing.T) {
	t.Parallel()
	strip := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(&stripWriter{ResponseWriter: w}, r)
		})
	}
	as := newFakeAS(t, nil)
	p := newProtectedServer(t, as, strip)
	c, err := Connect(t.Context(), HTTP(p.url), WithOAuth(OAuthConfig{RedirectURL: "http://127.0.0.1:9/cb", Authorize: browser(t)}))
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	// Without a scope in the challenge, the client asks for the scopes the
	// resource supports.
	if got := as.last("authorize").Get("scope"); got != "files:read files:write offline_access" {
		t.Errorf("scope = %q", got)
	}

	// Metadata for another resource is refused.
	other := newFakeAS(t, nil)
	mux := http.NewServeMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+ts.URL+`/md"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.Handle("/md", ProtectedResourceMetadataHandler(&ProtectedResourceMetadata{
		Resource: "https://other.example/mcp", AuthorizationServers: []string{other.issuer()}}))
	_, err = Connect(t.Context(), HTTP(ts.URL+"/mcp"), WithOAuth(OAuthConfig{RedirectURL: "http://127.0.0.1:9/cb", Authorize: browser(t)}))
	if !errors.Is(err, ErrAuthorization) || !strings.Contains(err.Error(), "other.example") {
		t.Errorf("foreign resource: %v", err)
	}

	// Authorization servers must use https.
	mux.Handle("/md2", ProtectedResourceMetadataHandler(&ProtectedResourceMetadata{
		Resource: ts.URL + "/mcp2", AuthorizationServers: []string{"http://as.example"}}))
	mux.HandleFunc("/mcp2", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+ts.URL+`/md2"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	_, err = Connect(t.Context(), HTTP(ts.URL+"/mcp2"), WithOAuth(OAuthConfig{RedirectURL: "http://127.0.0.1:9/cb", Authorize: browser(t)}))
	if !errors.Is(err, ErrAuthorization) || !strings.Contains(err.Error(), "must use https") {
		t.Errorf("http authorization server: %v", err)
	}
}

// stripWriter removes resource_metadata from challenges.
type stripWriter struct{ http.ResponseWriter }

func (s *stripWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *stripWriter) WriteHeader(code int) {
	if v := s.Header().Get("WWW-Authenticate"); v != "" {
		s.Header().Set("WWW-Authenticate", "Bearer")
	}
	s.ResponseWriter.WriteHeader(code)
}

func TestOAuthAuthorizationResponseValidation(t *testing.T) {
	t.Parallel()
	as := newFakeAS(t, nil)
	p := newProtectedServer(t, as, nil)
	for _, tt := range []struct {
		name   string
		change func(*AuthorizationResponse)
		want   string
	}{
		{"issuer mismatch", func(r *AuthorizationResponse) { r.Issuer, r.Error = "https://attacker.example", "access_denied" }, "issuer"},
		{"state mismatch", func(r *AuthorizationResponse) { r.State = "forged" }, "state"},
		{"error", func(r *AuthorizationResponse) {
			r.Code, r.Error, r.ErrorDescription = "", "access_denied", "user said no"
		}, "user said no"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			authorize := func(ctx context.Context, authURL string) (*AuthorizationResponse, error) {
				r, err := browser(t)(ctx, authURL)
				if err == nil {
					tt.change(r)
				}
				return r, err
			}
			_, err := Connect(t.Context(), HTTP(p.url), WithOAuth(OAuthConfig{RedirectURL: "http://127.0.0.1:9/cb", Authorize: authorize}))
			if !errors.Is(err, ErrAuthorization) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Connect = %v, want %q", err, tt.want)
			}
		})
	}
	_, err := Connect(t.Context(), HTTP(p.url), WithOAuth(OAuthConfig{}))
	if !errors.Is(err, ErrAuthorization) || !strings.Contains(err.Error(), "Authorize") {
		t.Errorf("without Authorize: %v", err)
	}
	_, err = Connect(t.Context(), HTTP(p.url), WithOAuth(OAuthConfig{RedirectURL: "http://example.com/cb", Authorize: browser(t)}))
	if !errors.Is(err, ErrAuthorization) || !strings.Contains(err.Error(), "loopback or https") {
		t.Errorf("insecure redirect: %v", err)
	}
	if _, err := Connect(t.Context(), Stdio("true"), WithOAuth(OAuthConfig{})); err == nil {
		t.Error("WithOAuth over stdio succeeded")
	}
}

func TestOAuthLoopbackRedirect(t *testing.T) {
	t.Parallel()
	as := newFakeAS(t, nil)
	p := newProtectedServer(t, as, nil)
	lr, err := NewLoopbackRedirect("")
	if err != nil {
		t.Fatal(err)
	}
	defer lr.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	var browsers sync.WaitGroup
	defer func() {
		cancel()
		browsers.Wait()
	}()
	var shown string
	lr.Open = func(authURL string) error {
		shown = authURL
		// The browser follows the redirect to the loopback listener.
		browsers.Go(func() {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, authURL, http.NoBody)
			if err != nil {
				t.Errorf("browser request: %v", err)
				return
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				if ctx.Err() == nil {
					t.Errorf("browser authorization: %v", err)
				}
				return
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("browser callback status = %d", resp.StatusCode)
			}
		})
		return nil
	}
	cfg := OAuthConfig{}
	lr.Configure(&cfg)
	if !strings.HasPrefix(cfg.RedirectURL, "http://127.0.0.1:") || !strings.HasSuffix(cfg.RedirectURL, "/callback") {
		t.Fatalf("redirect URL %q", cfg.RedirectURL)
	}
	c, err := Connect(ctx, HTTP(p.url), WithOAuth(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	browsers.Wait()
	if !strings.HasPrefix(shown, as.issuer()+"/authorize?") {
		t.Errorf("shown %q", shown)
	}
	// A response nobody waits for is refused.
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, lr.URL()+"?code=x&state=unknown", http.NoBody)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unexpected callback: %d", resp.StatusCode)
	}
	if _, err := NewLoopbackRedirect("192.0.2.1:0"); err == nil {
		t.Error("non-loopback address accepted")
	}
}

func TestParseChallenges(t *testing.T) {
	t.Parallel()
	got := parseChallenges([]string{
		`Basic realm="x", Bearer resource_metadata="https://a.example/.well-known/oauth-protected-resource",` +
			` scope="files:read files:write", error="insufficient_scope", error_description="needs \"write\""`,
		`DPoP algs="ES256"`,
	})
	if len(got) != 3 || got[0].scheme != "Basic" || got[0].params["realm"] != "x" || got[1].scheme != "Bearer" ||
		got[1].params["scope"] != "files:read files:write" || got[1].params["error"] != "insufficient_scope" ||
		got[1].params["error_description"] != `needs "write"` || got[2].scheme != "DPoP" {
		t.Errorf("challenges = %+v", got)
	}
}

func TestServerAuthChallenges(t *testing.T) {
	t.Parallel()
	as := newFakeAS(t, nil)
	p := newProtectedServer(t, as, nil)
	body, h := modernRequest(1, "tools/list", nil)
	r := rawDo(t, http.MethodPost, p.url, h, body)
	wantMD := ProtectedResourceMetadataURL(p.resource)
	if r.status != 401 || r.header.Get("WWW-Authenticate") != `Bearer resource_metadata="`+wantMD+`", scope="files:read"` {
		t.Errorf("no token: %d %q", r.status, r.header.Get("WWW-Authenticate"))
	}
	h["Authorization"] = "Bearer nope"
	r = rawDo(t, http.MethodPost, p.url, h, body)
	if r.status != 401 || !strings.Contains(r.header.Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Errorf("invalid token: %d %q", r.status, r.header.Get("WWW-Authenticate"))
	}
	// Tokens for another resource, or without the required scope.
	as.mu.Lock()
	as.tokens["aud"] = &asToken{subject: "u", resource: "https://other.example/mcp", scope: "files:read"}
	as.tokens["weak"] = &asToken{subject: "u", resource: p.resource, scope: "profile"}
	as.tokens["good"] = &asToken{subject: "u", resource: p.resource, scope: "files:read"}
	as.tokens["eve"] = &asToken{subject: "eve", resource: p.resource, scope: "files:read"}
	as.mu.Unlock()
	h["Authorization"] = "Bearer aud"
	if r := rawDo(t, http.MethodPost, p.url, h, body); r.status != 401 {
		t.Errorf("audience: %d", r.status)
	}
	h["Authorization"] = "Bearer weak"
	r = rawDo(t, http.MethodPost, p.url, h, body)
	if r.status != 403 || !strings.Contains(r.header.Get("WWW-Authenticate"), `error="insufficient_scope"`) {
		t.Errorf("scope: %d %q", r.status, r.header.Get("WWW-Authenticate"))
	}
	h["Authorization"] = "Bearer good"
	if r := rawDo(t, http.MethodPost, p.url, h, body); r.status != 200 {
		t.Errorf("good token: %d %s", r.status, r.body)
	}
	cb, ch := modernRequest(2, "tools/call", map[string]any{"name": "delete", "arguments": map[string]any{"text": "x"}})
	ch["Authorization"] = "Bearer good"
	r = rawDo(t, http.MethodPost, p.url, ch, cb)
	if r.status != 403 || !strings.Contains(r.header.Get("WWW-Authenticate"), `scope="files:read files:write"`) {
		t.Errorf("operation scope: %d %q", r.status, r.header.Get("WWW-Authenticate"))
	}
	// The metadata is served at its well-known URL.
	md := rawDo(t, http.MethodGet, wantMD, nil, "")
	if md.status != 200 || !strings.Contains(md.body, `"authorization_servers":["`+as.issuer()+`"]`) ||
		md.header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("metadata: %d %s", md.status, md.body)
	}
	// A session belongs to the principal that created it.
	init := map[string]string{"Authorization": "Bearer good"}
	r = rawDo(t, http.MethodPost, p.url, init,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`)
	sid := r.header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatalf("initialize: %d %s", r.status, r.body)
	}
	eve := map[string]string{"Authorization": "Bearer eve", "Mcp-Session-Id": sid}
	if r := rawDo(t, http.MethodPost, p.url, eve, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`); r.status != 404 {
		t.Errorf("another principal's session: %d", r.status)
	}
}

func TestServerMetadataAtRoot(t *testing.T) {
	t.Parallel()
	s := NewServer(Implementation{}, ServerOptions{})
	md := &ProtectedResourceMetadata{Resource: "https://mcp.example.com", AuthorizationServers: []string{"https://as.example"}}
	h := s.HTTPHandler(HTTPOptions{Auth: &AuthOptions{Metadata: md, Verify: func(context.Context, string, *http.Request) (*TokenInfo, error) {
		return nil, errors.New("no")
	}}})
	ts := httptest.NewServer(h)
	defer ts.Close()
	r := rawDo(t, http.MethodGet, ts.URL+"/.well-known/oauth-protected-resource", nil, "")
	if r.status != 200 || !strings.Contains(r.body, `"resource":"https://mcp.example.com"`) {
		t.Errorf("metadata at root: %d %s", r.status, r.body)
	}
	if got := ProtectedResourceMetadataURL("https://mcp.example.com/a/mcp/"); got != "https://mcp.example.com/.well-known/oauth-protected-resource/a/mcp" {
		t.Errorf("ProtectedResourceMetadataURL = %q", got)
	}
}

func TestPrivateKeyJWTRejectsUnsupportedKeys(t *testing.T) {
	t.Parallel()
	k384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if _, err := PrivateKeyJWT("c", k384, "")(t.Context(), "", "https://as/token"); err == nil {
		t.Error("P-384 key accepted")
	}
}

func TestMemoryTokenStoreCopies(t *testing.T) {
	t.Parallel()
	s := NewMemoryTokenStore()
	tok := &Token{AccessToken: "a"}
	_ = s.SetToken(t.Context(), "srv", tok)
	tok.AccessToken = "changed"
	got, _ := s.Token(t.Context(), "srv")
	if got == nil || got.AccessToken != "a" {
		t.Fatalf("token = %+v", got)
	}
	got.AccessToken = "changed through lookup"
	if got, _ := s.Token(t.Context(), "srv"); got == nil || got.AccessToken != "a" {
		t.Errorf("stored token changed through lookup: %+v", got)
	}
	_ = s.SetToken(t.Context(), "srv", nil)
	if got, _ := s.Token(t.Context(), "srv"); got != nil {
		t.Errorf("deleted token = %+v", got)
	}
	client := &ClientRegistration{ClientID: "c"}
	_ = s.SetClient(t.Context(), "iss", client)
	client.ClientID = "changed"
	reg, _ := s.Client(t.Context(), "iss")
	if reg == nil || reg.ClientID != "c" {
		t.Fatalf("client = %+v", reg)
	}
	reg.ClientID = "changed through lookup"
	if got, _ := s.Client(t.Context(), "iss"); got == nil || got.ClientID != "c" {
		t.Errorf("stored client changed through lookup: %+v", got)
	}
}

func TestSameRedirect(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		a, b string
		want bool
	}{
		{"http://127.0.0.1:9/cb", "http://127.0.0.1:10/cb", true},
		{"http://[::1]:9/cb", "http://[::1]:10/cb", true},
		{"http://127.0.0.1:9/cb", "http://127.0.0.1:9/other", false},
		{"http://localhost:9/cb", "http://localhost:10/cb", false},
		{"https://app.example/cb", "https://app.example/cb", true},
		{"https://app.example:1/cb", "https://app.example:2/cb", false},
	} {
		if got := sameRedirect(tt.a, tt.b); got != tt.want {
			t.Errorf("sameRedirect(%s, %s) = %v", tt.a, tt.b, got)
		}
	}
}

func TestTokenEndpointFailures(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"expired code"}`))
		case "/text":
			http.Error(w, "upstream down", http.StatusBadGateway)
		case "/mac":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"x","token_type":"mac"}`))
		}
	}))
	defer ts.Close()
	tr := (&OAuthConfig{}).Transport(nil).(*oauthTransport)
	reg := &ClientRegistration{ClientID: "c"}
	for path, want := range map[string]string{"/json": "invalid_grant: expired code", "/text": "http 502: upstream down", "/mac": "token type"} {
		md := &AuthorizationServerMetadata{Issuer: ts.URL, TokenEndpoint: ts.URL + path}
		_, err := tr.tokenRequest(t.Context(), md, reg, url.Values{"grant_type": {"refresh_token"}}, "", nil)
		if !errors.Is(err, ErrAuthorization) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", path, err)
		}
	}
	var oe *OAuthError
	md := &AuthorizationServerMetadata{Issuer: ts.URL, TokenEndpoint: ts.URL + "/json"}
	if _, err := tr.tokenRequest(t.Context(), md, reg, url.Values{}, "", nil); !errors.As(err, &oe) || oe.StatusCode != http.StatusBadRequest || oe.Code != "invalid_grant" {
		t.Errorf("OAuthError = %+v", oe)
	}
}

// Authorization server responses larger than the limit fail explicitly
// instead of being truncated into malformed JSON.
func TestOAuthResponseLimit(t *testing.T) {
	t.Parallel()
	// sized returns a token response of exactly n bytes.
	sized := func(n int) []byte {
		const head, tail = `{"access_token":"x","token_type":"Bearer","pad":"`, `"}`
		return []byte(head + strings.Repeat("x", n-len(head)-len(tail)) + tail)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/exact":
			_, _ = w.Write(sized(maxOAuthResponse))
		case "/over":
			_, _ = w.Write(sized(maxOAuthResponse + 1))
		case "/no-client":
			_, _ = w.Write([]byte(`{"pad":"` + strings.Repeat("x", maxOAuthResponse/2) + `"}`))
		case "/over-error":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write(sized(maxOAuthResponse + 1))
		}
	}))
	defer ts.Close()
	tr := (&OAuthConfig{RedirectURL: "http://127.0.0.1:9/callback"}).Transport(nil).(*oauthTransport)
	reg := &ClientRegistration{ClientID: "c"}
	token := func(path string) error {
		md := &AuthorizationServerMetadata{Issuer: ts.URL, TokenEndpoint: ts.URL + path}
		_, err := tr.tokenRequest(t.Context(), md, reg, url.Values{"grant_type": {"refresh_token"}}, "", nil)
		return err
	}
	if err := token("/exact"); err != nil {
		t.Errorf("token response at the limit: %v", err)
	}
	const tooLarge = "response larger than 1048576 bytes"
	for name, err := range map[string]error{
		"metadata": tr.getJSON(t.Context(), ts.URL+"/over", new(AuthorizationServerMetadata)),
		"token":    token("/over"),
		"register": func() error {
			_, err := tr.register(t.Context(), &AuthorizationServerMetadata{Issuer: ts.URL, RegistrationEndpoint: ts.URL + "/over"}, nil)
			return err
		}(),
	} {
		if !errors.Is(err, boundedio.ErrLimitExceeded) || !strings.Contains(err.Error(), tooLarge) {
			t.Errorf("%s: %v, want %q", name, err, tooLarge)
		}
		if name != "metadata" && !errors.Is(err, ErrAuthorization) {
			t.Errorf("%s: %v, want ErrAuthorization", name, err)
		}
	}
	// An invalid registration quotes only the start of the response.
	_, err := tr.register(t.Context(), &AuthorizationServerMetadata{Issuer: ts.URL, RegistrationEndpoint: ts.URL + "/no-client"}, nil)
	if !errors.Is(err, ErrAuthorization) || !strings.Contains(err.Error(), "invalid response") || len(err.Error()) > 500 {
		t.Errorf("invalid registration: %d bytes: %.200s", len(err.Error()), err)
	}
	// The status of an error response wins over the size of its body.
	if err := token("/over-error"); !errors.Is(err, ErrAuthorization) || !strings.Contains(err.Error(), "http 400") {
		t.Errorf("oversized error response: %v", err)
	}
}

// authRecorder is a server that records the Authorization headers it
// receives.
type authRecorder struct {
	ts   *httptest.Server
	mu   sync.Mutex
	auth []string
}

func newAuthRecorder(t *testing.T) *authRecorder {
	r := &authRecorder{}
	r.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.auth = append(r.auth, req.Header.Get("Authorization"))
		r.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(r.ts.Close)
	return r
}

func (r *authRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.auth)
}

// A round tripper serving several hosts sends each only its own token.
func TestOAuthTokenStaysWithItsServer(t *testing.T) {
	t.Parallel()
	as := newFakeAS(t, nil)
	p := newProtectedServer(t, as, nil)
	other := newAuthRecorder(t)
	cfg := OAuthConfig{RedirectURL: "http://localhost:9/callback", Authorize: browser(t)}
	hc := &http.Client{Transport: cfg.Transport(nil)}
	ht := HTTP(p.url)
	ht.Client = hc
	c, err := Connect(t.Context(), ht)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if out, _ := callText(t, c, "echo", `{"text":"x"}`); out != "echo: x" {
		t.Fatalf("echo = %q", out)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, other.ts.URL+"/mcp", strings.NewReader("{}"))
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := other.seen(); len(got) != 1 || got[0] != "" {
		t.Fatalf("the other server received Authorization %q", got)
	}
}

// The authorized client does not follow a server's redirect to another
// origin, which would carry the token there.
func TestOAuthRefusesCrossOriginRedirect(t *testing.T) {
	t.Parallel()
	as := newFakeAS(t, nil)
	other := newAuthRecorder(t)
	var redirect atomic.Bool
	p := newProtectedServer(t, as, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if redirect.Load() {
				http.Redirect(w, r, other.ts.URL+"/mcp", http.StatusTemporaryRedirect)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	cfg := OAuthConfig{RedirectURL: "http://localhost:9/callback", Authorize: browser(t)}
	c, err := Connect(t.Context(), HTTP(p.url), WithOAuth(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if out, _ := callText(t, c, "echo", `{"text":"x"}`); out != "echo: x" {
		t.Fatalf("echo = %q", out)
	}
	redirect.Store(true)
	if _, err := c.CallTool(t.Context(), "echo", json.RawMessage(`{"text":"y"}`)); err == nil {
		t.Fatal("CallTool followed a cross-origin redirect")
	}
	if got := other.seen(); len(got) != 0 {
		t.Fatalf("the other origin received requests with Authorization %q", got)
	}
}
