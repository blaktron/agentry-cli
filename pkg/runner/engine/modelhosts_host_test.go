//go:build !windows

// The model-host tests that run the engine's host path, which (as in
// host_test.go) is built off Windows only: hostFixture puts a shell-script
// harness on PATH.

package engine

import (
	"context"
	"net"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The whole path: the proxy container gets the pins as --add-host
// entries, the agent does not, and the receipt names them.
func TestTheProxyGetsTheModelHostPinsAndTheReceiptSaysSo(t *testing.T) {
	fakeLookup(t, map[string][]string{"macmini": {"100.64.0.7"}})
	p, f, _, _ := hostFixture(t)
	p.Plan.Proxy.Allow = []string{"macmini:8000"}
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	proxy, ok := f.FindRun("-proxy")
	if !ok {
		t.Fatal("no proxy container")
	}
	if !reflect.DeepEqual(proxy.ExtraHosts, []string{"macmini:100.64.0.7"}) {
		t.Errorf("proxy ExtraHosts = %q", proxy.ExtraHosts)
	}
	line := env.CapabilitiesEnforced["llm-proxy.hosts"]
	if !strings.Contains(line, "macmini:100.64.0.7") || !strings.Contains(line, "enforced by name") {
		t.Errorf("receipt llm-proxy.hosts = %q", line)
	}
}

// Nothing resolves: no pin and no receipt line, the run as it was.
func TestNoPinWhenTheHostResolvesNothing(t *testing.T) {
	p, f, _, _ := hostFixture(t)
	p.Plan.Proxy.Allow = []string{"macmini:8000"}
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	proxy, _ := f.FindRun("-proxy")
	if len(proxy.ExtraHosts) != 0 {
		t.Errorf("proxy ExtraHosts = %q, want none", proxy.ExtraHosts)
	}
	if _, ok := env.CapabilitiesEnforced["llm-proxy.hosts"]; ok {
		t.Error("a receipt line for no pins")
	}
}

// A model endpoint on this computer is pinned to the engine's host gateway
// in a local run, and never in a hosted one.
func TestALocalEndpointIsPinnedToTheHostGateway(t *testing.T) {
	fakeLookup(t, map[string][]string{"localhost": {"127.0.0.1", "::1"}})
	p, f, _, _ := hostFixture(t)
	p.Plan.Proxy.Allow = []string{"localhost:11434"}
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	proxy, _ := f.FindRun("-proxy")
	if !reflect.DeepEqual(proxy.ExtraHosts, []string{"localhost:host-gateway"}) {
		t.Errorf("proxy ExtraHosts = %q", proxy.ExtraHosts)
	}
	if line := env.CapabilitiesEnforced["llm-proxy.hosts"]; !strings.Contains(line, "localhost, which names this computer, at the container engine's host gateway") {
		t.Errorf("receipt llm-proxy.hosts = %q", line)
	}

	p, f, _, _ = hostFixture(t)
	p.Plan.Proxy.Allow = []string{"localhost:11434"}
	p.Hosted = true
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatalf("hosted Run: %v", err)
	}
	proxy, _ = f.FindRun("-proxy")
	if len(proxy.ExtraHosts) != 0 {
		t.Errorf("hosted proxy ExtraHosts = %q, want none", proxy.ExtraHosts)
	}
}

// A model host that does not exist is handed to the proxy as unresolved,
// and the receipt says so.
func TestAnUnresolvedModelHostIsHandedToTheProxy(t *testing.T) {
	old := lookupModelHost
	lookupModelHost = func(_ context.Context, host string) ([]net.IP, error) {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	t.Cleanup(func() { lookupModelHost = old })
	p, f, _, _ := hostFixture(t)
	p.Plan.Proxy.Allow = []string{"gone.local:8000"}
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	proxy, _ := f.FindRun("-proxy")
	if i := slices.Index(proxy.Argv, "--unresolved"); i < 0 || i+1 >= len(proxy.Argv) || proxy.Argv[i+1] != "gone.local" {
		t.Errorf("proxy argv = %q, want --unresolved gone.local", proxy.Argv)
	}
	if line := env.CapabilitiesEnforced["llm-proxy.hosts"]; !strings.Contains(line, "gone.local does not resolve on this machine") {
		t.Errorf("receipt llm-proxy.hosts = %q", line)
	}
}
