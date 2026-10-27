package engine

import (
	"context"
	"errors"
	"net"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/hostpin"
)

// fakeLookup answers from a table for the length of the test; a name not
// in it does not resolve.
func fakeLookup(t *testing.T, table map[string][]string) *[]string {
	t.Helper()
	var asked []string
	old := lookupModelHost
	lookupModelHost = func(_ context.Context, host string) ([]net.IP, error) {
		asked = append(asked, host)
		addrs, ok := table[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		var ips []net.IP
		for _, a := range addrs {
			ips = append(ips, net.ParseIP(a))
		}
		return ips, nil
	}
	oldName := thisHostname
	thisHostname = func() (string, error) { return "workmac.example.org", nil }
	t.Cleanup(func() { lookupModelHost, thisHostname = old, oldName })
	return &asked
}

func TestModelHostPinsWhatThisMachineResolves(t *testing.T) {
	asked := fakeLookup(t, map[string][]string{
		"macmini":          {"100.64.0.7", "fd7a:115c:a1e0::1"},
		"studio.local":     {"192.168.1.20", "192.168.1.20"},
		"api.example.com":  {"203.0.113.7"},
		"localhost":        {"127.0.0.1", "::1"},
		"zero.example.com": {"0.0.0.0"},
	})
	allow := []string{
		"macmini:8000", "MacMini:8001", "studio.local:11434", "api.example.com",
		"localhost:8080", "zero.example.com:443", "10.0.0.5:8000", "[fd00::1]:8000", "nowhere.invalid:443",
		"workmac.local:8000",
	}
	got := modelHosts(context.Background(), allow, false).Pins
	want := []string{"api.example.com:203.0.113.7", "macmini:100.64.0.7", "studio.local:192.168.1.20"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("hosted pins = %q, want %q", got, want)
	}
	// A local run also reaches this computer: localhost, and its own name
	// though the lookup failed.
	got = modelHosts(context.Background(), allow, true).Pins
	want = []string{"api.example.com:203.0.113.7", "localhost:host-gateway", "macmini:100.64.0.7", "studio.local:192.168.1.20", "workmac.local:host-gateway"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("local pins = %q, want %q", got, want)
	}
	// IP literals are never looked up, and each name once.
	for _, h := range *asked {
		if net.ParseIP(h) != nil {
			t.Errorf("looked up the address %q", h)
		}
	}
	if n := strings.Count(strings.Join(*asked, " "), "macmini"); n != 2 {
		t.Errorf("macmini looked up %d times, want once a resolution (asked %q)", n, *asked)
	}
	if slices.Contains(*asked, "localhost") && strings.Count(strings.Join(*asked, " "), "localhost") != 1 {
		t.Errorf("a local run looked localhost up (asked %q)", *asked)
	}
}

func TestModelHostsLine(t *testing.T) {
	for _, c := range []struct {
		mh   hostpin.ModelHosts
		want string
	}{
		{hostpin.ModelHosts{}, ""},
		{hostpin.ModelHosts{Pins: []string{"macmini:100.64.0.7"}},
			"the llm-proxy reaches macmini:100.64.0.7 at the address this machine resolves for it, pinned into the proxy's /etc/hosts; the allowlist is still enforced by name"},
		{hostpin.ModelHosts{Pins: []string{"localhost:host-gateway", "macmini:100.64.0.7"}, Gateway: []string{"localhost"}, Unresolved: []string{"gone.local"}},
			"the llm-proxy reaches macmini:100.64.0.7 at the address this machine resolves for it; the llm-proxy reaches localhost, which names this computer, at the container engine's host gateway, pinned into the proxy's /etc/hosts; the allowlist is still enforced by name; gone.local does not resolve on this machine, and the proxy says so to a request for it"},
		{hostpin.ModelHosts{Unresolved: []string{"gone.local"}},
			"gone.local does not resolve on this machine, and the proxy says so to a request for it"},
	} {
		if got := modelHostsLine(c.mh); got != c.want {
			t.Errorf("modelHostsLine(%v) =\n%q, want\n%q", c.mh, got, c.want)
		}
	}
}

func TestAllowHost(t *testing.T) {
	for in, want := range map[string]string{
		"macmini:8000": "macmini", "API.Example.com": "api.example.com",
		"[fd00::1]:8000": "fd00::1", " llm.lan:80 ": "llm.lan", "": "",
	} {
		if got := allowHost(in); got != want {
			t.Errorf("allowHost(%q) = %q, want %q", in, got, want)
		}
	}
}
