package engine

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// The gateway's relay network (agentry-cli#108, DR-22). docker/mcp-gateway
// v0.43.3 launches every declared server with egress hosts on one internal
// network with a fixed name, docker-mcp-proxies-int, and puts one relay
// sidecar per declared host there (pkg/gateway/proxies/l4proxy.go,
// l7proxy.go); the name is not configurable, so every gateway on the host
// — each agentry run's, and the user's own `docker mcp gateway` — shares
// it, and a server that declared egress to host A could reach another
// gateway's relay by name. The contract says a server reaches its declared
// hosts and nothing else, so the runner refuses to share: a run whose plan
// needs relays does not start while another gateway's relays are on the
// network, and the relays its own gateway started — named in the
// gateway's log — are removed at teardown, on every path, so they never
// linger to refuse the next run (agentry-cli#106).
//
// Since agentry-cli#220 this is the path only where the gateway's docker
// filter does not run (Docker Desktop, agentry-cli#322). On Linux the
// filter gives the gateway the run's own two relay networks under the
// fixed names (dockerfilter.go): runs with relays share a host without
// reaching each other's, and neither the lock nor the refusal is taken.
// The relays are still reaped by name at teardown, on every path.

// RelayLabel is the label the gateway puts on every relay sidecar. The MCP
// server survey reads it too (scripts/mcp-server-survey).
const RelayLabel = "docker-mcp-proxy=true"

// relaysNeeded reports whether the gateway will start relays for this
// plan: a declared server with egress hosts (the gateway's L4 relays), or
// the agent's own declared egress hosts, which the tools server's fetch
// reaches through the gateway's L7 relay (agentry-cli#346).
func relaysNeeded(plan *spec.Plan) bool { return len(relayHosts(plan)) > 0 }

// refuseForeignRelays is the check before the gateway starts: relay
// sidecars on the host now belong to another gateway (this run's has not
// started), and a declared server launched beside them would reach them.
func refuseForeignRelays(ctx context.Context, d dockerx.Client) error {
	names, err := d.PSByLabel(ctx, RelayLabel)
	if err != nil {
		return Classify(ExitGateway, fmt.Errorf("list relay sidecars on the shared relay network: %w", err))
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	return Classify(ExitGateway, fmt.Errorf(
		"another MCP gateway's relay sidecars are on the shared relay network (docker-mcp-proxies-int): %s. A declared server of this run reaches its declared hosts through relays there and would reach these too, so the run does not start. Wait for the run that owns them to end; relays a gateway left behind are removed with `docker rm -f <name>`",
		strings.Join(names, ", ")))
}

// relayStartRE matches the gateway's own line for each relay it started,
// in the two exact shapes v0.43.3 writes: "starting l4 proxy
// docker-mcp-l4proxy-<id> for <host>, ports …" at the start of a line, and
// "    - Starting l7 proxy docker-mcp-l7proxy-<id> for <hosts>" (a gateway
// bump re-checks them). Exact on purpose: a server's stderr reaches the
// same log, but every line of it carries the gateway's "- <server>: "
// prefix (pkg/logs/prefix.go prefixes after every newline), so no server
// can write a line in either shape and have a container of its choosing
// removed.
var relayStartRE = regexp.MustCompile(`(?m)^(?:starting l4 proxy (docker-mcp-l4proxy-\S+) for \S+, ports |[ \t]+- Starting l7 proxy (docker-mcp-l7proxy-\S+) for )`)

// RelaysStartedBy returns the relay sidecars the gateway's log says it
// started, in log order, each once. Exported for the MCP server survey,
// which starts the gateway itself and reaps its relays the same way
// (scripts/mcp-server-survey, agentry-cli#658).
func RelaysStartedBy(logs string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range relayStartRE.FindAllStringSubmatch(logs, -1) {
		name := m[1] + m[2] // one group matches per alternative
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// relayReaper removes, at teardown, the relays this run's gateway started.
// The names come from the gateway's log, read twice: once the gateway has
// served its health probe (startInfra), when the relays it starts while
// loading its configuration are named and nothing of the agent's has run;
// and again at teardown, because a relay is also started later — the l4
// relay for a declared server's egress host starts when that server first
// connects, which is after the probe, and a run that read the log once
// left it behind on the shared network, where the next run's
// refuseForeignRelays then refused to start (agentry-cli#184, found by
// runner-r2's mcp-osv fixture). The second read happens after the agent
// ran, so a tool call — logged with its name verbatim (--log-calls) — is
// the one way a server could plant a line there; every name is therefore
// confirmed against the relay label before removal, and where the filter
// runs against the run's own gateway label too, so a planted name reaches
// no relay but the run's own, and those are removed whichever read named
// them.
// By name and confirmed relay, never by label alone, which on the shared
// network would take another gateway's. Best-effort, like every teardown
// step.
type relayReaper struct {
	names []string
	// runID, where the gateway's docker filter runs, confines the reap to
	// relays its gateway made (labelled agentry.gateway.run=<run>): runs
	// with relays share a host since agentry-cli#220, and a name planted
	// in the log must not reach another run's relay.
	runID string
}

// snapshot records the relays the gateway's log names now.
func (r *relayReaper) snapshot(logs string) {
	r.names = RelaysStartedBy(logs)
}

func (r *relayReaper) reap(ctx context.Context, d dockerx.Client, gwName string) {
	names := append([]string{}, r.names...)
	if logs, err := d.Logs(ctx, gwName, dockerx.LogsAll); err == nil {
		seen := make(map[string]bool, len(names))
		for _, n := range names {
			seen[n] = true
		}
		for _, n := range RelaysStartedBy(logs) {
			if !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
	}
	if len(names) == 0 {
		return
	}
	labelled, err := d.PSByLabel(ctx, RelayLabel)
	if err != nil {
		return
	}
	isRelay := make(map[string]bool, len(labelled))
	for _, n := range labelled {
		isRelay[n] = true
	}
	if r.runID != "" {
		ours, err := d.PSByLabel(ctx, gatewayOwnerLabel+"="+r.runID)
		if err != nil {
			return
		}
		mine := make(map[string]bool, len(ours))
		for _, n := range ours {
			mine[n] = true
		}
		for n := range isRelay {
			if !mine[n] {
				delete(isRelay, n)
			}
		}
	}
	for _, name := range names {
		if isRelay[name] {
			_ = d.RemoveForce(ctx, name)
		}
	}
}
