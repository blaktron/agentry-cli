package mcpauth

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/netip"
	"time"

	"github.com/blaktron/agentry/pkg/egressguard"
)

// FetchTimeout bounds one request to a vendor: discovery, registration,
// the code exchange, a refresh or a revocation.
const FetchTimeout = 20 * time.Second

// BlockedAddressError is a vendor address that resolved to a non-public
// one, or to none.
type BlockedAddressError = egressguard.BlockedAddressError

// Guard is the egress rule every fetch to a vendor goes through: https only,
// each candidate address public unicast, the address dialled the one checked,
// no redirect followed and no proxy taken from the environment. The rule is
// the registry's egress guard (package egressguard); Guard fixes its timeout
// at FetchTimeout and its scheme at https, and leaves no way to name a proxy.
//
// AllowLoopback, TLS and LookupNetIP are test seams: a test's httptest server
// is on loopback with its own certificate, and a lookup answers a name with
// an address of the test's choosing. Production leaves all three zero.
type Guard struct {
	AllowLoopback bool
	TLS           *tls.Config
	LookupNetIP   func(ctx context.Context, host string) ([]netip.Addr, error)
}

func (g Guard) guard() egressguard.Guard {
	return egressguard.Guard{
		AllowLoopback: g.AllowLoopback,
		LookupNetIP:   g.LookupNetIP,
		TLS:           g.TLS,
		Timeout:       FetchTimeout,
		HTTPSOnly:     true,
	}
}

// IsAllowedIP reports whether ip is a public unicast address: not loopback,
// private, link-local (the cloud metadata address included), multicast,
// unspecified, 0.0.0.0/8 or carrier-grade NAT; IPv4-mapped addresses are
// unmapped and an IPv6 address embedding an IPv4 one (6to4, NAT64,
// IPv4-compatible) is judged by the address it embeds.
func (g Guard) IsAllowedIP(ip netip.Addr) bool { return g.guard().IsAllowedIP(ip) }

// Client is the HTTP client bound to the guard.
func (g Guard) Client() *http.Client { return g.guard().Client() }
