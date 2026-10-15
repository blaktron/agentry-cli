// Declared-MCP-server wiring for `agentry run`: resolve the manifest pins
// (offline when the signature's store already holds matching resolutions
// — the recorded pin is the trust decision), install into the store as
// part of the run's install step, and widen the guided profile to the
// servers' secret names.
package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/mcpinstall"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// serverResolutions is ensureServerResolutions, a seam for tests: the
// configured MCP registries must be https, which a test cannot serve with
// a certificate the resolver trusts.
var serverResolutions = ensureServerResolutions

// ensureServerResolutions returns the resolutions for every declared
// server. When the store already records a resolution for each pin, those
// are reused verbatim — no network — so cached installs run offline;
// otherwise the registries are consulted (and EnsureInstalled later
// refuses if a recorded pin disagrees with what came back).
func ensureServerResolutions(ctx context.Context, errOut io.Writer, serversDir string,
	pol *policy.Policy, m *manifest.Manifest,
) ([]mcpinstall.Resolved, error) {
	// A remote server is neither resolved nor installed.
	declared := mcpinstall.Packages(m.Capabilities.MCPServers)
	if len(declared) == 0 {
		return nil, nil
	}
	stored := make([]mcpinstall.Resolved, 0, len(declared))
	for _, srv := range declared {
		meta, err := mcpinstall.LoadMeta(serversDir, mcpinstall.IDFor(srv))
		if err != nil {
			return nil, err
		}
		if meta == nil || meta.Package != srvPackage(srv) || meta.Version != srv.Version {
			stored = nil
			break
		}
		stored = append(stored, *meta)
	}
	if len(stored) == len(declared) && len(declared) > 0 {
		return stored, nil
	}
	_, _ = fmt.Fprintf(errOut, "resolving %d declared MCP server(s) against the configured registries\n", len(declared))
	return mcpinstall.ResolveAll(ctx, nil, pol.MCPRegistries, declared)
}

func srvPackage(srv manifest.MCPServer) string {
	if srv.NPM != "" {
		return srv.NPM
	}
	return srv.PyPI
}

// allSecretNames is the union of agent-level and per-server secret names —
// what guided setup collects and the engine must resolve.
func allSecretNames(m *manifest.Manifest) []string {
	set := map[string]bool{}
	for _, n := range m.Capabilities.Secrets {
		set[n] = true
	}
	for _, srv := range m.Capabilities.MCPServers {
		for _, n := range srv.Secrets {
			set[n] = true
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// vulnerabilityVerdict turns the registry's statuses into the run's
// decision: the receipt line, and an error when a declared server carries
// an advisory the gate rates high or critical (the advisory's own severity
// from a registry that does not serve the gate's rating) and nothing
// overrides it. An override — the flag or the policy — is named in the
// line so the receipt shows the run was allowed on purpose; an unreachable
// registry is named too, so a clean line never hides a check that did not
// happen.
func vulnerabilityVerdict(statuses []mcpinstall.VulnStatus, registry string, allowFlag, allowPolicy bool) (string, error) {
	line := "checked against " + registry + ": " + mcpinstall.Summary(statuses)
	var refusing []mcpinstall.VulnStatus
	for _, s := range statuses {
		if s.Refuses() {
			refusing = append(refusing, s)
		}
	}
	if len(refusing) == 0 {
		return line, nil
	}
	switch {
	case allowFlag:
		return line + "; run allowed by --allow-vulnerable-mcp", nil
	case allowPolicy:
		return line + "; run allowed by policy mcpServers.allowVulnerable", nil
	}
	parts := make([]string, 0, len(refusing))
	names := make([]string, 0, len(refusing))
	for _, s := range refusing {
		names = append(names, s.Server)
		p := s.Ref + " has " + s.Describe(s.Refusing())
		if fixed := s.FixedIn(); len(fixed) > 0 {
			p += ", fixed in " + strings.Join(fixed, ", ")
		}
		parts = append(parts, p)
	}
	return line, receipt.WithReason(fmt.Errorf("declared MCP server refused by the registry's vulnerability scan: %s — pin a fixed version, or override with --allow-vulnerable-mcp or policy mcpServers.allowVulnerable",
		strings.Join(parts, "; ")), receipt.ReasonMCPVulnerable, "", names...)
}
