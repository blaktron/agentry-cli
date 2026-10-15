package targethost

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// Refusal is a target host the run will not reach, with the receipt's
// detail for why (receipt.ReasonHostRefused).
type Refusal struct {
	ID     string
	Detail string
	Err    error
}

func (r *Refusal) Error() string { return fmt.Sprintf("target host %s: %v", r.ID, r.Err) }
func (r *Refusal) Unwrap() error { return r.Err }

// The details of a refusal: the receipt's own words (receipt.Host*).
const (
	RefusedAddress    = receipt.HostAddress
	RefusedKeyUnknown = receipt.HostKeyUnknown
	RefusedKeyChanged = receipt.HostKeyChanged
	RefusedCredential = receipt.HostCredential
	RefusedUnreached  = receipt.HostUnreachable
)

func refuse(id, detail string, format string, args ...any) *Refusal {
	return &Refusal{ID: id, Detail: detail, Err: fmt.Errorf(format, args...)}
}

// lookupIP resolves a name with this machine's own resolver: a Tailscale, mDNS
// or split-DNS name resolves as the person's own ssh would resolve it, which
// docker's embedded DNS would not. A variable for tests.
var lookupIP = func(ctx context.Context, host string) ([]netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// ResolveAddress is the IPv4 address a run reaches a target host at: the
// host if it is an address, else the first IPv4 answer this machine's
// resolver gives for it. It refuses what a target host is never
// (loopback, link-local, an unspecified, multicast or broadcast address),
// what the policy's hosts lists exclude, and a host with no IPv4
// address: the relay that reaches it is on docker's IPv4 network.
func ResolveAddress(ctx context.Context, id string, a Answer, rule policy.HostsPolicy) (netip.Addr, error) {
	var addrs []netip.Addr
	if ip, err := netip.ParseAddr(a.Host); err == nil {
		addrs = []netip.Addr{ip}
	} else {
		got, err := lookupIP(ctx, a.Host)
		if err != nil {
			var dnsErr *net.DNSError
			if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
				return netip.Addr{}, refuse(id, RefusedAddress, "%s does not resolve on this machine", a.Host)
			}
			return netip.Addr{}, refuse(id, RefusedAddress, "resolve %s: %v", a.Host, err)
		}
		addrs = got
	}
	var v4 netip.Addr
	for _, ip := range addrs {
		if ip = ip.Unmap(); ip.Is4() {
			v4 = ip
			break
		}
	}
	if !v4.IsValid() {
		return netip.Addr{}, refuse(id, RefusedAddress, "%s has no IPv4 address; a run reaches a target host over IPv4 only", a.Host)
	}
	if err := CheckAddress(v4); err != nil {
		return netip.Addr{}, refuse(id, RefusedAddress, "%s is %v", a.Host, err)
	}
	if ok, why := rule.Admits(v4); !ok {
		return netip.Addr{}, refuse(id, RefusedAddress, "%s: %s", a.Host, why)
	}
	return v4, nil
}

// thisNetwork is 0.0.0.0/8, which no host is reached at.
var thisNetwork = netip.MustParsePrefix("0.0.0.0/8")

// CheckAddress refuses an address a target host is never at: this
// machine's own loopback, a link-local address (a cloud's metadata service
// among them), and addresses that are no one host. Private, LAN and
// Tailscale addresses are reached.
func CheckAddress(a netip.Addr) error {
	a = a.Unmap()
	switch {
	case a.IsLoopback():
		return errors.New("a loopback address, this machine's own: a target host is another machine")
	case a.IsLinkLocalUnicast(), a.IsLinkLocalMulticast():
		return errors.New("a link-local address, which a run never reaches")
	case a.IsUnspecified(), a.IsMulticast(), a.IsInterfaceLocalMulticast(), a == netip.AddrFrom4([4]byte{255, 255, 255, 255}),
		a.Is4() && thisNetwork.Contains(a):
		return errors.New("not the address of one host")
	}
	return nil
}
