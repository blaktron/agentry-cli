package engine

import (
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// TestTheReceiptNamesWhatTheFilterGivesTheGatewaysContainers: where the
// run's docker filter runs, the containers line names every capability
// dropped and each class's own; without it (Docker Desktop) it says the
// gateway leaves them as they are.
func TestTheReceiptNamesWhatTheFilterGivesTheGatewaysContainers(t *testing.T) {
	filtered := launchedLine(true)
	for _, want := range []string{"every capability dropped", "pids bound", "CHOWN, DAC_OVERRIDE, FOWNER, KILL, SETGID, SETUID",
		"a declared MCP server DAC_READ_SEARCH", "DAC_OVERRIDE too where it writes", "a relay none"} {
		if !strings.Contains(filtered, want) {
			t.Errorf("filtered line lacks %q: %s", want, filtered)
		}
	}
	if plain := launchedLine(false); !strings.Contains(plain, "their capabilities and root as it leaves them") || strings.Contains(plain, "dropped") {
		t.Errorf("unfiltered line: %s", plain)
	}
	t.Run("the containers line", func(t *testing.T) {
		agent := spec.ContainerSpec{ReadOnlyRoot: true, Tmpfs: []string{"/tmp:rw", "/home/agent:rw"}}
		plan := &spec.Plan{
			Browser:  &spec.BrowserSpec{Image: "browser:img"},
			HostLink: &spec.HostLinkSpec{Image: "hostlink:img"},
			Harness:  spec.HarnessSpec{Agent: agent},
		}
		line := withContainers(map[string]string{}, Params{Plan: plan})["containers"]
		for _, want := range []string{"the llm proxy, the gateway, the probe, the agent, the browse proxy: all capabilities dropped, no-new-privileges",
			"the gateway keeps DAC_READ_SEARCH alone, to read the run's secrets file", "read-only root: the llm proxy, the probe, the agent (tmpfs /tmp, /home/agent), the browse proxy (tmpfs /tmp)",
			"the browser server (browser:img) is one of those the gateway launches", "the host link (hostlink:img) is one of those the gateway launches"} {
			if !strings.Contains(line, want) {
				t.Errorf("containers line lacks %q:\n%s", want, line)
			}
		}
		hostRun := &spec.Plan{Harness: spec.HarnessSpec{Host: &spec.HostProcessSpec{}}}
		if line := withContainers(map[string]string{}, Params{Plan: hostRun})["containers"]; strings.Contains(line, "the agent") {
			t.Errorf("a host-run harness is among the containers: %s", line)
		}
		if line := withContainers(map[string]string{}, Params{Plan: &spec.Plan{}})["containers"]; strings.Contains(line, "browse proxy") || strings.Contains(line, "browser server") || strings.Contains(line, "host link") {
			t.Errorf("a plan with neither a browser nor a host link names them: %s", line)
		}
	})
	t.Run("the posture", func(t *testing.T) {
		if got := gatewayLimitArgs(policy.Limits{Memory: "1g", CPUs: 2}); !slices.Equal(got, []string{"--cpus", "2", "--memory", "1g"}) {
			t.Errorf("gateway limit flags %v", got)
		}
		for readOnly, want := range map[bool]bool{true: true, false: false} {
			p := Params{Plan: &spec.Plan{Harness: spec.HarnessSpec{Agent: spec.ContainerSpec{ReadOnlyRoot: readOnly, Tmpfs: []string{"/tmp"}}}}}
			got := hardenedAgent(dockerx.RunSpec{Name: "agent"}, p)
			if got.ReadOnly != want || (want && !slices.Equal(got.Tmpfs, []string{"/tmp"})) || !slices.Equal(got.CapDrop, []string{"ALL"}) || !got.NoNewPrivileges {
				t.Errorf("an agent whose adapter says read-only root is %v: %+v", readOnly, got)
			}
		}
	})
	t.Run("the enforcement map", func(t *testing.T) {
		p := Params{Plan: &spec.Plan{Enforcement: map[string]string{"network": "the engine's line"}}, Enforced: map[string]string{"network": "a caller's line", "extra": "a caller's own"}}
		e := enforcedAtStart(p)
		if e["network"] != "the engine's line" || e["extra"] != "a caller's own" || e["containers"] == "" || e["daemon"] == "" {
			t.Errorf("enforcement at start: %v", e)
		}
	})
}
