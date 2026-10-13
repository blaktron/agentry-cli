package sockfilter

import (
	"strings"
	"testing"
)

// TestASidecarsConfigurationIsTheFiltersOwn (agentry-cli#322): what the
// engine hands the docker-filter role reads back as the same policy, less
// the three fields that are the container's, and a field the role does
// not know is refused rather than dropped.
func TestASidecarsConfigurationIsTheFiltersOwn(t *testing.T) {
	in := Config{
		RunID: "A", Label: lbl, OwnerLabel: own, Listen: "/host/f.sock", Upstream: "/host/d.sock", Log: &strings.Builder{},
		BindRoots: []string{"/c/Users/me/run"}, WritableRoots: []string{"/c/Users/me/run/out"},
		SharedNetworks: []string{intNet}, EgressNetworks: map[string][]string{ext: {relayPin}},
		NetworkNames: map[string]string{"docker-mcp-proxies-int": intNet}, RelayHosts: []string{"api.osv.dev"},
		ToolsImage: "agentry-runner:test", ToolsCmd: []string{"tools", "--spec", "/etc/agentry/toolspec.json"},
		ToolsTmpfs: map[string]string{"/tmp": "size=1"}, Limits: Limits{MemoryBytes: 1 << 30, NanoCPUs: 1e9, Pids: 512}, Runtime: "runsc",
	}
	b, err := MarshalConfig(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "/host/") {
		t.Fatalf("the container's own fields went into the configuration: %s", b)
	}
	out, err := UnmarshalConfig(b)
	if err != nil {
		t.Fatal(err)
	}
	if out.RunID != "A" || out.BindRoots[0] != "/c/Users/me/run" || out.EgressNetworks[ext][0] != relayPin || out.Limits.Pids != 512 ||
		out.ToolsCmd[2] != "/etc/agentry/toolspec.json" || out.Runtime != "runsc" || out.Listen != "" || out.Log != nil {
		t.Fatalf("read back as %+v", out)
	}
	if _, err := UnmarshalConfig([]byte(`{"RunID":"A","AllowEverything":true}`)); err == nil {
		t.Fatal("an unknown field was accepted")
	}
}

func TestCountRefused(t *testing.T) {
	log := "2026-10-13T00:00:00Z allow POST /containers/create\n" +
		"2026-10-13T00:00:01Z refuse POST /containers/create: a privileged container\n" +
		"2026-10-13T00:00:02Z absent GET /containers/x/json: No such container: x\n" +
		"2026-10-13T00:00:03Z refuse DELETE /volumes/v: not this run's\n"
	if n := CountRefused(strings.NewReader(log)); n != 2 {
		t.Fatalf("counted %d refusals, want 2", n)
	}
}
