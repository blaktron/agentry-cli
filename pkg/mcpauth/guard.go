package mcpauth

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// FetchTimeout bounds one request to a vendor: discovery, registration,
// the code exchange, a refresh or a revocation.
const FetchTimeout = 20 * time.Second

// ErrBlockedAddress is a vendor address that resolved to a non-public one.
type ErrBlockedAddress struct {
	Host string
	IP   string
}

func (e *ErrBlockedAddress) Error() string {
	return fmt.Sprintf("refused to reach %s: it resolves to %s, which is not a public address", e.Host, e.IP)
}

// Guard is the egress rule every fetch to a vendor goes through (plan
// mcp-oauth-connections.md §3 invariant 6): https only, each candidate
// address public unicast, the address dialled the one checked, no redirect
// followed and no proxy taken from the environment. It holds the core's
// webhook.Guard rules (agentry internal/webhook, T-08), which a module
// outside the core cannot import; guard_test.go carries its cases.
//
// AllowLoopback and TLS are test seams: a test's httptest server is on
// loopback with its own certificate. Production leaves both zero.
type Guard struct {
	AllowLoopback bool
	TLS           *tls.Config
	LookupNetIP   func(ctx context.Context, host string) ([]netip.Addr, error)
}

// IsAllowedIP reports whether ip is a public unicast address: not
// loopback, private, link-local (the cloud metadata address included),
// multicast, unspecified, 0.0.0.0/8 or carrier-grade NAT; IPv4-mapped
// addresses are unmapped and an IPv6 address embedding an IPv4 one (6to4,
// NAT64, IPv4-compatible) is judged by the address it embeds.
func (g Guard) IsAllowedIP(ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	ip = ip.Unmap()
	if v4, ok := embeddedIPv4(ip); ok {
		return g.IsAllowedIP(v4)
	}
	if ip.IsLoopback() {
		return g.AllowLoopback
	}
	if ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	if ip.Is4() {
		b := ip.As4()
		if b[0] == 0 {
			return false // 0.0.0.0/8, routed to loopback on Linux
		}
		if b[0] == 100 && b[1] >= 64 && b[1] <= 127 {
			return false // 100.64.0.0/10
		}
	}
	return ip.IsGlobalUnicast()
}

// embeddedIPv4 is the IPv4 address an IPv6 one routes to under 6to4, the
// NAT64 well-known prefix or the IPv4-compatible form; the NAT64 local-use
// prefix is reported as unspecified so it is refused.
func embeddedIPv4(ip netip.Addr) (netip.Addr, bool) {
	if !ip.Is6() || ip.Is4In6() {
		return netip.Addr{}, false
	}
	b := ip.As16()
	switch {
	case b[0] == 0x20 && b[1] == 0x02:
		return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), true
	case b[0] == 0 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b && b[4] == 0 && b[5] == 1:
		return netip.IPv4Unspecified(), true
	case b[0] == 0 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b && [8]byte(b[4:12]) == [8]byte{}:
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	case [12]byte(b[:12]) == [12]byte{} && !ip.IsUnspecified() && !ip.IsLoopback():
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	}
	return netip.Addr{}, false
}

// dialContext resolves the host, refuses the whole answer if any address
// in it is not public, and dials the first checked address itself, so a
// second DNS answer cannot change where the connection goes.
func (g Guard) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var candidates []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		candidates = []netip.Addr{ip}
	} else {
		lookup := g.LookupNetIP
		if lookup == nil {
			lookup = func(ctx context.Context, h string) ([]netip.Addr, error) {
				return net.DefaultResolver.LookupNetIP(ctx, "ip", h)
			}
		}
		if candidates, err = lookup(ctx, host); err != nil {
			return nil, err
		}
	}
	if len(candidates) == 0 {
		return nil, &ErrBlockedAddress{Host: host, IP: "no address"}
	}
	for _, ip := range candidates {
		if !g.IsAllowedIP(ip) {
			return nil, &ErrBlockedAddress{Host: host, IP: ip.String()}
		}
	}
	var d net.Dialer
	return d.DialContext(ctx, network, net.JoinHostPort(candidates[0].String(), port))
}

// Client is the HTTP client bound to the guard.
func (g Guard) Client() *http.Client {
	return &http.Client{
		Timeout: FetchTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &guardTransport{inner: &http.Transport{
			DialContext:           g.dialContext,
			TLSClientConfig:       g.TLS,
			TLSHandshakeTimeout:   FetchTimeout,
			ResponseHeaderTimeout: FetchTimeout,
			DisableKeepAlives:     true,
			Proxy:                 nil,
		}},
	}
}

// guardTransport refuses any request that is not https before it dials.
type guardTransport struct{ inner http.RoundTripper }

func (t *guardTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" {
		return nil, fmt.Errorf("refused to reach %s: a vendor is reached over https only", r.URL.Redacted())
	}
	return t.inner.RoundTrip(r)
}
