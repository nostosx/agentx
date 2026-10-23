package builtin

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	osexec "os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestProxyAddr(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"http://proxy.example":          "proxy.example:80",
		"http://proxy.example:3128":     "proxy.example:3128",
		"https://proxy.example":         "proxy.example:443",
		"https://proxy.example:8443":    "proxy.example:8443",
		"socks5://proxy.example":        "proxy.example:1080",
		"socks5h://proxy.example":       "proxy.example:1080",
		"socks5://user:pw@10.0.0.1":     "10.0.0.1:1080",
		"http://[::1]":                  "[::1]:80",
		"http://[2001:db8::1]:8080":     "[2001:db8::1]:8080",
		"http://user:secret@127.0.0.1/": "127.0.0.1:80",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := proxyAddr(u); got != want {
			t.Errorf("proxyAddr(%s) = %q, want %q", raw, got, want)
		}
	}
}

// proxyChildEnv marks the process started by TestWebFetchThroughProxy.
// http.ProxyFromEnvironment reads the environment once per process, so the
// proxied client is tested in a child process whose environment names the
// proxy from the start; no other test sees that environment.
const proxyChildEnv = "AGENTX_TEST_WEB_FETCH_PROXY_CHILD"

// publicIP is a public address that is never dialed: the proxy answers for
// it.
const publicIP = "93.184.215.14"

func TestWebFetchThroughProxy(t *testing.T) {
	if os.Getenv(proxyChildEnv) != "" {
		webFetchThroughProxyChild(t)
		return
	}
	t.Parallel()
	var mu sync.Mutex
	var seen []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.RequestURI)
		mu.Unlock()
		switch {
		case r.Method == http.MethodConnect:
			http.Error(w, "no tunnels", http.StatusForbidden)
		case r.URL.Path == "/redirect":
			http.Redirect(w, r, "http://10.0.0.1/secret", http.StatusFound)
		default:
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprintf(w, "via proxy for %s", r.Host)
		}
	}))
	defer proxy.Close()

	cmd := osexec.Command(os.Args[0], "-test.run=^TestWebFetchThroughProxy$", "-test.count=1", "-test.v")
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch strings.ToLower(name) {
		case "http_proxy", "https_proxy", "no_proxy", "all_proxy", "request_method":
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	cmd.Env = append(cmd.Env, proxyChildEnv+"=1", "HTTP_PROXY="+proxy.URL, "HTTPS_PROXY="+proxy.URL)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "proxy child ran") {
		t.Fatalf("child: %v\n%s", err, out)
	}
	mu.Lock()
	defer mu.Unlock()
	// Public destinations went through the proxy, which the dialer reached
	// although it listens on loopback. The refused destinations never
	// reached it, and neither did the private redirect target.
	want := []string{
		"GET http://" + publicIP + "/page",
		"GET http://" + publicIP + "/redirect",
		"CONNECT " + publicIP + ":443",
	}
	if !slices.Equal(seen, want) {
		t.Fatalf("proxy saw %q, want %q", seen, want)
	}
}

func webFetchThroughProxyChild(t *testing.T) {
	if os.Getenv("HTTP_PROXY") == "" {
		t.Fatal("child started without a proxy")
	}
	f := WebFetch(Config{})
	if res := call(t, f, map[string]any{"url": "http://" + publicIP + "/page"}); res.IsError || !strings.HasSuffix(res.Content, "via proxy for "+publicIP) {
		t.Errorf("public fetch: %q", res.Content)
	}
	// Private destinations are refused by name before any request, even
	// though the proxy would resolve and reach them.
	for _, u := range []string{"http://10.0.0.1/", "http://localhost/", "http://app.localhost/", "http://[fd00::1]/", "http://169.254.169.254/latest"} {
		if res := call(t, f, map[string]any{"url": u}); !res.IsError || !strings.Contains(res.Content, "refusing") {
			t.Errorf("%s: %q", u, res.Content)
		}
	}
	// A redirect to a private address is refused as well.
	if res := call(t, f, map[string]any{"url": "http://" + publicIP + "/redirect"}); !res.IsError || !strings.Contains(res.Content, "refusing") {
		t.Errorf("redirect: %q", res.Content)
	}
	// HTTPS is tunneled through the proxy; this one refuses the tunnel.
	if res := call(t, f, map[string]any{"url": "https://" + publicIP + "/"}); !res.IsError || strings.Contains(res.Content, "refusing") {
		t.Errorf("https: %q", res.Content)
	}
	t.Log("proxy child ran")
}
