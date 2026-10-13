// Package netproxy is the llm-proxy of the agent-container topology
// (RUNNER-PLAN §R4): the run network is created --internal, so the ONLY
// way anything the agent container runs can reach the outside world is a
// CONNECT tunnel through this proxy — and the proxy tunnels only to the
// operator's model endpoints (policy.model.proxyAllow). Manifest egress
// hosts never appear here; those are tool-plane grants served by fetch.
//
// The proxy deliberately does not decrypt TLS (RUNNER-DEVIATIONS DR-6):
// the control is which host:port a tunnel may reach, not what crosses it.
// It reads one thing in clear, the ClientHello's server name, and refuses
// a tunnel whose name is not the host it was opened to (agentry-cli#616,
// sni.go), because a CDN front end routes by that name.
// One exception to "tunnels only": a plain-http model endpoint the
// operator chose (DR-45, agentry-cli#377) is served by forwarding the
// harness's absolute-form request, for the host:ports in PlainHTTP and
// nothing else.
// A harness that ignores HTTPS_PROXY fails closed — the internal network
// has no other route out — which is the acceptable failure direction.
//
// Part of the embedded runner-image build context (§R7): stdlib-only,
// enforced by a test in the parent package.
package netproxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
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
	// forwarded to (an entry without a port means 80): the operator's
	// http model endpoints, each also on Allow (DR-45). Empty — the zero
	// value, and every run whose model endpoints are https — means no
	// plaintext proxying at all.
	PlainHTTP []string
	// PublicOnly dials a permitted host only at a public address: the
	// proxy resolves the name itself, refuses the tunnel when any address
	// it resolves to is private, loopback, link-local, shared (CGNAT, the
	// tailnet's range) or otherwise not on the public internet, and dials
	// the address it checked, so a second answer cannot slip in between.
	// An input source's fetch step runs behind such a proxy (plan
	// input-sources.md §5, invariant 3; agentry-cli#833).
	PublicOnly bool
	// Resolver answers PublicOnly's lookups; nil is the system's.
	Resolver Resolver
	// Logw receives one line per decision (CONNECT, and plain http as
	// "allow http" / "deny http"); nil discards. The
	// engine points this at stderr, which lands in the run's receipts.
	Logw io.Writer

	logMu     sync.Mutex
	transport *http.Transport
	tOnce     sync.Once
}

// ListenAndServe runs the proxy on addr until the listener fails (the
// container's lifetime — the engine tears the proxy down with the run).
func (p *Proxy) ListenAndServe(addr string) error {
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
		// Plaintext proxying only toward an http model endpoint the
		// operator named (DR-45); anything else is refused as before.
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
	if !p.permits(host, port) {
		p.logf("deny %s:%s", host, port)
		http.Error(w, fmt.Sprintf("forbidden: %s:%s is not in the egress allowlist", host, port), http.StatusForbidden)
		return
	}
	dialHost := host
	if p.PublicOnly {
		ip, err := p.publicAddress(r.Context(), host)
		if err != nil {
			p.logf("deny %s:%s (%v)", host, port, err)
			http.Error(w, fmt.Sprintf("forbidden: %s: %v", host, err), http.StatusForbidden)
			return
		}
		dialHost = ip.String()
	}
	p.logf("allow %s:%s", host, port)
	upstream, err := net.DialTimeout("tcp", net.JoinHostPort(dialHost, port), 30*time.Second)
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
	// tunnel's host (agentry-cli#616); a plain-http model endpoint the
	// operator named (DR-45) is the one target that carries no TLS. The
	// upstream was dialled first so that an unreachable host is still a
	// 502, but it has been sent nothing.
	if !p.plainTarget(host, port) {
		hello, ok := p.checkClientHello(client, bufrw, host, port)
		if !ok {
			_ = client.Close()
			_ = upstream.Close()
			return
		}
		if _, err := upstream.Write(hello); err != nil {
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
	// Full duplex: the upstream transport may still be reading the request
	// body when the answer's first bytes are written back. Without it the
	// server closes that body at the first write, the transport fails its
	// next read and drops the upstream connection, and the answer is cut
	// after its first event (agentry-cli#756). The proxy's own HTTP/1
	// server supports it; a writer that does not is used as it is.
	rc := http.NewResponseController(w)
	_ = rc.EnableFullDuplex()
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Host = r.URL.Host
	out.Header = r.Header.Clone()
	dropHopHeaders(out.Header)
	resp, err := p.plainTransport().RoundTrip(out)
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

// notPublic are the ranges Go's net.IP methods leave out (the host's
// egress guard keeps its own list, pkg/mcpauth/guard.go): this network,
// shared address space (CGNAT, where a tailnet's addresses live), IETF
// protocol assignments, the documentation and benchmarking ranges, the
// reserved range, NAT64 and 6to4 (which embed an IPv4 address of any
// kind), Teredo, and IPv6 documentation.
var notPublic = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
		"64:ff9b::/96", "64:ff9b:1::/48", "2002::/16", "2001::/32", "2001:db8::/32",
	} {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		out = append(out, n)
	}
	return out
}()

// Public says whether an address is on the public internet.
func Public(ip net.IP) bool {
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || !ip.IsGlobalUnicast() {
		return false
	}
	for _, n := range notPublic {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

// plainTarget is true for a CONNECT to a plain-http model endpoint the
// operator named (DR-45), the one tunnel whose first bytes are not TLS.
// An input source's proxy (PublicOnly) has none.
func (p *Proxy) plainTarget(host, port string) bool {
	return !p.PublicOnly && permitsIn(p.PlainHTTP, "80", host, port)
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
