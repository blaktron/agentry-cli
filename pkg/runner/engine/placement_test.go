package engine

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/runnerimage/sockfilter"
)

func TestPlacementByKernel(t *testing.T) {
	linux := hostFacts{goos: "linux", hostname: "box", kernel: "7.1.13"}
	podman := dockerx.Engine{Kind: dockerx.KindPodman, CLI: "podman", Version: "5.8.4", Rootless: true,
		Endpoint: "unix:///run/user/1000/podman/podman.sock", DaemonHost: "box", DaemonKernel: "7.1.13"}
	desktop := dockerx.Engine{Kind: dockerx.KindDocker, CLI: "docker", Version: "29.0.0",
		Endpoint: "unix:///home/me/.docker/desktop/docker.sock", DaemonHost: "docker-desktop", DaemonKernel: "6.10.14-linuxkit"}
	for _, c := range []struct {
		name                  string
		e                     dockerx.Engine
		h                     hostFacts
		override              string
		inProcess             bool
		upstream, mountSource string
		reason                string
	}{
		{"a local daemon", podman, linux, "", true, "/run/user/1000/podman/podman.sock", "/run/user/1000/podman/podman.sock", "this machine's hostname (box) and kernel (7.1.13)"},
		{"a daemon in a VM on Linux", desktop, linux, "", false, "/home/me/.docker/desktop/docker.sock", DockerSocket, "not this machine's box and 7.1.13, so it runs in a VM"},
		{"a VM with this machine's kernel", dockerx.Engine{Endpoint: "unix:///x.sock", DaemonHost: "lima-default", DaemonKernel: "7.1.13"}, linux, "", false, "/x.sock", DockerSocket, "runs in a VM"},
		{"macOS", desktop, hostFacts{goos: "darwin", hostname: "mac", kernel: ""}, "", false, "/home/me/.docker/desktop/docker.sock", DockerSocket, "VM (docker-desktop, kernel 6.10.14-linuxkit) on darwin"},
		{"the CLI in a container on the daemon's machine", podman, hostFacts{goos: "linux", hostname: "c0ffee", kernel: "7.1.13", inContainer: true}, "", true, "/run/user/1000/podman/podman.sock", "/run/user/1000/podman/podman.sock", "the CLI runs in a container on it"},
		{"a container that is not on the daemon's kernel", desktop, hostFacts{goos: "linux", hostname: "c0ffee", kernel: "7.1.13", inContainer: true}, "", false, "/home/me/.docker/desktop/docker.sock", DockerSocket, "runs in a VM"},
		{"sidecar asked for on a local daemon", podman, linux, "sidecar", false, "/run/user/1000/podman/podman.sock", "/run/user/1000/podman/podman.sock", EnvDockerFilter + "=sidecar"},
		{"process asked for in a VM", desktop, linux, "process", true, "/home/me/.docker/desktop/docker.sock", DockerSocket, EnvDockerFilter + "=process"},
		{"Docker Desktop's pipe on Windows", dockerx.Engine{Endpoint: "npipe:////./pipe/dockerDesktopLinuxEngine", DaemonHost: "docker-desktop"}, hostFacts{goos: "windows", hostname: "pc"}, "", false, "//./pipe/dockerDesktopLinuxEngine", DockerSocket, "on windows"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, err := placeFilter(c.e, c.h, c.override)
			if err != nil {
				t.Fatal(err)
			}
			if p.inProcess != c.inProcess || p.upstream != c.upstream || p.mountSource != c.mountSource || !strings.Contains(p.reason, c.reason) {
				t.Errorf("placement %+v\nwant in-process %v, upstream %q, mount %q, a reason with %q", p, c.inProcess, c.upstream, c.mountSource, c.reason)
			}
		})
	}
}

// An endpoint the filter cannot be put in front of is refused with the
// gateway's exit class, naming it.
func TestPlacementRefusesARemoteEndpoint(t *testing.T) {
	linux := hostFacts{goos: "linux", hostname: "box", kernel: "7.1.13"}
	for _, c := range []struct {
		endpoint, override, want string
		h                        hostFacts
	}{
		{"tcp://10.0.0.5:2376", "", "tcp://10.0.0.5:2376 is not a Unix socket on this machine", linux},
		{"ssh://me@build-box", "", "ssh://me@build-box is not a Unix socket", linux},
		{"npipe:////./pipe/docker_engine", "", "is not a Unix socket", linux},
		{"npipe:////./pipe/docker_engine", "process", "=process needs an engine on a Unix socket", hostFacts{goos: "windows"}},
		{"unix:///var/run/docker.sock", "both", `is "both": it takes process or sidecar`, linux},
	} {
		_, err := placeFilter(dockerx.Engine{Endpoint: c.endpoint, DaemonHost: "box", DaemonKernel: "7.1.13"}, c.h, c.override)
		var ce *ClassifiedError
		if !errors.As(err, &ce) || ce.Code != ExitGateway || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s (%s): got %v, want exit %d naming %q", c.endpoint, c.override, err, ExitGateway, c.want)
		}
	}
}

func TestEngineLine(t *testing.T) {
	p, err := placeFilter(dockerx.Engine{Kind: dockerx.KindPodman, CLI: "podman", Version: "5.8.4", Rootless: true,
		Endpoint: "unix:///run/user/1000/podman/podman.sock", DaemonHost: "box", DaemonKernel: "7.1.13"},
		hostFacts{goos: "linux", hostname: "box", kernel: "7.1.13"}, "")
	if err != nil {
		t.Fatal(err)
	}
	want := "podman 5.8.4, rootless, /run/user/1000/podman/podman.sock, docker filter in-process: the daemon reports this machine's hostname (box) and kernel (7.1.13)"
	if got := p.line(); got != want {
		t.Errorf("line\n%s\nwant\n%s", got, want)
	}
}

// withHost makes this machine the given one for a test.
func withHost(t *testing.T, h hostFacts) {
	t.Helper()
	was := thisHost
	thisHost = func() hostFacts { return h }
	t.Cleanup(func() { thisHost = was })
}

// fakePodmanSocket serves the Docker API's /_ping on a Unix socket, counting
// the requests that reach it.
func fakePodmanSocket(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	dir, err := os.MkdirTemp("", "pm") // short: a socket path has a length limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "podman.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { //nolint:gosec // test server
		hits.Add(1)
		_, _ = w.Write([]byte("OK"))
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock, &hits
}

// TestARunOnAPodmanSocket: on an engine whose socket is not the plan's
// /var/run/docker.sock, the CLI's filter dials the engine's own socket, the
// gateway mounts the filter where the plan names the daemon's socket, its
// SELinux exemption holds, the receipt names the engine, and the plan that
// ran is byte for byte the one a Docker Engine run has.
func TestARunOnAPodmanSocket(t *testing.T) {
	if !filterSupported {
		t.Skip("no docker filter on this platform")
	}
	withHost(t, hostFacts{goos: "linux", hostname: "box", kernel: "7.1.13"})
	runPlan := func(e dockerx.Engine, onGateway func(dockerx.RunSpec)) (map[string]string, []byte) {
		p, f := fixture(t)
		happyFake(f)
		f.EngineInfo = &e
		f.OnDetached = func(r dockerx.RunSpec) {
			if strings.HasSuffix(r.Name, "-gateway") && onGateway != nil {
				onGateway(r)
			}
		}
		env, err := Run(context.Background(), f, p)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		b, err := os.ReadFile(filepath.Join(p.OutDir, "receipts", "run-plan.json"))
		if err != nil {
			t.Fatal(err)
		}
		return env.CapabilitiesEnforced, b
	}

	sock, hits := fakePodmanSocket(t)
	podman := dockerx.Engine{Kind: dockerx.KindPodman, CLI: "podman", Version: "5.8.4", Rootless: true,
		Endpoint: "unix://" + sock, DaemonHost: "box", DaemonKernel: "7.1.13"}
	var gw dockerx.RunSpec
	enforced, podmanPlan := runPlan(podman, func(r dockerx.RunSpec) {
		gw = r
		for _, m := range r.Mounts {
			if m.Target == DockerSocket {
				if err := ping(m.Source); err != nil {
					t.Errorf("the gateway's socket: %v", err)
				}
			}
		}
	})
	var mounted string
	for _, m := range gw.Mounts {
		if m.Target == DockerSocket {
			mounted = m.Source
		}
	}
	if mounted == "" || mounted == sock || mounted == DockerSocket {
		t.Errorf("the gateway mounts %q at %s, want the run's filter", mounted, DockerSocket)
	}
	if hits.Load() == 0 {
		t.Error("a request through the run's filter never reached the engine's socket")
	}
	if !gw.DisableSELinuxLabel {
		t.Error("the gateway lost its SELinux exemption")
	}
	if want := "podman 5.8.4, rootless, " + sock + ", docker filter in-process"; !strings.HasPrefix(enforced["engine"], want) {
		t.Errorf("engine line %q, want it to start %q", enforced["engine"], want)
	}

	_, dockerPlan := runPlan(dockerx.Engine{Kind: dockerx.KindDocker, CLI: "docker", Version: "29.7.2",
		Endpoint: "unix:///var/run/docker.sock", DaemonHost: "box", DaemonKernel: "7.1.13"}, nil)
	if string(podmanPlan) != string(dockerPlan) {
		t.Error("the plan that ran depends on the engine")
	}
}

// ping asks a Docker API socket for /_ping.
func ping(sock string) error {
	c, err := net.Dial("unix", sock)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte("GET /_ping HTTP/1.0\r\nHost: d\r\n\r\n")); err != nil {
		return err
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.Contains(line, " 200") {
		return errors.New(strings.TrimSpace(line))
	}
	return nil
}

// A remote engine stops the run before anything touches the daemon.
func TestARunOnARemoteEngineIsRefused(t *testing.T) {
	p, f := fixture(t)
	f.EngineInfo = &dockerx.Engine{Kind: dockerx.KindDocker, Endpoint: "tcp://10.0.0.5:2376"}
	env, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitGateway || !strings.Contains(err.Error(), "tcp://10.0.0.5:2376") {
		t.Fatalf("got %v", err)
	}
	if env.ExitCode != ExitGateway {
		t.Errorf("envelope exit %d", env.ExitCode)
	}
	for _, c := range f.Calls {
		if c.Op == "RunDetached" || c.Op == "NetworkCreate" {
			t.Fatalf("the daemon was driven: %v", f.Ops())
		}
	}
}

// A daemon in a VM: the filter container holds the VM's own socket path,
// with the SELinux exemption its role carries.
func TestARunOnADaemonInAVMMountsItsOwnSocket(t *testing.T) {
	if !filterSupported {
		t.Skip("no docker filter on this platform")
	}
	withHost(t, hostFacts{goos: "linux", hostname: "box", kernel: "7.1.13"})
	p, f := fixture(t)
	happyFake(f)
	f.LogsByName[containerName("cafe0000", "docker-filter")] = "ready\n" + sockfilter.SidecarReady + "\n"
	f.EngineInfo = &dockerx.Engine{Kind: dockerx.KindDocker, CLI: "docker", Version: "29.0.0",
		Endpoint: "unix:///home/me/.docker/desktop/docker.sock", DaemonHost: "docker-desktop", DaemonKernel: "6.10.14-linuxkit"}
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sidecar dockerx.RunSpec
	for _, c := range f.Calls {
		if c.Op == "RunDetached" && strings.HasSuffix(c.Arg, "-docker-filter") {
			sidecar = c.Run
		}
	}
	if !sidecar.DisableSELinuxLabel {
		t.Error("the filter container has no SELinux exemption")
	}
	found := false
	for _, m := range sidecar.Mounts {
		if m.Target == DockerSocket {
			found = m.Source == DockerSocket
		}
	}
	if !found {
		t.Errorf("the filter container's mounts %+v: want the VM's own %s", sidecar.Mounts, DockerSocket)
	}
	if !strings.Contains(env.CapabilitiesEnforced["engine"], "docker filter sidecar: the daemon reports docker-desktop") {
		t.Errorf("engine line %q", env.CapabilitiesEnforced["engine"])
	}
}
