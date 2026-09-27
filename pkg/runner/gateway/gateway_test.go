// First-use install tests (RQ2): images pulled only when absent, the
// runner image built from the embedded context only when absent.
package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

func TestEnsureInstalledFirstUse(t *testing.T) {
	pol := policy.Default()
	f := &dockerx.Fake{}
	dir := filepath.Join(t.TempDir(), "image")
	if err := EnsureInstalled(context.Background(), f, pol, "agentry-runner:test", dir, os.Stderr, true); err != nil {
		t.Fatalf("EnsureInstalled: %v", err)
	}
	ops := strings.Join(f.Ops(), " ")
	for _, want := range []string{
		"Pull:" + pol.Images.Gateway,
		"Pull:" + pol.Images.HarnessDockerAgent,
		"Build:agentry-runner:test<-" + dir,
	} {
		if !strings.Contains(ops, want) {
			t.Errorf("missing %q in %v", want, f.Ops())
		}
	}
	// The embedded context must actually have materialized.
	if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); err != nil {
		t.Errorf("build context not materialized: %v", err)
	}
}

func TestEnsureInstalledSkipsPresent(t *testing.T) {
	pol := policy.Default()
	f := &dockerx.Fake{ExistingImages: map[string]bool{
		pol.Images.Gateway:            true,
		pol.Images.HarnessDockerAgent: true,
		"agentry-runner:test":         true,
	}}
	if err := EnsureInstalled(context.Background(), f, pol, "agentry-runner:test", t.TempDir(), os.Stderr, true); err != nil {
		t.Fatal(err)
	}
	ops := strings.Join(f.Ops(), " ")
	if strings.Contains(ops, "Pull:") || strings.Contains(ops, "Build:") {
		t.Errorf("pulled or built despite images present: %v", f.Ops())
	}
}

// docker.runtime is enforced at ensure, never preferred (DR-23): a daemon
// without it refuses before anything is pulled, naming both sides; one
// with it proceeds; an unset runtime asks the daemon nothing.
func TestEnsureRuntime(t *testing.T) {
	pol := policy.Default()
	pol.Docker.Runtime = "runsc"
	f := &dockerx.Fake{RuntimeNames: []string{"io.containerd.runc.v2", "runc"}}
	err := EnsureInstalled(context.Background(), f, pol, "agentry-runner:test", filepath.Join(t.TempDir(), "image"), os.Stderr, true)
	if err == nil {
		t.Fatal("a missing runtime was accepted")
	}
	for _, want := range []string{`"runsc"`, "runc", "never falls back"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if ops := strings.Join(f.Ops(), " "); strings.Contains(ops, "Pull:") || strings.Contains(ops, "Build:") {
		t.Errorf("a refused runtime still pulled or built: %v", f.Ops())
	}

	f = &dockerx.Fake{RuntimeNames: []string{"runc", "runsc"}, ExistingImages: map[string]bool{
		pol.Images.Gateway: true, pol.Images.HarnessDockerAgent: true, "agentry-runner:test": true,
	}}
	if err := EnsureInstalled(context.Background(), f, pol, "agentry-runner:test", filepath.Join(t.TempDir(), "image"), os.Stderr, true); err != nil {
		t.Fatalf("a registered runtime was refused: %v", err)
	}

	f = &dockerx.Fake{RuntimeNames: []string{"runc"}, ExistingImages: map[string]bool{
		pol.Images.Gateway: true, pol.Images.HarnessDockerAgent: true, "agentry-runner:test": true,
	}}
	if err := EnsureInstalled(context.Background(), f, policy.Default(), "agentry-runner:test", filepath.Join(t.TempDir(), "image"), os.Stderr, true); err != nil {
		t.Fatalf("an unset runtime was refused: %v", err)
	}
	if strings.Contains(strings.Join(f.Ops(), " "), "Runtimes") {
		t.Error("an unset runtime queried the daemon's runtimes")
	}

	// The gateway's own runtime is held to the same rule (agentry-cli#337).
	pol.Docker.GatewayRuntime = "runsc-uds"
	f = &dockerx.Fake{RuntimeNames: []string{"runc", "runsc"}}
	err = EnsureRuntime(context.Background(), f, pol)
	if err == nil || !strings.Contains(err.Error(), `docker.gatewayRuntime "runsc-uds"`) || !strings.Contains(err.Error(), "--host-uds=open") {
		t.Errorf("an unregistered gateway runtime: %v", err)
	}
	// It is held to what it runs, not its name (agentry-cli#396): the
	// runtime's binary registered again with --host-uds=open alone.
	runsc := dockerx.Runtime{Path: "/usr/bin/runsc"}
	f = &dockerx.Fake{RuntimeTable: map[string]dockerx.Runtime{"runc": {Path: "runc"}, "runsc": runsc,
		"runsc-uds": {Path: "/usr/bin/runsc", Args: []string{dockerx.HostUDSOpen}}}}
	if err := EnsureRuntime(context.Background(), f, pol); err != nil {
		t.Errorf("a registered gateway runtime was refused: %v", err)
	}
	for name, reg := range map[string]dockerx.Runtime{
		"runc named runsc-uds": {Path: "runc", Args: []string{dockerx.HostUDSOpen}},
		"no --host-uds=open":   runsc,
		"a flag beside it":     {Path: "/usr/bin/runsc", Args: []string{dockerx.HostUDSOpen, "--network=host"}},
	} {
		f = &dockerx.Fake{RuntimeTable: map[string]dockerx.Runtime{"runc": {Path: "runc"}, "runsc": runsc, "runsc-uds": reg}}
		err := EnsureRuntime(context.Background(), f, pol)
		if err == nil || !strings.Contains(err.Error(), `docker.gatewayRuntime "runsc-uds"`) || !strings.Contains(err.Error(), "claim a sandbox") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
