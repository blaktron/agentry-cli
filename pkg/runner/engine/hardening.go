package engine

import (
	"fmt"
	"maps"
	"strconv"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
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

// proxySandboxed says whether the run's llm-proxy runs under the plan's
// runtime (agentry-cli#337): only with the host's name servers to read, and
// never for a host-run harness, whose proxy publishes ports to the host.
func proxySandboxed(plan *spec.Plan) bool {
	return len(plan.SandboxResolvers()) > 0 && plan.Harness.Host == nil
}

// relaysSandboxed says whether the run's relays run under its runtime
// (agentry-cli#337): the docker filter gives it to them, with the run's
// resolv.conf, where it runs and the host's name servers are named.
func relaysSandboxed(plan *spec.Plan) bool {
	return filterSupported && relaysNeeded(plan) && len(plan.SandboxResolvers()) > 0
}

// gatewaySandboxed says whether the gateway runs under
// docker.gatewayRuntime (agentry-cli#337): only through the docker filter,
// whose socket is the one the registration's --host-uds=open opens. Where
// the filter does not run the gateway holds the daemon's own socket, and
// no sandbox is claimed for it.
func gatewaySandboxed(plan *spec.Plan) bool {
	return filterSupported && plan.GatewayRuntime() != ""
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
	rs := p.Plan.SandboxResolvers()
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
	if proxySandboxed(p.Plan) {
		under = append(under, "the llm-proxy (on an egress network of the run's own, joined at create)")
	}
	line := rt + ": created with docker --runtime " + rt + ": " + strings.Join(under, ", ") +
		" (each finds the containers it talks to through /etc/hosts, since a sandboxed network stack does not reach docker's embedded DNS)"
	if gatewaySandboxed(p.Plan) {
		line += "; the gateway is created with docker --runtime " + p.Plan.GatewayRuntime() + " (docker.gatewayRuntime), which opens the bind-mounted socket of the run's docker filter"
	}
	launched, is, takes := "the tools server", "is", "takes"
	if servers {
		launched, is, takes = "the tools server and the declared MCP servers", "are", "take"
	}
	if filterSupported {
		line += "; " + launched + ", which the gateway launches, " + is + " given " + rt + " by the run's docker filter" +
			" (a server's relay links pinned into its /etc/hosts at the relay's address)"
		if rs := p.Plan.WebResolvers(); len(rs) > 0 {
			// The web tools' server reaches past the host by name
			// (agentry-cli#386): it resolves through the host's servers.
			line += "; the web tools' server resolves host names through " + strings.Join(rs, ", ") + " (docker.resolvers), from a resolv.conf of the run's own"
		}
	} else {
		line += "; " + launched + ", which the gateway launches, " + takes + " the daemon's default: the docker filter that sets the runtime runs on Linux only (agentry-cli#322)"
	}
	if p.Plan.Harness.Host != nil {
		line += "; the harness is a host process, under no container runtime at all"
	}
	// The classes that resolve host names read the host's own name servers
	// under the runtime (agentry-cli#337).
	var named []string
	if proxySandboxed(p.Plan) {
		named = append(named, "the llm-proxy")
	}
	if servers && len(rs) > 0 {
		named = append(named, "the MCP server install proxy")
	}
	if relaysSandboxed(p.Plan) {
		named = append(named, "the relays, which the filter gives "+rt)
	}
	if len(named) > 0 {
		line += "; " + strings.Join(named, ", ") + " resolve host names through " + strings.Join(rs, ", ") +
			" (docker.resolvers), from a resolv.conf of the run's own bound read-only in place of docker's, each created with every network it joins"
	}
	var gaps []string
	if !filterSupported {
		gaps = append(gaps, launched+" (the docker filter does not run here)")
	}
	switch {
	case proxySandboxed(p.Plan):
	case len(rs) > 0:
		gaps = append(gaps, "the llm-proxy (it publishes ports to the host-run harness)")
	default:
		gaps = append(gaps, "the llm-proxy (it resolves the model endpoint through docker's embedded DNS, which "+rt+" does not reach, and docker.resolvers names no server)")
	}
	if servers && len(rs) == 0 {
		gaps = append(gaps, "the MCP server install proxy (it resolves the registry the same way)")
	}
	switch {
	case !relaysNeeded(p.Plan) || relaysSandboxed(p.Plan):
	case len(rs) == 0:
		gaps = append(gaps, "the relays (each resolves its declared host through docker's embedded DNS, which "+rt+" does not reach)")
	default:
		gaps = append(gaps, "the relays (the docker filter that gives them "+rt+" does not run here)")
	}
	switch {
	case gatewaySandboxed(p.Plan):
	case p.Plan.GatewayRuntime() != "":
		gaps = append(gaps, "the gateway (it holds the daemon's own socket where the docker filter does not run, so docker.gatewayRuntime is not applied)")
	default:
		gaps = append(gaps, "the gateway (it reaches the daemon through a host Unix socket, which "+rt+" does not open; docker.gatewayRuntime names no registration with --host-uds=open)")
	}
	if len(gaps) == 0 {
		return line + "; no container class of the run is on the daemon's default runtime"
	}
	return line + "; on the daemon's default runtime, as measured under " + rt + " (DR-23): " + strings.Join(gaps, ", ")
}
