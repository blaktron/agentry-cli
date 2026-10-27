package hostpin

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestUpstreamsSkipTheLocalStub: a resolv.conf naming only a local stub (as
// systemd-resolved's does) gives way to the next file, and loopback, IPv6
// and repeated servers are left out, three at most.
func TestUpstreamsSkipTheLocalStub(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "stub.conf")
	upstream := filepath.Join(dir, "upstream.conf")
	must(t, os.WriteFile(stub, []byte("nameserver 127.0.0.53\noptions edns0 trust-ad\nsearch example.com\n"), 0o600))
	must(t, os.WriteFile(upstream, []byte("# upstream\nnameserver 192.168.1.1\nnameserver fd00::1\nnameserver 192.168.1.1\nnameserver 0.0.0.0\nnameserver 1.1.1.1\nnameserver 9.9.9.9\nnameserver 8.8.8.8\n"), 0o600))
	old := resolvConfs
	t.Cleanup(func() { resolvConfs = old })

	resolvConfs = []string{stub, upstream}
	if got, want := Upstreams(), []string{"192.168.1.1", "1.1.1.1", "9.9.9.9"}; !slices.Equal(got, want) {
		t.Errorf("Upstreams() = %v, want %v", got, want)
	}
	resolvConfs = []string{upstream, stub}
	if got := Upstreams(); len(got) != 3 || got[0] != "192.168.1.1" {
		t.Errorf("the first file that names a server wins: %v", got)
	}
	resolvConfs = []string{stub, filepath.Join(dir, "missing.conf")}
	if got := Upstreams(); got != nil {
		t.Errorf("only a local stub named: %v, want none", got)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// tableLookup answers from a table: a name missing from it does not exist,
// and one mapped to nil times out.
func tableLookup(table map[string][]string) func(context.Context, string) ([]net.IP, error) {
	return func(_ context.Context, host string) ([]net.IP, error) {
		addrs, ok := table[host]
		switch {
		case !ok:
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		case addrs == nil:
			return nil, &net.DNSError{Err: "i/o timeout", Name: host, IsTimeout: true}
		}
		var ips []net.IP
		for _, a := range addrs {
			ips = append(ips, net.ParseIP(a))
		}
		return ips, nil
	}
}

// TestResolveModelHosts: an endpoint on this computer is pinned to the
// engine's host gateway, one elsewhere to its IPv4 answers, and a name
// that does not exist is reported unresolved.
func TestResolveModelHosts(t *testing.T) {
	lookup := tableLookup(map[string][]string{
		"macmini":                  {"100.64.0.7"},
		"studio.local":             {"127.0.0.1", "192.168.1.20", "::1"},
		"debian-box":               {"127.0.1.1"},
		"v6only.example":           {"::1"},
		"slow.example":             nil,
		"api.example.com":          {"203.0.113.7"},
		"localhost":                {"127.0.0.1"},
		"host.docker.internal":     {"192.168.65.254"},
		"host.containers.internal": {"169.254.1.2"},
	})
	allow := []string{
		"localhost:11434", "LLM.localhost:8000", "studio.local:8000", "debian-box:8000",
		"v6only.example:8000", "macmini:8000", "api.example.com:443",
		"WorkMac.local:8000", "workmac:8001", "nowhere.local:8000", "slow.example:8000", "slow.local:8000",
		"host.docker.internal:8000", "host.containers.internal:8000", "127.0.0.1:8000", "[::1]:8000",
		"agentry-host.internal:11434",
	}
	got := ResolveModelHosts(context.Background(), allow, lookup, "WorkMac.example.org", true)
	wantPins := []string{
		"agentry-host.internal:host-gateway", "api.example.com:203.0.113.7", "debian-box:host-gateway", "llm.localhost:host-gateway",
		"localhost:host-gateway", "macmini:100.64.0.7", "studio.local:host-gateway",
		"v6only.example:host-gateway", "workmac.local:host-gateway", "workmac:host-gateway",
	}
	if !slices.Equal(got.Pins, wantPins) {
		t.Errorf("pins = %q,\nwant %q", got.Pins, wantPins)
	}
	wantGateway := []string{"agentry-host.internal", "debian-box", "llm.localhost", "localhost", "studio.local", "v6only.example", "workmac", "workmac.local"}
	if !slices.Equal(got.Gateway, wantGateway) {
		t.Errorf("gateway = %q, want %q", got.Gateway, wantGateway)
	}
	if want := []string{"nowhere.local", "slow.local"}; !slices.Equal(got.Unresolved, want) {
		t.Errorf("unresolved = %q, want %q (a timeout is a missing name for .local alone)", got.Unresolved, want)
	}
}

// TestResolveModelHostsHostedNeverReachesTheHost: a hosted run's allowlist
// is pinned only to addresses elsewhere, so a name that means the run's
// host (localhost, a loopback answer, the host's own name) reaches nothing
// new.
func TestResolveModelHostsHostedNeverReachesTheHost(t *testing.T) {
	lookup := tableLookup(map[string][]string{
		"localhost":         {"127.0.0.1"},
		"localtest.example": {"127.0.0.1"},
		"api.example.com":   {"203.0.113.7"},
	})
	got := ResolveModelHosts(context.Background(),
		[]string{"localhost:22", "localtest.example:22", "runhost:22", "api.example.com:443", "gone.example:443", "agentry-host.internal:22"},
		lookup, "runhost", false)
	if want := []string{"api.example.com:203.0.113.7"}; !slices.Equal(got.Pins, want) {
		t.Errorf("pins = %q, want %q", got.Pins, want)
	}
	for _, p := range got.Pins {
		if strings.HasSuffix(p, ":"+HostGateway) {
			t.Errorf("a hosted run pinned %q to its host", p)
		}
	}
	if want := []string{"gone.example", "runhost"}; !slices.Equal(got.Unresolved, want) {
		t.Errorf("unresolved = %q, want %q", got.Unresolved, want)
	}
}

// TestPinsSkipLoopbackAnswers: the other proxies' pins skip loopback
// answers and pin nothing to the host gateway.
func TestPinsSkipLoopbackAnswers(t *testing.T) {
	lookup := tableLookup(map[string][]string{"localhost": {"127.0.0.1"}, "lan.example": {"10.0.0.4", "127.0.0.1"}})
	got := Pins(context.Background(), []string{"localhost:80", "lan.example:443", "missing.example:443"}, lookup)
	if want := []string{"lan.example:10.0.0.4"}; !slices.Equal(got, want) {
		t.Errorf("Pins = %q, want %q", got, want)
	}
}
