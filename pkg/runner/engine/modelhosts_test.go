package engine

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
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
	t.Cleanup(func() { lookupModelHost = old })
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
	got := modelHostPins(context.Background(), []string{
		"macmini:8000", "MacMini:8001", "studio.local:11434", "api.example.com",
		"localhost:8080", "zero.example.com:443", "10.0.0.5:8000", "[fd00::1]:8000", "nowhere.invalid:443",
	})
	want := []string{"api.example.com:203.0.113.7", "macmini:100.64.0.7", "studio.local:192.168.1.20"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pins = %q, want %q", got, want)
	}
	// IP literals are never looked up, and each name once.
	for _, h := range *asked {
		if net.ParseIP(h) != nil {
			t.Errorf("looked up the address %q", h)
		}
	}
	if n := strings.Count(strings.Join(*asked, " "), "macmini"); n != 1 {
		t.Errorf("macmini looked up %d times, want once (asked %q)", n, *asked)
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
