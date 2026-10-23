package builtin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

// errPrivateAddress reports a destination web_fetch refuses by default.
var errPrivateAddress = errors.New("refusing to fetch a loopback, private or link-local address")

var (
	cgnat     = netip.MustParsePrefix("100.64.0.0/10")
	thisNet   = netip.MustParsePrefix("0.0.0.0/8")
	nat64Well = netip.MustParsePrefix("64:ff9b::/96")
)

// isPublic reports whether addr is a globally routable unicast address.
func isPublic(addr netip.Addr) bool {
	addr = addr.Unmap()
	if nat64Well.Contains(addr) {
		// A NAT64 address embeds the IPv4 destination in its last 4 bytes.
		b := addr.As16()
		addr = netip.AddrFrom4([4]byte(b[12:]))
	}
	return addr.IsGlobalUnicast() && !addr.IsPrivate() && !cgnat.Contains(addr) && !thisNet.Contains(addr)
}

// checkHost resolves host and fails unless every address is public.
//
// It is a check made before the request, by name. For direct connections
// the dialer checks the address actually dialed as well. A request sent
// through a proxy is checked here only: the proxy resolves the name again,
// so a DNS answer that changes between this check and the proxy's lookup
// (DNS rebinding) can still reach an address behind the proxy.
func checkHost(ctx context.Context, host string) error {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return fmt.Errorf("%w: %s", errPrivateAddress, host)
	}
	if addr, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		if !isPublic(addr) {
			return fmt.Errorf("%w: %s", errPrivateAddress, host)
		}
		return nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	for _, addr := range addrs {
		if !isPublic(addr) {
			return fmt.Errorf("%w: %s resolves to %s", errPrivateAddress, host, addr)
		}
	}
	return nil
}

// Default limits of the web_fetch clients. A caller's Config.HTTPClient
// sets its own. They are variables for tests.
var (
	// fetchTimeout bounds a whole fetch, including reading the body, so a
	// server that sends its response slowly cannot hang the tool.
	fetchTimeout = 60 * time.Second
	// fetchHeaderTimeout bounds the wait for response headers.
	fetchHeaderTimeout = 30 * time.Second
)

// fetchClient returns a client with the web_fetch timeouts and the proxy
// from the environment. guarded selects publicClient.
func fetchClient(guarded bool) *http.Client {
	if guarded {
		return publicClient()
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = fetchHeaderTimeout
	return &http.Client{Transport: tr, Timeout: fetchTimeout}
}

// publicClient returns the default web_fetch client. It checks the
// destination host of every request and redirect with checkHost.
//
// For direct connections it also checks the address actually dialed, so
// DNS rebinding cannot reach an internal address. The proxy from the
// environment is still used, even on a private address, but then only
// the checkHost check applies: the proxy resolves the destination itself,
// and DNS rebinding between the check and the proxy's lookup is not
// detected. Unset the proxy, or pass a Config.HTTPClient whose proxy
// enforces its own policy, where that matters.
func publicClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = fetchHeaderTimeout
	var proxies sync.Map // "host:port" of proxies chosen for requests
	tr.Proxy = func(r *http.Request) (*url.URL, error) {
		u, err := http.ProxyFromEnvironment(r)
		if u != nil {
			proxies.Store(proxyAddr(u), true)
		}
		return u, err
	}
	plain := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	guarded := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return err
			}
			if !isPublic(ap.Addr()) {
				return fmt.Errorf("%w: %s", errPrivateAddress, ap.Addr())
			}
			return nil
		}}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if _, ok := proxies.Load(addr); ok {
			return plain.DialContext(ctx, network, addr)
		}
		return guarded.DialContext(ctx, network, addr)
	}
	return &http.Client{
		Transport: tr,
		Timeout:   fetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("redirect to unsupported scheme %q", req.URL.Scheme)
			}
			return checkHost(req.Context(), req.URL.Hostname())
		},
	}
}

// proxyAddr returns the address the transport dials for proxy u.
func proxyAddr(u *url.URL) string {
	if u.Port() != "" {
		return u.Host
	}
	port := "80"
	switch u.Scheme {
	case "https":
		port = "443"
	case "socks5", "socks5h":
		port = "1080"
	}
	return net.JoinHostPort(u.Hostname(), port)
}
