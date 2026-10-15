// Package netproxy is the llm-proxy of the agent-container topology:
// the run network is created --internal, so the ONLY
// way anything the agent container runs can reach the outside world is a
// CONNECT tunnel through this proxy — and the proxy tunnels only to the
// operator's model endpoints (policy.model.proxyAllow). Manifest egress
// hosts never appear here; those are tool-plane grants served by fetch.
//
// The proxy deliberately does not decrypt TLS:
// the control is which host:port a tunnel may reach, not what crosses it.
// It reads one thing in clear, the ClientHello's server name, and refuses
// a tunnel whose name is not the host it was opened to
// (sni.go), because a CDN front end routes by that name.
// One exception to "tunnels only": a plain-http model endpoint the
// policy names is served by forwarding the
// harness's absolute-form request, for the host:ports in PlainHTTP and
// nothing else.
// A harness that ignores HTTPS_PROXY fails closed — the internal network
// has no other route out — which is the acceptable failure direction.
//
// Part of the embedded runner-image build context: stdlib-only,
// enforced by a test in the parent package.
package netproxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/blaktron/agentry-cli/runnerimage/netaddr"
)

// Proxy is an allowlisting CONNECT proxy. The zero value denies
// everything, which is the right default for a security device.
type Proxy struct {
	// Allow lists permitted CONNECT targets as host[:port]. An entry
	// without a port means 443 — model endpoints are TLS, so that is the
	// only sensible shorthand. Hosts compare case-insensitively and
	// exactly; no wildcards, because a wildcard here would quietly widen
	// the one hole in the agent's network isolation.
	Allow []string
	// PlainHTTP lists the host[:port] targets a plain-http request may be
	// forwarded to (an entry without a port means 80): the policy's
	// http model endpoints, each also on Allow. Empty — the zero
	// value, and every run whose model endpoints are https — means no
	// plaintext proxying at all.
	PlainHTTP []string
	// PublicOnly dials a permitted host only at a public address: the
	// proxy resolves the name itself, refuses the tunnel when any address
	// it resolves to is private, loopback, link-local, shared (CGNAT, the
	// tailnet's range) or otherwise not on the public internet, and dials
	// the address it checked, so a second answer cannot slip in between.
	// An input source's fetch step runs behind such a proxy.
	PublicOnly bool
	// Scope, where set, makes this a browse proxy (browse.go):
	// CONNECT and plain http are judged by the scope,
	// every name is resolved here and only the address checked is
	// dialled, and Allow, PlainHTTP and PublicOnly are not read.
	Scope *Scope
	// Resolver answers PublicOnly's and Scope's lookups; nil is the system's.
	Resolver Resolver
	// Logw receives one line per decision (CONNECT, and plain http as
	// "allow http" / "deny http"); nil discards. The
	// engine points this at stderr, which lands in the run's receipts.
	Logw io.Writer

	// dial opens an upstream connection to the address judged; nil is a
	// plain TCP dial. A test stands in for the internet with it.
	dial func(ctx context.Context, address string) (net.Conn, error)

	logMu     sync.Mutex
	transport *http.Transport
	tOnce     sync.Once
}

// ListenAndServe runs the proxy on addr until the listener fails (the
// container's lifetime — the engine tears the proxy down with the run).
func (p *Proxy) ListenAndServe(addr string) error {
	if p.Scope != nil {
		p.logf("browse-proxy listening on %s (sites: %s; hosts: %s; public: %v; private sites: %s)", addr,
			strings.Join(p.Scope.Sites, ", "), strings.Join(p.Scope.Hosts, ", "), p.Scope.Public, strings.Join(p.Scope.Private, ", "))
		return (&http.Server{Addr: addr, Handler: p.Handler()}).ListenAndServe()
	}
	p.logf("llm-proxy listening on %s (allow: %s)", addr, strings.Join(p.Allow, ", "))
	if len(p.PlainHTTP) > 0 {
		p.logf("llm-proxy forwards plain http to: %s", strings.Join(p.PlainHTTP, ", "))
	}
	srv := &http.Server{Addr: addr, Handler: p.Handler()}
	return srv.ListenAndServe()
}

// Handler exposes the proxy as an http.Handler so tests can drive it on
// a listener they control.
func (p *Proxy) Handler() http.Handler {
	return http.HandlerFunc(p.serveHTTP)
}

func (p *Proxy) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		// A browse proxy forwards a page's plain http under its scope.
		if p.Scope != nil {
			if r.URL.IsAbs() && r.URL.Scheme == "http" {
				p.forwardScoped(w, r)
				return
			}
			p.denyBrowse(w, "", r.Host, "", "not a proxy request")
			return
		}
		// Plaintext proxying only toward an http model endpoint the
		// policy names; anything else is refused as before.
		if r.URL.IsAbs() && r.URL.Scheme == "http" && len(p.PlainHTTP) > 0 && !p.PublicOnly {
			p.forwardPlain(w, r)
			return
		}
		http.Error(w, "method not allowed: this proxy accepts CONNECT tunnels only", http.StatusMethodNotAllowed)
		return
	}
	target := r.Host
	if target == "" {
		target = r.URL.Host
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		http.Error(w, fmt.Sprintf("malformed CONNECT target %q (want host:port)", target), http.StatusBadRequest)
		return
	}
	dialHost := host
	switch {
	case p.Scope != nil:
		a, why := p.scopedAddress(r.Context(), host, port)
		if why != "" {
			p.denyBrowse(w, "", host, port, why)
			return
		}
		dialHost = a.String()
	case !p.permits(host, port):
		p.logf("deny %s:%s", host, port)
		http.Error(w, fmt.Sprintf("forbidden: %s:%s is not in the egress allowlist", host, port), http.StatusForbidden)
		return
	case p.PublicOnly:
		ip, err := p.publicAddress(r.Context(), host)
		if err != nil {
			p.logf("deny %s:%s (%v)", host, port, err)
			http.Error(w, fmt.Sprintf("forbidden: %s: %v", host, err), http.StatusForbidden)
			return
		}
		dialHost = ip.String()
	}
	p.logf("allow %s:%s", host, port)
	upstream, err := p.dialUpstream(r.Context(), net.JoinHostPort(dialHost, port))
	if err != nil {
		http.Error(w, fmt.Sprintf("bad gateway: dial %s: %v", target, err), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "cannot hijack client connection", http.StatusInternalServerError)
		return
	}
	client, bufrw, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		p.logf("hijack failed: %v", err)
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = client.Close()
		_ = upstream.Close()
		return
	}
	// The client side reads through bufrw because the hijacked buffer may
	// already hold bytes the client pipelined behind its CONNECT. Before
	// any of them goes upstream, the ClientHello's server name must be the
	// tunnel's host; a plain-http model endpoint the
	// policy names is the one target that carries no TLS. The
	// upstream was dialled first so that an unreachable host is still a
	// 502, but it has been sent nothing.
	if !p.plainTarget(host, port) {
		hello, ok := p.checkClientHello(client, bufrw, host, port)
		if ok {
			_, err = upstream.Write(hello)
		}
		if !ok || err != nil {
			_ = client.Close()
			_ = upstream.Close()
			return
		}
	}
	// Pure byte relay from here: no buffering beyond io.Copy. When
	// either half closes, both sides are torn down — a half-open tunnel
	// serves no one and leaks two sockets.
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, bufrw)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		done <- struct{}{}
	}()
	<-done
	_ = client.Close()
	_ = upstream.Close()
}

// dialUpstream opens the connection to the address the proxy judged.
func (p *Proxy) dialUpstream(ctx context.Context, address string) (net.Conn, error) {
	if p.dial != nil {
		return p.dial(ctx, address)
	}
	return (&net.Dialer{Timeout: 30 * time.Second}).DialContext(ctx, "tcp", address)
}

// hopHeaders are the connection-scoped headers a proxy must not pass on
// (RFC 9110 §7.6.1), Proxy-Authorization and Proxy-Connection included.
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func dropHopHeaders(h http.Header) {
	for _, f := range h.Values("Connection") {
		for _, name := range strings.Split(f, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopHeaders {
		h.Del(name)
	}
}

// forwardPlain relays one absolute-form http request to a PlainHTTP
// target. The upstream connection goes to the URL's host:port, which is
// what the allowlist judged, and the Host header is set to it, so a
// request cannot name one host to the check and another to the server.
// No redirect is followed (the transport does not follow, and the 30x
// goes back to the harness as it came); the body streams both ways, and
// the response is flushed as it arrives, since a model streams its
// answer.
func (p *Proxy) forwardPlain(w http.ResponseWriter, r *http.Request) {
	host, port := r.URL.Hostname(), r.URL.Port()
	if port == "" {
		port = "80"
	}
	// On PlainHTTP and on the CONNECT allowlist both: the plain list is
	// never wider than what a tunnel could reach, whoever built it.
	if host == "" || !permitsIn(p.PlainHTTP, "80", host, port) || !p.permits(host, port) {
		p.logf("deny http %s:%s", host, port)
		http.Error(w, fmt.Sprintf("forbidden: http://%s:%s is not a plain-http model endpoint of this run", host, port), http.StatusForbidden)
		return
	}
	p.logf("allow http %s:%s", host, port)
	p.relayPlain(w, r, p.plainTransport(), host, port)
}

// relayPlain sends one absolute-form request upstream through t and
// streams the answer back.
func (p *Proxy) relayPlain(w http.ResponseWriter, r *http.Request, t *http.Transport, host, port string) {
	// Full duplex: the upstream transport may still be reading the request
	// body when the answer's first bytes are written back. Without it the
	// server closes that body at the first write, the transport fails its
	// next read and drops the upstream connection, and the answer is cut
	// after its first event. The proxy's own HTTP/1
	// server supports it; a writer that does not is used as it is.
	rc := http.NewResponseController(w)
	_ = rc.EnableFullDuplex()
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Host = r.URL.Host
	out.Header = r.Header.Clone()
	dropHopHeaders(out.Header)
	resp, err := t.RoundTrip(out)
	if err != nil {
		http.Error(w, fmt.Sprintf("bad gateway: http://%s:%s: %v", host, port, err), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	dropHopHeaders(resp.Header)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	buf := make([]byte, 32<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			_ = rc.Flush()
		}
		if rerr == io.EOF {
			return
		}
		if rerr != nil {
			// A stream cut upstream must not reach the harness as a
			// clean end: abort, so the client sees a reset connection
			// rather than a closing chunk on half an answer.
			p.logf("upstream http %s:%s: %v", host, port, rerr)
			panic(http.ErrAbortHandler)
		}
	}
}

// plainTransport is the one upstream transport for plain forwarding: no
// proxy of its own, no transparent decompression (the harness asked for
// the encoding it can read), and no HTTP/2 over cleartext.
func (p *Proxy) plainTransport() *http.Transport {
	p.tOnce.Do(func() {
		p.transport = &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 30 * time.Second}).DialContext(ctx, network, addr)
			},
			// No response-header timeout: a slow model may think for
			// minutes before its first byte, as it may through a
			// tunnel; the request's context (the harness hanging up,
			// the run's own timeout) bounds it.
			DisableCompression: true,
			IdleConnTimeout:    90 * time.Second,
		}
	})
	return p.transport
}

// Resolver looks a name up; *net.Resolver is one.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// publicAddress resolves host and answers its first address, refusing the
// host when any address is not public.
func (p *Proxy) publicAddress(ctx context.Context, host string) (net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if !Public(ip) {
			return nil, fmt.Errorf("%s is not a public address", ip)
		}
		return ip, nil
	}
	var res Resolver = net.DefaultResolver
	if p.Resolver != nil {
		res = p.Resolver
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	addrs, err := res.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve: %w", err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("it resolves to no address")
	}
	for _, a := range addrs {
		if !Public(a.IP) {
			return nil, fmt.Errorf("it resolves to %s, which is not a public address", a.IP)
		}
	}
	return addrs[0].IP, nil
}

// Public says whether an address is on the public internet: the runner's
// one list (netaddr), shared with web_read.
func Public(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	return ok && netaddr.Public(a)
}

// plainTarget is true for a CONNECT to a plain-http model endpoint the
// policy names, the one tunnel whose first bytes are not TLS.
// An input source's proxy (PublicOnly) has none.
func (p *Proxy) plainTarget(host, port string) bool {
	return !p.PublicOnly && p.Scope == nil && permitsIn(p.PlainHTTP, "80", host, port)
}

// permits applies the allowlist to one host:port pair.
func (p *Proxy) permits(host, port string) bool {
	return permitsIn(p.Allow, "443", host, port)
}

// permitsIn is true when list names host:port exactly, an entry without
// a port meaning defPort. Hosts compare case-insensitively in ASCII only
// (asciiEqualFold), so no Unicode folding widens an entry.
func permitsIn(list []string, defPort, host, port string) bool {
	for _, entry := range list {
		eh, ep := splitEntry(entry, defPort)
		if asciiEqualFold(eh, host) && ep == port {
			return true
		}
	}
	return false
}

// asciiEqualFold compares two names case-insensitively in ASCII alone.
// strings.EqualFold applies Unicode folding, under which "ſ" (U+017F)
// matches "s", so "api.oſv.dev" would pass for "api.osv.dev".
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// splitEntry parses one list entry, defaulting the port to defPort.
func splitEntry(entry, defPort string) (host, port string) {
	entry = strings.TrimSpace(entry)
	if h, p, err := net.SplitHostPort(entry); err == nil {
		return h, p
	}
	return entry, defPort
}

// logf serializes decision lines: handlers run concurrently and receipts
// are only useful when each line is whole.
func (p *Proxy) logf(format string, args ...any) {
	if p.Logw == nil {
		return
	}
	p.logMu.Lock()
	defer p.logMu.Unlock()
	_, _ = fmt.Fprintf(p.Logw, format+"\n", args...)
}

// splitAllowEntry parses one CONNECT allowlist entry, defaulting the port
// to 443.
func splitAllowEntry(entry string) (host, port string) { return splitEntry(entry, "443") }
