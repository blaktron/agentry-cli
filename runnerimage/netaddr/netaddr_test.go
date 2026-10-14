package netaddr

import (
	"net/netip"
	"testing"
)

// TestPublic holds the one list: each range the runner refuses, and the
// public addresses beside them that it reaches.
func TestPublic(t *testing.T) {
	for _, a := range []string{
		"0.1.2.3", "10.1.2.3", "100.64.0.1", "100.127.255.254", "127.0.0.1", "169.254.169.254", "172.16.5.4", "172.31.255.255",
		"192.0.0.8", "192.0.2.1", "192.88.99.1", "192.168.1.1", "198.18.0.1", "198.19.255.255", "198.51.100.7", "203.0.113.9",
		"224.0.0.1", "239.255.255.250", "240.0.0.1", "255.255.255.255", "168.63.129.16",
		"::", "::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1", "64:ff9b::a00:1", "64:ff9b:1::1", "100::1", "2001::1", "2001:db8::1",
		"2002:a00:1::1", "fc00::1", "fd12::1", "fe80::1", "fec0::1", "ff02::1",
	} {
		if Public(netip.MustParseAddr(a)) {
			t.Errorf("%s reads as public", a)
		}
	}
	for _, a := range []string{"1.1.1.1", "8.8.8.8", "93.184.216.34", "100.128.0.1", "172.32.0.1", "168.63.129.17", "2606:4700::1111", "::ffff:8.8.8.8"} {
		if !Public(netip.MustParseAddr(a)) {
			t.Errorf("%s reads as not public", a)
		}
	}
	if Public(netip.Addr{}) {
		t.Error("the zero address reads as public")
	}
}
