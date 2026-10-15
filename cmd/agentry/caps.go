// Capability-summary rendering (install-time display): the declared
// security contract is shown before anything is written to disk, and
// network egress (the release's own or a package MCP server's), exec, a
// remote MCP server or a target host requires confirmation.
package main

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/blaktron/agentry/pkg/manifest"
)

// capabilityLines renders the capability block as stable, indentable
// lines. A fully-zero block renders as the single pure-text line.
func capabilityLines(c manifest.Capabilities) []string { return capabilityLinesSized(c, nil) }

// capabilityLinesSized is capabilityLines with each shipped knowledge pack's
// size beside its name where sizes has it, "knowledge: ships aws-docs (4.2
// MiB)": the consent shows what the release carries into the agent's context
// as well as its name.
func capabilityLinesSized(c manifest.Capabilities, sizes map[string]int64) []string {
	if isZeroCapabilities(c) {
		return []string{"none declared (pure text agent)"}
	}
	lines := []string{
		"network.egress: " + renderEgress(c.Network.Egress),
		"filesystem.read: " + renderFSAccess(c.Filesystem.Read),
		"filesystem.write: " + renderFSAccess(c.Filesystem.Write),
		"exec: " + renderExec(c.Exec),
		"secrets: " + renderList(c.Secrets),
	}
	// Which declared secrets a run may go without. Every declared
	// secret stays on its own line, optional or not: the consent names
	// them all, which is what lets a flip between optional and required
	// ask nothing again.
	if len(c.OptionalSecrets) > 0 {
		lines = append(lines, "optionalSecrets: "+renderList(c.OptionalSecrets))
	}
	lines = append(lines, "dataAccess: "+renderDataAccess(c.DataAccess))
	// Declared MCP servers: one line each — the consent gate and
	// guided setup must show exactly what will be installed.
	for _, srv := range c.MCPServers {
		if srv.IsRemote() {
			lines = append(lines, remoteServerLine(srv))
			continue
		}
		line := "mcpServer: " + srv.SourceID() + "@" + srv.Version
		if len(srv.Egress) > 0 {
			line += " (egress: " + renderList(srv.Egress)
			if slices.Contains(srv.Egress, "*") {
				line += " (unrestricted — policy-gated)"
			}
			line += ")"
		}
		if len(srv.Secrets) > 0 {
			line += " (secrets: " + renderList(srv.Secrets) + ")"
		}
		if len(srv.Env) > 0 {
			// Plain configuration, so the values are shown: they are what
			// the server receives and what the person consents to.
			var pairs []string
			for _, k := range slices.Sorted(maps.Keys(srv.Env)) {
				pairs = append(pairs, k+"="+srv.Env[k])
			}
			line += " (env: " + strings.Join(pairs, ", ") + ")"
		}
		if srv.OAuth != nil {
			// The sign-in is part of the contract: its issuer and scopes
			// are what the person's consent at the vendor will ask.
			line += " (OAuth sign-in at " + srv.OAuth.Issuer
			if len(srv.OAuth.Scopes) > 0 {
				line += ", scopes: " + renderList(srv.OAuth.Scopes)
			}
			line += "; given an access token alone)"
		}
		lines = append(lines, line)
	}
	// Target hosts: a machine the person names at the run's start, worked
	// as the account they sign in with.
	if len(c.TargetHosts) > 0 {
		lines = append(lines, "targetHosts: "+renderList(c.TargetHosts)+
			" (asks for a server when it runs, and can run any command on it as the account you sign in with)")
	}
	// The browser: where it may go, and the secrets it may type.
	if b := c.Browser; b != nil {
		lines = append(lines, "browser: "+browserLine(b))
	}
	// Shipped knowledge: packs inside the release the agent
	// reads, part of what is consented to.
	if len(c.Knowledge) > 0 {
		names := make([]string, 0, len(c.Knowledge))
		for _, name := range c.Knowledge {
			if n, ok := sizes[name]; ok {
				name += " (" + byteSize(n) + ")"
			}
			names = append(names, name)
		}
		lines = append(lines, "knowledge: ships "+strings.Join(names, ", "))
	}
	return lines
}

// capabilityNeedsConfirm reports whether install must ask before proceeding:
// declared network egress (the release's own or a package MCP server's),
// subprocess execution, a remote MCP server, a target host or a browser.
func capabilityNeedsConfirm(c manifest.Capabilities) bool {
	return len(confirmedCapabilities(c)) > 0
}

// confirmedCapabilities names what c declares that the install gate asks
// about, in the words the prompt uses: the question names what the
// release has, never a capability it does not.
func confirmedCapabilities(c manifest.Capabilities) []string {
	var what []string
	if len(c.Network.Egress.Hosts) > 0 {
		what = append(what, "network egress")
	}
	if len(c.Exec.Commands) > 0 {
		what = append(what, "exec")
	}
	// A remote server is network reach to its vendor, whatever the
	// release's own egress says.
	if slices.ContainsFunc(c.MCPServers, manifest.MCPServer.IsRemote) {
		what = append(what, "a remote MCP server")
	}
	// A package server's own egress is network reach from its container,
	// whatever the release's own egress says; any host is called out.
	for _, srv := range c.MCPServers {
		switch {
		case srv.IsRemote() || len(srv.Egress) == 0:
		case slices.Contains(srv.Egress, "*"):
			what = append(what, "UNRESTRICTED network egress for MCP server "+srv.SourceID()+"@"+srv.Version)
		default:
			what = append(what, "network egress for MCP server "+srv.SourceID()+"@"+srv.Version)
		}
	}
	// A target host is a machine the person names, worked as the account
	// they sign in with.
	switch n := len(c.TargetHosts); {
	case n == 1:
		what = append(what, "a target host")
	case n > 1:
		what = append(what, "target hosts")
	}
	// A browser reaches the sites the person names and the declared hosts,
	// or the public web.
	if c.Browser != nil {
		what = append(what, "a web browser")
	}
	return what
}

// confirmedCapabilitiesWords is confirmedCapabilities as one phrase:
// "network egress, exec and a target host".
func confirmedCapabilitiesWords(c manifest.Capabilities) string {
	what := confirmedCapabilities(c)
	if len(what) < 2 {
		return strings.Join(what, "")
	}
	return strings.Join(what[:len(what)-1], ", ") + " and " + what[len(what)-1]
}

// isZeroCapabilities reports a completely undeclared capability block.
func isZeroCapabilities(c manifest.Capabilities) bool {
	return c.Network.Egress.IsZero() &&
		c.Filesystem.Read.IsZero() && c.Filesystem.Write.IsZero() &&
		c.Exec.IsZero() &&
		len(c.Secrets) == 0 && len(c.DataAccess) == 0 &&
		len(c.MCPServers) == 0 && len(c.Knowledge) == 0 && len(c.TargetHosts) == 0 && c.Browser == nil
}

// browserLine says what a browser line lets the release do, as the
// install consent shows it.
func browserLine(b *manifest.Browser) string {
	var where []string
	if len(b.Sites) > 0 {
		where = append(where, "the site you name for "+renderList(b.Sites)+" and its subdomains")
	}
	if len(b.Hosts) > 0 {
		where = append(where, renderList(b.Hosts))
	}
	if b.IsPublic() {
		where = append(where, "ANY public site (policy-gated)")
	}
	line := "a web browser that can open " + strings.Join(where, ", and ")
	if len(b.Secrets) > 0 {
		line += "; it can type " + renderList(b.Secrets) + " into the site's sign-in forms (it never sees them)"
	}
	return line
}

// renderEgress prints an egress declaration; "*" is flagged as policy-gated.
func renderEgress(e manifest.Egress) string {
	if e.None || len(e.Hosts) == 0 {
		return "none"
	}
	s := strings.Join(e.Hosts, ", ")
	for _, h := range e.Hosts {
		if h == "*" {
			return s + " (unrestricted — policy-gated)"
		}
	}
	return s
}

// renderFSAccess prints one filesystem direction.
func renderFSAccess(a manifest.FSAccess) string {
	switch {
	case a.IsZero(), a.Mode == manifest.FSModeNone:
		return "none"
	case len(a.Paths) > 0:
		return "paths: " + strings.Join(a.Paths, ", ")
	default:
		return string(a.Mode)
	}
}

// renderExec prints an exec declaration.
func renderExec(x manifest.ExecCapability) string {
	if x.IsZero() || x.None || len(x.Commands) == 0 {
		return "none"
	}
	return "commands: " + strings.Join(x.Commands, ", ")
}

// renderList prints a string list or "none".
func renderList(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}

// renderDataAccess prints the semantic data categories or "none".
func renderDataAccess(items []manifest.DataAccess) string {
	if len(items) == 0 {
		return "none"
	}
	out := make([]string, len(items))
	for i, d := range items {
		out[i] = string(d)
	}
	return strings.Join(out, ", ")
}

// modeStrings converts a mode set for joining.
func modeStrings[T ~string](modes []T) []string {
	out := make([]string, len(modes))
	for i, m := range modes {
		out[i] = string(m)
	}
	return out
}

// modesLabel renders a mode set for one-line output ("autonomous,interactive");
// an empty set reads "none".
func modesLabel[T ~string](modes []T) string {
	if len(modes) == 0 {
		return "none"
	}
	return strings.Join(modeStrings(modes), ",")
}

// parseModes reads a comma-separated --modes value into the manifest's
// closed set, in the order given, refusing repeats and unknown names.
func parseModes(s string) ([]manifest.Mode, error) {
	var out []manifest.Mode
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		m := manifest.Mode(part)
		if !m.Valid() {
			return nil, fmt.Errorf("unknown mode %q in --modes (want autonomous, interactive, or autonomous,interactive)", part)
		}
		if slices.Contains(out, m) {
			return nil, fmt.Errorf("--modes lists %s twice", m)
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, errors.New("--modes must name at least one run mode: autonomous, interactive")
	}
	return out, nil
}

// remoteServerLine is a remote server's consent line: its address, how
// it is signed in to, and what it may be asked; it runs at its vendor,
// reached through the runner image's bridge.
func remoteServerLine(srv manifest.MCPServer) string {
	line := "mcpServer: " + srv.URL + " (remote, at its vendor; auth: " + string(srv.Auth)
	switch srv.Auth {
	case manifest.AuthSecret:
		line += ", sends " + renderList(srv.Secrets) + " as its bearer"
	case manifest.AuthOAuth:
		if len(srv.Scopes) > 0 {
			line += ", scopes: " + renderList(srv.Scopes)
		}
	}
	line += ")"
	if len(srv.Tools) > 0 {
		line += " (tools: " + renderList(srv.Tools) + ")"
	} else {
		line += " (tools: every tool it serves)"
	}
	return line
}
