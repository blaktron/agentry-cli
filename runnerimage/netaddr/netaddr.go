// Package netaddr is the runner's one rule for a public address:
// the ranges web_read, the input-source proxy and the browse proxy never
// connect to, among them the 6to4 relay anycast range, Azure's
// wireserver, the IPv6 discard prefix and site-local space.
//
// Part of the embedded runner-image build context: stdlib-only.
package netaddr

import "net/netip"

// blocked are the ranges no runner connection goes to beside those Go's
// netip methods name: this network, private and shared address space
// (CGNAT, where a tailnet's addresses live), link-local (where cloud
// metadata answers), IETF protocol assignments, documentation,
// benchmarking, the 6to4 relay anycast range, multicast and reserved
// space, Azure's wireserver, and the IPv6 transition ranges that embed an
// IPv4 address the rule would otherwise have to decode.
var blocked = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		// Azure's wireserver and metadata host sits in public space.
		"168.63.129.16/32",
		"::/128", "::1/128", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/32", "2001:db8::/32",
		"2002::/16", "fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8",
	} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

// Public says whether a is an address on the public internet.
func Public(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() || !a.IsGlobalUnicast() {
		return false
	}
	for _, p := range blocked {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// Network says whether a is an address a person may have named as their own
// network: a public one, or one in private space (10/8, 172.16/12, 192.168/16,
// fc00::/7), shared space (100.64/10, where a tailnet's addresses live) or
// loopback. It is wider than Public by exactly those ranges. Link-local space
// stays out (169.254/16 and fe80::/10, where cloud metadata answers), as does
// everything else Public refuses (unspecified, multicast, documentation,
// reserved, transition ranges that embed another address, and Azure's
// wireserver).
func Network(a netip.Addr) bool {
	a = a.Unmap()
	if Public(a) {
		return true
	}
	if !a.IsValid() {
		return false
	}
	if cloudMetadataV6.Contains(a) {
		return false
	}
	return a.IsLoopback() || a.IsPrivate() || sharedSpace.Contains(a)
}

// cloudMetadataV6 is AWS's IPv6 metadata range, which sits inside the unique
// local space Network otherwise admits.
var cloudMetadataV6 = netip.MustParsePrefix("fd00:ec2::/32")

var sharedSpace = netip.MustParsePrefix("100.64.0.0/10")
