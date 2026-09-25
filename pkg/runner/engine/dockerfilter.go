package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/sockfilter"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// The gateway's daemon socket is a per-run filter (hosted runs in
// production D7 a, agentry-cli#219, DR-36). The compiled plan mounts the
// daemon's socket in the gateway, the privilege §R4 documents; on Linux
// the engine serves a filter in the run's directory and mounts that in
// its place, so what the gateway can reach is this run's containers,
// networks and volumes and nothing else on the host (package sockfilter
// says exactly what passes). Elsewhere (Docker Desktop on macOS and
// Windows) a host socket cannot be bind-mounted into the daemon's VM, so
// the gateway keeps the daemon's socket and the receipt says so.

// dockerFilterLog is the filter's decision log in the receipts.
const dockerFilterLog = "docker-filter.log"

// filterSupported says whether this platform runs the filter.
var filterSupported = runtime.GOOS == "linux"

// The pinned gateway's two fixed relay networks (DR-22), which the filter
// lets a run use without its label until agentry-cli#220 names them per
// run: the internal one its declared servers reach their relays on, and
// the relays' way out, which only the relays may join.
const (
	relayNetwork       = "docker-mcp-proxies-int"
	relayEgressNetwork = "docker-mcp-proxies-ext"
)

// relayImages are the relays of the pinned gateway (docker/mcp-gateway
// v0.43.3 names them docker/mcp-l4proxy:v1 and docker/mcp-l7proxy:v1),
// pinned by the digests those tags resolved to on 2026-09-25: a moved tag
// is refused, and a new gateway pin brings new digests with it.
var relayImages = []string{
	"docker/mcp-l4proxy@sha256:121b87decc25cda901dbd4ffbd20b116fffbd0fbeecc827c228fa45094a9934c",
	"docker/mcp-l7proxy@sha256:ef8fd775fdf8ad060af897018c0db3c52229c493cfde437e86c754f3fcd59233",
}

// relayNetworkLabels are the labels the gateway puts on its own relay
// networks, kept when the runner makes them.
var relayNetworkLabels = map[string]string{"docker-mcp": "true"}

// maxSocketPath is below the kernel's sun_path limit (108 on Linux)
// with room for the terminator.
const maxSocketPath = 103

// startDaemonFilter serves the run's filter and returns the socket the
// gateway mounts. The bind roots are the gateway's own resolved lists
// (MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS and …_WRITABLE_PATHS): the filter
// enforces for a compromised gateway what the gateway enforces for itself.
func startDaemonFilter(rd *runDirs, runID string, gwEnv map[string]string, lim policy.Limits, servers []spec.MCPServerSpec) (*sockfilter.Filter, string, error) {
	// The socket lives in the run's directory: under runhost the CLI runs
	// in a container whose /tmp the daemon cannot see, and the runner home
	// is the path shared with the host. A home too long for a socket path
	// falls back to a short private directory under the system's temp
	// directory; were that ever inside such a container, the daemon would
	// bind a directory in its place and the gateway's health probe fails
	// the run — it never runs unfiltered.
	sock := filepath.Join(rd.root, "docker.sock")
	if len(sock) > maxSocketPath {
		dir, err := os.MkdirTemp("", "agentry-gw-")
		if err != nil {
			return nil, "", Classify(ExitGateway, fmt.Errorf("docker filter directory: %w", err))
		}
		rd.filterDir = dir
		sock = filepath.Join(dir, "docker.sock")
	}
	logf, err := os.OpenFile(filepath.Join(rd.receiptsDir, dockerFilterLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // a receipt the run owns
	if err != nil {
		closeDaemonFilter(rd)
		return nil, "", Classify(ExitGateway, fmt.Errorf("docker filter log: %w", err))
	}
	rd.filterLog = logf
	mem, err := memoryBytes(lim.Memory)
	if err != nil {
		closeDaemonFilter(rd)
		return nil, "", Classify(ExitPolicy, err)
	}
	// The shared relay networks are the filter's to admit only for a run
	// whose servers need relays; any other run's gateway has no business
	// on them, where another run's relays could be reached.
	var shared []string
	var egress map[string][]string
	if relaysNeeded(servers) {
		shared = []string{relayNetwork}
		egress = map[string][]string{relayEgressNetwork: relayImages}
	}
	f, err := sockfilter.Start(sockfilter.Config{
		RunID:          runID,
		Label:          runLabel,
		OwnerLabel:     gatewayOwnerLabel,
		Listen:         sock,
		Upstream:       DockerSocket,
		BindRoots:      pathList(gwEnv["MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS"]),
		WritableRoots:  pathList(gwEnv["MCP_GATEWAY_DOCKER_BIND_ALLOW_WRITABLE_PATHS"]),
		SharedNetworks: shared,
		EgressNetworks: egress,
		RelayHosts:     relayHosts(servers),
		Limits: sockfilter.Limits{
			MemoryBytes: mem,
			NanoCPUs:    int64(lim.CPUs) * 1e9,
			Pids:        int64(lim.Pids),
		},
		Log: logf,
	})
	if err != nil {
		closeDaemonFilter(rd)
		return nil, "", Classify(ExitGateway, fmt.Errorf("start the gateway's docker filter: %w", err))
	}
	return f, sock, nil
}

// gatewayOwnerLabel marks what the gateway made through its filter, and so
// may touch: the runner's own run containers carry runLabel alone.
const gatewayOwnerLabel = "agentry.gateway.run"

// relayHosts are the run's declared servers' egress entries, the only
// places a relay may be pointed (sockfilter.Config.RelayHosts). Empty when
// no server declares egress, which admits no relay.
func relayHosts(servers []spec.MCPServerSpec) []string {
	var hosts []string
	for _, srv := range servers {
		for _, h := range srv.Egress {
			if h != "" {
				hosts = append(hosts, h)
			}
		}
	}
	return hosts
}

func pathList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ":") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// memoryBytes reads docker's size forms ("512m", "2g", "1.5g", "1073741824").
func memoryBytes(s string) (int64, error) {
	in := s
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0, nil
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'b':
		s = s[:len(s)-1]
	case 'k':
		mult, s = 1<<10, s[:len(s)-1]
	case 'm':
		mult, s = 1<<20, s[:len(s)-1]
	case 'g':
		mult, s = 1<<30, s[:len(s)-1]
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("docker.limits.memory %q is not a size", in)
	}
	return int64(n * float64(mult)), nil
}

// ensureRelayNetworks makes the gateway's two shared relay networks, as
// the gateway would, when a run needs relays: the filter lets no gateway
// create or remove them, so none can make the internal one routable or
// take the way out away from another run.
func ensureRelayNetworks(ctx context.Context, d dockerx.Client) error {
	for _, n := range []struct {
		name     string
		internal bool
	}{{relayNetwork, true}, {relayEgressNetwork, false}} {
		if err := d.NetworkCreate(ctx, n.name, n.internal, relayNetworkLabels); err != nil && !strings.Contains(err.Error(), "already exists") {
			return Classify(ExitGateway, fmt.Errorf("create the relay network %s: %w", n.name, err))
		}
	}
	return nil
}

// throughFilter points the gateway's daemon-socket mount at the filter.
func throughFilter(mounts []dockerx.Mount, sock string) []dockerx.Mount {
	out := make([]dockerx.Mount, len(mounts))
	for i, m := range mounts {
		if m.Source == DockerSocket {
			m.Source = sock
			m.Relabel = false
		}
		out[i] = m
	}
	return out
}

// daemonLine is the receipt's account of how the gateway reaches the
// daemon, with what the filter refused.
func daemonLine(rd *runDirs) string {
	if !filterSupported {
		return "the gateway holds the docker daemon's own socket: the per-run filter runs on Linux only, where hosted runs execute; Docker Desktop is agentry-cli#322"
	}
	line := "the gateway reaches the docker daemon only through this run's filter: every container, network and volume it creates carries the run's label, " +
		"it may touch no container, network, volume or exec without it (the pinned gateway's shared relay networks excepted, DR-22; only its digest-pinned relays join the one that leaves the host), " +
		"lists and events show only the run's, create bodies are checked as the daemon reads them, and it may not create a privileged, host-namespace, device, " +
		"port-publishing, runtime-choosing or security-weakened container, bind a host path outside the run's directories or write outside its writable ones, " +
		"or exceed the run's memory, CPU and process limits; every decision is in receipts/" + dockerFilterLog
	if rd != nil && rd.filter != nil {
		if n := rd.filter.Refused(); n > 0 {
			line += fmt.Sprintf("; %d request(s) refused", n)
		}
	}
	return line
}

// withRefusals names the filter's refusals in an error that ended a run,
// since a refusal is the likeliest cause of a gateway that did not start.
func withRefusals(err error, rd *runDirs) error {
	if err == nil || rd == nil || rd.filter == nil {
		return err
	}
	n := rd.filter.Refused()
	if n == 0 {
		return err
	}
	note := fmt.Sprintf(" (the gateway's docker filter refused %d request(s); see receipts/%s)", n, dockerFilterLog)
	var ce *ClassifiedError
	if errors.As(err, &ce) {
		return Classify(ce.Code, fmt.Errorf("%w%s", ce.Err, note))
	}
	return fmt.Errorf("%w%s", err, note)
}

// closeDaemonFilter stops the filter after the gateway, and closes its log.
func closeDaemonFilter(rd *runDirs) {
	if rd.filter != nil {
		_ = rd.filter.Close()
	}
	if rd.filterLog != nil {
		_ = rd.filterLog.Close()
	}
	if rd.filterDir != "" {
		_ = os.RemoveAll(rd.filterDir)
	}
}
