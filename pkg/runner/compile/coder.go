package compile

import (
	"fmt"
	"slices"
	"strings"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/persistent/coder"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/hostlink"
)

// The Coding agent. Its grant (Inputs.Coder) is what its roster has on: any
// program in its own exec image, built on its toolchain image; open egress
// for its tools server and those programs; the browser on any public site;
// the person's dev server as a target host. Each is a part of the persistent
// grant class, gated by the person's agreement for this agent rather than by
// the machine policy, so it is given to a persistent built-in alone, beside
// its folders, and the manifest declares none of it.

// coderGrant checks the Coding agent's grant and sets it on caps.
func coderGrant(in Inputs, caps *spec.CapSummary) error {
	if in.Mode != manifest.ModePersistent || in.Source != "builtin" {
		return &PolicyRefusal{Reason: "the Coding agent's grants (any program, open egress, the browser on any public site, a dev server) are a persistent built-in's alone"}
	}
	if in.Manifest.Namespace != coder.Namespace || in.Manifest.Name != coder.Name {
		return &PolicyRefusal{Reason: fmt.Sprintf("the Coding agent's grants are %s/%s's alone", coder.Namespace, coder.Name)}
	}
	if in.AgentFiles == nil {
		return &PolicyRefusal{Reason: "the Coding agent's grants come with its folders, where its programs work"}
	}
	c := in.Manifest.Capabilities
	if caps.EgressMode != "none" || len(caps.ExecCommands) > 0 || len(c.TargetHosts) > 0 || c.Browser != nil {
		return &PolicyRefusal{Reason: "the Coding agent's grants replace what a manifest declares: it declares no egress, exec, target host or browser of its own"}
	}
	g := *in.Coder
	if err := checkCoderPorts(g.Ports); err != nil {
		return err
	}
	if err := checkCoderSecrets(g.Secrets, c.Secrets); err != nil {
		return err
	}
	if err := checkCoderAccounts(g); err != nil {
		return err
	}
	caps.Coder = &g
	if g.OpenEgress {
		caps.EgressMode = "all"
		// As for a manifest's "*": under a runtime the tools server's own
		// network resolves through the host's name servers alone.
		if rt := in.Policy.Docker.Runtime; rt != "" && len(in.Policy.Docker.Resolvers) == 0 {
			return &PolicyRefusal{Reason: fmt.Sprintf("the Coding agent's open egress puts its tools server on a network of its own, which under the %[1]s runtime resolves host names only through the host's own name servers: name them in the runner policy's docker.resolvers (docker's embedded DNS is not reachable from %[1]s)", rt)}
		}
	}
	return nil
}

// coderExecEnforcement is the exec line of the Coding agent's plan.
func coderExecEnforcement(img *spec.ExecImageSpec) string {
	return fmt.Sprintf("run_command runs any program the agent names, the Coding agent's grant, which the person agreed to for this agent (its roster's commands entry), not a declared allowlist: "+
		"in the Coding agent's exec image, built on this machine from its toolchain image %s (pinned by digest) with the runner binary copied in, not minimized, the programs found on the image's own PATH; "+
		"this machine's exec catalogue is not consulted; each program runs in a folder beneath %s, as the person's ids where the engine maps them (as root on a rootless engine, which is the person), with HOME at %s, an environment of PATH, HOME, LANG, the toolchain's settings and the person's stored secrets its roster has on alone, "+
		"its writes held by Landlock beneath /tmp, the scratch folder and %s (where there is no Landlock, no program runs), in a process group ended with the call, at most %d seconds and the output cap a call",
		img.Coder, spec.AgentRoot, spec.CoderHome, spec.AgentRoot, coderExecTimeoutSeconds)
}

// devHostEnforcement is the targetHosts line of a plan whose one target
// host is the Coding agent's dev server.
func devHostEnforcement() string {
	return fmt.Sprintf("the Coding agent's dev server, saved by the person for this agent with its host key pinned and its sign-in in their keychain, readied at each segment's start, is reached as the target host %q only by the host link (%s, its own container) through one relay to the address and port the person's machine resolved; "+
		"its sign-in is in files bound into the host link alone, and the agent works the host through %s, every call logged; what the host prints reaches the agent unchanged, past the gateway's secret block and the harness's scrub, but for the sign-in",
		spec.DevHostID, hostlink.ServerName, strings.Join(hostlink.Tools, ", "))
}

// checkCoderPorts refuses published ports that are not each a distinct name
// on a distinct port from 1024 to 65535.
func checkCoderPorts(ports []spec.CoderPort) error {
	names, numbers := map[string]bool{}, map[int]bool{}
	for _, p := range ports {
		if p.Name == "" || p.Port < 1024 || p.Port > 65535 || names[p.Name] || numbers[p.Port] {
			return &PolicyRefusal{Reason: fmt.Sprintf("the Coding agent's port %q on %d is not a distinct name on a distinct port from 1024 to 65535", p.Name, p.Port)}
		}
		names[p.Name], numbers[p.Port] = true, true
	}
	return nil
}

// checkCoderSecrets refuses stored secrets' names that are not names a
// person may store (spec.CheckStoredSecretName), that repeat, or that a
// manifest's own declared secret names: each is set in the tools
// container's environment under its name.
func checkCoderSecrets(names, declared []string) error {
	seen := map[string]bool{}
	for _, n := range names {
		switch {
		case spec.CheckStoredSecretName(n) != nil:
			return &PolicyRefusal{Reason: fmt.Sprintf("the Coding agent's stored secret %q: %v", n, spec.CheckStoredSecretName(n))}
		case seen[n]:
			return &PolicyRefusal{Reason: fmt.Sprintf("the Coding agent's stored secret %s is given twice", n)}
		case slices.Contains(declared, n):
			return &PolicyRefusal{Reason: fmt.Sprintf("the Coding agent's stored secret %s has the name of a secret its manifest declares", n)}
		}
		seen[n] = true
	}
	return nil
}

// checkCoderAccounts refuses git host accounts the Coding agent's programs
// cannot be given (spec.CheckCoderAccounts), and any without its commands,
// which are the only way an account is used.
func checkCoderAccounts(g spec.CoderGrant) error {
	if len(g.Accounts) == 0 {
		return nil
	}
	if !g.Commands {
		return &PolicyRefusal{Reason: "the Coding agent's git host accounts are given to the programs it runs, and its commands are off"}
	}
	if err := spec.CheckCoderAccounts(g.Accounts); err != nil {
		return &PolicyRefusal{Reason: err.Error()}
	}
	return nil
}

// coderAccountsEnforcement is the plan's line for the Coding agent's git
// host accounts.
func coderAccountsEnforcement(accounts []spec.CoderAccount) string {
	var named []string
	for _, a := range accounts {
		whose := "the person's account"
		if a.Own {
			whose = "the agent's own account"
		}
		named = append(named, fmt.Sprintf("%s as %s (%s, %s)", a.Host, a.User, whose, a.Kind))
	}
	return "the person's git host accounts " + strings.Join(named, ", ") + ", whose tokens they keep in their keychain and agreed to give this agent (its roster's account entries), are given to every program run_command runs: " +
		"git signs in with a token over https to its own host alone, through a credential helper set as command-scope configuration (GIT_CONFIG_COUNT) that reads it from the program's environment, and commits in a repository whose remote is on that host carry the account's name and email; " +
		"gh, glab and tea's login are given the token of their kind's host in the variables they read; Agentry writes none of it into HOME, so an account switched off is gone at the next segment, though a program can keep a token itself (a tea login made outside /tmp, a token put in a remote URL or a file); " +
		"each token comes from the gateway's secrets file, which the engine writes for the segment and removes at its end, the plan holding the hosts and names alone; " +
		"the tools server masks each whole token wherever it appears in a result of its tools and the engine's redactor in the receipts, the transcript and the events, but what a program prints of one in another form (encoded, or split across two pages of a read) is not masked; " +
		"a program can read a token and use or send it anywhere it reaches, with whatever that account can do on its host"
}

// coderSecretsEnforcement is the plan's line for the Coding agent's stored
// secrets.
func coderSecretsEnforcement(names []string) string {
	return "the person's stored secrets " + strings.Join(names, ", ") + ", which they keep in their keychain for this agent and agreed to give it (its roster's secret entries), are set in the environment of every program run_command runs, " +
		"from the gateway's secrets file, which the engine writes for the segment and removes at its end; the plan holds their names alone; " +
		"the tools server masks each whole value wherever it appears in a result of its tools and refuses a file it writes that holds one, the engine's redactor masks each in the receipts, the transcript and the events; " +
		"a program can read a value and send it anywhere it reaches, and what it prints of one in another form (encoded, or split across two pages of a read) is not masked"
}

// coderPortsEnforcement is the plan's line for the Coding agent's published
// ports.
func coderPortsEnforcement(ports []spec.CoderPort) string {
	var named []string
	for _, p := range ports {
		named = append(named, fmt.Sprintf("%s on %d", p.Name, p.Port))
	}
	return "the Coding agent's tools server publishes " + strings.Join(named, ", ") + " on the person's computer, each at 127.0.0.1 and the same number, which the person named for this agent and agreed to (its roster's port entries): " +
		"the runner listens on this computer's loopback at each port alone and carries each connection into the tools server's own loopback at the same port through the engine's exec, so no port of the container is published by the engine and no other port is reached; " +
		"a port another program on this computer holds is not published, which the port relay's receipt says"
}
