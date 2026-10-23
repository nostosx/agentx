package mcp

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"
	"uuid"
)

// LoopbackRedirect receives authorization responses on a local HTTP
// listener, the redirect of native applications in RFC 8252, section 7.3.
// Use its URL as OAuthConfig.RedirectURL and its Authorize method as
// OAuthConfig.Authorize; Configure does both.
type LoopbackRedirect struct {
	// Open shows the authorization URL to the user. Defaults to
	// OpenBrowser.
	Open func(authURL string) error

	ln  net.Listener
	srv *http.Server
	url string

	mu      sync.Mutex
	waiting map[string]chan *AuthorizationResponse
}

// NewLoopbackRedirect starts listening on addr, "127.0.0.1:0" (a free port
// of the IPv4 loopback address) if empty, for redirects to /callback.
func NewLoopbackRedirect(addr string) (*LoopbackRedirect, error) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("mcp: loopback redirect: %w", err)
	}
	if !isLoopback(host) {
		return nil, fmt.Errorf("mcp: loopback redirect: %s is not a loopback address", host)
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("mcp: loopback redirect: %w", err)
	}
	l := &LoopbackRedirect{ln: ln, waiting: map[string]chan *AuthorizationResponse{}}
	l.url = "http://" + ln.Addr().String() + "/callback"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /callback", l.callback)
	l.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = l.srv.Serve(ln) }()
	return l, nil
}

// URL returns the redirect URL.
func (l *LoopbackRedirect) URL() string { return l.url }

// Configure sets the redirect URL and the Authorize function of cfg.
func (l *LoopbackRedirect) Configure(cfg *OAuthConfig) {
	cfg.RedirectURL = l.url
	cfg.Authorize = l.Authorize
}

// Authorize shows authURL to the user with Open and waits, bounded by ctx,
// for the authorization response with the request's state.
func (l *LoopbackRedirect) Authorize(ctx context.Context, authURL string) (*AuthorizationResponse, error) {
	u, err := url.Parse(authURL)
	if err != nil {
		return nil, err
	}
	state := u.Query().Get("state")
	if state == "" {
		return nil, errors.New("mcp: authorization URL without state")
	}
	ch := make(chan *AuthorizationResponse, 1)
	l.mu.Lock()
	l.waiting[state] = ch
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.waiting, state)
		l.mu.Unlock()
	}()
	open := l.Open
	if open == nil {
		open = OpenBrowser
	}
	if err := open(authURL); err != nil {
		return nil, err
	}
	select {
	case res := <-ch:
		return res, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (l *LoopbackRedirect) callback(w http.ResponseWriter, r *http.Request) {
	res := ParseAuthorizationResponse(r.URL.Query())
	l.mu.Lock()
	ch := l.waiting[res.State]
	delete(l.waiting, res.State)
	l.mu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if ch == nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "<!doctype html><p>Unexpected authorization response.</p>")
		return
	}
	ch <- res
	msg := "Authorization complete. You can close this window."
	if res.Error != "" {
		msg = "Authorization failed: " + res.Error
	}
	_, _ = io.WriteString(w, "<!doctype html><p>"+html.EscapeString(msg)+"</p>")
}

// Close stops the listener.
func (l *LoopbackRedirect) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return l.srv.Shutdown(ctx)
}

// OpenBrowser prints authURL to standard error and tries to open it in the
// default browser.
func OpenBrowser(authURL string) error {
	fmt.Fprintf(os.Stderr, "Open this URL to authorize access:\n%s\n", authURL)
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler"}
	default:
		name = "xdg-open"
	}
	cmd := exec.CommandContext(context.Background(), name, append(args, authURL)...)
	// Without a browser, the user opens the printed URL by hand.
	if cmd.Start() == nil {
		go func() { _ = cmd.Wait() }()
	}
	return nil
}

// PrivateKeyJWT returns an OAuthConfig.ClientAssertion that signs RFC 7523
// client assertions for clientID with key: an *rsa.PrivateKey (RS256) or an
// *ecdsa.PrivateKey on P-256 (ES256). keyID, if set, is the kid of the
// key in the client's JWKS. Each assertion is valid for five minutes and
// names the token endpoint as its audience.
func PrivateKeyJWT(clientID string, key crypto.Signer, keyID string) func(ctx context.Context, issuer, tokenEndpoint string) (string, error) {
	return func(_ context.Context, _, tokenEndpoint string) (string, error) {
		var alg string
		switch k := key.(type) {
		case *rsa.PrivateKey:
			alg = "RS256"
		case *ecdsa.PrivateKey:
			if k.Curve != elliptic.P256() {
				return "", errors.New("mcp: PrivateKeyJWT: ECDSA keys must use P-256")
			}
			alg = "ES256"
		default:
			return "", fmt.Errorf("mcp: PrivateKeyJWT: unsupported key type %T", key)
		}
		header := map[string]string{"alg": alg, "typ": "JWT"}
		if keyID != "" {
			header["kid"] = keyID
		}
		now := time.Now()
		claims := map[string]any{
			"iss": clientID, "sub": clientID, "aud": tokenEndpoint,
			"jti": uuid.New().String(), "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		}
		enc := func(v any) string {
			b, _ := json.Marshal(v)
			return base64.RawURLEncoding.EncodeToString(b)
		}
		input := enc(header) + "." + enc(claims)
		sum := sha256.Sum256([]byte(input))
		sig, err := key.Sign(rand.Reader, sum[:], crypto.SHA256)
		if err != nil {
			return "", err
		}
		if alg == "ES256" {
			// JWS uses the fixed-size r || s form, not ASN.1.
			var rs struct{ R, S *big.Int }
			if _, err := asn1.Unmarshal(sig, &rs); err != nil {
				return "", err
			}
			sig = make([]byte, 64)
			rs.R.FillBytes(sig[:32])
			rs.S.FillBytes(sig[32:])
		}
		return input + "." + base64.RawURLEncoding.EncodeToString(sig), nil
	}
}
