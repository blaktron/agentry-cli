package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage/sockfilter"
)

func sidecarRig(t *testing.T) (*dockerx.Fake, *runDirs, *teardown) {
	t.Helper()
	root := t.TempDir()
	rd := &runDirs{cfg: filepath.Join(root, "cfg"), receiptsDir: filepath.Join(root, "receipts")}
	for _, d := range []string{filepath.Join(rd.cfg, "gateway"), rd.receiptsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	d := &dockerx.Fake{LogsByName: map[string]string{containerName("R", "docker-filter"): "x\n" + sockfilter.SidecarReady + "\n"}}
	return d, rd, &teardown{d: d}
}

// TestDockerDesktopsFilterRunsInAContainerOfTheRuns (agentry-cli#322): on
// Docker Desktop the filter is the runner image's docker-filter role, on
// no network, as root with every capability dropped and a read-only root,
// holding the daemon's socket, its configuration read-only, the run's
// roots read-only at the gateway's spelling of them, and its socket in a
// volume of the run's that the gateway then mounts at /var/run; its
// teardown removes the container before the volume.
func TestDockerDesktopsFilterRunsInAContainerOfTheRuns(t *testing.T) {
	d, rd, td := sidecarRig(t)
	was := gatewayOS
	gatewayOS = "windows"
	t.Cleanup(func() { gatewayOS = was })
	cfg := sockfilter.Config{RunID: "R", Label: runLabel, OwnerLabel: gatewayOwnerLabel,
		BindRoots:     []string{"/c/Users/me/.agentry/runs/R", "/c/Users/me/.agentry/runs/R/out"},
		WritableRoots: []string{"/c/Users/me/.agentry/runs/R/out"},
		WebSpec:       `C:\Users\me\.agentry\runs\R\cfg\gateway\toolspec.json`,
	}
	vol, err := startFilterSidecar(context.Background(), d, rd, "R", "agentry-runner:test", cfg, policy.Limits{Memory: "2g", CPUs: 1, Pids: 512}, td)
	if err != nil {
		t.Fatal(err)
	}
	if vol != filterVolume("R") {
		t.Fatalf("volume %q", vol)
	}
	var run dockerx.RunSpec
	for _, c := range d.Calls {
		if c.Op == "RunDetached" {
			run = c.Run
		}
	}
	if run.Image != "agentry-runner:test" || run.Argv[0] != "docker-filter" || run.Network != "none" || run.User != "0" ||
		!slices.Equal(run.CapDrop, []string{"ALL"}) || len(run.CapAdd) != 0 || !run.ReadOnly || !run.NoNewPrivileges || run.PidsLimit != 512 ||
		run.Labels[runLabel] != "R" || run.Labels[gatewayOwnerLabel] != "" {
		t.Fatalf("the filter container: %+v", run)
	}
	want := map[string]dockerx.Mount{
		DockerSocket:                  {Source: DockerSocket, Target: DockerSocket},
		sidecarListenDir:              {Source: vol, Target: sidecarListenDir},
		sidecarConfig:                 {Source: filepath.Join(rd.cfg, "gateway", "docker-filter.json"), Target: sidecarConfig, ReadOnly: true},
		sidecarLogDir:                 {Source: rd.receiptsDir, Target: sidecarLogDir},
		"/c/Users/me/.agentry/runs/R": {Source: `C:\Users\me\.agentry\runs\R`, Target: "/c/Users/me/.agentry/runs/R", ReadOnly: true},
	}
	if len(run.Mounts) != len(want) {
		t.Errorf("mounts %+v, want %d of them (one root; the rest lie beneath it)", run.Mounts, len(want))
	}
	for _, m := range run.Mounts {
		if w, ok := want[m.Target]; !ok || w != m {
			t.Errorf("mount %+v, want %+v", m, w)
		}
	}
	raw, err := os.ReadFile(filepath.Join(rd.cfg, "gateway", "docker-filter.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := sockfilter.UnmarshalConfig(raw)
	if err != nil || got.WebSpec != "/c/Users/me/.agentry/runs/R/cfg/gateway/toolspec.json" || got.BindRoots[0] != cfg.BindRoots[0] {
		t.Fatalf("the configuration the container reads: %+v (%v)", got, err)
	}
	gw := throughFilterVolume([]dockerx.Mount{{Source: DockerSocket, Target: DockerSocket}, {Source: "/x", Target: "/y"}}, vol)
	if gw[0] != (dockerx.Mount{Source: vol, Target: gatewaySocketDir}) || gw[1].Source != "/x" {
		t.Fatalf("the gateway's mounts: %+v", gw)
	}
	// Teardown: the container goes before its volume.
	d.Calls = nil
	td.run()
	var order []string
	for _, c := range d.Calls {
		if c.Op == "RemoveForce" || c.Op == "VolumeRemove" {
			order = append(order, c.Op+" "+c.Arg)
		}
	}
	if len(order) < 2 || order[0] != "RemoveForce "+containerName("R", "docker-filter") || !slices.Contains(order, "VolumeRemove "+vol) {
		t.Fatalf("teardown order %v", order)
	}
	if rd.filterSidecar == "" {
		t.Fatal("the decision log was not recorded for the receipt's refusals")
	}
}

// A filter container that never says it is ready fails the run at the
// gateway, naming what it printed: the gateway never starts unfiltered.
func TestAFilterContainerThatNeverServesFailsTheRun(t *testing.T) {
	d, rd, td := sidecarRig(t)
	d.LogsByName = map[string]string{containerName("R", "docker-filter"): "agentry-runner: docker-filter: docker filter configuration: unknown field"}
	was := sidecarReadyWait
	sidecarReadyWait = 300 * time.Millisecond
	t.Cleanup(func() { sidecarReadyWait = was })
	_, err := startFilterSidecar(context.Background(), d, rd, "R", "agentry-runner:test", sockfilter.Config{RunID: "R"}, policy.Limits{Memory: "2g", CPUs: 1, Pids: 512}, td)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitGateway || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("got %v", err)
	}
}

func TestNativeOfGatewayPath(t *testing.T) {
	for _, c := range []struct{ in, goos, want string }{
		{"/c/Users/me/x", "windows", `C:\Users\me\x`},
		{"/c", "windows", `C:\`},
		{"/cd/x", "windows", "/cd/x"},
		{"/Users/me/x", "darwin", "/Users/me/x"},
		{"/c/Users/me", "darwin", "/c/Users/me"},
	} {
		if got := nativeOfGatewayPath(c.in, c.goos); got != c.want {
			t.Errorf("nativeOfGatewayPath(%q, %s) = %q, want %q", c.in, c.goos, got, c.want)
		}
		if c.goos == "windows" && c.in != "/cd/x" && gatewayHostPath(nativeOfGatewayPath(c.in, c.goos), "windows") != c.in {
			t.Errorf("%q does not round-trip", c.in)
		}
	}
}
