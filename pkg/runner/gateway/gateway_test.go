// First-use install tests: images pulled only when absent, the
// runner image built from the embedded context only when absent.
package gateway

import (
	"context"
	"errors"
	"io"
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

// docker.runtime is enforced at ensure, never preferred: a daemon
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

	// The gateway's own runtime is held to the same rule.
	pol.Docker.GatewayRuntime = "runsc-uds"
	f = &dockerx.Fake{RuntimeNames: []string{"runc", "runsc"}}
	err = EnsureRuntime(context.Background(), f, pol)
	if err == nil || !strings.Contains(err.Error(), `docker.gatewayRuntime "runsc-uds"`) || !strings.Contains(err.Error(), "--host-uds=open") {
		t.Errorf("an unregistered gateway runtime: %v", err)
	}
	// It is held to what it runs, not its name: the runtime's binary registered
	// again with --host-uds=open alone.
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

// On podman a policy's runtime is checked by name, before anything is
// pulled: podman's own CLI must drive it (the docker CLI's view cannot read
// a container's runtime back), the name must answer, and the gateway's
// runtime must be a wrapper that adds --host-uds=open to the same runtime.
func TestEnsureRuntimeOnPodmanChecksEachName(t *testing.T) {
	pol := policy.Default()
	pol.Docker.Runtime = "runsc"
	viaDocker := &dockerx.Fake{RuntimeNames: []string{"crun", "runsc"}, EngineInfo: &dockerx.Engine{Kind: dockerx.KindPodman, CLI: "docker", Endpoint: "unix:///run/podman/podman.sock"}}
	err := EnsureInstalled(context.Background(), viaDocker, pol, "agentry-runner:test", filepath.Join(t.TempDir(), "image"), os.Stderr, true)
	if err == nil || !strings.Contains(err.Error(), "only through podman's own CLI (AGENTRY_ENGINE=podman)") {
		t.Fatalf("the docker CLI on podman: %v", err)
	}
	if ops := strings.Join(viaDocker.Ops(), " "); strings.Contains(ops, "Pull:") || strings.Contains(ops, "Build:") {
		t.Errorf("a refused runtime still pulled or built: %v", viaDocker.Ops())
	}
	podman := &dockerx.Engine{Kind: dockerx.KindPodman, CLI: "podman", Endpoint: "unix:///run/user/1000/podman/podman.sock", Rootless: true}
	unregistered := &dockerx.Fake{RuntimeTable: map[string]dockerx.Runtime{"crun": {Path: "/usr/bin/crun"}}, EngineInfo: podman}
	err = EnsureRuntime(context.Background(), unregistered, pol)
	if err == nil || !strings.Contains(err.Error(), `docker.runtime "runsc"`) || !strings.Contains(err.Error(), "[engine.runtimes]") || !strings.Contains(err.Error(), "never falls back") {
		t.Errorf("an unregistered runtime on podman: %v", err)
	}
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "runsc-uds")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec /opt/runsc --ignore-cgroups --host-uds=open \"$@\"\n"), 0o755); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	registered := &dockerx.Fake{RuntimeTable: map[string]dockerx.Runtime{"crun": {Path: "/usr/bin/crun"}, "runsc": {Path: "/opt/runsc"}, "runsc-uds": {Path: wrapper}}, EngineInfo: podman}
	if err := EnsureRuntime(context.Background(), registered, pol); err != nil {
		t.Errorf("a registered runtime on podman was refused: %v", err)
	}
	pol.Docker.GatewayRuntime = "runsc-uds"
	if err := EnsureRuntime(context.Background(), registered, pol); err != nil {
		t.Errorf("a registered gateway wrapper on podman was refused: %v", err)
	}
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("#!/bin/sh\nexec /opt/runsc --ignore-cgroups \"$@\"\n"), 0o755); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	registered.RuntimeTable["runsc-uds"] = dockerx.Runtime{Path: plain}
	err = EnsureRuntime(context.Background(), registered, pol)
	if err == nil || !strings.Contains(err.Error(), `docker.gatewayRuntime "runsc-uds" is registered at `+plain) || !strings.Contains(err.Error(), "claim a sandbox") {
		t.Errorf("a wrapper without --host-uds=open: %v", err)
	}
	delete(registered.RuntimeTable, "runsc-uds")
	if err = EnsureRuntime(context.Background(), registered, pol); err == nil || !strings.Contains(err.Error(), `docker.gatewayRuntime "runsc-uds"`) {
		t.Errorf("an unregistered gateway runtime: %v", err)
	}
	// No runtime asks nothing of the engine.
	registered.EngineErr = errors.New("not asked")
	if err := EnsureRuntime(context.Background(), registered, policy.Default()); err != nil {
		t.Errorf("an unset runtime: %v", err)
	}
}

// TestEnsureInstalledNamesAStaleGoBuilder: a builder older than the runner
// context's go line refuses inside docker's output; the error names the
// policy setting to change.
func TestEnsureInstalledNamesAStaleGoBuilder(t *testing.T) {
	pol := policy.Default()
	pol.Images.GoBuilder = "registry.example/golang:1.27.1-alpine"
	f := &dockerx.Fake{FailBuild: map[string]error{
		"agentry-runner:test": errors.New("exit status 1: go: go.mod requires go >= 1.27.2 (running go 1.27.1; GOTOOLCHAIN=local)"),
	}}
	err := EnsureInstalled(context.Background(), f, pol, "agentry-runner:test", filepath.Join(t.TempDir(), "image"), io.Discard, true)
	if err == nil || !strings.Contains(err.Error(), "images.goBuilder registry.example/golang:1.27.1-alpine is older than the Go the runner needs") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "running go 1.27.1") {
		t.Errorf("the docker output is dropped: %v", err)
	}
}
