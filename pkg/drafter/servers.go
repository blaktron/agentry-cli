package drafter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
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
//
// It also holds each pin to the drafting guide's server lists
// (agentry-cli#660, plan drafter-knowledge.md §4.6): a pin Does not run
// here covers (ServerList.NotRunHere), or one whose direct dependencies,
// from the metadata the resolution already fetched, include a package its
// dependencies section lists, is a rules line, so it goes to the repair
// turn and keepFirst holds it. An npm install script is a sign for the
// notes, not a rule. No request is added: the list is embedded, and the
// dependencies come with the resolution. The notes say what admitted each
// server (noteServerProblems).
func (p PostStep) checkServers(ctx context.Context, d *Draft) {
	d.serverProblems, d.serversUnchecked, d.serverSigns, d.serversAdmitted = nil, nil, nil, nil
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
	// The embedded list is held by TestTheEmbeddedServerListLoads; were it
	// unreadable, the check would go on without it, as before M3.
	list, _ := serverList()
	down := false
	for i, srv := range servers {
		pin := fmt.Sprintf("%s@%s", srv.SourceID(), srv.Version)
		eco, pkg := packageOf(srv)
		problems := len(d.serverProblems)
		var runs ServerEntry
		var runsListed, runsSeen, notRun bool
		if list != nil {
			if e, ok := list.NotRunHere(eco, pkg, srv.Version); ok {
				notRun = true
				d.serverProblems = append(d.serverProblems, notRunHereLine(i, pin, e))
			}
			runs, runsListed, runsSeen = list.RunsHereEntry(eco, pkg, srv.Version)
		}
		c, seen := p.serverChecks[pin]
		if !seen {
			if down || ctx.Err() != nil {
				d.serversUnchecked = append(d.serversUnchecked, pin)
				d.admit(problems, pin, runs, runsListed, runsSeen)
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
			// A listed dependency holds whatever list the server is on (the
			// page says so); an install script is no sign for a version seen
			// to run without it.
			if list != nil && !notRun {
				if dep, ok := list.ListedDependency(eco, c.r.Dependencies); ok {
					d.serverProblems = append(d.serverProblems, dependencyLine(i, pin, dep))
				}
				if len(c.r.InstallScripts) > 0 && !runsSeen {
					d.serverSigns = append(d.serverSigns, fmt.Sprintf("%s has an npm %s script, which a run's install skips (npm installs run with --ignore-scripts); check before a run that it works without it.",
						pin, strings.Join(c.r.InstallScripts, " and ")))
				}
			}
		}
		d.admit(problems, pin, runs, runsListed, runsSeen)
	}
}

// serverList is the drafting guide's lists, read once from the embedded
// pack (LoadServerList, which TestTheEmbeddedServerListLoads holds).
var serverList = sync.OnceValues(func() (*ServerList, error) { return LoadServerList(FS()) })

// fixWithAnotherServer ends a Does not run here line: what the repair can do.
const fixWithAnotherServer = "no setting makes it run; pin another server (the drafting guide's servers/runs-here.md lists servers seen to run), or do the task without one"

// notRunHereLine is the rules line for a pin Does not run here covers.
func notRunHereLine(i int, pin string, e ServerEntry) string {
	clause, _ := NotRunReason(e.Reason)
	return fmt.Sprintf("capabilities.mcpServers[%d]: %s: does not run here: %s: it %s (seen at %s on %s; %s); %s",
		i, pin, e.Reason, clause, e.Versions, e.Runtime, e.Evidence, fixWithAnotherServer)
}

// dependencyLine is the rules line for a pin whose direct dependency the
// dependencies section lists.
func dependencyLine(i int, pin string, dep DependencyEntry) string {
	clause, _ := NotRunReason(dep.Reason)
	return fmt.Sprintf("capabilities.mcpServers[%d]: %s: does not run here: %s: it depends directly on %s, which %s (%s); %s",
		i, pin, dep.Reason, dep.Package, clause, dep.Evidence, fixWithAnotherServer)
}

// admit records what admitted a pin the check found no problem with since
// problems (the count of rules lines before the pin was checked): a Runs
// here entry for its version, or the Drafter's judgement with the guide.
func (d *Draft) admit(problems int, pin string, runs ServerEntry, listed, seen bool) {
	if len(d.serverProblems) > problems {
		return
	}
	switch {
	case seen:
		d.serversAdmitted = append(d.serversAdmitted, fmt.Sprintf("%s: on Runs here, seen to start and list its tools on %s (%s).", pin, runs.Runtime, runs.Evidence))
	case listed:
		d.serversAdmitted = append(d.serversAdmitted, fmt.Sprintf("%s: judged with the drafting guide; Runs here lists version %s of it, not this one.", pin, runs.Versions))
	default:
		d.serversAdmitted = append(d.serversAdmitted, pin+": on neither list, so judged with the drafting guide, not seen to run.")
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
//
// It also says what admitted each server it found no problem with, and the
// signs to check before a run (checkServers).
func (d *Draft) noteServerProblems() {
	if len(d.serverProblems) == 0 && len(d.serversUnchecked) == 0 && len(d.serverSigns) == 0 && len(d.serversAdmitted) == 0 {
		return
	}
	var b strings.Builder
	section := func(head string, lines []string) {
		if len(lines) == 0 {
			return
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(head)
		for _, l := range lines {
			b.WriteString("- " + l + "\n")
		}
	}
	section("## MCP servers to fix before a run\n\nThis draft declares a server version its runs cannot run:\n\n", d.serverProblems)
	if len(d.serversUnchecked) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "## MCP servers not checked\n\nWhether these run on the runner's Node %s and Python %s was not checked, because their package registry could not be read from this computer: %s.\n",
			policy.DefaultNodeVersion, policy.DefaultPythonVersion, strings.Join(d.serversUnchecked, ", "))
	}
	section("## MCP servers to check before a run\n\n", d.serverSigns)
	section("## What admitted each MCP server\n\n", d.serversAdmitted)
	d.composeNotes(b.String())
}
