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
	return s.WithPosture(lim.Memory, lim.CPUs, lim.Pids)
}

// hardenedAgent is the agent container's spec with the posture, and its
// root read-only when the adapter says where the harness writes; the exec
// path and the interactive session share it.
func hardenedAgent(s dockerx.RunSpec, p Params) dockerx.RunSpec {
	s = hardened(s, p.limits())
	if a := p.Plan.Harness.Agent; a.ReadOnlyRoot {
		s = readOnlyRoot(s, a.Tmpfs...)
	}
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
// what this run's containers were given and the two exceptions.
func withContainers(enforced map[string]string, p Params) map[string]string {
	lim := p.limits()
	out := maps.Clone(enforced)
	if out == nil {
		out = map[string]string{}
	}
	out["daemon"] = daemonLine(nil)
	containers := []string{"the llm proxy", "the gateway", "the probe"}
	readOnly := []string{"the llm proxy", "the probe"}
	switch {
	case p.Plan.Harness.Host != nil:
		// The harness is a host process: there is no agent container.
	case p.Plan.Harness.Agent.ReadOnlyRoot:
		containers = append(containers, "the agent")
		var paths []string
		for _, t := range p.Plan.Harness.Agent.Tmpfs {
			path, _, _ := strings.Cut(t, ":")
			paths = append(paths, path)
		}
		readOnly = append(readOnly, "the agent (tmpfs "+strings.Join(paths, ", ")+")")
	default:
		containers = append(containers, "the agent")
	}
	if len(p.Plan.MCPServers) > 0 {
		containers = append(containers, "the MCP server installer")
	}
	line := strings.Join(containers, ", ") +
		fmt.Sprintf(": all capabilities dropped, no-new-privileges, memory %s, cpus %d, pids %d", lim.Memory, lim.CPUs, lim.Pids) +
		"; the gateway keeps DAC_READ_SEARCH alone, to read the run's secrets file" +
		"; read-only root: " + strings.Join(readOnly, ", ")
	if p.Plan.Mode == "interactive" {
		line += "; the session store's one-shot chown keeps CHOWN alone"
	}
	line += "; the tool and MCP server containers the gateway launches get the same memory and cpus through its flags and the no-new-privileges its documentation gives them, their capabilities and root as it leaves them"
	out["containers"] = line
	if p.Plan.Runtime() != "" {
		out["runtime"] = runtimeLine(p)
	}
	return out
}
