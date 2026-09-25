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
)

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
	f, sock, err := startDaemonFilter(rd, "cafe0000", map[string]string{"MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS": dir}, policy.Limits{Memory: "2g", CPUs: 1, Pids: 512}, nil, "")
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
// host, not under the installed BriefAgent's run directory, which is too
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
	f, sock, err := startDaemonFilter(rd, "c8c45187", nil, policy.Limits{}, nil, home)
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
	f, sock, err := startDaemonFilter(rd, "cafe0000", nil, policy.Limits{}, nil, "")
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

// A run with relays makes the two shared relay networks itself, before
// the gateway, and tolerates their being there already.
func TestARunWithRelaysMakesTheSharedNetworks(t *testing.T) {
	if !filterSupported {
		t.Skip("the filter runs on Linux only")
	}
	p, f := serverFixture(t)
	happyFake(f)
	f.LogsByName = map[string]string{"agentry-cafe0000-gateway": "- Those servers are enabled: agentry-tools, mcp-npm-scope-server-x\n> 4 tools listed in 1.2s\n"}
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatalf("run: %v", err)
	}
	ops := f.Ops()
	in := slices.Index(ops, "NetworkCreate:"+relayNetwork+"(internal=true)")
	out := slices.Index(ops, "NetworkCreate:"+relayEgressNetwork+"(internal=false)")
	gw := slices.IndexFunc(ops, func(op string) bool { return strings.HasPrefix(op, "RunDetached:agentry-cafe0000-gateway") })
	if in < 0 || out < 0 || gw < 0 || in > gw || out > gw {
		t.Errorf("want both relay networks made before the gateway; got %v", ops)
	}
	for _, op := range ops {
		if strings.HasPrefix(op, "NetworkRemove:docker-mcp-proxies") {
			t.Errorf("a shared relay network was removed: %s", op)
		}
	}
	if err := ensureRelayNetworks(context.Background(), &existsFake{Fake: &dockerx.Fake{}}); err != nil {
		t.Errorf("networks already there: %v", err)
	}
}

// existsFake answers every network create as docker does for one that is
// already there.
type existsFake struct{ *dockerx.Fake }

func (*existsFake) NetworkCreate(context.Context, string, bool, map[string]string) error {
	return errors.New("Error response from daemon: network with name docker-mcp-proxies-int already exists")
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
