// Declared-MCP-server wiring for `agentry run` (RUNNER-PLAN-3): resolve
// the manifest pins (offline when the signature's store already holds
// matching resolutions — the recorded pin is the trust decision), install
// into the store as part of the run's install step, and widen the guided
// profile to the servers' secret names.
package main

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/mcpinstall"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// ensureServerResolutions returns the resolutions for every declared
// server. When the store already records a resolution for each pin, those
// are reused verbatim — no network — so cached installs run offline;
// otherwise the registries are consulted (and EnsureInstalled later
// refuses if a recorded pin disagrees with what came back).
func ensureServerResolutions(ctx context.Context, errOut io.Writer, serversDir string,
	pol *policy.Policy, m *manifest.Manifest,
) ([]mcpinstall.Resolved, error) {
	declared := m.Capabilities.MCPServers
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
