package dockerx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A container that must run under the policy's runtime is created, its
// runtime read back from the engine, and only then started, detached or
// attached; one without a runtime is run with `run`.
func TestRunUnderCreatesChecksThenStarts(t *testing.T) {
	ctx := context.Background()
	answers := `case "$1" in inspect) echo runsc;; esac; exit 0`
	c, record := stub(t, engineAnswers("Engine")+answers)
	r := RunSpec{Name: "agentry-R-agent", Image: "img", Runtime: "runsc", Interactive: true, AutoRemove: true}
	code, err := RunForegroundUnder(ctx, c, r, "agent container", strings.NewReader(""), &strings.Builder{}, &strings.Builder{})
	if err != nil || code != 0 {
		t.Fatalf("(%d, %v)", code, err)
	}
	if err := RunDetachedUnder(ctx, c, RunSpec{Name: "agentry-R-proxy", Image: "img", Runtime: "runsc"}, "llm proxy"); err != nil {
		t.Fatal(err)
	}
	if err := RunDetachedUnder(ctx, c, RunSpec{Name: "agentry-R-plain", Image: "img"}, "plain"); err != nil {
		t.Fatal(err)
	}
	got := ownArgv(recorded(t, record))
	want := []string{
		"create --rm -i --name agentry-R-agent --runtime runsc img",
		"inspect --format {{.HostConfig.Runtime}} agentry-R-agent",
		"start -a -i agentry-R-agent",
		"create --name agentry-R-proxy --runtime runsc img",
		"inspect --format {{.HostConfig.Runtime}} agentry-R-proxy",
		"start agentry-R-proxy",
		"run -d --name agentry-R-plain img",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the client saw\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// podman's own CLI names the runtime it created the container with.
	pc, precord := podmanStub(t, `case "$1" in inspect) echo runsc;; esac; exit 0`)
	pc.engine = &Engine{Kind: KindPodman, CLI: "podman"} // described already, as a run has by then
	if err := RunDetachedUnder(ctx, pc, RunSpec{Name: "agentry-R-proxy", Image: "img", Runtime: "runsc"}, "llm proxy"); err != nil {
		t.Fatal(err)
	}
	if got := recorded(t, precord)[1]; got != "inspect --format {{.OCIRuntime}} agentry-R-proxy" {
		t.Errorf("podman inspected with %q", got)
	}
}

// An engine that takes the runtime and does not apply it stops the run:
// the container is removed, never started, and the error names the class.
func TestRunUnderRefusesADroppedRuntime(t *testing.T) {
	ctx := context.Background()
	f := &Fake{DropRuntime: true, DroppedRuntime: "crun"}
	err := RunDetachedUnder(ctx, f, RunSpec{Name: "agentry-R-gateway", Image: "gw", Runtime: "runsc-uds"}, "gateway")
	var mm *RuntimeMismatch
	if !errors.As(err, &mm) || mm.Class != "gateway" || mm.Got != "crun" || mm.Want != "runsc-uds" {
		t.Fatalf("got %v", err)
	}
	for _, want := range []string{"the gateway (agentry-R-gateway)", `created under "crun"`, `docker.runtime "runsc-uds"`, "the run stops"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	ops := f.Ops()
	if !slices.Equal(ops, []string{"Create:agentry-R-gateway", "ContainerRuntime:agentry-R-gateway", "RemoveForce:agentry-R-gateway"}) {
		t.Errorf("ops %v", ops)
	}
	f = &Fake{DropRuntime: true}
	code, err := RunForegroundUnder(ctx, f, RunSpec{Name: "agentry-R-agent", Image: "a", Runtime: "runsc"}, "agent container", nil, &strings.Builder{}, &strings.Builder{})
	if code != -1 || !errors.As(err, &mm) || !strings.Contains(err.Error(), "under the engine's default") {
		t.Errorf("(%d, %v)", code, err)
	}
	if slices.Contains(f.Ops(), "StartAttached:agentry-R-agent") {
		t.Error("a mismatched container was started")
	}
	// A client that cannot create and inspect is refused a runtime rather
	// than run it unchecked.
	var plain Client = struct{ Client }{Client: &Fake{}}
	if err := RunDetachedUnder(ctx, plain, RunSpec{Name: "agentry-R-proxy", Image: "p", Runtime: "runsc"}, "llm proxy"); err == nil || !strings.Contains(err.Error(), "cannot read a container's runtime back") {
		t.Errorf("a client without Creator: %v", err)
	}
	if err := RunDetachedUnder(ctx, plain, RunSpec{Name: "agentry-R-plain", Image: "p"}, "plain"); err != nil {
		t.Errorf("a client without Creator and no runtime: %v", err)
	}
	// A faithful engine lets both halves run, and the spec is findable.
	f = &Fake{}
	if err := RunDetachedUnder(ctx, f, RunSpec{Name: "agentry-R-gateway", Image: "gw", Runtime: "runsc-uds"}, "gateway"); err != nil {
		t.Fatal(err)
	}
	if r, ok := f.FindRun("-gateway"); !ok || r.Runtime != "runsc-uds" {
		t.Errorf("FindRun after a checked start: %+v %v", r, ok)
	}
}

// On podman on an SELinux host a sandboxed container is created with its
// label disabled, since gVisor refuses a labelled one; Docker Engine and a
// host without SELinux leave the label as it is.
func TestRunUnderDisablesTheLabelOnPodmanWithSELinux(t *testing.T) {
	prev := selinuxActive
	t.Cleanup(func() { selinuxActive = prev })
	for _, tc := range []struct {
		name    string
		kind    string
		selinux bool
		runtime string
		want    bool
	}{
		{"podman on SELinux under a runtime", KindPodman, true, "runsc", true},
		{"podman on SELinux without a runtime", KindPodman, true, "", false},
		{"podman without SELinux", KindPodman, false, "runsc", false},
		{"docker on SELinux", KindDocker, true, "runsc", false},
	} {
		selinuxActive = func() bool { return tc.selinux }
		e := Engine{Kind: tc.kind}
		if got := e.SandboxUnlabelled(tc.runtime); got != tc.want {
			t.Errorf("%s: SandboxUnlabelled = %v", tc.name, got)
		}
		if tc.runtime == "" {
			continue
		}
		f := &Fake{EngineInfo: &e}
		if err := RunDetachedUnder(context.Background(), f, RunSpec{Name: "agentry-R-proxy", Image: "p", Runtime: tc.runtime}, "llm proxy"); err != nil {
			t.Fatal(err)
		}
		if r, _ := f.FindRun("-proxy"); r.DisableSELinuxLabel != tc.want {
			t.Errorf("%s: created with label disabled = %v, want %v", tc.name, r.DisableSELinuxLabel, tc.want)
		}
	}
}

// On podman a runtime is checked by name, since podman's table lists every
// name its configuration holds; on docker the daemon's table answers.
func TestCheckRuntime(t *testing.T) {
	ctx := context.Background()
	c, record := podmanStub(t, `case "$2" in runsc) echo "runsc /opt/runsc";; *) echo 'Error: default OCI runtime "nosuch" not found' >&2; exit 125;; esac`)
	rt, err := c.CheckRuntime(ctx, "runsc")
	if err != nil || rt.Path != "/opt/runsc" {
		t.Fatalf("runsc: %+v, %v", rt, err)
	}
	if _, err := c.CheckRuntime(ctx, "nosuch"); err == nil || !strings.Contains(err.Error(), `runtime "nosuch" is not registered with podman`) || !strings.Contains(err.Error(), "[engine.runtimes]") {
		t.Errorf("nosuch: %v", err)
	}
	if got := recorded(t, record)[0]; got != "--runtime runsc info --format {{.Host.OCIRuntime.Name}} {{.Host.OCIRuntime.Path}}" {
		t.Errorf("podman asked %q", got)
	}
	d, _ := stub(t, `echo '{"runc":{"path":"runc"},"runsc":{"path":"/usr/bin/runsc"}}'`)
	if rt, err := d.CheckRuntime(ctx, "runsc"); err != nil || rt.Path != "/usr/bin/runsc" {
		t.Errorf("docker runsc: %+v, %v", rt, err)
	}
	if _, err := d.CheckRuntime(ctx, "youki"); err == nil || !strings.Contains(err.Error(), "runc, runsc") {
		t.Errorf("docker youki: %v", err)
	}
	f := &Fake{RuntimeTable: map[string]Runtime{"runsc": {Path: "/r"}}}
	if _, err := f.CheckRuntime(ctx, "runsc"); err != nil {
		t.Error(err)
	}
	if _, err := f.CheckRuntime(ctx, "crun"); err == nil {
		t.Error("the fake registered crun")
	}
}

// podman registers a runtime as a path and no arguments, so the gateway's
// runtime is a wrapper script: it is read for the flag and the runtime.
func TestCheckHostUDSWrapper(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil { //nolint:gosec // a test script
			t.Fatal(err)
		}
		return p
	}
	good := write("runsc-uds", "#!/bin/sh\nexec /opt/runsc --ignore-cgroups --host-uds=open \"$@\"\n")
	if err := CheckHostUDSWrapper(good, "/opt/runsc", "runsc-uds"); err != nil {
		t.Errorf("a right wrapper: %v", err)
	}
	for name, body := range map[string]string{
		"no flag":        "#!/bin/sh\nexec /opt/runsc --ignore-cgroups \"$@\"\n",
		"another binary": "#!/bin/sh\nexec /usr/bin/crun --host-uds=open \"$@\"\n",
		"a binary":       "\x7fELF--host-uds=open /opt/runsc",
	} {
		p := write(strings.ReplaceAll(name, " ", "-"), body)
		if err := CheckHostUDSWrapper(p, "/opt/runsc", "runsc-uds"); err == nil || !strings.Contains(err.Error(), `"runsc-uds" is registered at `+p) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := CheckHostUDSWrapper(filepath.Join(dir, "absent"), "/opt/runsc", "runsc-uds"); err == nil || !strings.Contains(err.Error(), "cannot be read") {
		t.Errorf("absent: %v", err)
	}
	// docker.runtime is itself a wrapper (rootless podman needs a flag of
	// its own): the gateway's wrapper names the binary that wrapper runs.
	bin := write("runsc", "\x7fELF the runtime itself")
	rt := write("runsc-rootless", "#!/bin/sh\nexec "+bin+" --ignore-cgroups \"$@\"\n")
	gw := write("runsc-rootless-uds", "#!/bin/sh\nexec "+bin+" --ignore-cgroups --host-uds=open \"$@\"\n")
	if got := RuntimeBinary(rt); got != bin {
		t.Errorf("RuntimeBinary(%s) = %q, want %q", rt, got, bin)
	}
	if got := RuntimeBinary(bin); got != bin {
		t.Errorf("RuntimeBinary of a binary = %q", got)
	}
	if err := CheckHostUDSWrapper(gw, rt, "runsc-rootless-uds"); err != nil {
		t.Errorf("a gateway wrapper naming the runtime's binary: %v", err)
	}
	other := write("other-uds", "#!/bin/sh\nexec /usr/bin/crun --host-uds=open \"$@\"\n")
	if err := CheckHostUDSWrapper(other, rt, "runsc-rootless-uds"); err == nil || !strings.Contains(err.Error(), "or the binary it runs") {
		t.Errorf("a gateway wrapper running another binary: %v", err)
	}
}

// The docker CLI pointed at podman's socket builds with the classic
// builder, and its labelled anonymous volumes become named ones as podman's
// own CLI makes them; Docker Engine keeps BuildKit and the anonymous form.
func TestTheDockerCLIOnPodmanFollowsTheEngine(t *testing.T) {
	ctx := context.Background()
	dump := filepath.Join(t.TempDir(), "env.txt")
	t.Setenv("AGENTRY_TEST_ENVDUMP", dump)
	script := `case "$1" in build) printf '%s' "${DOCKER_BUILDKIT:-unset}" > "$AGENTRY_TEST_ENVDUMP";; esac; case "$1 $2" in "volume ls") echo agentry-R-gateway-vol0;; esac; exit 0`
	c, record := stub(t, engineAnswers("Podman Engine")+script)
	if err := c.Build(ctx, "/ctx", "t:1", nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dump); string(b) != "0" { //nolint:gosec // test-owned path
		t.Errorf("DOCKER_BUILDKIT in the build's environment = %q, want 0", b)
	}
	r := RunSpec{Name: "agentry-R-gateway", Image: "gw", Volumes: []VolumeMount{{Target: "/misc", Labels: map[string]string{"agentry.run": "R"}}}}
	if err := c.RunDetached(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveForce(ctx, "agentry-R-gateway"); err != nil {
		t.Fatal(err)
	}
	got := ownArgv(recorded(t, record))
	want := []string{
		"build -t t:1 /ctx",
		"volume create --label agentry.run=R --label agentry.volume-of=agentry-R-gateway agentry-R-gateway-vol0",
		"run -d --name agentry-R-gateway --mount type=volume,src=agentry-R-gateway-vol0,dst=/misc gw",
		"rm -fv agentry-R-gateway",
		"volume ls --filter label=agentry.volume-of=agentry-R-gateway --format {{.Name}}",
		"volume rm -f agentry-R-gateway-vol0",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the docker CLI on podman saw\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	d, drecord := stub(t, engineAnswers("Engine")+script)
	if err := d.Build(ctx, "/ctx", "t:1", nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dump); string(b) != "unset" { //nolint:gosec // test-owned path
		t.Errorf("Docker Engine's build got DOCKER_BUILDKIT=%q", b)
	}
	if err := d.RunDetached(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := d.RemoveForce(ctx, "agentry-R-gateway"); err != nil {
		t.Fatal(err)
	}
	got = ownArgv(recorded(t, drecord))
	want = []string{
		"build -t t:1 /ctx",
		"run -d --name agentry-R-gateway --mount type=volume,dst=/misc,volume-label=agentry.run=R gw",
		"rm -fv agentry-R-gateway",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Docker Engine saw\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// The per-run service is podman's own API service with the runtime as its
// default, on the socket the run names; the docker CLI has none.
func TestPodmanServiceCommand(t *testing.T) {
	c, _ := podmanStub(t, `exit 0`)
	cmd, err := c.PodmanServiceCommand(context.Background(), "runsc", "/run/x/podman.sock")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cmd.Args[1:], " "); got != "--runtime runsc system service --time=0 unix:///run/x/podman.sock" {
		t.Errorf("argv %q", got)
	}
	d, _ := stub(t, `exit 0`)
	if _, err := d.PodmanServiceCommand(context.Background(), "runsc", "/run/x/podman.sock"); err == nil {
		t.Error("the docker CLI offered a podman service")
	}
}
