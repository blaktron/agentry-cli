package engine

import (
	"fmt"
	"maps"
	"strconv"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// Container hardening (agentry-cli#163, DR-30). Every container the
// engine starts for a run drops every capability, cannot gain privileges
// through a setuid binary, and is bounded in memory, CPUs and processes by
// the policy's docker.limits. The proxy and the probe are the runner's own
// binary and write nothing, so their root is read-only; the agent's is
// read-only when its adapter says where the harness writes instead. The
// gateway keeps a writable root: it is a vendor image, and what it writes
// is its own business. The tool and server containers the gateway launches
// are not ours to shape: it gives them no-new-privileges itself, and the
// limits pass through its --cpus and --memory flags.

// limits is the run's per-container bounds, defaults filled in.
func (p Params) limits() policy.Limits {
	if p.Policy == nil {
		return policy.Limits{}.WithDefaults()
	}
	return p.Policy.Docker.Limits.WithDefaults()
}

// hardened is spec with the posture every run container takes.
func hardened(s dockerx.RunSpec, lim policy.Limits) dockerx.RunSpec {
	s.CapDrop = []string{"ALL"}
	s.NoNewPrivileges = true
	s.PidsLimit = lim.Pids
	s.Memory = lim.Memory
	s.CPUs = lim.CPUs
	return s
}

// readOnlyRoot is spec with a read-only root and tmpfs as its scratch.
func readOnlyRoot(s dockerx.RunSpec, tmpfs ...string) dockerx.RunSpec {
	s.ReadOnly = true
	s.Tmpfs = tmpfs
	return s
}

// gatewayLimitArgs are the flags that bound the containers the gateway
// launches, the same bounds as the run's own.
func gatewayLimitArgs(lim policy.Limits) []string {
	return []string{"--cpus", strconv.Itoa(lim.CPUs), "--memory", lim.Memory}
}

// withContainers adds the receipt's line for the posture above, naming
// only what this run's containers were given.
func withContainers(enforced map[string]string, p Params) map[string]string {
	lim := p.limits()
	out := maps.Clone(enforced)
	if out == nil {
		out = map[string]string{}
	}
	bounds := fmt.Sprintf("memory %s, cpus %d, pids %d", lim.Memory, lim.CPUs, lim.Pids)
	var readOnly []string
	readOnly = append(readOnly, "the llm proxy", "the probe")
	agent := "the agent container"
	switch {
	case p.Plan.Harness.Host != nil:
		agent = "no agent container (the harness runs on the host)"
	case p.Plan.Harness.Agent.ReadOnlyRoot:
		var paths []string
		for _, t := range p.Plan.Harness.Agent.Tmpfs {
			path, _, _ := strings.Cut(t, ":")
			paths = append(paths, path)
		}
		readOnly = append(readOnly, "the agent (tmpfs "+strings.Join(paths, ", ")+")")
	}
	out["containers"] = "the llm proxy, the gateway, the probe and " + agent +
		": all capabilities dropped, no-new-privileges, " + bounds +
		"; read-only root: " + strings.Join(readOnly, ", ") +
		"; the tool and MCP server containers the gateway launches: its own no-new-privileges and the same memory and cpus through its flags, capabilities and root as the gateway leaves them"
	return out
}
