package dockerx

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"
)

// devRuntimes is agentry-dev-web's `docker info --format '{{json .Runtimes}}'`
// on 2026-09-27, each runtime's status trimmed: the table a registration
// is checked against, parsed through the same struct tags Runtimes uses.
func devRuntimes(t *testing.T) map[string]Runtime {
	t.Helper()
	var table map[string]Runtime
	if err := json.Unmarshal([]byte(`{
		"io.containerd.runc.v2": {"path": "runc", "status": {"org.opencontainers.runtime-spec.features": "{}"}},
		"runc": {"path": "runc"},
		"runsc": {"path": "/usr/local/bin/runsc"},
		"runsc-nat": {"path": "/usr/local/bin/runsc", "runtimeArgs": ["--reproduce-nat"]},
		"runsc-uds": {"path": "/usr/local/bin/runsc", "runtimeArgs": ["--host-uds=open"]}
	}`), &table); err != nil {
		t.Fatal(err)
	}
	return table
}

// TestCheckHostUDSRegistration (agentry-cli#396): the gateway's runtime is
// held to what it runs, not its name. It must be the runtime's own binary
// with the runtime's own arguments plus --host-uds=open and nothing else;
// a registration named for gVisor that runs runc, plain runsc, another
// flag, or a wider one beside it is refused with the reason.
func TestCheckHostUDSRegistration(t *testing.T) {
	dev := devRuntimes(t)
	if err := CheckHostUDSRegistration(dev, "runsc", "runsc-uds"); err != nil {
		t.Fatalf("dev's runsc-uds: %v", err)
	}
	with := func(name string, r Runtime) map[string]Runtime {
		m := maps.Clone(dev)
		m[name] = r
		return m
	}
	for name, c := range map[string]struct {
		table   map[string]Runtime
		gateway string
		want    string
	}{
		"runc":            {dev, "runc", "runs runc, not /usr/local/bin/runsc"},
		"plain runsc":     {dev, "runsc", `must be "runsc"'s own ([]) plus --host-uds=open`},
		"another flag":    {dev, "runsc-nat", `registered with runtimeArgs ["--reproduce-nat"]`},
		"forged uds":      {with("runsc-uds", Runtime{Path: "runc", Args: []string{"--host-uds=open"}}), "runsc-uds", "runs runc"},
		"host network":    {with("runsc-uds", Runtime{Path: "/usr/local/bin/runsc", Args: []string{"--host-uds=open", "--network=host"}}), "runsc-uds", "nothing else"},
		"wider uds":       {with("runsc-uds", Runtime{Path: "/usr/local/bin/runsc", Args: []string{"--host-uds=all", "--host-uds=open"}}), "runsc-uds", "nothing else"},
		"flag used twice": {with("runsc-uds", Runtime{Path: "/usr/local/bin/runsc", Args: []string{"--host-uds=open", "--host-uds=open"}}), "runsc-uds", `extra ["--host-uds=open"]`},
		"named extra":     {with("runsc-uds", Runtime{Path: "/usr/local/bin/runsc", Args: []string{"--host-uds=open", "--network=host"}}), "runsc-uds", `extra ["--network=host"], missing []`},
		"no binary":       {with("runsc-uds", Runtime{Args: []string{"--host-uds=open"}}), "runsc-uds", "names no binary"},
		"not registered":  {dev, "runsc-gw", `"runsc-gw" is not registered`},
	} {
		err := CheckHostUDSRegistration(c.table, "runsc", c.gateway)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	// The runtime's own arguments are carried over, in any order.
	systrap := with("runsc", Runtime{Path: "/usr/bin/runsc", Args: []string{"--platform=systrap"}})
	systrap["runsc-uds"] = Runtime{Path: "/usr/bin/runsc", Args: []string{"--host-uds=open", "--platform=systrap"}}
	if err := CheckHostUDSRegistration(systrap, "runsc", "runsc-uds"); err != nil {
		t.Errorf("the runtime's own arguments plus the flag: %v", err)
	}
	systrap["runsc-uds"] = Runtime{Path: "/usr/bin/runsc", Args: []string{"--host-uds=open"}}
	if err := CheckHostUDSRegistration(systrap, "runsc", "runsc-uds"); err == nil {
		t.Error("a registration dropping the runtime's own arguments was accepted")
	}
	if err := CheckHostUDSRegistration(dev, "kata", "runsc-uds"); err == nil || !strings.Contains(err.Error(), `the runtime "kata" is not registered`) {
		t.Errorf("a missing runtime: %v", err)
	}
	// Two registrations naming no binary are not "the same binary".
	bare := map[string]Runtime{"runsc": {}, "runsc-uds": {Args: []string{HostUDSOpen}}}
	if err := CheckHostUDSRegistration(bare, "runsc", "runsc-uds"); err == nil {
		t.Error("registrations with no binary were accepted")
	}
	if got := SortedRuntimeNames(dev); strings.Join(got, ",") != "io.containerd.runc.v2,runc,runsc,runsc-nat,runsc-uds" {
		t.Errorf("RuntimeNames: %v", got)
	}
}
