package dockerx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func readEngineFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "engine", name)) //nolint:gosec // test fixture
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseEndpoint(t *testing.T) {
	for _, c := range []struct{ in, scheme, addr string }{
		{"unix:///var/run/docker.sock", SchemeUnix, "/var/run/docker.sock"},
		{"unix:///run/user/1000/podman/podman.sock\n", SchemeUnix, "/run/user/1000/podman/podman.sock"},
		{"/run/podman/podman.sock", SchemeUnix, "/run/podman/podman.sock"},
		{"npipe:////./pipe/dockerDesktopLinuxEngine", SchemeNpipe, "//./pipe/dockerDesktopLinuxEngine"},
		{"tcp://10.0.0.5:2376", "tcp", "10.0.0.5:2376"},
		{"ssh://me@build-box", "ssh", "build-box"},
	} {
		scheme, addr, err := ParseEndpoint(c.in)
		if err != nil || scheme != c.scheme || addr != c.addr {
			t.Errorf("ParseEndpoint(%q) = %q, %q, %v; want %q, %q", c.in, scheme, addr, err, c.scheme, c.addr)
		}
	}
	for _, bad := range []string{"", "docker.sock", "unix://relative.sock", "npipe://", "tcp://", "://x"} {
		if s, a, err := ParseEndpoint(bad); err == nil {
			t.Errorf("ParseEndpoint(%q) = %q, %q; want an error", bad, s, a)
		}
	}
}

func TestEngineSocketPathIsOnlyAUnixSocket(t *testing.T) {
	if got := (Engine{Endpoint: "unix:///run/podman/podman.sock"}).SocketPath(); got != "/run/podman/podman.sock" {
		t.Errorf("unix: %q", got)
	}
	for _, ep := range []string{"tcp://h:2375", "ssh://h", "npipe:////./pipe/docker_engine", ""} {
		if got := (Engine{Endpoint: ep}).SocketPath(); got != "" {
			t.Errorf("%q: SocketPath = %q, want none", ep, got)
		}
	}
}

// The docker CLI's answers on a rootful Docker Engine, recorded.
func TestDockerEngineFromRecordedRootfulDocker(t *testing.T) {
	e, err := parseDockerEngine(readEngineFixture(t, "docker-rootful.endpoint"),
		readEngineFixture(t, "docker-rootful.version.json"), readEngineFixture(t, "docker-rootful.info.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := Engine{Kind: KindDocker, CLI: "docker", Version: "29.7.2", Endpoint: "unix:///var/run/docker.sock",
		DaemonHost: "host-1", DaemonKernel: "7.1.13-200.fc44.x86_64", OS: "Fedora Linux 44 (KDE Plasma Desktop Edition)"}
	if e != want {
		t.Errorf("engine %+v\nwant   %+v", e, want)
	}
	if got := e.Summary(); got != "docker 29.7.2, rootful" {
		t.Errorf("summary %q", got)
	}
}

// The docker CLI pointed at rootless podman's socket: the kind is what the
// daemon says, the CLI what was driven.
func TestDockerEngineFromRecordedDockerCLIOnPodman(t *testing.T) {
	e, err := parseDockerEngine(readEngineFixture(t, "docker-cli-on-podman.endpoint"),
		readEngineFixture(t, "docker-cli-on-podman.version.json"), readEngineFixture(t, "docker-cli-on-podman.info.json"))
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != KindPodman || e.CLI != "docker" || e.Version != "5.8.4" || !e.Rootless ||
		e.SocketPath() != "/run/user/1000/podman/podman.sock" || e.OS != "fedora" {
		t.Errorf("engine %+v", e)
	}
	if got := e.Summary(); got != "podman 5.8.4 through the docker CLI, rootless" {
		t.Errorf("summary %q", got)
	}
}

func TestDockerEngineRefusesAnUnknownServer(t *testing.T) {
	_, err := parseDockerEngine("unix:///x.sock", `{"Version":"1","Components":[{"Name":"Something Else","Version":"1"}]}`, `{}`)
	if err == nil || !strings.Contains(err.Error(), `"Something Else"`) {
		t.Errorf("got %v", err)
	}
	if _, err := parseDockerEngine("unix:///x.sock", `{"Version":"1"}`, `{}`); err == nil {
		t.Error("a server with no component was given a kind")
	}
}

// podman's own CLI answers from `podman info`, recorded on rootless podman.
func TestPodmanEngineFromRecordedInfo(t *testing.T) {
	c, _ := podmanStub(t, `case "$1" in info) cat "$AGENTRY_TEST_INFO";; esac`)
	t.Setenv("AGENTRY_TEST_INFO", filepath.Join(mustAbs(t, "testdata/engine"), "podman-rootless.info.json"))
	info, err := c.podmanInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e := parsePodmanEngine(info)
	want := Engine{Kind: KindPodman, CLI: "podman", Version: "5.8.4", Endpoint: "unix:///run/user/1000/podman/podman.sock",
		Rootless: true, DaemonHost: "host-1", DaemonKernel: "7.1.13-200.fc44.x86_64", OS: "fedora"}
	if e != want {
		t.Errorf("engine %+v\nwant   %+v", e, want)
	}
	table, err := c.Runtimes(context.Background())
	if err != nil || len(table) != 1 || table["crun"].Path != "/usr/bin/crun" {
		t.Errorf("runtimes %v, %v: podman's answer is its one default runtime", table, err)
	}
	if os, err := c.OperatingSystem(context.Background()); err != nil || os != "fedora" {
		t.Errorf("operating system %q, %v", os, err)
	}
}

// A podman whose API socket does not answer is refused by name, with the
// command that starts it: a socket file left behind is not an engine.
func TestPodmanEngineRefusesASocketThatDoesNotAnswer(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "podman.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	info := `{"host":{"hostname":"h","kernel":"k","remoteSocket":{"path":"` + sock + `","exists":true},"security":{"rootless":true}},"version":{"Version":"5.8.4"}}`
	c, _ := podmanStub(t, `case "$1" in info) printf '%s' "$AGENTRY_TEST_INFO";; esac`)
	t.Setenv("AGENTRY_TEST_INFO", info)
	_, err := c.Engine(context.Background())
	if err == nil || !strings.Contains(err.Error(), "does not answer") || !strings.Contains(err.Error(), "systemctl --user enable --now podman.socket") {
		t.Fatalf("got %v", err)
	}
}

func TestPodmanEngineAnswersWhenItsSocketPings(t *testing.T) {
	sock := pingServer(t, http.StatusOK)
	info := `{"host":{"hostname":"h","kernel":"k","remoteSocket":{"path":"` + sock + `","exists":true},"security":{"rootless":false}},"version":{"Version":"5.8.4"}}`
	c, record := podmanStub(t, `case "$1" in info) printf '%s' "$AGENTRY_TEST_INFO";; esac`)
	t.Setenv("AGENTRY_TEST_INFO", info)
	e, err := c.Engine(context.Background())
	if err != nil || e.SocketPath() != sock || e.Rootless {
		t.Fatalf("engine %+v, %v", e, err)
	}
	// Kept for the life of the client.
	if _, err := c.Engine(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(recorded(t, record)); n != 1 {
		t.Errorf("podman was asked %d times, want once", n)
	}
}

func TestPingSocket(t *testing.T) {
	if err := pingSocket(context.Background(), pingServer(t, http.StatusOK)); err != nil {
		t.Errorf("a ping answered 200: %v", err)
	}
	if err := pingSocket(context.Background(), pingServer(t, http.StatusInternalServerError)); err == nil {
		t.Error("a ping answered 500 was taken")
	}
	if err := pingSocket(context.Background(), filepath.Join(t.TempDir(), "none.sock")); err == nil {
		t.Error("a missing socket was taken")
	}
}

// pingServer serves /_ping on a Unix socket with the given status.
func pingServer(t *testing.T, status int) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ping") // short: a socket path has a length limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { //nolint:gosec // test server
		if r.URL.Path != "/_ping" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte("OK"))
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

func TestNewChoosesTheEngineNamed(t *testing.T) {
	t.Setenv(EnvDocker, "")
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv(EnvPodman, "/opt/podman/bin/podman")
	t.Setenv(EnvEngine, "podman")
	if c := New(); !c.Podman || c.Bin != "/opt/podman/bin/podman" || c.detect {
		t.Errorf("AGENTRY_ENGINE=podman: %+v", c)
	}
	t.Setenv(EnvEngine, "docker")
	if c := New(); c.Podman || c.Bin != "docker" || c.detect {
		t.Errorf("AGENTRY_ENGINE=docker: %+v", c)
	}
	t.Setenv(EnvEngine, "")
	if c := New(); c.Podman || c.Bin != "docker" || !c.detect {
		t.Errorf("no engine named: %+v, want docker until one is found", c)
	}
	t.Setenv(EnvDocker, "/opt/odd/docker")
	if c := New(); c.Podman || c.detect {
		t.Errorf("a docker binary given: %+v, want it and no detection", c)
	}
	t.Setenv(EnvDocker, "")
	for _, k := range []string{"DOCKER_HOST", "DOCKER_CONTEXT"} {
		t.Setenv(k, "x")
		if c := New(); c.Podman || c.detect {
			t.Errorf("%s given: %+v, want docker and no detection", k, c)
		}
		t.Setenv(k, "")
	}
}

// Detection: a docker CLI that reaches a daemon is kept; one that does not,
// beside a podman that answers, gives way to podman; with neither, docker
// stays and the miss is not kept.
func TestDetectionPrefersADockerThatAnswersThenPodman(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil { //nolint:gosec // test stub must be executable
			t.Fatal(err)
		}
		return p
	}
	dockerUp := write("docker-up", `echo 29.7.2`)
	dockerDown := write("docker-down", `echo "Cannot connect" >&2; exit 1`)
	podman := write("podman", `echo amd64`)

	c := &CLI{Bin: dockerUp, detect: true}
	t.Setenv(EnvPodman, podman)
	c.resolve(context.Background())
	if c.Podman || c.Bin != dockerUp || c.detect {
		t.Errorf("docker answering: %+v", c)
	}
	c = &CLI{Bin: dockerDown, detect: true}
	c.resolve(context.Background())
	if !c.Podman || c.Bin != podman || c.detect {
		t.Errorf("docker silent, podman answering: %+v", c)
	}
	t.Setenv(EnvPodman, filepath.Join(dir, "no-podman"))
	c = &CLI{Bin: dockerDown, detect: true}
	c.resolve(context.Background())
	if c.Podman || c.Bin != dockerDown || !c.detect {
		t.Errorf("neither: %+v, want docker, still to be found", c)
	}
}

func TestQualifiedRef(t *testing.T) {
	for in, want := range map[string]string{
		"alpine:3.20":                       "docker.io/library/alpine:3.20",
		"docker/mcp-gateway@sha256:abc":     "docker.io/docker/mcp-gateway@sha256:abc",
		"ghcr.io/blaktron/agentry-runner:1": "ghcr.io/blaktron/agentry-runner:1",
		"localhost/agentry-exec:x":          "localhost/agentry-exec:x",
		"registry:5000/team/img":            "registry:5000/team/img",
	} {
		if got := qualifiedRef(in); got != want {
			t.Errorf("qualifiedRef(%q) = %q, want %q", in, got, want)
		}
	}
}

// podman takes docker's argv but for the alias of a container on several
// networks, which it writes name:alias=….
func TestPodmanArgvForSeveralNetworks(t *testing.T) {
	r := RunSpec{Name: "c", Image: "img", Network: "run", Alias: "proxy", ExtraNetworks: []string{"egress"}}
	got := r.argsFor(true, true)
	if !slices.Contains(got, "run:alias=proxy") || !slices.Contains(got, "egress") || slices.Contains(got, "name=run,alias=proxy") {
		t.Errorf("podman argv %q", got)
	}
	if d := r.args(true); !slices.Contains(d, "name=run,alias=proxy") {
		t.Errorf("docker argv %q", d)
	}
}

// A container's variable that podman would read as its own configuration
// travels as argv on podman, as HOME does on docker; any other name travels
// by name through the client's environment on both.
func TestPodmanKeepsItsOwnEnvironmentNames(t *testing.T) {
	r := RunSpec{Name: "c", Image: "img", Env: map[string]string{"XDG_RUNTIME_DIR": "/x", "TMPDIR": "/t", "CONTAINERS_CONF": "/c", "MODEL_KEY": "sekret"}}
	pairs := r.envPairsFor(true)
	if !slices.Equal(pairs, []string{"MODEL_KEY=sekret"}) {
		t.Errorf("podman's environment pairs %q", pairs)
	}
	argv := strings.Join(r.argsFor(true, true), " ")
	for _, want := range []string{"-e XDG_RUNTIME_DIR=/x", "-e TMPDIR=/t", "-e CONTAINERS_CONF=/c", "-e MODEL_KEY "} {
		if !strings.Contains(argv+" ", want) {
			t.Errorf("podman argv %q lacks %q", argv, want)
		}
	}
	if strings.Contains(argv, "sekret") {
		t.Error("a value reached podman's argv")
	}
	// docker's own environment is not podman's.
	if got := r.envPairsFor(false); len(got) != 4 {
		t.Errorf("docker's environment pairs %q", got)
	}
}

// podmanStub is stub's podman: a CLI driving podman's own commands.
func podmanStub(t *testing.T, script string) (*CLI, string) {
	t.Helper()
	c, record := stub(t, script)
	c.Podman = true
	return c, record
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// podman refuses a label on an anonymous volume, so its dialect makes each
// labelled volume by name, mounts it by name, and RemoveForce takes the
// volumes it made with their container.
func TestPodmanMakesLabelledVolumesByName(t *testing.T) {
	c, record := podmanStub(t, `case "$1 $2" in "volume ls") echo agentry-R-gateway-vol0;; esac`)
	r := RunSpec{Name: "agentry-R-gateway", Image: "gw", Volumes: []VolumeMount{{Target: "/misc", Labels: map[string]string{"org.agentry.run": "R"}}}}
	if err := c.RunDetached(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveForce(context.Background(), "agentry-R-gateway"); err != nil {
		t.Fatal(err)
	}
	got := recorded(t, record)
	want := []string{
		"volume create --label agentry.volume-of=agentry-R-gateway --label org.agentry.run=R agentry-R-gateway-vol0",
		"run -d --name agentry-R-gateway --mount type=volume,src=agentry-R-gateway-vol0,dst=/misc gw",
		"rm -fv agentry-R-gateway",
		"volume ls --filter label=agentry.volume-of=agentry-R-gateway --format {{.Name}}",
		"volume rm -f agentry-R-gateway-vol0",
	}
	if !slices.Equal(got, want) {
		t.Errorf("podman saw\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if r.named != nil || len(r.Volumes) != 1 {
		t.Error("the caller's spec changed")
	}
}

func TestPodmanReadSide(t *testing.T) {
	c, record := podmanStub(t, `case "$1" in version) echo linux/arm64;; esac`)
	arch, err := c.ServerArch(context.Background())
	if err != nil || arch != "arm64" {
		t.Errorf("arch %q, %v", arch, err)
	}
	if _, err := c.Logs(context.Background(), "c", LogsAll); err != nil {
		t.Fatal(err)
	}
	if err := c.Pull(context.Background(), "docker/mcp-gateway@sha256:abc"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"version --format {{.Client.OsArch}}",
		"logs --tail -1 c",
		"pull docker.io/docker/mcp-gateway@sha256:abc",
	}
	if got := recorded(t, record); !slices.Equal(got, want) {
		t.Errorf("podman saw %q, want %q", got, want)
	}
}

func TestPersonUser(t *testing.T) {
	if os.Getuid() < 0 {
		t.Skip("no POSIX ids")
	}
	own := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	if u, err := PersonUser(context.Background(), &Fake{}); err != nil || u != own {
		t.Errorf("rootful: %q, %v; want %q", u, err, own)
	}
	if u, err := PersonUser(context.Background(), &Fake{EngineInfo: &Engine{Rootless: true}}); err != nil || u != "0" {
		t.Errorf("rootless: %q, %v; want root, which is the person there", u, err)
	}
	if _, err := PersonUser(context.Background(), &Fake{EngineErr: errors.New("down")}); err == nil {
		t.Error("an engine that could not say was taken as rootful")
	}
}

// podman's default network is named in its info; docker's "bridge" is
// connected to that.
func TestPodmanConnectsDockersBridgeToItsDefaultNetwork(t *testing.T) {
	c, record := podmanStub(t, `case "$1" in info) cat "$AGENTRY_TEST_INFO";; esac`)
	t.Setenv("AGENTRY_TEST_INFO", filepath.Join(mustAbs(t, "testdata/engine"), "podman-rootless.info.json"))
	if err := c.NetworkConnect(context.Background(), DefaultBridge, "proxy"); err != nil {
		t.Fatal(err)
	}
	if err := c.NetworkConnect(context.Background(), "agentry-R-net", "proxy"); err != nil {
		t.Fatal(err)
	}
	got := recorded(t, record)
	want := []string{"info --format json", "network connect podman proxy", "network connect agentry-R-net proxy"}
	if !slices.Equal(got, want) {
		t.Errorf("podman saw %q, want %q", got, want)
	}
}
