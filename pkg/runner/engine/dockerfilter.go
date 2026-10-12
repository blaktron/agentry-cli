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
	"github.com/blaktron/agentry-cli/runnerimage/hostlink"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
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

// The pinned gateway's two fixed relay network names (DR-22): the filter
// gives it the run's own networks under them (agentry-cli#220). A gateway
// outside the filter creates the shared networks of these names itself,
// as Docker's own MCP Toolkit does; the runner did not create them, other
// gateways use them, and so no teardown or sweep removes them
// (agentry-cli#458).
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

// maxSocketPath is below the kernel's sun_path limit (108 on Linux)
// with room for the terminator.
const maxSocketPath = 103

// filterSocketPath places the filter's socket where the daemon can mount
// it. Under runhost the CLI runs in a container whose /tmp the daemon
// cannot see; the runner home is the path it shares with the host, at the
// same place inside and out. The run's own directory is under the
// installed QuickAgent's, too deep for a socket path there (measured on
// dev's runhost, 2026-09-25), so the socket is <runner home>/s/<run>.sock.
// A home too long even for that falls back (startDaemonFilter) to a short
// private directory under the system's temp directory; were that ever
// inside such a container, the daemon would bind a directory in its place
// and the gateway's health probe fails the run: it never runs unfiltered.
func filterSocketPath(rd *runDirs, runID, runnerHome string) (string, error) {
	if runnerHome == "" {
		return filepath.Join(rd.root, "docker.sock"), nil
	}
	dir := filepath.Join(runnerHome, "s")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", Classify(ExitGateway, fmt.Errorf("docker filter directory: %w", err))
	}
	return filepath.Join(dir, runID+".sock"), nil
}

// startDaemonFilter serves the run's filter and returns the socket the
// gateway mounts. The bind roots are the gateway's own resolved lists
// (MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS and …_WRITABLE_PATHS): the filter
// enforces for a compromised gateway what the gateway enforces for itself.
func startDaemonFilter(rd *runDirs, runID string, gwEnv map[string]string, lim policy.Limits, plan *spec.Plan, runnerHome string) (*sockfilter.Filter, string, error) {
	sock, err := filterSocketPath(rd, runID, runnerHome)
	if err != nil {
		return nil, "", err
	}
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
	// The relay networks are the run's own, and exist only for a run
	// whose servers need relays; the gateway reaches them under the fixed
	// names it knows (agentry-cli#220).
	var shared []string
	var egress map[string][]string
	var names map[string]string
	if relaysNeeded(plan) {
		internal, out := relayNetworks(runID)
		shared = []string{internal}
		egress = map[string][]string{out: relayImages}
		names = map[string]string{relayNetwork: internal, relayEgressNetwork: out}
	}
	// The web tools' server runs in the runner image (the compiler grants
	// the tools to no package with exec, so no exec image).
	var webNet, webImage, webSpec, webResolv string
	var webCmd []string
	if plan.Capabilities.WebTools {
		webNet, webImage = webNetwork(runID), plan.Proxy.Image
		webCmd = spec.ToolsCmd()
		webSpec = filepath.Join(rd.cfg, "gateway", "toolspec.json")
		if len(plan.WebResolvers()) > 0 {
			webResolv = filepath.Join(rd.cfg, "gateway", resolvConfFile)
		}
	}
	// Under a runtime with the host's name servers, the relays run under
	// it too, reading the run's own resolv.conf (agentry-cli#337); each is
	// created with both its networks, so none is connected after start.
	var relayResolv string
	if relaysSandboxed(plan) {
		relayResolv = filepath.Join(rd.cfg, "gateway", resolvConfFile)
	}
	f, err := sockfilter.Start(sockfilter.Config{
		RunID:           runID,
		Label:           runLabel,
		OwnerLabel:      gatewayOwnerLabel,
		Listen:          sock,
		Upstream:        DockerSocket,
		BindRoots:       pathList(gwEnv["MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS"]),
		WritableRoots:   pathList(gwEnv["MCP_GATEWAY_DOCKER_BIND_ALLOW_WRITABLE_PATHS"]),
		SharedNetworks:  shared,
		EgressNetworks:  egress,
		NetworkNames:    names,
		RelayHosts:      append(relayHosts(plan), rd.hostRelays()...),
		WebNetwork:      webNet,
		WebImage:        webImage,
		WebCmd:          webCmd,
		WebSpec:         webSpec,
		WebSpecTarget:   spec.ToolSpecMount,
		WebResolvConf:   webResolv,
		RelayResolvConf: relayResolv,
		ToolsImage:      plan.ToolsImage(),
		ToolsCmd:        spec.ToolsCmd(),
		ToolsTmpfs:      toolsTmpfs(plan),
		ToolsShared:     toolsShared(plan, rd),
		Limits: sockfilter.Limits{
			MemoryBytes: mem,
			NanoCPUs:    int64(lim.CPUs) * 1e9,
			Pids:        int64(lim.Pids),
		},
		Runtime: plan.Runtime(),
		Log:     logf,
	})
	if err != nil {
		closeDaemonFilter(rd)
		return nil, "", Classify(ExitGateway, fmt.Errorf("start the gateway's docker filter: %w", err))
	}
	return f, sock, nil
}

// toolsTmpfs is what the docker filter mounts into the tools server: its
// /tmp always (agentry-cli#599), and the scratch folder when the plan
// grants it (agentry-cli#729), each a tmpfs of spec.ToolsTmpfsBytes that
// anyone may write (a declared command may run as nobody).
func toolsTmpfs(plan *spec.Plan) map[string]string {
	opts := fmt.Sprintf("size=%d,mode=1777", spec.ToolsTmpfsBytes)
	m := map[string]string{spec.ToolsTmpMount: opts}
	if plan.Capabilities.Scratch {
		m[spec.ScratchMount] = opts
	}
	return m
}

// toolsShared is the one bind the tools server may have beneath its scratch
// tmpfs: the host link's copy folder (agentry-cli#861), for a plan with a
// host link.
func toolsShared(plan *spec.Plan, rd *runDirs) map[string]string {
	if plan.HostLink == nil || !plan.Capabilities.Scratch {
		return nil
	}
	return map[string]string{hostlink.FilesPath: rd.hostFilesDir()}
}

// gatewayOwnerLabel marks what the gateway made through its filter, and so
// may touch: the runner's own run containers carry runLabel alone.
const gatewayOwnerLabel = "agentry.gateway.run"

// relayHosts are where a relay may be pointed (sockfilter.Config.
// RelayHosts): the run's declared servers' egress entries, the agent's own
// egress hosts on 443, the port the compiler gives the tools server's fetch
// (agentry-cli#346), and the review grant's TypeSafe API on 443. Empty when
// nothing declares egress, which admits no relay.
func relayHosts(plan *spec.Plan) []string {
	var hosts []string
	for _, srv := range plan.MCPServers {
		for _, h := range srv.Egress {
			if h != "" {
				hosts = append(hosts, h)
			}
		}
	}
	if plan.Capabilities.EgressMode == "hosts" {
		for _, h := range plan.Capabilities.EgressHosts {
			if h != "" {
				hosts = append(hosts, h+":443")
			}
		}
	}
	// The review grant's one host, which the compiler gives the tools
	// server as its allowHosts (agentry-cli#503).
	if plan.Capabilities.ReviewTools {
		hosts = append(hosts, toolsrv.ReviewHost+":443")
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

// relayNetworks are this run's two relay networks, which the filter
// gives the gateway under the pinned gateway's fixed names: the internal
// one its declared servers reach their relays on, and the relays' way out.
func relayNetworks(runID string) (internal, egress string) {
	return "agentry-" + runID + "-relays", "agentry-" + runID + "-relays-out"
}

// makeRelayNetworks makes the run's two relay networks and queues their
// removal, which runs after the gateway and its relays are gone.
func makeRelayNetworks(ctx context.Context, d dockerx.Client, runID string, td *teardown) error {
	internal, egress := relayNetworks(runID)
	labels := runLabels(runID)
	for _, n := range []struct {
		name     string
		internal bool
	}{{internal, true}, {egress, false}} {
		if err := d.NetworkCreate(ctx, n.name, n.internal, labels); err != nil {
			return Classify(ExitGateway, fmt.Errorf("create the run's relay network %s: %w", n.name, err))
		}
		name := n.name
		td.add(func(c context.Context) { _ = d.NetworkRemove(c, name) })
		td.networks = append(td.networks, name)
	}
	return nil
}

// webNetwork is the run's own network with a way out of the host for the
// web tools' server (sockfilter.Config.WebNetwork, agentry-cli#345).
func webNetwork(runID string) string { return "agentry-" + runID + "-web" }

// makeWebNetwork makes the run's web network and queues its removal, which
// runs after the gateway and the tools server are gone.
func makeWebNetwork(ctx context.Context, d dockerx.Client, runID string, td *teardown) error {
	name := webNetwork(runID)
	if err := d.NetworkCreate(ctx, name, false, runLabels(runID)); err != nil {
		return Classify(ExitGateway, fmt.Errorf("create the run's web network %s: %w", name, err))
	}
	td.add(func(c context.Context) { _ = d.NetworkRemove(c, name) })
	td.networks = append(td.networks, name)
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
	if rd != nil && rd.web {
		line += "; for the Drafter's web tools one more network leaves the host, the run's own " + webNetwork(rd.runID) +
			", which only the run's tools server joins, in its one shape (its image's entrypoint, the tools role, the run's spec read-only, no other network, no link), and nothing execs into or writes into"
	}
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
