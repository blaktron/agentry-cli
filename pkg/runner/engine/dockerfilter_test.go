package engine

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
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
	f, sock, err := startDaemonFilter(rd, "cafe0000", map[string]string{"MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS": dir}, policy.Limits{Memory: "2g", CPUs: 1, Pids: 512}, &spec.Plan{}, "")
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
}

// The socket sits under the runner home, the path runhost shares with the
// host, not under the installed QuickAgent's run directory, which is too
// deep for a socket path there (dev's runhost, 2026-09-25).
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
	f, sock, err := startDaemonFilter(rd, "c8c45187", nil, policy.Limits{}, &spec.Plan{}, home)
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
	f, sock, err := startDaemonFilter(rd, "cafe0000", nil, policy.Limits{}, &spec.Plan{}, "")
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
}

// A run with relays gets two relay networks of its own, made before the
// gateway and removed after it, and neither takes the host-wide relay lock
// nor refuses another gateway's relays: runs with relays share a host
// (agentry-cli#220). The lock is held by "another run" throughout, and the
// run goes ahead anyway.
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
		if strings.Contains(op, "docker-mcp-proxies") || op == "PSByLabel:"+relayLabel && slices.Index(ops, op) < gw {
			t.Errorf("the run touched the host's relay network or refused on another gateway's relays: %s", op)
		}
	}
}

// Where runs with relays share a host, the reaper takes a name from the
// gateway's log only if it is a relay this run's gateway made: a name
// planted in the log reaches no other run's relay.
func TestTheReaperTakesOnlyTheRunsOwnRelays(t *testing.T) {
	f := &dockerx.Fake{ByLabel: map[string][]string{
		relayLabel:                      {"docker-mcp-l4proxy-ours", "docker-mcp-l4proxy-theirs"},
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

// The agent's own egress counts as relays (agentry-cli#346): a run whose
// release declares network.egress and no server gets the run's relay
// networks, and the filter admits the tools server's L7 relay to its hosts
// on 443 alongside the servers' entries.
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
}

// A gateway that could not start the run's tools server carries on with
// the declared servers alone; the run fails at the gateway instead of
// running an agent with none of its tools (agentry-cli#346).
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
