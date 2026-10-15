package policy

import (
	"fmt"
	"net/netip"
)

// HostsPolicy is the operator's rule for target hosts: IPv4 networks in CIDR
// form. Neither set, any address is reached but the ones a target host never
// is (loopback, link-local; targethost.CheckAddress).
type HostsPolicy struct {
	// Allow, when set, is every network a target host's address may be in.
	Allow []string `yaml:"allow,omitempty"`
	// Deny is the networks a target host's address may never be in; it
	// wins over Allow.
	Deny []string `yaml:"deny,omitempty"`
}

func (h HostsPolicy) validate() error {
	for _, l := range []struct {
		name string
		v    []string
	}{{"hosts.allow", h.Allow}, {"hosts.deny", h.Deny}} {
		for _, c := range l.v {
			p, err := netip.ParsePrefix(c)
			if err != nil || !p.Addr().Is4() || p.Masked() != p {
				return fmt.Errorf("%s %q must be an IPv4 network in CIDR form, as 192.168.1.0/24", l.name, c)
			}
		}
	}
	return nil
}

// Admits says whether the policy lets a run reach a, and why not when it
// does not.
func (h HostsPolicy) Admits(a netip.Addr) (bool, string) {
	a = a.Unmap()
	for _, c := range h.Deny {
		if p, err := netip.ParsePrefix(c); err == nil && p.Contains(a) {
			return false, fmt.Sprintf("%s is in %s, which this machine's policy denies (hosts.deny)", a, c)
		}
	}
	if len(h.Allow) == 0 {
		return true, ""
	}
	for _, c := range h.Allow {
		if p, err := netip.ParsePrefix(c); err == nil && p.Contains(a) {
			return true, ""
		}
	}
	return false, fmt.Sprintf("%s is in none of the networks this machine's policy allows (hosts.allow)", a)
}
