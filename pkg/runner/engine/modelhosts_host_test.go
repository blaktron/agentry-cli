//go:build !windows

// The model-host tests that run the engine's host path, which (as in
// host_test.go) is built off Windows only: hostFixture puts a shell-script
// harness on PATH.

package engine

import (
	"context"
	"reflect"
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
