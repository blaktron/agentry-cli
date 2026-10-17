package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage/sockfilter"
)

// Rancher Desktop's dockerd runs in a WSL 2 distribution whose drives are
// mounted under /mnt, and the filter container and the gateway reach that
// dockerd on its own socket, past the named-pipe proxy that rewrites C:\
// paths for Windows clients. So on that engine a drive path is
// /mnt/<drive>/…, where Docker Desktop's socket takes /<drive>/….
func TestDriveRootFollowsTheEngine(t *testing.T) {
	for _, c := range []struct {
		os, want string
	}{
		{"Rancher Desktop WSL Distribution", wslDriveRoot},
		{"Docker Desktop", ""},
		{"Fedora Linux 44", ""},
		{"", ""},
	} {
		if got := driveRootOf(dockerx.Engine{Kind: dockerx.KindDocker, OS: c.os}); got != c.want {
			t.Errorf("driveRootOf(OS %q) = %q, want %q", c.os, got, c.want)
		}
	}
}

func TestGatewayHostPathUnderWSLDriveRoot(t *testing.T) {
	for _, c := range []struct{ in, goos, want string }{
		{`C:\Users\blakt\.agentry\runner\runs\ae8a24dd\cfg`, "windows", "/mnt/c/Users/blakt/.agentry/runner/runs/ae8a24dd/cfg"},
		{`d:\work\out\`, "windows", "/mnt/d/work/out"},
		{`C:\`, "windows", "/mnt/c"},
		{`\\server\share\x`, "windows", `\\server\share\x`},
		{"/home/u/runs/r", "linux", "/home/u/runs/r"},
	} {
		if got := gatewayHostPathUnder(c.in, c.goos, wslDriveRoot); got != c.want {
			t.Errorf("gatewayHostPathUnder(%q, %s, %q) = %q, want %q", c.in, c.goos, wslDriveRoot, got, c.want)
		}
	}
}

func TestNativeOfGatewayPathUnderWSLDriveRoot(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"/mnt/c/Users/me/x", `C:\Users\me\x`},
		{"/mnt/c", `C:\`},
		{"/mnt/cd/x", "/mnt/cd/x"},
		// Docker Desktop's spelling is not this engine's: left as it is.
		{"/c/Users/me", "/c/Users/me"},
		{"/mnt", "/mnt"},
	} {
		got := nativeOfGatewayPathUnder(c.in, "windows", wslDriveRoot)
		if got != c.want {
			t.Errorf("nativeOfGatewayPathUnder(%q) = %q, want %q", c.in, got, c.want)
		}
		if got != c.in && gatewayHostPathUnder(got, "windows", wslDriveRoot) != c.in {
			t.Errorf("%q does not round-trip", c.in)
		}
	}
	if got := nativeOfGatewayPathUnder("/mnt/c/x", "darwin", wslDriveRoot); got != "/mnt/c/x" {
		t.Errorf("off Windows a path is spelled one way, got %q", got)
	}
}

// A run on Rancher Desktop hands the gateway /mnt/c/… paths in its bind
// allow-lists and in the tools server's volumes, rendered by the run's own
// wiring.
func TestGatewaySeesWSLPathsOnRancherDesktop(t *testing.T) {
	p, _ := fixtureEdited(t, nil, nil)
	setGatewayOS(t, "windows")
	run := `C:\Users\blakt\.agentry\runner\agents\agentry\drafter\1.0.0-1ab1787b6504\runs\ae8a24dd`
	rd := &runDirs{
		root:       run,
		workspace:  run + `\workspace`,
		inputs:     run + `\inputs`,
		inputsText: run + `\inputs-text`,
		knowledge:  run + `\knowledge`,
		cfg:        t.TempDir(),
		outDir:     `C:\Users\blakt\Downloads\agentry-out\agentry-drafter-ae8a24dd`,
		driveRoot:  wslDriveRoot,
	}
	if err := renderGatewayConfig(p, rd); err != nil {
		t.Fatalf("render the gateway config: %v", err)
	}
	env, err := rd.gatewayEnv(p.Plan, "tok")
	if err != nil {
		t.Fatal(err)
	}
	roots := strings.Split(env["MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS"], ":")
	for _, r := range roots {
		if !strings.HasPrefix(r, "/mnt/c/") && r != rd.gatewayPath(rd.cfg) {
			t.Errorf("gateway root %q is not a /mnt/c/ path (roots %q)", r, roots)
		}
	}
	catalog, err := os.ReadFile(filepath.Join(rd.cfg, "gateway", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(catalog), "/mnt/c/Users/blakt/Downloads/agentry-out/agentry-drafter-ae8a24dd") {
		t.Errorf("the tools server's output volume is not spelled under /mnt:\n%s", catalog)
	}
	if strings.Contains(string(catalog), " /c/Users/") || strings.Contains(string(catalog), "- /c/") {
		t.Errorf("the catalog keeps Docker Desktop's spelling:\n%s", catalog)
	}
}

// The filter container on Rancher Desktop sees the run's roots where the
// gateway names them, /mnt/c/…, mounted from the native path the CLI's own
// docker call gives the pipe; its configuration's host paths are the
// gateway's spelling.
func TestRancherDesktopsFilterContainerSeesWSLPaths(t *testing.T) {
	d, rd, td := sidecarRig(t)
	rd.driveRoot = wslDriveRoot
	setGatewayOS(t, "windows")
	cfg := sockfilter.Config{RunID: "R", Label: runLabel, OwnerLabel: gatewayOwnerLabel,
		BindRoots: []string{"/mnt/c/Users/me/.agentry/runs/R"},
		WebSpec:   `C:\Users\me\.agentry\runs\R\cfg\gateway\toolspec.json`,
	}
	if _, err := startFilterSidecar(context.Background(), d, rd, "R", "agentry-runner:test", DockerSocket, cfg, policy.Limits{Memory: "2g", CPUs: 1, Pids: 512}, td); err != nil {
		t.Fatal(err)
	}
	var run dockerx.RunSpec
	for _, c := range d.Calls {
		if c.Op == "RunDetached" {
			run = c.Run
		}
	}
	want := dockerx.Mount{Source: `C:\Users\me\.agentry\runs\R`, Target: "/mnt/c/Users/me/.agentry/runs/R", ReadOnly: true}
	found := false
	for _, m := range run.Mounts {
		if m == want {
			found = true
		}
	}
	if !found {
		t.Errorf("mounts %+v lack %+v", run.Mounts, want)
	}
	raw, err := os.ReadFile(filepath.Join(rd.cfg, "gateway", "docker-filter.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := sockfilter.UnmarshalConfig(raw)
	if err != nil || got.WebSpec != "/mnt/c/Users/me/.agentry/runs/R/cfg/gateway/toolspec.json" {
		t.Fatalf("the configuration the container reads: %+v (%v)", got, err)
	}
}
