package policy

import (
	"net/netip"
	"strings"
	"testing"
)

// TestTheHostlinkImageIsPinnedByDigest holds the host link's base to the
// files image's rule: the default is pinned and resolved at use, and an
// operator's value must be pinned too.
func TestTheHostlinkImageIsPinnedByDigest(t *testing.T) {
	if !PinnedByDigest(DefaultHostlinkImage) {
		t.Fatalf("DefaultHostlinkImage %q is not pinned by digest", DefaultHostlinkImage)
	}
	p := Default()
	if p.Images.Hostlink != "" || p.Images.HostlinkImage() != DefaultHostlinkImage {
		t.Fatalf("images.hostlink %q, resolved %q", p.Images.Hostlink, p.Images.HostlinkImage())
	}
	p.Images.Hostlink = "registry.example/hostlink:1@sha256:" + strings.Repeat("a", 64)
	if err := p.Validate(); err != nil || p.Images.HostlinkImage() != p.Images.Hostlink {
		t.Fatalf("a digest-pinned images.hostlink: %v", err)
	}
	p.Images.Hostlink = "registry.example/hostlink:1"
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "images.hostlink") {
		t.Errorf("a tag-only images.hostlink was accepted (%v)", err)
	}
}

func TestTheHostsListsNarrowTheAddresses(t *testing.T) {
	h := HostsPolicy{Allow: []string{"192.168.1.0/24", "100.64.0.0/10"}, Deny: []string{"192.168.1.128/25"}}
	p := Default()
	p.Hosts = h
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	for addr, want := range map[string]bool{
		"192.168.1.10": true, "100.101.102.103": true, "192.168.1.200": false, "203.0.113.4": false,
	} {
		if ok, why := h.Admits(netip.MustParseAddr(addr)); ok != want || (!ok && why == "") {
			t.Errorf("Admits(%s) = %v %q, want %v", addr, ok, why, want)
		}
	}
	if ok, _ := (HostsPolicy{}).Admits(netip.MustParseAddr("10.1.2.3")); !ok {
		t.Error("no lists refused an address")
	}
	for _, bad := range [][]string{{"192.168.1.1/24"}, {"10.0.0.0"}, {"fd00::/8"}, {"x"}} {
		p.Hosts = HostsPolicy{Deny: bad}
		if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "hosts.deny") {
			t.Errorf("hosts.deny %q was accepted (%v)", bad, err)
		}
	}
}
