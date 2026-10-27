package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/hostlink"
	"github.com/blaktron/agentry-cli/runnerimage/sockfilter"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// The gateway's daemon socket is a per-run filter. The compiled plan
// mounts the daemon's socket in the
// gateway, the privilege documents; where the daemon shares this machine's
// kernel the engine serves a filter in the run's directory, in front of the
// engine's own socket, and mounts that in its place, so what the
// gateway can reach is this run's containers, networks and volumes and
// nothing else on the host (package sockfilter says exactly what
// passes). Where the daemon runs in a VM a host socket cannot be
// bind-mounted into it, so the same filter runs in a container beside the
// gateway instead (filtersidecar.go); placement.go decides which.

// dockerFilterLog is the filter's decision log in the receipts.
const dockerFilterLog = "docker-filter.log"

// filterSupported says whether the gateway reaches the daemon through the
// filter here: the CLI serves it, or a container of the run's own does
// (placement.go).
var filterSupported = runtime.GOOS == "linux" || runtime.GOOS == "darwin" || runtime.GOOS == "windows"

// The pinned gateway's two fixed relay network names: the filter gives it
// the run's own networks under them. A gateway outside the filter creates
// the shared networks of these names itself, as Docker's own MCP Toolkit
// does; the runner did not create them, other gateways use them, and so
// no teardown or sweep removes them.
const (
	relayNetwork       = "docker-mcp-proxies-int"
	relayEgressNetwork = "docker-mcp-proxies-ext"
)

// relayImages are the relays of the pinned gateway (docker/mcp-gateway
// v0.43.3 names them docker/mcp-l4proxy:v1 and docker/mcp-l7proxy:v1),
// pinned by the digests those tags resolve to: a moved tag
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
// installed QuickAgent's, too deep for a socket path there, so the
// socket is <runner home>/s/<run>.sock.
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
func startDaemonFilter(d dockerx.Client, rd *runDirs, runID string, gwEnv map[string]string, lim policy.Limits, plan *spec.Plan, runnerHome string, place *filterPlacement, upstream string) (*sockfilter.Filter, string, error) {
	cfg, err := filterConfig(rd, runID, gwEnv, lim, plan, place)
	if err != nil {
		return nil, "", err
	}
	// The CLI serves this filter and has the engine's CLI at hand: each
	// container the gateway creates under the run's runtime is checked
	// natively before the gateway sees it.
	cfg.Verify = runtimeVerifier(d)
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
	cfg.Listen, cfg.Upstream, cfg.Log = sock, upstream, logf
	f, err := sockfilter.Start(cfg)
	if err != nil {
		closeDaemonFilter(rd)
		return nil, "", Classify(ExitGateway, fmt.Errorf("start the gateway's docker filter: %w", err))
	}
	return f, sock, nil
}

// shareRoot is the share folder in the gateway's spelling, as a root: none
// when the run has no share.
func (rd *runDirs) shareRoot() []string {
	if rd.share == "" {
		return nil
	}
	return []string{rd.gatewayPath(rd.share)}
}

// mirrorRoot is the share's mirror in the gateway's spelling, as a root:
// none when no server of the run sees it.
func (rd *runDirs) mirrorRoot() []string {
	if rd.shareMirror == nil {
		return nil
	}
	return []string{rd.gatewayPath(rd.shareMirror.dir)}
}

// dependencyDBRoot is the folder of OSV's databases in the gateway's
// spelling, as a root: none when the run has no dependency scan.
func (rd *runDirs) dependencyDBRoot() []string {
	if rd.dependencyDB == "" {
		return nil
	}
	return []string{rd.gatewayPath(rd.dependencyDB)}
}

// skillsRoot is the agent's skills folder in the gateway's spelling, as a
// root: none when the run has no skills.
func (rd *runDirs) skillsRoot() []string {
	if rd.skills == "" {
		return nil
	}
	return []string{rd.gatewayPath(rd.skills)}
}

// toolsOnlyShare is the share folder as a root the tools server alone may
// bind at all: unless a server sees the folder itself, which only the
// Coding agent's roster gives one.
func toolsOnlyShare(rd *runDirs, plan *spec.Plan) []string {
	if spec.SharesAs(plan.MCPServers, spec.ServerShareLive) {
		return nil
	}
	return rd.shareRoot()
}

// filterConfig is the run's filter policy, wherever it is served: in the
// CLI (startDaemonFilter) or in the filter container (startFilterSidecar).
// place is the run's engine and placement; nil is Docker Engine.
func filterConfig(rd *runDirs, runID string, gwEnv map[string]string, lim policy.Limits, plan *spec.Plan, place *filterPlacement) (sockfilter.Config, error) {
	mem, err := memoryBytes(lim.Memory)
	if err != nil {
		return sockfilter.Config{}, Classify(ExitPolicy, err)
	}
	eng := rd.engine
	if place != nil {
		eng = place.engine
	}
	// The relay networks are the run's own, and exist only for a run
	// whose servers need relays; the gateway reaches them under the fixed
	// names it knows.
	var shared []string
	var egress map[string][]string
	var names map[string]string
	if relaysNeeded(plan) {
		internal, out := relayNetworks(runID)
		shared = []string{internal}
		egress = map[string][]string{out: relayImages}
		names = map[string]string{relayNetwork: internal, relayEgressNetwork: out}
	}
	// The tools server that reaches past the host joins the run's web
	// network: the web tools' in the runner image (the compiler grants the
	// tools to no package with exec), or, for unrestricted egress, the
	// tools server in whichever image runs it, the run's exec image or the
	// runner's.
	var webNet, webImage, webSpec, webResolv string
	var webCmd []string
	if plan.Capabilities.ToolsReachPastHost() {
		webNet, webImage = webNetwork(runID), plan.ToolsImage()
		webCmd = spec.ToolsCmd()
		webSpec = filepath.Join(rd.cfg, "gateway", "toolspec.json")
		if len(plan.WebResolvers()) > 0 {
			webResolv = filepath.Join(rd.cfg, "gateway", resolvConfFile)
		}
	}
	// Under a runtime with the host's name servers, the relays run under
	// it too, reading the run's own resolv.conf; each is created with both
	// its networks, so none is connected after start.
	var relayResolv string
	if relaysSandboxed(plan) {
		relayResolv = filepath.Join(rd.cfg, "gateway", resolvConfFile)
	}
	// Beneath the run's cfg folder, only what the catalog binds there, and
	// only into the entries that bind it.
	bindRoots, writableRoots, heldCfg, err := runCfgBinds(rd,
		pathList(gwEnv["MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS"]), pathList(gwEnv["MCP_GATEWAY_DOCKER_BIND_ALLOW_WRITABLE_PATHS"]))
	if err != nil {
		return sockfilter.Config{}, err
	}
	// An image server's own volume, into the entry that mounts it alone.
	heldVolumes, err := runVolumeHolders(rd, plan)
	if err != nil {
		return sockfilter.Config{}, err
	}
	return sockfilter.Config{
		RunID:         runID,
		Label:         runLabel,
		OwnerLabel:    gatewayOwnerLabel,
		BindRoots:     bindRoots,
		WritableRoots: writableRoots,
		// The share folder: written by the tools server alone, and read by
		// no other container but a server the Coding agent's roster lets
		// see it; the agent's skills, by the tools server alone. The share,
		// its mirror and the share outbox reach no
		// container but the tools server that joins a network.
		ToolsWritableRoots: rd.shareRoot(),
		ToolsOnlyRoots:     slices.Concat(toolsOnlyShare(rd, plan), rd.skillsRoot(), rd.dependencyDBRoot()),
		OfflineRoots:       slices.Concat(rd.shareRoot(), rd.mirrorRoot(), rd.shareOutRoot()),
		HeldRoots:          heldCfg,
		HeldVolumes:        heldVolumes,
		SharedNetworks:     shared,
		EgressNetworks:     egress,
		NetworkNames:       names,
		RelayHosts:         slices.Concat(relayHosts(plan), rd.hostRelays(), rd.browseProxyRelay()),
		RelayPins:          rd.browseProxyPin(),
		WebNetwork:         webNet,
		WebImage:           webImage,
		WebCmd:             webCmd,
		WebSpec:            webSpec,
		WebSpecTarget:      spec.ToolSpecMount,
		WebResolvConf:      webResolv,
		RelayResolvConf:    relayResolv,
		ToolsImage:         plan.ToolsImage(),
		ToolsCmd:           spec.ToolsCmd(),
		ToolsTmpfs:         toolsTmpfs(plan),
		ToolsShared:        toolsShared(plan, rd),
		ToolsVolumes:       toolsVolumes(rd),
		AppLinkVolume:      rd.appLinkRelay,
		AppLinkImage:       appLinkImageOf(plan, rd),
		AppLinkCmd:         appLinkCmdOf(plan, rd),
		BrowserImage:       browserImageOf(plan),
		BrowserUser:        rd.personUser(),
		BrowserTmpfs:       browserTmpfsOf(plan),
		BrowserNetwork:     browserNetworkOf(plan, runID),
		BrowserNetworkFor:  browserNetworkForOf(plan, runID),
		BrowseProxy:        rd.browseProxyName(),
		Limits: sockfilter.Limits{
			MemoryBytes: mem,
			NanoCPUs:    int64(lim.CPUs) * 1e9,
			Pids:        int64(lim.Pids),
		},
		Runtime: plan.Runtime(),
		// podman's API differs from Docker Engine's where the filter must
		// follow the engine (sockfilter.Config.Podman), and under a runtime
		// on an SELinux podman host gVisor refuses a labelled container.
		Podman:            eng.Kind == dockerx.KindPodman,
		SELinuxUnconfined: eng.SandboxUnlabelled(plan.Runtime()),
	}, nil
}

// runtimeVerifier reads back, from the engine's own CLI, the runtime a
// container the gateway created was given, and refuses one the engine put
// under another: the container goes, and the mismatch names the class
// (sockfilter.Config.Verify). nil for a client that cannot inspect.
func runtimeVerifier(d dockerx.Client) func(context.Context, string, string, string) error {
	cr, ok := d.(dockerx.Creator)
	if !ok {
		return nil
	}
	return func(ctx context.Context, id, class, want string) error {
		got, err := cr.ContainerRuntime(ctx, id)
		if err != nil {
			_ = d.RemoveForce(ctx, id)
			return fmt.Errorf("read the runtime the %s the gateway launched (%s) was created with: %w", class, shortID(id), err)
		}
		if got != want {
			_ = d.RemoveForce(ctx, id)
			return &dockerx.RuntimeMismatch{Class: class + " the gateway launched", Name: shortID(id), Want: want, Got: got}
		}
		return nil
	}
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// toolsTmpfs is what the docker filter mounts into the tools server: its
// /tmp always, and the scratch folder when the plan grants it, each a
// tmpfs of spec.ToolsTmpfsBytes that anyone may write (a declared
// command may run as nobody).
func toolsTmpfs(plan *spec.Plan) map[string]string {
	opts := fmt.Sprintf("size=%d,mode=1777", spec.ToolsTmpfsBytes)
	m := map[string]string{spec.ToolsTmpMount: opts}
	if plan.Capabilities.Scratch {
		m[spec.ScratchMount] = opts
	}
	return m
}

// toolsShared is the one bind the tools server may have beneath its scratch
// tmpfs: the host link's copy folder, for a plan with a host link.
func toolsShared(plan *spec.Plan, rd *runDirs) map[string]string {
	if plan.HostLink == nil || !plan.Capabilities.Scratch {
		return nil
	}
	return map[string]string{hostlink.FilesPath: rd.hostFilesDir()}
}

// browserImageOf and browserTmpfsOf are the docker filter's browser class
// for a plan with a browser server, and nothing without.
func browserImageOf(plan *spec.Plan) string {
	if plan.Browser == nil {
		return ""
	}
	return plan.Browser.Image
}

func browserNetworkOf(plan *spec.Plan, runID string) string {
	if plan.Browser == nil {
		return ""
	}
	return browserNetwork(runID)
}

func browserNetworkForOf(plan *spec.Plan, runID string) string {
	if plan.Browser == nil {
		return ""
	}
	internal, _ := relayNetworks(runID)
	return internal
}

func browserTmpfsOf(plan *spec.Plan) map[string]string {
	if plan.Browser == nil {
		return nil
	}
	return browserTmpfs()
}

// gatewayOwnerLabel marks what the gateway made through its filter, and so
// may touch: the runner's own run containers carry runLabel alone.
const gatewayOwnerLabel = "agentry.gateway.run"

// relayHosts are where a relay may be pointed (sockfilter.Config.
// RelayHosts): the run's declared servers' egress entries, the agent's own
// egress hosts on 443, the port the compiler gives the tools server's
// fetch, the review grant's TypeSafe API on 443, and the calendar feeds'
// hosts on 443. Empty when nothing declares egress, which admits no relay.
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
	// server as its allowHosts.
	if plan.Capabilities.ReviewTools {
		hosts = append(hosts, toolsrv.ReviewHost+":443")
	}
	// The calendar feeds' hosts, which the compiler gives the tools server
	// as its allowHosts.
	for _, h := range plan.Capabilities.Calendar.Hosts() {
		hosts = append(hosts, h+":443")
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

// webNetwork is the run's own network with a way out of the host for a
// tools server that reaches past it (sockfilter.Config.WebNetwork).
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
		return "the gateway holds the docker daemon's own socket: the per-run filter runs on Linux and on Docker Desktop (macOS, Windows), not on this platform"
	}
	line := "the gateway reaches the docker daemon only through this run's filter: every container, network and volume it creates carries the run's label, " +
		"it may touch no container, network, volume or exec without it (the pinned gateway's shared relay networks excepted; only its digest-pinned relays join the one that leaves the host), " +
		"lists and events show only the run's, create bodies are checked as the daemon reads them, and it may not create a privileged, host-namespace, device, " +
		"port-publishing, runtime-choosing or security-weakened container, bind a host path outside the run's directories or write outside its writable ones, " +
		"or exceed the run's memory, CPU and process limits; a container that names no network is created on none, never on the daemon's default bridge; " +
		"every decision is in receipts/" + dockerFilterLog
	if rd != nil && (rd.web || rd.anyEgress) {
		why := "the Drafter's web tools"
		switch {
		case rd.web:
		case rd.coderEgress:
			why = "the Coding agent's open egress (its roster's egress entry)"
		default:
			why = "the tools server's unrestricted egress (manifest \"*\")"
		}
		line += "; for " + why + " one more network leaves the host, the run's own " + webNetwork(rd.runID) +
			", which only the run's tools server joins, in its one shape (its image's entrypoint, the tools role, the run's spec read-only, no other network, no link), and nothing execs into or writes into"
	}
	if rd != nil {
		if n := refusedBy(rd); n > 0 {
			line += fmt.Sprintf("; %d request(s) refused", n)
		}
	}
	return line
}

// refusedBy is how many requests the run's filter refused, wherever it ran.
func refusedBy(rd *runDirs) int {
	switch {
	case rd.filter != nil:
		return rd.filter.Refused()
	case rd.filterSidecar != "":
		f, err := os.Open(rd.filterSidecar)
		if err != nil {
			return 0
		}
		defer func() { _ = f.Close() }()
		return sockfilter.CountRefused(f)
	}
	return 0
}

// withRefusals names the filter's refusals in an error that ended a run,
// since a refusal is the likeliest cause of a gateway that did not start.
func withRefusals(err error, rd *runDirs) error {
	if err == nil || rd == nil {
		return err
	}
	n := refusedBy(rd)
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

// runtimeMismatch is the first container the gateway created under another
// runtime than the policy's, as the run's filter refused it, or nil.
func (rd *runDirs) runtimeMismatch() error {
	if rd == nil || rd.filter == nil {
		return nil
	}
	return rd.filter.RuntimeMismatch()
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

// appLinkImageOf and appLinkCmdOf name the app link's container for the
// filter, where its relay volume is the run's (startAppLinkRelay): the
// runner image and the catalog entry's command.
func appLinkImageOf(plan *spec.Plan, rd *runDirs) string {
	if rd.appLinkRelay == "" {
		return ""
	}
	return plan.Proxy.Image
}

func appLinkCmdOf(plan *spec.Plan, rd *runDirs) []string {
	if rd.appLinkRelay == "" {
		return nil
	}
	return compile.AppLinkCommand(plan.Capabilities.AppLink)
}

// toolsVolumes are the volumes not the run's that its tools server may
// mount: a persistent agent's data volume and its memory volume, by name.
func toolsVolumes(rd *runDirs) []string {
	var out []string
	for _, v := range []string{rd.data, rd.memory} {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}
