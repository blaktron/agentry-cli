package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/owner"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// The test binary stands in for `podman system service`: with
// fakePodmanServiceEnv naming a socket, it answers the Docker API's /_ping
// there, appends a line per request to the file fakePodmanHitsEnv names,
// and exits on SIGTERM.
const (
	fakePodmanServiceEnv = "AGENTRY_TEST_PODMAN_SERVICE_SOCKET"
	fakePodmanHitsEnv    = "AGENTRY_TEST_PODMAN_SERVICE_HITS"
)

func playPodmanService(sock string) int {
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return 1
	}
	hits := os.Getenv(fakePodmanHitsEnv)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { //nolint:gosec // a test stand-in
		if hits != "" {
			f, _ := os.OpenFile(hits, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // test-owned path
			_, _ = f.WriteString(r.Method + " " + r.URL.Path + "\n")
			_ = f.Close()
		}
		_, _ = w.Write([]byte("OK"))
	})}
	go func() { _ = srv.Serve(ln) }()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM)
	<-stop
	_ = srv.Close()
	return 0
}

// fakePodmanService is the stand-in as a Fake's PodmanService, recording
// the requests it serves into hits.
func fakePodmanService(t *testing.T, hits string) func(runtime, socket string) *exec.Cmd {
	t.Helper()
	return func(_, socket string) *exec.Cmd {
		cmd := exec.Command(os.Args[0]) //nolint:gosec // this test binary
		cmd.Env = append(os.Environ(), fakePodmanServiceEnv+"="+socket, fakePodmanHitsEnv+"="+hits)
		return cmd
	}
}

// alive says whether a process with pid exists.
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// On podman a run whose policy names a runtime drives its gateway through a
// podman API service of its own, started with that runtime: the filter's
// upstream is the service's socket, the service is recorded by pid and
// stopped with the run, and the receipt says so. The runner's own classes
// are created with podman run --runtime and checked.
func TestARunOnPodmanHoldsTheRuntimeThroughAService(t *testing.T) {
	if !filterSupported {
		t.Skip("no docker filter on this platform")
	}
	withHost(t, hostFacts{goos: "linux", hostname: "box", kernel: "7.1.13"})
	p, f := fixtureWith(t, func(pol *policy.Policy) {
		pol.Docker.Runtime = "runsc"
		pol.Docker.GatewayRuntime = "runsc-uds"
	})
	happyFake(f)
	sock, _ := fakePodmanSocket(t)
	f.EngineInfo = &dockerx.Engine{Kind: dockerx.KindPodman, CLI: "podman", Version: "5.8.4", Rootless: true,
		Endpoint: "unix://" + sock, DaemonHost: "box", DaemonKernel: "7.1.13"}
	hits := filepath.Join(t.TempDir(), "hits.log")
	f.PodmanService = fakePodmanService(t, hits)
	var rec podmanServiceRecord
	var pidFile string
	var gwRuntime string
	f.OnDetached = func(r dockerx.RunSpec) {
		if !strings.HasSuffix(r.Name, "-gateway") {
			return
		}
		gwRuntime = r.Runtime
		pidFile = filepath.Join(p.RunRoot, "runs", strings.TrimSuffix(strings.TrimPrefix(r.Name, "agentry-"), "-gateway"), podmanServicePIDFile)
		b, err := os.ReadFile(pidFile) //nolint:gosec // the test's own run directory
		if err != nil {
			t.Errorf("the service's record while the run is up: %v", err)
			return
		}
		if err := json.Unmarshal(b, &rec); err != nil {
			t.Errorf("the service's record: %v", err)
		}
		// Through the filter the gateway mounts, a request reaches the
		// run's service, not podman's own socket.
		for _, m := range r.Mounts {
			if m.Target == DockerSocket {
				if err := ping(m.Source); err != nil {
					t.Errorf("the gateway's socket: %v", err)
				}
			}
		}
	}
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gwRuntime != "runsc-uds" {
		t.Errorf("gateway created with runtime %q, want runsc-uds", gwRuntime)
	}
	if rec.PID <= 0 || rec.Socket == "" || !strings.HasSuffix(rec.Socket, "podman.sock") {
		t.Fatalf("the service's record = %+v", rec)
	}
	if b, _ := os.ReadFile(hits); !strings.Contains(string(b), "GET /_ping") { //nolint:gosec // test-owned path
		t.Errorf("no request reached the run's service: %q", b)
	}
	if alive(rec.PID) {
		t.Errorf("the service (pid %d) outlived the run", rec.PID)
	}
	for _, left := range []string{rec.Socket, pidFile} {
		if _, err := os.Stat(left); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s outlived the run: %v", left, err)
		}
	}
	ops := f.Ops()
	if !slices.Contains(ops, "PodmanServiceCommand:runsc@"+rec.Socket) {
		t.Errorf("the service was not started with the policy's runtime: %v", ops)
	}
	for _, class := range []string{"-agent", "-probe"} {
		c, ok := f.FindRun(class)
		if !ok || c.Runtime != "runsc" {
			t.Errorf("%s runtime = %q (found %v), want runsc", class, c.Runtime, ok)
		}
		if !slices.Contains(ops, "ContainerRuntime:"+c.Name) {
			t.Errorf("%s was not checked after create: %v", class, ops)
		}
	}
	line := env.CapabilitiesEnforced["runtime"]
	for _, want := range []string{"runsc: created with podman --runtime runsc, each checked",
		"created through a podman API service of the run's own started with --runtime runsc",
		"the relays run under runsc through the run's podman API service too", "net.ipv4.ip_unprivileged_port_start=0",
		"the gateway is created with podman --runtime runsc-uds"} {
		if !strings.Contains(line, want) {
			t.Errorf("receipt runtime line lacks %q: %q", want, line)
		}
	}
	if strings.Contains(line, "the relays (each resolves") {
		t.Errorf("the relays are named as a gap on podman: %q", line)
	}
	if _, err := os.Stat(filepath.Join(p.OutDir, "receipts", podmanServiceLog)); err != nil {
		t.Errorf("the service's log is not among the receipts: %v", err)
	}
}

// An engine that takes a runtime and does not apply it stops the run at the
// first class checked, naming it, and the container never starts.
func TestARunRefusesAContainerCreatedUnderAnotherRuntime(t *testing.T) {
	p, f := fixtureWith(t, func(pol *policy.Policy) { pol.Docker.Runtime = "runsc" })
	happyFake(f)
	f.DropRuntime = true
	env, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitGateway {
		t.Fatalf("got %v", err)
	}
	var mm *dockerx.RuntimeMismatch
	if !errors.As(err, &mm) || mm.Class != "gateway's health probe" || mm.Want != "runsc" {
		t.Fatalf("got %v", err)
	}
	for _, want := range []string{"the gateway's health probe (", "created under the engine's default", `docker.runtime "runsc"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if env.ExitCode != ExitGateway {
		t.Errorf("envelope exit %d", env.ExitCode)
	}
	probe, _ := f.FindRun("-probe")
	ops := f.Ops()
	if slices.Contains(ops, "StartAttached:"+probe.Name) || !slices.Contains(ops, "RemoveForce:"+probe.Name) {
		t.Errorf("the mismatched probe was started, or not removed: %v", ops)
	}
	if _, ok := f.FindRun("-agent"); ok {
		t.Error("the agent ran after the refusal")
	}
}

// A run that died leaves its service's record; the next run's sweep ends
// the process when it is still the one recorded, never a reused pid, and
// removes the socket and the record.
func TestTheSweepStopsADeadRunsPodmanService(t *testing.T) {
	dir := t.TempDir()
	short, err := os.MkdirTemp("", "ps")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	sock := filepath.Join(short, "podman.sock")
	cmd := fakePodmanService(t, "")("runsc", sock)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	deadline := time.Now().Add(10 * time.Second)
	for dockerx.PingSocket(context.Background(), sock) != nil {
		if time.Now().After(deadline) {
			t.Fatal("the stand-in never answered")
		}
		time.Sleep(50 * time.Millisecond)
	}
	start, _ := owner.StartTime(cmd.Process.Pid)
	write := func(rec podmanServiceRecord) {
		b, _ := json.Marshal(rec)
		if err := os.WriteFile(filepath.Join(dir, podmanServicePIDFile), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A record whose start time is another process's: the process is
	// left alone (a reused pid), the dead run's socket and record go.
	write(podmanServiceRecord{PID: cmd.Process.Pid, Start: start + 7, Socket: sock})
	stopLeftPodmanService(dir)
	if !alive(cmd.Process.Pid) {
		t.Fatal("a reused pid was killed")
	}
	if _, err := os.Stat(filepath.Join(dir, podmanServicePIDFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the record of a dead run was kept: %v", err)
	}
	// The recorded process: ended, with its socket and record.
	write(podmanServiceRecord{PID: cmd.Process.Pid, Start: start, Socket: sock})
	done := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(done) }()
	stopLeftPodmanService(dir)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the dead run's service was not stopped")
	}
	for _, left := range []string{sock, filepath.Join(dir, podmanServicePIDFile)} {
		if _, err := os.Stat(left); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s left after the sweep: %v", left, err)
		}
	}
	// No record: nothing to do.
	stopLeftPodmanService(dir)
}

// On a rootless engine the steps that write the person's files run as the
// container's root, which is the person there: the tools server hands its
// files to 0:0, and the fetch, browser and host link users follow.
func TestRootlessRunsThePersonsStepsAsRoot(t *testing.T) {
	if os.Getuid() < 0 {
		t.Skip("no POSIX ids")
	}
	withHost(t, hostFacts{goos: "linux", hostname: "box", kernel: "7.1.13"})
	p, f := fixture(t)
	happyFake(f)
	sock, _ := fakePodmanSocket(t)
	f.EngineInfo = &dockerx.Engine{Kind: dockerx.KindPodman, CLI: "podman", Version: "5.8.4", Rootless: true,
		Endpoint: "unix://" + sock, DaemonHost: "box", DaemonKernel: "7.1.13"}
	var owner struct {
		Owner *struct {
			UID int `json:"uid"`
			GID int `json:"gid"`
		} `json:"owner"`
	}
	var personUser string
	f.OnDetached = func(r dockerx.RunSpec) {
		if !strings.HasSuffix(r.Name, "-gateway") {
			return
		}
		runID := strings.TrimSuffix(strings.TrimPrefix(r.Name, "agentry-"), "-gateway")
		b, err := os.ReadFile(filepath.Join(p.RunRoot, "runs", runID, "cfg", "gateway", "toolspec.json")) //nolint:gosec // the test's own run directory
		if err != nil {
			t.Errorf("toolspec: %v", err)
			return
		}
		if err := json.Unmarshal(b, &owner); err != nil {
			t.Error(err)
		}
		rd := runDirsAt(filepath.Join(p.RunRoot, "runs", runID))
		rd.engine = *f.EngineInfo
		personUser = rd.personUser()
	}
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if owner.Owner == nil || owner.Owner.UID != 0 || owner.Owner.GID != 0 {
		t.Errorf("the tools server hands files to %+v on a rootless engine, want 0:0", owner.Owner)
	}
	if personUser != "0" {
		t.Errorf("the person's user on a rootless engine = %q, want 0", personUser)
	}
	// On a rootful engine they are the invoking user's.
	e := dockerx.LocalDockerEngine()
	if uid, gid, ok := e.PersonIDs(); !ok || uid != os.Getuid() || gid != os.Getgid() {
		t.Errorf("rootful person ids = %d:%d (%v)", uid, gid, ok)
	}
}

// Where the CLI does not serve the filter on podman (the override, or a
// daemon that reports another kernel), a runtime is refused before the run
// drives the daemon: only the CLI can start the service that holds it and
// read each container's runtime back.
func TestARuntimeOnPodmanNeedsTheCLIsFilter(t *testing.T) {
	if !filterSupported {
		t.Skip("no docker filter on this platform")
	}
	withHost(t, hostFacts{goos: "linux", hostname: "box", kernel: "7.1.13"})
	t.Setenv(EnvDockerFilter, "sidecar")
	p, f := fixtureWith(t, func(pol *policy.Policy) { pol.Docker.Runtime = "runsc" })
	happyFake(f)
	f.EngineInfo = &dockerx.Engine{Kind: dockerx.KindPodman, CLI: "podman", Version: "5.8.4", Rootless: true,
		Endpoint: "unix:///run/user/1000/podman/podman.sock", DaemonHost: "box", DaemonKernel: "7.1.13"}
	env, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitGateway || !strings.Contains(err.Error(), EnvDockerFilter) || !strings.Contains(err.Error(), "only where the CLI serves the run's docker filter") {
		t.Fatalf("got %v", err)
	}
	if env.ExitCode != ExitGateway {
		t.Errorf("envelope exit %d", env.ExitCode)
	}
	for _, c := range f.Calls {
		if strings.HasSuffix(c.Arg, "-gateway") && (c.Op == "RunDetached" || c.Op == "Create") {
			t.Fatal("the gateway was started under an unenforced runtime")
		}
	}
	if strings.Contains(env.CapabilitiesEnforced["runtime"], "podman API service") {
		t.Errorf("the receipt claims the service: %q", env.CapabilitiesEnforced["runtime"])
	}
}

// The service's socket sits beside the filter's under the runner home, and
// moves to a short private directory when that path would not bind.
func TestThePodmanServiceSocketFitsASocketPath(t *testing.T) {
	rd := &runDirs{root: t.TempDir()}
	sock, err := podmanServiceSocket(rd, "cafe0000", "")
	if err != nil || sock != filepath.Join(rd.root, "podman.sock") {
		t.Errorf("no runner home: %q, %v", sock, err)
	}
	home := t.TempDir()
	sock, err = podmanServiceSocket(rd, "cafe0000", home)
	if err != nil || sock != filepath.Join(home, "s", "cafe0000-podman.sock") {
		t.Errorf("a runner home: %q, %v", sock, err)
	}
	long := filepath.Join(t.TempDir(), strings.Repeat("deep/", 30))
	if err := os.MkdirAll(long, 0o700); err != nil {
		t.Fatal(err)
	}
	sock, err = podmanServiceSocket(rd, "cafe0000", long)
	if err != nil || len(sock) > maxSocketPath || strings.HasPrefix(sock, long) {
		t.Errorf("a home too deep: %q (%d), %v", sock, len(sock), err)
	}
	s := &podmanService{socket: sock, pidFile: filepath.Join(rd.root, podmanServicePIDFile)}
	s.stop()
	if _, err := os.Stat(filepath.Dir(sock)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the short directory was kept: %v", err)
	}
}
