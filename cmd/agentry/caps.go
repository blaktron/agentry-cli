// Capability-summary rendering (§5.4 install-time display): the declared
// security contract is shown before anything is written to disk, and
// network egress or exec declarations require confirmation (§11 install).
package main

import (
	"strings"

	"github.com/blaktron/agentry/pkg/manifest"
)

// capabilityLines renders the §5.4 capability block as stable, indentable
// lines. A fully-zero block renders as the single pure-text line.
func capabilityLines(c manifest.Capabilities) []string {
	if isZeroCapabilities(c) {
		return []string{"none declared (pure text agent)"}
	}
	return []string{
		"network.egress: " + renderEgress(c.Network.Egress),
		"filesystem.read: " + renderFSAccess(c.Filesystem.Read),
		"filesystem.write: " + renderFSAccess(c.Filesystem.Write),
		"exec: " + renderExec(c.Exec),
		"secrets: " + renderList(c.Secrets),
		"dataAccess: " + renderDataAccess(c.DataAccess),
	}
}

// capabilityNeedsConfirm reports whether install must ask before proceeding:
// declared network egress or subprocess execution (§11 install row).
func capabilityNeedsConfirm(c manifest.Capabilities) bool {
	return len(c.Network.Egress.Hosts) > 0 || len(c.Exec.Commands) > 0
}

// isZeroCapabilities reports a completely undeclared capability block.
func isZeroCapabilities(c manifest.Capabilities) bool {
	return c.Network.Egress.IsZero() &&
		c.Filesystem.Read.IsZero() && c.Filesystem.Write.IsZero() &&
		c.Exec.IsZero() &&
		len(c.Secrets) == 0 && len(c.DataAccess) == 0
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

// defaultTarget maps an agent kind to its §5.6 default install target.
func defaultTarget(kind manifest.Kind) string {
	switch kind {
	case manifest.KindMCPServer:
		return manifest.InterfaceMCP
	case manifest.KindPromptAgent:
		return manifest.InterfaceAgentsMD
	default: // manifest.KindSkill and anything future
		return manifest.InterfaceClaudeSkill
	}
}
