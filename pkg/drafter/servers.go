package drafter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/mcpinstall"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry/pkg/manifest"
)

// ServerResolver resolves one declared MCP server's pin to its registry
// metadata (mcpinstall.Resolved, whose DeclaredRuntime is the release's own
// engines.node or requires_python).
type ServerResolver func(context.Context, manifest.MCPServer) (mcpinstall.Resolved, error)

// registryResolver resolves against the registries a run would install
// from. A test replaces it, so no unit test reaches a package registry.
var registryResolver = func(regs policy.MCPRegistries) ServerResolver {
	client := &http.Client{Timeout: 15 * time.Second}
	return func(ctx context.Context, srv manifest.MCPServer) (mcpinstall.Resolved, error) {
		res, err := mcpinstall.ResolveAll(ctx, client, regs, []manifest.MCPServer{srv})
		if err != nil {
			return mcpinstall.Resolved{}, err
		}
		if len(res) != 1 {
			return mcpinstall.Resolved{}, fmt.Errorf("%s is not a package a registry publishes", srv.SourceID())
		}
		return res[0], nil
	}
}

// serverCheck is one pin's resolution, kept for the post-step's run so a
// repair turn that keeps a pin does not fetch it again.
type serverCheck struct {
	r   mcpinstall.Resolved
	err error
}

// checkServers is the Drafter's rule on declared MCP servers
// (agentry-cli#609): each pinned release must be one the run's registry
// publishes, and its own runtime range must admit the runtime a run gives
// it (policy.DefaultNodeVersion for npm, policy.DefaultPythonVersion for
// PyPI). A release it excludes installs and then fails every tool call
// (the OSV server 0.1.15 on Node 22, agentry-cli#608), or is refused at
// install. A registry that does not answer is this computer's reason, not
// the draft's: the pin is noted, never refused, and after the first such
// failure the rest are noted without waiting on it again. Only the first
// manifest.MaxMCPServers are read; validate refuses more.
func (p PostStep) checkServers(ctx context.Context, d *Draft) {
	d.serverProblems, d.serversUnchecked = nil, nil
	servers := declaredServers(d.Manifest)
	if len(servers) > manifest.MaxMCPServers {
		servers = servers[:manifest.MaxMCPServers]
	}
	// A remote server has no package to look up (agentry-cli#663).
	servers = mcpinstall.Packages(servers)
	if len(servers) == 0 {
		return
	}
	resolve := p.Servers
	if resolve == nil {
		regs := p.Registries
		if regs.NPM == "" && regs.PyPI == "" {
			regs = policy.Default().MCPRegistries
		}
		resolve = registryResolver(regs)
	}
	if p.Log != nil {
		_, _ = fmt.Fprintf(p.Log, "Checking that the draft's %d MCP server(s) run on Node %s and Python %s, from their package registry.\n",
			len(servers), policy.DefaultNodeVersion, policy.DefaultPythonVersion)
	}
	down := false
	for i, srv := range servers {
		pin := fmt.Sprintf("%s@%s", srv.SourceID(), srv.Version)
		c, seen := p.serverChecks[pin]
		if !seen {
			if down || ctx.Err() != nil {
				d.serversUnchecked = append(d.serversUnchecked, pin)
				continue
			}
			c.r, c.err = resolve(ctx, srv)
			if p.serverChecks != nil {
				p.serverChecks[pin] = c
			}
		}
		var pe *mcpinstall.PinError
		switch {
		case errors.As(c.err, &pe):
			d.serverProblems = append(d.serverProblems, fmt.Sprintf("capabilities.mcpServers[%d]: %s: %s, so a run could not install it; pin a version its registry publishes, or another server",
				i, pin, pe.Error()))
		case c.err != nil:
			down = true
			d.serversUnchecked = append(d.serversUnchecked, pin)
		default:
			if why := mcpinstall.RuntimeMismatch(c.r, policy.DefaultNodeVersion, policy.DefaultPythonVersion); why != "" {
				d.serverProblems = append(d.serverProblems, fmt.Sprintf("capabilities.mcpServers[%d]: %s: %s, so its runs would fail; pin a version whose range admits the run's runtime (the package registry lists each version's), or another server",
					i, pin, why))
			}
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
		fmt.Fprintf(&b, "## MCP servers not checked\n\nWhether these run on the runner's Node %s and Python %s was not checked, because their package registry could not be read from this computer: %s.\n",
			policy.DefaultNodeVersion, policy.DefaultPythonVersion, strings.Join(d.serversUnchecked, ", "))
	}
	d.composeNotes(b.String())
}
