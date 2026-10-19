package runnerimage

import (
	"fmt"
	"net/netip"
	"testing"

	"github.com/blaktron/agentry/pkg/egressguard"

	"github.com/blaktron/agentry-cli/runnerimage/netaddr"
)

// The runner image compiles from stdlib-only embedded sources with no module
// download, so its tools server and proxies cannot import the core's egress
// guard and keep their own address rule, netaddr.Public. These tests hold
// that rule to the core's: web_read, the input-source proxy and the browse
// proxy never reach an address the core's guard refuses.

// refusedByTheCore are addresses the core's guard refuses; netaddr.Public
// must refuse every one.
var refusedByTheCore = []string{
	"127.0.0.1", "127.255.255.254", "10.0.0.1", "172.16.0.1", "172.31.255.255", "192.168.1.1",
	"169.254.169.254", "169.254.0.1", "0.1.2.3", "0.0.0.0", "100.64.0.1", "100.127.255.254",
	"224.0.0.1", "239.255.255.250", "255.255.255.255",
	"::", "::1", "fc00::1", "fd12::1", "fe80::1", "ff02::1", "ff01::1",
	"::ffff:10.0.0.1", "::ffff:127.0.0.1", "::ffff:169.254.169.254",
	"2002:7f00:1::1", "2002:a00:1::1", "64:ff9b::a00:1", "64:ff9b::a9fe:a9fe", "64:ff9b:1::1",
	"::127.0.0.1", "::10.0.0.1",
	// the special-purpose ranges that are not globally reachable: the IETF
	// protocol assignments, documentation, the 6to4 relay anycast range,
	// benchmarking, reserved space, Teredo and the other IPv6 assignments,
	// the IPv6 documentation, discard and site-local prefixes
	"192.0.0.8", "192.0.2.1", "192.88.99.1", "198.18.0.1", "198.19.255.255", "198.51.100.7", "203.0.113.9",
	"240.0.0.1",
	"2001::1", "2001:db8::1", "100::1", "fec0::1",
}

// publicToBoth are public addresses both rules admit.
var publicToBoth = []string{"1.1.1.1", "8.8.8.8", "93.184.216.34", "100.128.0.1", "172.32.0.1", "2606:4700::1111", "::ffff:8.8.8.8"}

// refusedOnlyByTheImage is the stricter side: addresses the core's guard
// admits and netaddr.Public refuses. Azure's wireserver is a globally
// routable address that only means something from inside an Azure VM, so the
// core's guard, which follows the special-purpose registries, admits it.
var refusedOnlyByTheImage = []string{
	"168.63.129.16",
}

func TestTheImageRuleRefusesWhatTheCoreRefuses(t *testing.T) {
	core := egressguard.Guard{}
	for _, s := range refusedByTheCore {
		a := netip.MustParseAddr(s)
		if core.IsAllowedIP(a) {
			t.Errorf("%s: the core's guard allows it; move it out of the shared list", s)
		}
		if netaddr.Public(a) {
			t.Errorf("%s: the core's guard refuses it and the runner image's rule allows it", s)
		}
	}
	for _, s := range publicToBoth {
		a := netip.MustParseAddr(s)
		if !core.IsAllowedIP(a) || !netaddr.Public(a) {
			t.Errorf("%s: core allows %v, image allows %v; both should", s, core.IsAllowedIP(a), netaddr.Public(a))
		}
	}
}

func TestTheImageRuleIsStricterOnlyWhereListed(t *testing.T) {
	core := egressguard.Guard{}
	for _, s := range refusedOnlyByTheImage {
		a := netip.MustParseAddr(s)
		if !core.IsAllowedIP(a) {
			t.Errorf("%s: the core's guard refuses it now; move it to the shared list", s)
		}
		if netaddr.Public(a) {
			t.Errorf("%s: the runner image's rule allows it", s)
		}
	}
}

// TestNoAddressTheCoreRefusesReachesTheImage sweeps the first two octets of
// IPv4 space, bare and wrapped in each IPv6 form that embeds one (mapped,
// 6to4, NAT64, compatible, IPv4-translated and ISATAP), and requires that
// nothing the core refuses is public to the image.
func TestNoAddressTheCoreRefusesReachesTheImage(t *testing.T) {
	core := egressguard.Guard{}
	forms := []string{
		"%[1]s", "::ffff:%[1]s", "2002:%[2]s::1", "64:ff9b::%[1]s", "::%[1]s",
		// IPv4-translated, and ISATAP identifiers (local and universal) under
		// a public prefix
		"::ffff:0:%[1]s", "2606:4700::5efe:%[1]s", "2606:4700::200:5efe:%[1]s",
	}
	for a := 0; a < 256; a++ {
		for b := 0; b < 256; b++ {
			for _, tail := range [...][2]int{{0, 1}, {255, 254}, {128, 7}} {
				v4 := fmt.Sprintf("%d.%d.%d.%d", a, b, tail[0], tail[1])
				hex := fmt.Sprintf("%02x%02x:%02x%02x", a, b, tail[0], tail[1])
				for _, f := range forms {
					s := fmt.Sprintf(f, v4, hex)
					ip, err := netip.ParseAddr(s)
					if err != nil {
						t.Fatalf("%s: %v", s, err)
					}
					if !core.IsAllowedIP(ip) && netaddr.Public(ip) {
						t.Fatalf("%s: the core's guard refuses it and the runner image's rule allows it", s)
					}
				}
			}
		}
	}
}
