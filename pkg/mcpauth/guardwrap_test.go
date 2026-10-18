package mcpauth_test

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/egressguard"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
)

// TestTheGuardAnswersAsTheCoresGuardDoes: mcpauth.Guard adds no rule of its
// own to the egress guard it wraps, with or without the loopback seam.
func TestTheGuardAnswersAsTheCoresGuardDoes(t *testing.T) {
	for _, loopback := range []bool{false, true} {
		g := mcpauth.Guard{AllowLoopback: loopback}
		core := egressguard.Guard{AllowLoopback: loopback}
		for _, s := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "0.1.2.3", "224.0.0.1", "0.0.0.0",
			"::1", "fc00::1", "fe80::1", "::ffff:10.0.0.1", "2002:7f00:1::1", "64:ff9b::a00:1", "64:ff9b:1::1", "::10.0.0.1",
			"8.8.8.8", "2606:4700::1111", "::ffff:8.8.8.8"} {
			a := netip.MustParseAddr(s)
			if g.IsAllowedIP(a) != core.IsAllowedIP(a) {
				t.Errorf("loopback %v, %s: mcpauth %v, core %v", loopback, s, g.IsAllowedIP(a), core.IsAllowedIP(a))
			}
		}
	}
	if (mcpauth.Guard{}).IsAllowedIP(netip.Addr{}) {
		t.Error("the zero address is allowed")
	}
}

func TestTheGuardsClientHoldsTheFetchTimeout(t *testing.T) {
	if got := (mcpauth.Guard{}).Client().Timeout; got != mcpauth.FetchTimeout {
		t.Fatalf("timeout = %v, want %v", got, mcpauth.FetchTimeout)
	}
}

// TestTheGuardRefusesAnUnresolvedNameAndNamesTheHost: a name with no address
// is a refusal in the guard's own error type, carrying the host.
func TestTheGuardRefusesAnUnresolvedNameAndNamesTheHost(t *testing.T) {
	g := mcpauth.Guard{LookupNetIP: func(context.Context, string) ([]netip.Addr, error) { return nil, nil }}
	_, err := g.Client().Get("https://vendor.example/")
	var blocked *mcpauth.BlockedAddressError
	if !errors.As(err, &blocked) || blocked.Host != "vendor.example" {
		t.Fatalf("err = %v, want a blocked address naming vendor.example", err)
	}
	var core *egressguard.BlockedAddressError
	if !errors.As(err, &core) {
		t.Fatalf("err = %v, want the core's blocked address error", err)
	}
	if !strings.Contains(err.Error(), "vendor.example") {
		t.Fatalf("err = %v does not name the host", err)
	}
}
