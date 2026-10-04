package drafter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/mcpinstall"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry/pkg/manifest"
)

// ServerResolver resolves one declared MCP server's pin to its registry
// metadata (mcpinstall.Resolved, whose Runtime is the release's own
// engines.node or requires_python).
type ServerResolver func(context.Context, manifest.MCPServer) (mcpinstall.Resolved, error)

// defaultServerResolver resolves against the default npm and PyPI
// registries, the ones a run installs from unless its operator names
// others. A test replaces it, so no unit test reaches a registry.
var defaultServerResolver ServerResolver = func(ctx context.Context, srv manifest.MCPServer) (mcpinstall.Resolved, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	res, err := mcpinstall.ResolveAll(ctx, &http.Client{Timeout: 15 * time.Second}, policy.Default().MCPRegistries, []manifest.MCPServer{srv})
	if err != nil {
		return mcpinstall.Resolved{}, err
	}
	return res[0], nil
}

// checkServers is the Drafter's rule on declared MCP servers
// (agentry-cli#609): each pinned release's own runtime range must admit
// the runtime a run gives it (policy.DefaultNodeVersion for npm,
// policy.DefaultPythonVersion for PyPI). A release it excludes installs
// and then fails every tool call (the OSV server 0.1.15 on Node 22,
// agentry-cli#608), or is refused at install. A pin whose metadata cannot
// be read is this computer's reason, not the draft's: it is noted, never
// refused.
func (p PostStep) checkServers(ctx context.Context, d *Draft) {
	d.serverProblems, d.serversUnchecked = nil, nil
	resolve := p.Servers
	if resolve == nil {
		resolve = defaultServerResolver
	}
	for i, srv := range declaredServers(d.Manifest) {
		r, err := resolve(ctx, srv)
		if err != nil {
			d.serversUnchecked = append(d.serversUnchecked, fmt.Sprintf("%s@%s", srv.SourceID(), srv.Version))
			continue
		}
		if why := mcpinstall.RuntimeMismatch(r, policy.DefaultNodeVersion, policy.DefaultPythonVersion); why != "" {
			d.serverProblems = append(d.serverProblems, fmt.Sprintf("capabilities.mcpServers[%d]: %s@%s: %s, so its runs would fail; pin a version whose range admits the run's runtime (the package registry lists each version's), or another server",
				i, srv.SourceID(), srv.Version, why))
		}
	}
}

// declaredServers is the draft manifest's capabilities.mcpServers; nil when
// it declares none or does not parse (validate says so).
func declaredServers(raw json.RawMessage) []manifest.MCPServer {
	var m struct {
		Capabilities struct {
			MCPServers []manifest.MCPServer `json:"mcpServers"`
		} `json:"capabilities"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m.Capabilities.MCPServers
}

// noteServerProblems says in the notes that the finished draft still
// declares a server its runs cannot run, or one whose runtime could not be
// checked, so the person knows before running it.
func (d *Draft) noteServerProblems() {
	if len(d.serverProblems) == 0 && len(d.serversUnchecked) == 0 {
		return
	}
	var b strings.Builder
	if len(d.serverProblems) > 0 {
		b.WriteString("## MCP servers to fix before a run\n\nThis draft declares a server version its runs cannot run:\n\n")
		for _, p := range d.serverProblems {
			b.WriteString("- " + p + "\n")
		}
	}
	if len(d.serversUnchecked) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "## MCP servers not checked\n\nWhether these run on the runner's Node %s and Python %s could not be checked, because their registry did not answer: %s.\n",
			policy.DefaultNodeVersion, policy.DefaultPythonVersion, strings.Join(d.serversUnchecked, ", "))
	}
	d.composeNotes(b.String())
}
