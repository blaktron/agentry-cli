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

// enforcedAtStart is the receipt's enforcement map as a run or a session
// opens: the plan's lines, the exec packages' versions, the containers'
// posture, and the run's own knowledge and decode lines. One function for
// both paths, so a line added to one is on the other.
func enforcedAtStart(p Params) map[string]string {
	e := withContainers(enforcedWithPackages(p.Plan.Enforcement, p.ExecPackages), p)
	e = withKnowledge(e, p.Knowledge, p.Plan.Capabilities.ExecCommands)
	return withDecoded(e, p.Decoded)
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

// runtimeLine is the receipt's account of the policy's runtime (DR-23,
// agentry-cli#221): which of the run's container classes it holds, and why
// each other one takes the daemon's default. It is written at run time,
// not read from the plan, because two of its facts are the run's: whether
// the filter that gives the gateway's containers the runtime runs here,
// and what the run starts.
func runtimeLine(p Params) string {
	rt := p.Plan.Runtime()
	servers := len(p.Plan.MCPServers) > 0
	var under []string
	if p.Plan.Harness.Host == nil {
		under = append(under, "the agent container (the BriefAgent's code)")
		if s := p.Plan.Harness.Session; p.Plan.Mode == "interactive" && s != nil && s.StoreOwner != "" {
			under = append(under, "the session store's one-shot")
		}
	}
	if servers {
		under = append(under, "the MCP server installer")
	}
	under = append(under, "the gateway's health probe")
	line := rt + ": created with docker --runtime " + rt + ": " + strings.Join(under, ", ") +
		" (each finds the containers it talks to through /etc/hosts, since a sandboxed network stack does not reach docker's embedded DNS)"
	launched := "the tools server"
	if servers {
		launched += " and the declared MCP servers"
	}
	if filterSupported {
		line += "; " + launched + ", which the gateway launches, are given " + rt + " by the run's docker filter" +
			" (a server's relay links pinned into its /etc/hosts at the relay's address)"
	} else {
		line += "; " + launched + ", which the gateway launches, take the daemon's default: the docker filter that sets their runtime runs on Linux only (agentry-cli#322)"
	}
	if p.Plan.Harness.Host != nil {
		line += "; the harness is a host process, under no container runtime at all"
	}
	gaps := []string{"the llm-proxy (it resolves the model endpoint through docker's embedded DNS, which " + rt + " does not reach)"}
	if servers {
		gaps = append(gaps, "the MCP server install proxy (it resolves the registry the same way)")
	}
	if relaysNeeded(p.Plan.MCPServers) {
		gaps = append(gaps, "the relays (each resolves its declared host the same way)")
	}
	gaps = append(gaps, "the gateway (it reaches the daemon through a host Unix socket, which "+rt+" does not open)")
	return line + "; on the daemon's default runtime, as measured under " + rt + " (DR-23): " + strings.Join(gaps, ", ")
}
