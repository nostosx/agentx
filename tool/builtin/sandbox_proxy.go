package builtin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// domainProxy is the HTTP proxy through which sandboxed commands reach
// the hosts of SandboxPolicy.AllowedDomains, like Claude Code's sandbox
// proxy: it forwards plain HTTP requests and CONNECT tunnels to allowed
// hosts, refuses others with 403 Forbidden and records them. It decides by
// the requested host name and does not inspect TLS, so a client inside
// the sandbox could still reach other hosts behind an allowed one, for
// example by domain fronting.
type domainProxy struct {
	rules []domainRule
	tcp   net.Listener
	// unix and socket are the Unix socket listener, for the bridge into a
	// bubblewrap network namespace, and its path.
	unix   net.Listener
	socket string
	srv    *http.Server
	fwd    *httputil.ReverseProxy
	dialer net.Dialer
	// lookup resolves host names; tests replace it.
	lookup func(ctx context.Context, network, host string) ([]netip.Addr, error)

	mu      sync.Mutex
	tunnels map[net.Conn]bool
	denied  []denial
	closed  bool
}

type denial struct {
	host string
	at   time.Time
}

// maxDenials bounds the refusals the proxy remembers.
const maxDenials = 100

// domainRule is a parsed entry of AllowedDomains.
type domainRule struct {
	// host is a lowercase name, "*", "*.domain" or an IP address.
	host string
	// port is 0 for any port.
	port int
}

// parseDomainRule parses an AllowedDomains entry.
func parseDomainRule(s string) (domainRule, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return domainRule{}, errors.New("empty domain")
	}
	host, port := s, 0
	if strings.HasPrefix(s, "[") {
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return domainRule{}, fmt.Errorf("domain %q: missing ]", s)
		}
		host = s[1:end]
		if rest := s[end+1:]; rest != "" {
			p, ok := strings.CutPrefix(rest, ":")
			if !ok {
				return domainRule{}, fmt.Errorf("domain %q: unexpected %q", s, rest)
			}
			n, err := strconv.Atoi(p)
			if err != nil || n <= 0 || n > 65535 {
				return domainRule{}, fmt.Errorf("domain %q: bad port", s)
			}
			port = n
		}
		if _, err := netip.ParseAddr(host); err != nil {
			return domainRule{}, fmt.Errorf("domain %q: bad IPv6 address", s)
		}
		return domainRule{host: host, port: port}, nil
	}
	if strings.Count(s, ":") == 1 {
		h, p, _ := strings.Cut(s, ":")
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 || n > 65535 {
			return domainRule{}, fmt.Errorf("domain %q: bad port", s)
		}
		host, port = h, n
	} else if strings.Contains(s, ":") {
		return domainRule{}, fmt.Errorf("domain %q: write IPv6 addresses in brackets", s)
	}
	host = strings.TrimSuffix(host, ".")
	name := strings.TrimPrefix(host, "*.")
	if host != "*" && (name == "" || strings.ContainsAny(name, "*/ ")) {
		return domainRule{}, fmt.Errorf("domain %q: only a leading *. or a bare * may be a wildcard", s)
	}
	return domainRule{host: host, port: port}, nil
}

// matches reports whether the rule allows host, lowercase without a
// trailing dot, on port.
func (r domainRule) matches(host string, port int) bool {
	if r.port != 0 && r.port != port {
		return false
	}
	ip, err := netip.ParseAddr(host)
	isIP := err == nil
	switch {
	case r.host == "*":
		// A wildcard reaches IP addresses only where it would reach names:
		// not the local addresses dial refuses for names, such as the
		// loopback services of the host or a cloud metadata endpoint.
		return !isIP || !isLocalAddr(ip.Unmap())
	case strings.HasPrefix(r.host, "*."):
		return !isIP && strings.HasSuffix(host, r.host[1:])
	}
	if a, err := netip.ParseAddr(r.host); err == nil {
		return isIP && a.Unmap() == ip.Unmap()
	}
	return r.host == host
}

// newDomainProxy starts a proxy for domains on a loopback port and, if
// unixSocket is set, on a Unix socket in a new temporary directory.
func newDomainProxy(domains []string, unixSocket bool) (*domainProxy, error) {
	p := &domainProxy{tunnels: map[net.Conn]bool{}, lookup: net.DefaultResolver.LookupNetIP}
	p.dialer = net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	for _, d := range domains {
		r, err := parseDomainRule(d)
		if err != nil {
			return nil, err
		}
		p.rules = append(p.rules, r)
	}
	var lc net.ListenConfig
	tcp, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p.tcp = tcp
	if unixSocket {
		// socat splits addresses at ':' and ','; keep them out of the path.
		base := os.TempDir()
		if strings.ContainsAny(base, ":,") {
			base = "/tmp"
		}
		dir, err := os.MkdirTemp(base, "agentx-proxy-")
		if err != nil {
			tcp.Close()
			return nil, err
		}
		p.socket = filepath.Join(dir, "proxy.sock")
		unix, err := lc.Listen(context.Background(), "unix", p.socket)
		if err != nil {
			tcp.Close()
			os.RemoveAll(dir)
			return nil, err
		}
		p.unix = unix
	}
	tr := &http.Transport{
		DialContext:           p.dial,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	p.fwd = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			// The Host header follows the checked URL, so a request cannot
			// name an allowed host and ask the server for another.
			r.Out.URL = r.In.URL
			r.Out.Host = ""
		},
		Transport: tr,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "agentx sandbox proxy: "+err.Error(), http.StatusBadGateway)
		},
	}
	p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = p.srv.Serve(tcp) }()
	if p.unix != nil {
		go func() { _ = p.srv.Serve(p.unix) }()
	}
	return p, nil
}

// port returns the loopback port of the proxy.
func (p *domainProxy) port() int { return p.tcp.Addr().(*net.TCPAddr).Port }

func (p *domainProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var host, port string
	switch {
	case r.Method == http.MethodConnect:
		h, pt, err := net.SplitHostPort(r.Host)
		if err != nil {
			http.Error(w, "agentx sandbox proxy: CONNECT needs host:port", http.StatusBadRequest)
			return
		}
		host, port = h, pt
	case r.URL.IsAbs() && r.URL.Scheme == "http":
		host, port = r.URL.Hostname(), r.URL.Port()
		if port == "" {
			port = "80"
		}
	default:
		http.Error(w, "agentx sandbox proxy: only absolute http:// requests and CONNECT are proxied", http.StatusBadRequest)
		return
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	n, err := strconv.Atoi(port)
	if err != nil || !p.allows(host, n) {
		p.deny(net.JoinHostPort(host, port))
		http.Error(w, fmt.Sprintf("agentx sandbox proxy: %s is not an allowed domain", host), http.StatusForbidden)
		return
	}
	if r.Method != http.MethodConnect {
		p.fwd.ServeHTTP(w, r)
		return
	}
	upstream, err := p.dial(r.Context(), "tcp", net.JoinHostPort(host, port))
	if err != nil {
		http.Error(w, "agentx sandbox proxy: "+err.Error(), http.StatusBadGateway)
		return
	}
	rc := http.NewResponseController(w)
	conn, brw, err := rc.Hijack()
	if err != nil {
		upstream.Close()
		http.Error(w, "agentx sandbox proxy: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !p.track(conn, upstream) {
		conn.Close()
		upstream.Close()
		return
	}
	defer p.untrack(conn, upstream)
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() {
		// The client may have sent bytes after the request, such as a
		// TLS ClientHello, which the server already buffered.
		_, _ = io.Copy(upstream, brw.Reader)
		closeWrite(upstream)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, upstream)
		closeWrite(conn)
		done <- struct{}{}
	}()
	<-done
	<-done
}

// closeWrite half-closes c if it supports it.
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

func (p *domainProxy) track(conns ...net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	for _, c := range conns {
		p.tunnels[c] = true
	}
	return true
}

func (p *domainProxy) untrack(conns ...net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range conns {
		c.Close()
		delete(p.tunnels, c)
	}
}

// allows reports whether a rule allows host on port.
func (p *domainProxy) allows(host string, port int) bool {
	return slices.ContainsFunc(p.rules, func(r domainRule) bool { return r.matches(host, port) })
}

func (p *domainProxy) deny(host string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.denied = append(p.denied, denial{host: host, at: time.Now()})
	if len(p.denied) > maxDenials {
		p.denied = slices.Delete(p.denied, 0, len(p.denied)-maxDenials)
	}
}

// deniedSince returns the distinct hosts refused since t.
func (p *domainProxy) deniedSince(t time.Time) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var hosts []string
	for _, d := range p.denied {
		if !d.at.Before(t) && !slices.Contains(hosts, d.host) {
			hosts = append(hosts, d.host)
		}
	}
	return hosts
}

// dial connects to addr. A host name must resolve to an address other
// than a loopback, link-local or unspecified one or one of this machine,
// unless it is localhost; an IP address was allowed by a rule, which
// for a local address must name it explicitly.
func (p *domainProxy) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if _, err := netip.ParseAddr(host); err == nil {
		return p.dialer.DialContext(ctx, network, addr)
	}
	addrs, err := p.lookup(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	loopbackName := host == "localhost" || strings.HasSuffix(host, ".localhost")
	var errs []error
	for _, a := range addrs {
		a = a.Unmap()
		if !loopbackName && isLocalAddr(a) {
			errs = append(errs, fmt.Errorf("%s resolves to the local address %s", host, a))
			continue
		}
		c, err := p.dialer.DialContext(ctx, network, net.JoinHostPort(a.String(), port))
		if err == nil {
			return c, nil
		}
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return nil, fmt.Errorf("%s has no addresses", host)
	}
	return nil, errors.Join(errs...)
}

// isLocalAddr reports whether a is a loopback, link-local, unspecified or
// multicast address or one of this machine's interfaces.
func isLocalAddr(a netip.Addr) bool {
	if a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsUnspecified() || a.IsMulticast() {
		return true
	}
	ifaddrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, ia := range ifaddrs {
		if pfx, err := netip.ParsePrefix(ia.String()); err == nil && pfx.Addr().Unmap() == a {
			return true
		}
	}
	return false
}

// close stops the proxy and its tunnels.
func (p *domainProxy) close() error {
	p.mu.Lock()
	p.closed = true
	for c := range p.tunnels {
		c.Close()
	}
	p.mu.Unlock()
	err := p.srv.Close()
	if p.socket != "" {
		os.RemoveAll(filepath.Dir(p.socket))
	}
	return err
}
