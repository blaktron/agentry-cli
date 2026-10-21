// CLI wrapper tests. The pure argv/env assembly is table-tested directly;
// the exec-facing methods run hermetically against a stub `docker` binary
// (a shell script recording its argv) so every classification rule — env
// values via process env and never argv, not-found tolerance, the 125+
// docker-failure band — is proven without a docker daemon.
package dockerx

import (
	"context"
	"encoding/csv"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// stub installs a fake docker binary. The script body runs after a line
// that appends the invocation's argv to the record file; the record path
// travels via the AGENTRY_TEST_RECORD env var (the CLI passes the parent
// environment through, which is itself part of the contract under test).
func stub(t *testing.T, script string) (c *CLI, record string) {
	t.Helper()
	dir := t.TempDir()
	record = filepath.Join(dir, "argv.log")
	t.Setenv("AGENTRY_TEST_RECORD", record)
	bin := filepath.Join(dir, "docker")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$AGENTRY_TEST_RECORD\"\n" + script + "\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil { //nolint:gosec // test stub must be executable
		t.Fatal(err)
	}
	return &CLI{Bin: bin}, record
}

// recorded returns the argv lines the stub saw.
func recorded(t *testing.T, record string) []string {
	t.Helper()
	b, err := os.ReadFile(record) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatalf("stub was never invoked: %v", err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// engineAnswers is a stub script's prelude that answers the docker CLI's
// three engine questions as Docker Engine on this machine would, so a client
// that asks them once caches the answer; kind names the server's component
// ("Engine", or "Podman Engine" for podman behind the docker CLI).
func engineAnswers(kind string) string {
	return `case "$1 $2" in
  "context inspect") echo unix:///var/run/docker.sock; exit 0;;
  "version --format") echo '{"Version":"29.7.2","Components":[{"Name":"` + kind + `","Version":"29.7.2"}]}'; exit 0;;
  "info --format") case "$3" in *KernelVersion*) echo '{"name":"box","kernel":"7.1","security":[],"os":"Fedora"}'; exit 0;; esac;;
esac
`
}

// ownArgv drops the engine's own questions from the lines the stub saw.
func ownArgv(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.HasPrefix(l, "context inspect ") || strings.HasPrefix(l, "version --format ") || strings.HasPrefix(l, "info --format ") {
			continue
		}
		out = append(out, l)
	}
	return out
}

func TestNewHonorsEnvOverride(t *testing.T) {
	t.Setenv("AGENTRY_DOCKER", "/opt/odd/docker")
	if got := New().Bin; got != "/opt/odd/docker" {
		t.Errorf("Bin = %q", got)
	}
	t.Setenv("AGENTRY_DOCKER", "")
	if got := New().Bin; got != "docker" {
		t.Errorf("default Bin = %q", got)
	}
}

func TestRunSpecArgs(t *testing.T) {
	full := RunSpec{
		Name:  "agentry-x-gateway",
		Image: "img:1",
		Argv:  []string{"--flag", "value"},
		Env:   map[string]string{"B_TOKEN": "sekret", "A_KEY": "also-sekret"},
		Mounts: []Mount{
			{Source: "/host/cfg", Target: "/mcp", ReadOnly: true, Relabel: true},
			{Source: "/host/out", Target: "/out"},
		},
		Network:    "run-net",
		Alias:      "gateway",
		Labels:     map[string]string{"b": "2", "a": "1"},
		AutoRemove: true,
		Runtime:    "runsc",
		ExtraHosts: []string{"gateway:172.19.0.2", "llm-proxy:172.19.0.3"},
	}
	wantDetach := []string{
		"run", "-d", "--rm",
		"--name", "agentry-x-gateway",
		"--runtime", "runsc",
		"--add-host", "gateway:172.19.0.2", "--add-host", "llm-proxy:172.19.0.3",
		"--network", "run-net",
		"--network-alias", "gateway",
		"--label", "a=1", "--label", "b=2",
		"-e", "A_KEY", "-e", "B_TOKEN", // names only, sorted — values never in argv
		"-v", "/host/cfg:/mcp:ro,z",
		"-v", "/host/out:/out",
		"img:1", "--flag", "value",
	}
	if got := full.args(true); !reflect.DeepEqual(got, wantDetach) {
		t.Errorf("args(true) =\n%q\nwant\n%q", got, wantDetach)
	}
	for _, a := range full.args(true) {
		if strings.Contains(a, "sekret") {
			t.Fatalf("env value leaked into argv: %q", a)
		}
	}

	// A container joining several networks at create: the alias rides the first
	// network's long form, never a flag that would apply to every one.
	multi := RunSpec{Name: "p", Image: "i", Network: "run-net", Alias: "llm-proxy", ExtraNetworks: []string{"run-egress"}}
	if got, want := multi.args(true), []string{"run", "-d", "--name", "p", "--network", "name=run-net,alias=llm-proxy", "--network", "run-egress", "i"}; !reflect.DeepEqual(got, want) {
		t.Errorf("several networks: %q, want %q", got, want)
	}

	interactive := RunSpec{Name: "n", Image: "i", Interactive: true}
	got := interactive.args(false)
	want := []string{"run", "-i", "--name", "n", "i"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("foreground args = %q, want %q", got, want)
	}

	// A container whose stdout is data its caller reads keeps no log of it
	// (the MCP server installer's archive).
	unlogged := RunSpec{Name: "n", Image: "i", Interactive: true, NoLog: true}
	if got, want := unlogged.args(false), []string{"run", "-i", "--log-driver", "none", "--name", "n", "i"}; !reflect.DeepEqual(got, want) {
		t.Errorf("no-log args = %q, want %q", got, want)
	}

	// A container that writes into the user's directory runs as the user.
	asUser := RunSpec{Name: "n", Image: "i", User: "1000:1000"}
	if got := asUser.args(false); !reflect.DeepEqual(got, []string{"run", "--name", "n", "--user", "1000:1000", "i"}) {
		t.Errorf("user args = %q", got)
	}

	// A one-shot that borrows an image for a shell command replaces its
	// entrypoint and runs as root on no network (the session store's
	// ownership step); a volume mounts by name like a path.
	oneShot := RunSpec{Name: "n", Image: "i", Entrypoint: "sh", User: "0:0", Network: "none",
		Argv:   []string{"-c", `chown -- "$1" "$2"`, "sh", "docker-agent", "/session"},
		Mounts: []Mount{{Source: "agentry-x-session", Target: "/session"}}}
	if got := oneShot.args(false); !reflect.DeepEqual(got, []string{"run", "--name", "n", "--user", "0:0", "--entrypoint", "sh", "--network", "none",
		"-v", "agentry-x-session:/session", "i", "-c", `chown -- "$1" "$2"`, "sh", "docker-agent", "/session"}) {
		t.Errorf("one-shot args = %q", got)
	}

	minimal := RunSpec{Image: "i"}
	if got := minimal.args(false); !reflect.DeepEqual(got, []string{"run", "i"}) {
		t.Errorf("minimal args = %q", got)
	}
}

// TestCLIVolumeOps: the session store's volume is created with the run
// label, listed by it and removed with force.
func TestCLIVolumeOps(t *testing.T) {
	c, record := stub(t, `case "$*" in *"volume ls"*) printf 'agentry-x-session\n';; esac; exit 0`)
	ctx := context.Background()
	if err := c.VolumeCreate(ctx, "agentry-x-session", map[string]string{"agentry.run": "x"}); err != nil {
		t.Fatal(err)
	}
	names, err := c.VolumesByLabel(ctx, "agentry.run=x")
	if err != nil || !reflect.DeepEqual(names, []string{"agentry-x-session"}) {
		t.Fatalf("VolumesByLabel = %v, %v", names, err)
	}
	if err := c.VolumeRemove(ctx, "agentry-x-session"); err != nil {
		t.Fatal(err)
	}
	got := recorded(t, record)
	want := []string{
		"volume create --label agentry.run=x agentry-x-session",
		"volume ls --filter label=agentry.run=x --format {{.Name}}",
		"volume rm -f agentry-x-session",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv lines = %q\nwant %q", got, want)
	}
}

// TestCLINetworksByLabel: a dead run's networks are listed by its run
// label, one name a line.
func TestCLINetworksByLabel(t *testing.T) {
	c, record := stub(t, `case "$*" in *"network ls"*) printf 'agentry-x-net\nagentry-x-src-repo\n\n';; esac; exit 0`)
	names, err := c.NetworksByLabel(context.Background(), "agentry.run=x")
	if err != nil || !reflect.DeepEqual(names, []string{"agentry-x-net", "agentry-x-src-repo"}) {
		t.Fatalf("NetworksByLabel = %v, %v", names, err)
	}
	if got, want := recorded(t, record), []string{"network ls --filter label=agentry.run=x --format {{.Name}}"}; !reflect.DeepEqual(got, want) {
		t.Errorf("argv lines = %q\nwant %q", got, want)
	}
}

func TestEnvPairsSorted(t *testing.T) {
	r := RunSpec{Env: map[string]string{"Z": "26", "A": "1", "M": "13"}}
	want := []string{"A=1", "M=13", "Z=26"}
	if got := r.envPairs(); !reflect.DeepEqual(got, want) {
		t.Errorf("envPairs = %q, want %q", got, want)
	}
	if got := (RunSpec{}).envPairs(); len(got) != 0 {
		t.Errorf("empty env produced %q", got)
	}
}

func TestSortedKeys(t *testing.T) {
	got := sortedKeys(map[string]int{"c": 3, "a": 1, "b": 2})
	if !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("sortedKeys = %q", got)
	}
	if got := sortedKeys(map[string]int(nil)); len(got) != 0 {
		t.Errorf("nil map produced %q", got)
	}
}

func TestIsNotFound(t *testing.T) {
	tests := []struct {
		msg  string
		want bool
	}{
		{"Error response from daemon: No such container: agentry-x", true},
		{"Error response from daemon: network agentry-x-net not found", true},
		{"Error: No such network: agentry-x-net", false}, // legacy phrasing is NOT tolerated — documented behavior
		{"permission denied while trying to connect", false},
	}
	for _, tc := range tests {
		if got := isNotFound(errors.New(tc.msg)); got != tc.want {
			t.Errorf("isNotFound(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

func TestAsExitError(t *testing.T) {
	err := exec.Command("/bin/sh", "-c", "exit 3").Run()
	var exitErr *exec.ExitError
	if !asExitError(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Fatalf("asExitError on a real exit: %v / %v", err, exitErr)
	}
	exitErr = nil
	if asExitError(errors.New("plain"), &exitErr) {
		t.Fatal("plain error must not match")
	}
}

func TestCLIAvailable(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		c, record := stub(t, `[ "$1" = version ] && echo 29.8.1 && exit 0; exit 1`)
		if err := c.Available(context.Background()); err != nil {
			t.Fatalf("Available: %v", err)
		}
		// version, never info: info runs every CLI plugin before it asks
		// the daemon.
		if got := recorded(t, record); len(got) != 1 || got[0] != "version --format {{.Server.Version}}" {
			t.Errorf("argv = %q", got)
		}
	})
	t.Run("no server version", func(t *testing.T) {
		c, _ := stub(t, `echo; exit 0`)
		err := c.Available(context.Background())
		if err == nil || !strings.Contains(err.Error(), "daemon not reachable") {
			t.Fatalf("err = %v, want an empty server version taken as no daemon", err)
		}
	})
	t.Run("binary missing", func(t *testing.T) {
		c := &CLI{Bin: filepath.Join(t.TempDir(), "absent")}
		err := c.Available(context.Background())
		if err == nil || !strings.Contains(err.Error(), "docker CLI not found") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("daemon unreachable", func(t *testing.T) {
		c, _ := stub(t, `echo "Cannot connect to the Docker daemon" >&2; exit 1`)
		err := c.Available(context.Background())
		if err == nil || !strings.Contains(err.Error(), "daemon not reachable") ||
			!strings.Contains(err.Error(), "Cannot connect") {
			t.Fatalf("err = %v, want the daemon failure with stderr detail", err)
		}
	})
}

func TestCLIImageExists(t *testing.T) {
	c, record := stub(t, `case "$*" in *present:1*) exit 0;; *) exit 1;; esac`)
	ok, err := c.ImageExists(context.Background(), "present:1")
	if err != nil || !ok {
		t.Fatalf("present image: (%v, %v)", ok, err)
	}
	ok, err = c.ImageExists(context.Background(), "absent:1")
	if err != nil || ok {
		t.Fatalf("absent image: (%v, %v), want (false, nil) — inspect exit 1 is not an error", ok, err)
	}
	if got := recorded(t, record)[0]; got != "image inspect --format {{.Id}} present:1" {
		t.Errorf("argv = %q", got)
	}
}

func TestCLIPull(t *testing.T) {
	c, record := stub(t, `[ "$1" = pull ] && exit 0; exit 1`)
	if err := c.Pull(context.Background(), "docker/mcp-gateway:v1"); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if got := recorded(t, record)[0]; got != "pull docker/mcp-gateway:v1" {
		t.Errorf("argv = %q", got)
	}

	c, _ = stub(t, `echo "pull access denied" >&2; exit 1`)
	err := c.Pull(context.Background(), "private:1")
	if err == nil || !strings.Contains(err.Error(), "docker pull") || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("err = %v", err)
	}
}

func TestCLIBuildArgOrdering(t *testing.T) {
	c, record := stub(t, engineAnswers("Engine")+`exit 0`)
	err := c.Build(context.Background(), "/ctx/dir", "agentry-runner:test",
		map[string]string{"GO_IMAGE": "golang:1.27", "BASE": "alpine"})
	if err != nil {
		t.Fatal(err)
	}
	want := "build -t agentry-runner:test --build-arg BASE=alpine --build-arg GO_IMAGE=golang:1.27 /ctx/dir"
	if got := ownArgv(recorded(t, record))[0]; got != want {
		t.Errorf("argv = %q\nwant   %q", got, want)
	}
}

func TestCLINetworkOps(t *testing.T) {
	c, record := stub(t, `exit 0`)
	ctx := context.Background()
	if err := c.NetworkCreate(ctx, "run-net", true, map[string]string{"b": "2", "a": "1"}); err != nil {
		t.Fatal(err)
	}
	if err := c.NetworkConnect(ctx, "bridge", "agentry-x-proxy"); err != nil {
		t.Fatal(err)
	}
	if err := c.NetworkRemove(ctx, "run-net"); err != nil {
		t.Fatal(err)
	}
	got := recorded(t, record)
	want := []string{
		"network create --driver bridge --internal --label a=1 --label b=2 run-net",
		"network connect bridge agentry-x-proxy",
		"network rm run-net",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv lines = %q\nwant %q", got, want)
	}

	// External (non-internal) network drops the flag.
	c2, record2 := stub(t, `exit 0`)
	if err := c2.NetworkCreate(ctx, "n", false, nil); err != nil {
		t.Fatal(err)
	}
	if got := recorded(t, record2)[0]; got != "network create --driver bridge n" {
		t.Errorf("argv = %q", got)
	}

	// Not-found removal is tolerated; other failures are not.
	c3, _ := stub(t, `echo "Error response from daemon: network run-net not found" >&2; exit 1`)
	if err := c3.NetworkRemove(ctx, "run-net"); err != nil {
		t.Fatalf("not-found network rm must be nil, got %v", err)
	}
	c4, _ := stub(t, `echo "permission denied" >&2; exit 1`)
	if err := c4.NetworkRemove(ctx, "run-net"); err == nil {
		t.Fatal("real network rm failure swallowed")
	}
}

func TestCLIRunDetachedEnvNeverInArgv(t *testing.T) {
	dir := t.TempDir()
	envDump := filepath.Join(dir, "env.txt")
	t.Setenv("AGENTRY_TEST_ENVDUMP", envDump)
	c, record := stub(t, `printf '%s' "$GATEWAY_TOKEN" > "$AGENTRY_TEST_ENVDUMP"; exit 0`)
	err := c.RunDetached(context.Background(), RunSpec{
		Name:  "agentry-x-gateway",
		Image: "img:1",
		Env:   map[string]string{"GATEWAY_TOKEN": "sekret-token-value"},
	})
	if err != nil {
		t.Fatal(err)
	}
	argv := recorded(t, record)[0]
	if !strings.Contains(argv, "-e GATEWAY_TOKEN") {
		t.Errorf("argv missing -e NAME: %q", argv)
	}
	if strings.Contains(argv, "sekret-token-value") {
		t.Errorf("env VALUE leaked into argv (visible in process listings): %q", argv)
	}
	got, err := os.ReadFile(envDump) //nolint:gosec // test-owned path
	if err != nil || string(got) != "sekret-token-value" {
		t.Fatalf("value did not reach the docker client process env: (%q, %v)", got, err)
	}

	cFail, _ := stub(t, `echo "driver failed" >&2; exit 125`)
	err = cFail.RunDetached(context.Background(), RunSpec{Name: "agentry-x-gateway", Image: "img:1"})
	if err == nil || !strings.Contains(err.Error(), "agentry-x-gateway") || !strings.Contains(err.Error(), "driver failed") {
		t.Fatalf("err = %v, want the container name and stderr detail", err)
	}
}

// The docker client reads some of the names a container is given — HOME above
// all, which locates ~/.docker/config.json and with it the current context.
// Passed by name, the gateway's HOME=/root became the client's, and on Docker
// Desktop for macOS, whose context is the only way to its socket, every run
// failed at the gateway with /var/run/docker.sock not found. Those names go to
// the container in argv; the client keeps the caller's.
func TestCLIRunDetachedKeepsTheClientsOwnEnvironment(t *testing.T) {
	dir := t.TempDir()
	envDump := filepath.Join(dir, "env.txt")
	t.Setenv("AGENTRY_TEST_ENVDUMP", envDump)
	t.Setenv("HOME", "/Users/person")
	t.Setenv("DOCKER_CONTEXT", "desktop-linux")
	c, record := stub(t, `printf '%s|%s|%s' "$HOME" "$DOCKER_CONTEXT" "$GATEWAY_TOKEN" > "$AGENTRY_TEST_ENVDUMP"; exit 0`)
	err := c.RunDetached(context.Background(), RunSpec{
		Name:  "agentry-x-gateway",
		Image: "img:1",
		Env: map[string]string{
			"HOME":           "/root",
			"DOCKER_CONTEXT": "default",
			"GATEWAY_TOKEN":  "sekret-token-value",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(envDump) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatal(err)
	}
	if want := "/Users/person|desktop-linux|sekret-token-value"; string(got) != want {
		t.Errorf("the docker client ran with HOME|DOCKER_CONTEXT|token = %q, want %q", got, want)
	}
	argv := recorded(t, record)[0]
	for _, want := range []string{"-e DOCKER_CONTEXT=default", "-e HOME=/root", "-e GATEWAY_TOKEN"} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv missing %q: %q", want, argv)
		}
	}
	if strings.Contains(argv, "sekret-token-value") {
		t.Errorf("a secret's value reached argv: %q", argv)
	}
}

func TestClientReadsName(t *testing.T) {
	for name, want := range map[string]bool{
		"HOME": true, "PATH": true, "USERPROFILE": true, "Path": true,
		"DOCKER_HOST": true, "DOCKER_CONFIG": true, "DOCKER_CONTEXT": true,
		"GATEWAY_TOKEN": false, "ANTHROPIC_API_KEY": false, "HOMEDIR": false,
		"MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS": false,
	} {
		if got := clientReads(name); got != want {
			t.Errorf("clientReads(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestCLIRunForeground(t *testing.T) {
	ctx := context.Background()
	spec := RunSpec{Name: "agentry-x-agent", Image: "img:1"}

	t.Run("stdio and exit 0", func(t *testing.T) {
		c, _ := stub(t, `cat; echo "to stdout"; echo "to stderr" >&2; exit 0`)
		var out, errb strings.Builder
		code, err := c.RunForeground(ctx, spec, strings.NewReader("task in"), &out, &errb)
		if err != nil || code != 0 {
			t.Fatalf("(%d, %v)", code, err)
		}
		if got := out.String(); got != "task in"+"to stdout\n" {
			t.Errorf("stdout = %q", got)
		}
		if got := errb.String(); got != "to stderr\n" {
			t.Errorf("stderr = %q", got)
		}
	})
	t.Run("container exit code is a result, not an error", func(t *testing.T) {
		c, _ := stub(t, `exit 7`)
		code, err := c.RunForeground(ctx, spec, nil, &strings.Builder{}, &strings.Builder{})
		if err != nil || code != 7 {
			t.Fatalf("(%d, %v), want (7, nil)", code, err)
		}
	})
	t.Run("125 band is a docker failure", func(t *testing.T) {
		c, _ := stub(t, `exit 125`)
		code, err := c.RunForeground(ctx, spec, nil, &strings.Builder{}, &strings.Builder{})
		if code != 125 || err == nil || !strings.Contains(err.Error(), "exit 125") {
			t.Fatalf("(%d, %v), want the environment-class error", code, err)
		}
	})
	t.Run("exec failure", func(t *testing.T) {
		c := &CLI{Bin: filepath.Join(t.TempDir(), "absent")}
		code, err := c.RunForeground(ctx, spec, nil, &strings.Builder{}, &strings.Builder{})
		if code != -1 || err == nil {
			t.Fatalf("(%d, %v), want (-1, error)", code, err)
		}
	})
}

func TestCLIStopAndRemoveForce(t *testing.T) {
	ctx := context.Background()
	c, record := stub(t, engineAnswers("Engine")+`exit 0`)
	if err := c.Stop(ctx, "agentry-x-gateway", 10); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveForce(ctx, "agentry-x-gateway"); err != nil {
		t.Fatal(err)
	}
	got := ownArgv(recorded(t, record))
	want := []string{"stop --timeout 10 agentry-x-gateway", "rm -fv agentry-x-gateway"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv lines = %q, want %q", got, want)
	}

	notFound := `echo "Error response from daemon: No such container: agentry-x-gateway" >&2; exit 1`
	cNF, _ := stub(t, notFound)
	if err := cNF.Stop(ctx, "agentry-x-gateway", 10); err != nil {
		t.Fatalf("Stop of a gone container must be nil, got %v", err)
	}
	if err := cNF.RemoveForce(ctx, "agentry-x-gateway"); err != nil {
		t.Fatalf("RemoveForce of a gone container must be nil, got %v", err)
	}

	cErr, _ := stub(t, `echo "permission denied" >&2; exit 1`)
	if err := cErr.Stop(ctx, "x", 10); err == nil {
		t.Fatal("real Stop failure swallowed")
	}
	if err := cErr.RemoveForce(ctx, "x"); err == nil {
		t.Fatal("real RemoveForce failure swallowed")
	}
}

func TestCLIPSByLabel(t *testing.T) {
	c, record := stub(t, `printf 'one\n\n  two  \n'`)
	names, err := c.PSByLabel(context.Background(), "agentry.run=cafe0000")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"one", "two"}) {
		t.Errorf("names = %q (blank lines must be dropped, whitespace trimmed)", names)
	}
	if got := recorded(t, record)[0]; got != "ps -a --filter label=agentry.run=cafe0000 --format {{.Names}}" {
		t.Errorf("argv = %q", got)
	}

	cErr, _ := stub(t, `exit 1`)
	if _, err := cErr.PSByLabel(context.Background(), "l"); err == nil {
		t.Fatal("ps failure swallowed")
	}
}

func TestCLIPSLabels(t *testing.T) {
	c, record := stub(t, `printf 'agentry-a-gateway\tcafe0000\t4242\n\nagentry-b-agent\t\t\n'`)
	got, err := c.PSLabels(context.Background(), "agentry.owner.host=h", []string{"agentry.run", "agentry.owner.pid"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Labeled{
		{Name: "agentry-a-gateway", Labels: map[string]string{"agentry.run": "cafe0000", "agentry.owner.pid": "4242"}},
		{Name: "agentry-b-agent", Labels: map[string]string{"agentry.run": "", "agentry.owner.pid": ""}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PSLabels = %+v, want %+v", got, want)
	}
	if argv := recorded(t, record)[0]; argv != `ps -a --filter label=agentry.owner.host=h --format {{.Names}}	{{.Label "agentry.run"}}	{{.Label "agentry.owner.pid"}}` {
		t.Errorf("argv = %q", argv)
	}
	// A line that does not split into the asked fields is refused, never
	// read as some other container's labels.
	cBad, _ := stub(t, `printf 'only-a-name\n'`)
	if _, err := cBad.PSLabels(context.Background(), "l", []string{"k"}); err == nil {
		t.Error("a short line was accepted")
	}
	if _, err := c.PSLabels(context.Background(), "l", []string{`k"}}{{.ID`}); err == nil {
		t.Error("a key that would inject template text was accepted")
	}
	cErr, _ := stub(t, `exit 1`)
	if _, err := cErr.PSLabels(context.Background(), "l", nil); err == nil {
		t.Fatal("ps failure swallowed")
	}
}

func TestCLILogs(t *testing.T) {
	c, record := stub(t, `printf 'line1\nline2\n'`)
	out, err := c.Logs(context.Background(), "agentry-x-gateway", 40)
	if err != nil || out != "line1\nline2\n" {
		t.Fatalf("(%q, %v)", out, err)
	}
	if got := recorded(t, record)[0]; got != "logs --tail 40 agentry-x-gateway" {
		t.Errorf("argv = %q", got)
	}
}

// A run's own directories are relabeled so an SELinux-enabled daemon lets
// the container read them; anything else — the docker socket above all —
// is mounted untouched, because relabeling it would change a system-wide
// file's label for every container on the host.
func TestRunSpecArgsRelabel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mount Mount
		want  string
	}{
		{"a run directory", Mount{Source: "/run/cfg", Target: "/mcp", Relabel: true}, "/run/cfg:/mcp:z"},
		{"read-only too", Mount{Source: "/run/cfg", Target: "/mcp", ReadOnly: true, Relabel: true}, "/run/cfg:/mcp:ro,z"},
		{"the docker socket", Mount{Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"}, "/var/run/docker.sock:/var/run/docker.sock"},
		{"a read-only path of the machine's", Mount{Source: "/etc/thing", Target: "/thing", ReadOnly: true}, "/etc/thing:/thing:ro"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := RunSpec{Name: "n", Image: "i", Mounts: []Mount{tc.mount}}.args(true)
			var got string
			for i, a := range args {
				if a == "-v" && i+1 < len(args) {
					got = args[i+1]
				}
			}
			if got != tc.want {
				t.Errorf("-v %q, want %q", got, tc.want)
			}
		})
	}
}

// The hardening fields reach docker as their own flags,
// and a zero value adds none.
func TestRunSpecArgsHardening(t *testing.T) {
	hard := RunSpec{Name: "n", Image: "img:1", CapDrop: []string{"ALL"}, CapAdd: []string{"CHOWN"}, NoNewPrivileges: true,
		ReadOnly: true, Tmpfs: []string{"/tmp", "/work"}, PidsLimit: 512, Memory: "2g", CPUs: 2}
	got := strings.Join(hard.args(false), " ")
	for _, want := range []string{"--cap-drop ALL", "--cap-add CHOWN", "--security-opt no-new-privileges", "--read-only",
		"--tmpfs /tmp", "--tmpfs /work", "--pids-limit 512", "--memory 2g", "--cpus 2"} {
		if !strings.Contains(got, want) {
			t.Errorf("args lack %q: %s", want, got)
		}
	}
	plain := strings.Join(RunSpec{Name: "n", Image: "img:1"}.args(false), " ")
	for _, flag := range []string{"--cap-drop", "--cap-add", "no-new-privileges", "--read-only", "--tmpfs", "--pids-limit", "--memory", "--cpus"} {
		if strings.Contains(plain, flag) {
			t.Errorf("a spec that set nothing emitted %s: %s", flag, plain)
		}
	}
}

// A labelled anonymous volume is a --mount of type volume with each label
// as a volume-label field, sorted, and a field holding a comma or a quote
// is quoted as docker's CSV reader wants it.
func TestRunSpecMountsLabelledAnonymousVolumes(t *testing.T) {
	r := RunSpec{Name: "g", Image: "i", Volumes: []VolumeMount{{
		Target: "/misc",
		Labels: map[string]string{"agentry.run": "ab12cd34", "agentry.owner.start": "1,2", "q": `a"b`},
	}}}
	got := r.args(true)
	want := []string{"run", "-d", "--name", "g", "--mount", `type=volume,dst=/misc,"volume-label=agentry.owner.start=1,2",volume-label=agentry.run=ab12cd34,"volume-label=q=a""b"`, "i"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args =\n %q\nwant\n %q", got, want)
	}
	// Read back as docker reads a --mount value: one CSV record.
	fields, err := csv.NewReader(strings.NewReader(got[5])).Read()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"type=volume", "dst=/misc", "volume-label=agentry.owner.start=1,2", "volume-label=agentry.run=ab12cd34", `volume-label=q=a"b`}; !reflect.DeepEqual(fields, want) {
		t.Errorf("fields = %q, want %q", fields, want)
	}
}
