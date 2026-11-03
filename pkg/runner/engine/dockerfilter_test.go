package engine

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/pkg/runner/targethost"
	"github.com/blaktron/agentry-cli/runnerimage/hostlink"
	"github.com/blaktron/agentry-cli/runnerimage/sockfilter"
)

// withFilter sets filterSupported for one test. The receipt's lines are
// pure functions of it, so a test can check both platforms' wording on
// every OS; a test that runs the engine asserts the branch its own OS
// takes instead.
func withFilter(t *testing.T, on bool) {
	t.Helper()
	was := filterSupported
	filterSupported = on
	t.Cleanup(func() { filterSupported = was })
}

func TestMemoryBytesReadsDockersSizes(t *testing.T) {
	for in, want := range map[string]int64{
		"": 0, "2g": 2 << 30, "2G": 2 << 30, "512m": 512 << 20, "1.5g": 3 << 29, "64k": 64 << 10, "1073741824": 1 << 30, "100b": 100,
	} {
		got, err := memoryBytes(in)
		if err != nil || got != want {
			t.Errorf("memoryBytes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"g", "lots", "-1g", "0"} {
		if _, err := memoryBytes(bad); err == nil {
			t.Errorf("memoryBytes(%q) accepted", bad)
		}
	}
}

func TestThroughFilterSwapsOnlyTheDaemonSocket(t *testing.T) {
	in := []dockerx.Mount{
		{Source: "/run/cfg", Target: "/cfg", Relabel: true},
		{Source: DockerSocket, Target: DockerSocket, Relabel: true},
	}
	out := throughFilter(in, "/run/docker.sock")
	if out[0] != in[0] {
		t.Errorf("another mount changed: %+v", out[0])
	}
	if out[1].Source != "/run/docker.sock" || out[1].Target != DockerSocket || out[1].Relabel {
		t.Errorf("socket mount = %+v, want the filter at the daemon's path, not relabelled", out[1])
	}
	if in[1].Source != DockerSocket {
		t.Error("throughFilter changed its argument")
	}
}

// A refusal is named in the receipt and in the error that ended the run.
func TestRefusalsReachTheOutcome(t *testing.T) {
	if !filterSupported {
		t.Skip("the filter runs on Linux only")
	}
	dir, err := os.MkdirTemp("", "df") // short: a socket path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	rd := &runDirs{root: dir, receiptsDir: dir}
	f, sock, err := startDaemonFilter(&dockerx.Fake{}, rd, "cafe0000", map[string]string{"MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS": dir}, policy.Limits{Memory: "2g", CPUs: 1, Pids: 512}, &spec.Plan{}, "", nil, DockerSocket)
	if err != nil {
		t.Fatal(err)
	}
	rd.filter = f
	defer closeDaemonFilter(rd)
	if sock != filepath.Join(dir, "docker.sock") {
		t.Errorf("socket at %s, want the run's directory", sock)
	}
	if strings.Contains(daemonLine(rd), "refused") {
		t.Error("a filter that refused nothing says it refused")
	}
	// One refusal: a request the gateway never needs.
	if code := filterRequest(t, sock, "POST", "/build"); code != http.StatusForbidden {
		t.Fatalf("POST /build through the filter: %d", code)
	}
	if !strings.HasSuffix(daemonLine(rd), "; 1 request(s) refused") {
		t.Errorf("daemon line = %q", daemonLine(rd))
	}
	err = withRefusals(Classify(ExitGateway, errors.New("gateway health probe failed")), rd)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitGateway || !strings.Contains(err.Error(), "docker filter refused 1 request(s)") {
		t.Errorf("error = %v", err)
	}
	log, _ := os.ReadFile(filepath.Join(dir, dockerFilterLog))
	if !strings.Contains(string(log), "refuse POST /build") {
		t.Errorf("filter log = %q", log)
	}

	// A filter in a container of the run's: the decisions it logged are counted.
	side := filepath.Join(dir, "side.log")
	if err := os.WriteFile(side, []byte("2026-11-03T10:00:00Z refuse POST /build\n2026-11-03T10:00:01Z allow GET /_ping\n2026-11-03T10:00:02Z refuse DELETE /volumes/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	viaSidecar := &runDirs{filterSidecar: side}
	if !strings.HasSuffix(daemonLine(viaSidecar), "; 2 request(s) refused") {
		t.Errorf("daemon line = %q", daemonLine(viaSidecar))
	}
	if err := withRefusals(errors.New("gateway health probe failed"), viaSidecar); !strings.Contains(err.Error(), "docker filter refused 2 request(s)") {
		t.Errorf("error = %v", err)
	}
	if err := withRefusals(errors.New("gateway health probe failed"), &runDirs{}); err.Error() != "gateway health probe failed" {
		t.Errorf("a run whose filter refused nothing changed the error: %v", err)
	}
}

// TestTheFilterIsGivenTheRunsLimitsNetworksAndRuntime: the policy the run's
// filter takes is the run's own: its limits in the daemon's units, its
// runtime, the engine's kind, the relay networks only a plan that needs
// relays has, and the roots of the folders the run has, spelled the
// gateway's way; a folder the run lacks is no root at all.
func TestTheFilterIsGivenTheRunsLimitsNetworksAndRuntime(t *testing.T) {
	p, _ := fixtureWith(t, func(pol *policy.Policy) { pol.Docker.Runtime = "runsc" })
	rd := runDirsAt(t.TempDir())
	lim := policy.Limits{Memory: "1536m", CPUs: 3, Pids: 321}
	cfg, err := filterConfig(rd, "run1", nil, lim, p.Plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RunID != "run1" || cfg.Label != runLabel || cfg.OwnerLabel != gatewayOwnerLabel {
		t.Errorf("run %q, label %q, owner label %q", cfg.RunID, cfg.Label, cfg.OwnerLabel)
	}
	if cfg.Limits != (sockfilter.Limits{MemoryBytes: 1536 << 20, NanoCPUs: 3_000_000_000, Pids: 321}) {
		t.Errorf("limits %+v", cfg.Limits)
	}
	if cfg.Runtime != "runsc" || cfg.Runtime != p.Plan.Runtime() {
		t.Errorf("runtime %q, want the policy's runsc", cfg.Runtime)
	}
	if cfg.Podman {
		t.Error("Docker Engine was taken for podman")
	}
	internal, out := relayNetworks("run1")
	if !slices.Equal(cfg.SharedNetworks, []string{internal}) || !slices.Equal(cfg.EgressNetworks[out], relayImages) || len(cfg.EgressNetworks) != 1 ||
		cfg.NetworkNames[relayNetwork] != internal || cfg.NetworkNames[relayEgressNetwork] != out || len(cfg.NetworkNames) != 2 {
		t.Errorf("relay networks %v, egress %v, names %v", cfg.SharedNetworks, cfg.EgressNetworks, cfg.NetworkNames)
	}
	if cfg.ToolsImage != p.Plan.ToolsImage() || !slices.Equal(cfg.ToolsCmd, spec.ToolsCmd()) {
		t.Errorf("tools server %q %v", cfg.ToolsImage, cfg.ToolsCmd)
	}
	place := &filterPlacement{engine: dockerx.Engine{Kind: dockerx.KindPodman}}
	if cfg, err = filterConfig(rd, "run1", nil, lim, p.Plan, place); err != nil || !cfg.Podman {
		t.Errorf("podman was not told to the filter: %v", err)
	}

	// A plan that needs no relays has none.
	none := *p.Plan
	none.Capabilities.EgressMode, none.Capabilities.EgressHosts, none.MCPServers = "none", nil, nil
	if cfg, err = filterConfig(rd, "run1", nil, lim, &none, nil); err != nil || len(cfg.SharedNetworks) != 0 || len(cfg.EgressNetworks) != 0 || len(cfg.NetworkNames) != 0 {
		t.Errorf("a plan without relays got networks %v %v %v (%v)", cfg.SharedNetworks, cfg.EgressNetworks, cfg.NetworkNames, err)
	}

	// The run's folders: none of them is a root when the run lacks them.
	if len(cfg.ToolsWritableRoots)+len(cfg.ToolsOnlyRoots)+len(cfg.OfflineRoots) != 0 {
		t.Errorf("a run without a share, a mirror or skills got roots %v %v %v", cfg.ToolsWritableRoots, cfg.ToolsOnlyRoots, cfg.OfflineRoots)
	}
	rd.share, rd.shareMirror, rd.shareOut = "/home/p/share", &shareMirror{dir: "/home/p/mirror"}, "/home/p/outbox"
	rd.skills, rd.dependencyDB = "/home/p/skills", "/home/p/osv"
	if cfg, err = filterConfig(rd, "run1", nil, lim, &none, nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.ToolsWritableRoots, []string{"/home/p/share"}) ||
		!slices.Equal(cfg.ToolsOnlyRoots, []string{"/home/p/share", "/home/p/skills", "/home/p/osv"}) ||
		!slices.Equal(cfg.OfflineRoots, []string{"/home/p/share", "/home/p/mirror", "/home/p/outbox"}) {
		t.Errorf("roots: tools writable %v, tools only %v, offline %v", cfg.ToolsWritableRoots, cfg.ToolsOnlyRoots, cfg.OfflineRoots)
	}

	// The browse proxy is a relay host, pinned to its address.
	proxy := containerName(filepath.Base(rd.root), "browse-proxy")
	rd.browser = &browserRun{proxy: proxy, proxyIP: "10.9.0.4"}
	if cfg, err = filterConfig(rd, "run1", nil, lim, &none, nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.RelayHosts, []string{proxy + ":3128"}) || cfg.RelayPins[proxy] != "10.9.0.4" || cfg.BrowseProxy != proxy {
		t.Errorf("relay hosts %v, pins %v, browse proxy %q", cfg.RelayHosts, cfg.RelayPins, cfg.BrowseProxy)
	}
	rd.browser = nil

	// The tools server's own tmpfs, host link copy folder and volumes.
	rd.data, rd.memory = "agentry-data", "agentry-memory"
	tools := none
	tools.HostLink, tools.Capabilities.Scratch = &spec.HostLinkSpec{}, true
	if cfg, err = filterConfig(rd, "run1", nil, lim, &tools, nil); err != nil {
		t.Fatal(err)
	}
	if len(cfg.ToolsTmpfs) != 2 || cfg.ToolsTmpfs["/tmp"] != "size=268435456,mode=1777" || cfg.ToolsTmpfs["/scratch"] != "size=268435456,mode=1777" {
		t.Errorf("tools tmpfs %v", cfg.ToolsTmpfs)
	}
	if len(cfg.ToolsShared) != 1 || cfg.ToolsShared[hostlink.FilesPath] != rd.hostFilesDir() {
		t.Errorf("tools shared %v, want the host link's copy folder %s", cfg.ToolsShared, rd.hostFilesDir())
	}
	if !slices.Equal(cfg.ToolsVolumes, []string{"agentry-data", "agentry-memory"}) {
		t.Errorf("tools volumes %v", cfg.ToolsVolumes)
	}

	// A plan with a browser: its image, its own network, the relay network it
	// reaches the browse proxy on, and its tmpfs.
	browser := none
	browser.Browser = &spec.BrowserSpec{Image: "browser:img"}
	if cfg, err = filterConfig(rd, "run1", nil, lim, &browser, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.BrowserImage != "browser:img" || cfg.BrowserNetwork != "agentry-run1-browser" || cfg.BrowserNetworkFor != internal ||
		cfg.BrowserUser != rd.personUser() || len(cfg.BrowserTmpfs) != 2 || cfg.BrowserTmpfs["/tmp"] == "" {
		t.Errorf("browser %q on %q, relays on %q, as %q, tmpfs %v", cfg.BrowserImage, cfg.BrowserNetwork, cfg.BrowserNetworkFor, cfg.BrowserUser, cfg.BrowserTmpfs)
	}
	if cfg, err = filterConfig(rd, "run1", nil, lim, &none, nil); err != nil || cfg.BrowserImage != "" || cfg.BrowserNetwork != "" || cfg.BrowserNetworkFor != "" || cfg.BrowserTmpfs != nil {
		t.Errorf("a plan without a browser got one: %+v (%v)", cfg, err)
	}

	// The host link's targets are relay hosts.
	rd.targets = []*targethost.Host{{ID: "web1", Answer: targethost.Answer{Port: 2222}, Address: netip.MustParseAddr("10.1.2.3")}}
	if cfg, err = filterConfig(rd, "run1", nil, lim, &none, nil); err != nil || !slices.Equal(cfg.RelayHosts, []string{"10.1.2.3:2222"}) {
		t.Errorf("relay hosts %v (%v)", cfg.RelayHosts, err)
	}
}

// A container the gateway created is read back for its runtime: the engine's
// answer must be the policy's, and a container it put under another runtime,
// or whose runtime cannot be read, is removed.
func TestTheRuntimeVerifierRemovesWhatTheEngineCreatedUnderAnotherRuntime(t *testing.T) {
	if runtimeVerifier(struct{ dockerx.Client }{&dockerx.Fake{}}) != nil {
		t.Fatal("a client that cannot inspect got a verifier")
	}
	const id = "0123456789abcdef0123"
	create := func(f *dockerx.Fake, rt string) {
		t.Helper()
		if err := f.Create(context.Background(), dockerx.RunSpec{Name: id, Image: "img", Runtime: rt}); err != nil {
			t.Fatal(err)
		}
	}
	f := &dockerx.Fake{}
	create(f, "runsc")
	verify := runtimeVerifier(f)
	if err := verify(context.Background(), id, "tools server", "runsc"); err != nil {
		t.Fatalf("a container under the runtime asked for: %v", err)
	}
	if slices.Contains(f.Ops(), "RemoveForce:"+id) {
		t.Error("a container under the runtime asked for was removed")
	}
	f = &dockerx.Fake{DropRuntime: true, DroppedRuntime: "runc"}
	create(f, "runsc")
	err := runtimeVerifier(f)(context.Background(), id, "tools server", "runsc")
	var mm *dockerx.RuntimeMismatch
	if !errors.As(err, &mm) || mm.Class != "tools server the gateway launched" || mm.Name != id[:12] || mm.Want != "runsc" || mm.Got != "runc" {
		t.Fatalf("a container under another runtime: %v", err)
	}
	if !slices.Contains(f.Ops(), "RemoveForce:"+id) {
		t.Errorf("a container under another runtime was kept: %v", f.Ops())
	}
	f = &dockerx.Fake{}
	err = runtimeVerifier(f)(context.Background(), id, "tools server", "runsc")
	if err == nil || !strings.Contains(err.Error(), "read the runtime the tools server the gateway launched ("+id[:12]+") was created with") {
		t.Fatalf("a container whose runtime cannot be read: %v", err)
	}
	if !slices.Contains(f.Ops(), "RemoveForce:"+id) {
		t.Errorf("a container whose runtime cannot be read was kept: %v", f.Ops())
	}
	if got := shortID("0123456789ab"); got != "0123456789ab" {
		t.Errorf("a twelve-character id was cut to %q", got)
	}
}

// The socket sits under the runner home, the path runhost shares with the
// host, not under the installed QuickAgent's run directory, which is too
// deep for a socket path there.
func TestTheSocketIsUnderTheRunnerHome(t *testing.T) {
	if !filterSupported {
		t.Skip("the filter runs on Linux only")
	}
	home, err := os.MkdirTemp("", "rh") // short, as runhost's is
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	deep := filepath.Join(home, "profiles", "runs-checker-125725", "hosted-smoke", "0.1.0-0123456789ab", "runs", "c8c45187")
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	rd := &runDirs{root: deep, receiptsDir: deep}
	f, sock, err := startDaemonFilter(&dockerx.Fake{}, rd, "c8c45187", nil, policy.Limits{}, &spec.Plan{}, home, nil, DockerSocket)
	if err != nil {
		t.Fatal(err)
	}
	rd.filter = f
	defer closeDaemonFilter(rd)
	if sock != filepath.Join(home, "s", "c8c45187.sock") || rd.filterDir != "" {
		t.Errorf("socket at %s (fallback %q), want under the runner home", sock, rd.filterDir)
	}
	if fi, err := os.Stat(filepath.Join(home, "s")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("socket directory: %v %v", fi, err)
	}
	// Closing the run's filter stops it answering.
	if code := filterRequest(t, sock, "GET", "/_ping"); code == 0 {
		t.Fatal("the filter did not answer before it was closed")
	}
	closeDaemonFilter(rd)
	if c, err := net.Dial("unix", sock); err == nil {
		_ = c.Close()
		t.Error("the filter still answers after it was closed")
	}
}

// A runner home too long for a socket path puts the filter in a short
// private directory, removed with the filter.
func TestALongRunDirectoryFallsBackToAShortSocket(t *testing.T) {
	if !filterSupported {
		t.Skip("the filter runs on Linux only")
	}
	long := filepath.Join(t.TempDir(), strings.Repeat("d", 100))
	if err := os.MkdirAll(long, 0o700); err != nil {
		t.Fatal(err)
	}
	rd := &runDirs{root: long, receiptsDir: long}
	f, sock, err := startDaemonFilter(&dockerx.Fake{}, rd, "cafe0000", nil, policy.Limits{}, &spec.Plan{}, "", nil, DockerSocket)
	if err != nil {
		t.Fatal(err)
	}
	rd.filter = f
	if len(sock) > maxSocketPath || strings.HasPrefix(sock, long) || rd.filterDir == "" {
		t.Errorf("socket %s (%d bytes), fallback %q", sock, len(sock), rd.filterDir)
	}
	closeDaemonFilter(rd)
	if _, err := os.Stat(rd.filterDir); !os.IsNotExist(err) {
		t.Errorf("fallback directory left behind: %v", err)
	}

	// The limit is 103 bytes (a socket path binds in 108, with room for the
	// terminator): a path of exactly 103 stays where it is, one byte more
	// moves.
	base, err := os.MkdirTemp("", "df")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	for _, c := range []struct {
		length   int
		fallback bool
	}{{103, false}, {104, true}} {
		root := filepath.Join(base, strings.Repeat("d", c.length-len("/docker.sock")-len(base)-1))
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		rd := &runDirs{root: root, receiptsDir: root}
		f, sock, err := startDaemonFilter(&dockerx.Fake{}, rd, "cafe0000", nil, policy.Limits{}, &spec.Plan{}, "", nil, DockerSocket)
		if err != nil {
			t.Fatalf("a socket path of %d bytes: %v", c.length, err)
		}
		rd.filter = f
		if (rd.filterDir != "") != c.fallback || (len(sock) == c.length) == c.fallback {
			t.Errorf("a socket path of %d bytes: served at %q (%d bytes), fallback directory %q, want a fallback = %v", c.length, sock, len(sock), rd.filterDir, c.fallback)
		}
		closeDaemonFilter(rd)
	}
}

// A run with relays gets two relay networks of its own, made before the
// gateway and removed after it, and neither takes the host-wide relay lock
// nor refuses another gateway's relays: runs with relays share a host. The
// lock is held by "another run" throughout, and the run goes ahead anyway.
func TestARunWithRelaysHasRelayNetworksOfItsOwn(t *testing.T) {
	if !filterSupported {
		t.Skip("the filter runs on Linux only")
	}
	p, f := serverFixture(t)
	happyFake(f)
	f.LogsByName = map[string]string{"agentry-cafe0000-gateway": "- Those servers are enabled: agentry-tools, mcp-npm-scope-server-x\n> 4 tools listed in 1.2s\n"}
	release, err := holdRelayLock(p.RunnerHome)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatalf("run beside a held relay lock: %v", err)
	}
	ops := f.Ops()
	idx := func(want string) int {
		t.Helper()
		i := slices.IndexFunc(ops, func(op string) bool { return strings.HasPrefix(op, want) })
		if i < 0 {
			t.Fatalf("no %q in %v", want, ops)
		}
		return i
	}
	in := idx("NetworkCreate:agentry-cafe0000-relays(internal=true)")
	out := idx("NetworkCreate:agentry-cafe0000-relays-out(internal=false)")
	gw := idx("RunDetached:agentry-cafe0000-gateway")
	stop := idx("Stop:agentry-cafe0000-gateway")
	rmIn := idx("NetworkRemove:agentry-cafe0000-relays")
	rmOut := idx("NetworkRemove:agentry-cafe0000-relays-out")
	if in > gw || out > gw || rmIn < stop || rmOut < stop {
		t.Errorf("want the relay networks made before the gateway and removed after it stopped; got %v", ops)
	}
	for _, op := range ops {
		if strings.Contains(op, "docker-mcp-proxies") || op == "PSByLabel:"+RelayLabel && slices.Index(ops, op) < gw {
			t.Errorf("the run touched the host's relay network or refused on another gateway's relays: %s", op)
		}
	}
}

// Where runs with relays share a host, the reaper takes a name from the
// gateway's log only if it is a relay this run's gateway made: a name
// planted in the log reaches no other run's relay.
func TestTheReaperTakesOnlyTheRunsOwnRelays(t *testing.T) {
	f := &dockerx.Fake{ByLabel: map[string][]string{
		RelayLabel:                      {"docker-mcp-l4proxy-ours", "docker-mcp-l4proxy-theirs"},
		gatewayOwnerLabel + "=cafe0000": {"docker-mcp-l4proxy-ours", "mcp-server-x"},
	}}
	f.LogsByName = map[string]string{"gw": "starting l4 proxy docker-mcp-l4proxy-ours for a.example, ports 443\nstarting l4 proxy docker-mcp-l4proxy-theirs for b.example, ports 443\n"}
	r := &relayReaper{runID: "cafe0000"}
	r.reap(context.Background(), f, "gw")
	ops := strings.Join(f.Ops(), " ")
	if !strings.Contains(ops, "RemoveForce:docker-mcp-l4proxy-ours") || strings.Contains(ops, "RemoveForce:docker-mcp-l4proxy-theirs") {
		t.Errorf("reaped: %s", ops)
	}
}

// filterRequest sends one request through a filter's socket.
func filterRequest(t *testing.T, sock, method, path string) int {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
	req, _ := http.NewRequest(method, "http://docker"+path, nil)
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	return res.StatusCode
}

// The agent's own egress counts as relays: a run whose release declares
// network.egress and no server gets the run's relay networks, and the
// filter admits the tools server's L7 relay to its hosts on 443 alongside
// the servers' entries.
func TestTheAgentsEgressGetsRelays(t *testing.T) {
	if !filterSupported {
		t.Skip("the docker filter runs on Linux only")
	}
	p, f := fixture(t)
	p.Plan.MCPServers = nil
	if p.Plan.Capabilities.EgressMode != "hosts" || len(p.Plan.Capabilities.EgressHosts) == 0 {
		t.Fatalf("the fixture declares no agent egress: %+v", p.Plan.Capabilities)
	}
	if !relaysNeeded(p.Plan) {
		t.Fatal("a run whose agent declares egress hosts needs no relays")
	}
	hosts := relayHosts(p.Plan)
	if !slices.Contains(hosts, p.Plan.Capabilities.EgressHosts[0]+":443") {
		t.Fatalf("relay hosts %v lack the agent's egress host on 443", hosts)
	}
	happyFake(f)
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatalf("run: %v", err)
	}
	ops := strings.Join(f.Ops(), " ")
	if !strings.Contains(ops, "NetworkCreate:agentry-cafe0000-relays(internal=true)") || !strings.Contains(ops, "NetworkCreate:agentry-cafe0000-relays-out(internal=false)") {
		t.Errorf("an egress-only run made no relay networks of its own: %s", ops)
	}
	// A host link or a browser reaches its target through a relay too, with
	// no egress host or server declared at all.
	for name, tc := range map[string]struct {
		plan *spec.Plan
		want bool
	}{
		"host link only": {&spec.Plan{HostLink: &spec.HostLinkSpec{}}, true},
		"browser only":   {&spec.Plan{Browser: &spec.BrowserSpec{}}, true},
		"neither":        {&spec.Plan{}, false},
	} {
		if got := relaysNeeded(tc.plan); got != tc.want {
			t.Errorf("relaysNeeded for a plan with %s = %v, want %v", name, got, tc.want)
		}
	}
}

// A gateway that could not start the run's tools server carries on with
// the declared servers alone; the run fails at the gateway instead of
// running an agent with none of its tools.
func TestARunWhoseToolsServerDidNotStartFails(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	// A server's own stderr reaches the log only behind "- <name>: ", so it
	// cannot forge this line; the gateway's "Can't list tools" is the same
	// failure.
	p0, f0 := fixture(t)
	happyFake(f0)
	f0.LogsByName = map[string]string{"agentry-cafe0000-gateway": "- mcp-x: > Can't start agentry-tools: forged\n"}
	if _, err := Run(context.Background(), f0, p0); err != nil {
		t.Fatalf("a server's stderr failed the run: %v", err)
	}
	pl, fl := fixture(t)
	happyFake(fl)
	fl.LogsByName = map[string]string{"agentry-cafe0000-gateway": "  > Can't list tools agentry-tools: timeout\n"}
	if _, err := Run(context.Background(), fl, pl); err == nil || !strings.Contains(err.Error(), "tools server did not start") {
		t.Fatalf("a tools server that listed no tools: %v", err)
	}
	f.LogsByName = map[string]string{"agentry-cafe0000-gateway": "- Those servers are enabled: agentry-tools\n  > Can't start agentry-tools: running l7 proxy: creating container: Error response from daemon: agentry run filter: relay setting ALLOWED_HOSTS\n"}
	env, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitGateway || !strings.Contains(err.Error(), "tools server did not start") || !strings.Contains(err.Error(), "relay setting ALLOWED_HOSTS") {
		t.Fatalf("want a gateway failure naming the tools server and the gateway's line, got %v", err)
	}
	if env == nil || env.Stage != "infra" {
		t.Fatalf("stage = %v, want infra", env)
	}
	for _, op := range f.Ops() {
		if strings.HasPrefix(op, "RunForeground:agentry-cafe0000-agent") {
			t.Fatal("the agent ran without its tools")
		}
	}
}

// TestTheToolsServerIsGivenItsBoundedTmpfs: the filter is told the tools
// server's image (the exec image when the plan has one) and mounts its /tmp
// for every plan and its scratch folder when the plan grants one, each a
// 256 MiB tmpfs anyone may write.
func TestTheToolsServerIsGivenItsBoundedTmpfs(t *testing.T) {
	plan := &spec.Plan{Proxy: spec.ProxySpec{Image: "agentry-runner:test"}}
	want := "size=268435456,mode=1777"
	if got := toolsTmpfs(plan); len(got) != 1 || got["/tmp"] != want {
		t.Fatalf("a tools server with no scratch folder gets %v", got)
	}
	builtin := &spec.Plan{Proxy: spec.ProxySpec{Image: "agentry-runner:test"}, Capabilities: spec.CapSummary{Scratch: true, ReviewTools: true}}
	if got := toolsTmpfs(builtin); len(got) != 2 || got["/tmp"] != want || got["/scratch"] != want {
		t.Fatalf("a built-in's tools server gets %v", got)
	}
	if plan.ToolsImage() != "agentry-runner:test" {
		t.Fatalf("tools image %q", plan.ToolsImage())
	}
	plan.Capabilities.Scratch = true
	plan.ExecImage = &spec.ExecImageSpec{Tag: "agentry-runner-exec:abc"}
	if got := toolsTmpfs(plan); len(got) != 2 || got["/tmp"] != want || got["/scratch"] != want {
		t.Fatalf("a release's tools server gets %v", got)
	}
	if plan.ToolsImage() != "agentry-runner-exec:abc" {
		t.Fatalf("tools image %q, want the exec image", plan.ToolsImage())
	}
}

// TestTheRunsToolSpecSaysItsTmpfsIsBounded: where the docker filter runs,
// the run's own tools spec says so, and the tools role then refuses to
// serve without the tmpfs; the compiled plan's copy is not touched.
func TestTheRunsToolSpecSaysItsTmpfsIsBounded(t *testing.T) {
	p := filepath.Join(t.TempDir(), "toolspec.json")
	if err := os.WriteFile(p, []byte(`{"specVersion":"8","scratchDir":"/scratch"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := stampBoundedTmpfs(p); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"tmpfsBytes": 268435456`) || !strings.Contains(string(b), `"scratchDir": "/scratch"`) {
		t.Fatalf("stamped spec = %s", b)
	}
	if err := stampBoundedTmpfs(filepath.Join(t.TempDir(), "absent.json")); err != nil {
		t.Fatalf("a plan without a tools server: %v", err)
	}
}

// A tools server with unrestricted egress joins a network of the run's
// own, as the web tools' server does, in whichever image runs it: the
// filter would otherwise give it no network, since the gateway asks the
// daemon's default for it. A plan whose tools reach only declared hosts
// gets no such network.
func TestUnrestrictedEgressHasANetworkOfTheRunsOwn(t *testing.T) {
	withFilter(t, true)
	rd := &runDirs{cfg: t.TempDir(), runID: "cafe0000", anyEgress: true}
	for name, c := range map[string]struct {
		plan  spec.Plan
		net   string
		image string
	}{
		"unrestricted egress":           {spec.Plan{Capabilities: spec.CapSummary{EgressMode: "all"}, Proxy: spec.ProxySpec{Image: "agentry-runner:test"}}, "agentry-cafe0000-web", "agentry-runner:test"},
		"unrestricted egress with exec": {spec.Plan{Capabilities: spec.CapSummary{EgressMode: "all", ExecCommands: []string{"git"}}, Proxy: spec.ProxySpec{Image: "agentry-runner:test"}, ExecImage: &spec.ExecImageSpec{Tag: "agentry-exec:abc"}}, "agentry-cafe0000-web", "agentry-exec:abc"},
		"the web tools":                 {spec.Plan{Capabilities: spec.CapSummary{WebTools: true}, Proxy: spec.ProxySpec{Image: "agentry-runner:test"}}, "agentry-cafe0000-web", "agentry-runner:test"},
		"declared hosts":                {spec.Plan{Capabilities: spec.CapSummary{EgressMode: "hosts", EgressHosts: []string{"api.osv.dev"}}, Proxy: spec.ProxySpec{Image: "agentry-runner:test"}}, "", ""},
	} {
		cfg, err := filterConfig(rd, "cafe0000", nil, policy.Limits{}, &c.plan, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if cfg.WebNetwork != c.net || cfg.WebImage != c.image {
			t.Errorf("%s: network %q image %q, want %q %q", name, cfg.WebNetwork, cfg.WebImage, c.net, c.image)
		}
		if c.net != "" && (!slices.Equal(cfg.WebCmd, spec.ToolsCmd()) || cfg.WebSpec != filepath.Join(rd.cfg, "gateway", "toolspec.json")) {
			t.Errorf("%s: the server's shape is %v %s", name, cfg.WebCmd, cfg.WebSpec)
		}
	}
	line := daemonLine(rd)
	if !strings.Contains(line, "unrestricted egress") || !strings.Contains(line, "agentry-cafe0000-web") || !strings.Contains(line, "never on the daemon's default bridge") {
		t.Errorf("daemon line = %q", line)
	}

	// The web network leaves the host, so it is not an internal one; it
	// carries the run's label, is recorded for the sweep, and its removal is
	// queued with the teardown.
	f := &dockerx.Fake{}
	td := &teardown{}
	if err := makeWebNetwork(context.Background(), f, "cafe0000", td); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.Ops(), []string{"NetworkCreate:agentry-cafe0000-web(internal=false)"}) || !slices.Equal(td.networks, []string{"agentry-cafe0000-web"}) ||
		len(f.NetworkState()) != 1 || f.NetworkState()[0].Labels[runLabel] != "cafe0000" {
		t.Errorf("web network: ops %v, recorded %v, state %+v", f.Ops(), td.networks, f.NetworkState())
	}
	td.run()
	if got := f.Ops(); len(got) != 2 || got[1] != "NetworkRemove:agentry-cafe0000-web" {
		t.Errorf("teardown ops %v, want the web network removed", got)
	}

	// The relay networks are the same two the filter maps the gateway's fixed
	// names to: the inner one internal, the outer one leaving the host.
	f, td = &dockerx.Fake{}, &teardown{}
	if err := makeRelayNetworks(context.Background(), f, "cafe0000", td); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.Ops(), []string{"NetworkCreate:agentry-cafe0000-relays(internal=true)", "NetworkCreate:agentry-cafe0000-relays-out(internal=false)"}) ||
		!slices.Equal(td.networks, []string{"agentry-cafe0000-relays", "agentry-cafe0000-relays-out"}) {
		t.Errorf("relay networks: ops %v, recorded %v", f.Ops(), td.networks)
	}
	for _, n := range f.NetworkState() {
		if n.Labels[runLabel] != "cafe0000" {
			t.Errorf("relay network %s has labels %v, want the run's", n.Name, n.Labels)
		}
	}
	td.run()
	if got := f.Ops(); len(got) != 4 || got[2] != "NetworkRemove:agentry-cafe0000-relays-out" || got[3] != "NetworkRemove:agentry-cafe0000-relays" {
		t.Errorf("teardown ops %v, want the relay networks removed, the outer one first", got)
	}
}

// TestToolsVolumesAreTheAgentsOwn: the volumes the filter lets the tools
// server mount beside the run's are the segment's data volume and, for an
// agent that keeps one, its memory volume, by name, and none for a run.
func TestToolsVolumesAreTheAgentsOwn(t *testing.T) {
	if got := toolsVolumes(&runDirs{}); len(got) != 0 {
		t.Errorf("a run's tools volumes: %v", got)
	}
	if got := toolsVolumes(&runDirs{data: "d"}); !slices.Equal(got, []string{"d"}) {
		t.Errorf("a segment's without a memory: %v", got)
	}
	if got := toolsVolumes(&runDirs{data: "d", memory: "m"}); !slices.Equal(got, []string{"d", "m"}) {
		t.Errorf("a segment's with a memory: %v", got)
	}
}
