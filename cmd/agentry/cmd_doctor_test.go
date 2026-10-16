package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage"
)

// doctorFixture writes a configured policy under an isolated runner home
// and swaps the docker client for a Fake that has every pinned image.
func doctorFixture(t *testing.T) (policyPath string, fake *dockerx.Fake) {
	t.Helper()
	isolateEnv(t)
	_, policyPath = isolateRunnerHome(t)
	pol := policy.Default()
	pol.Model = policy.Model{Provider: "anthropic", Name: "claude-test", APIKeyEnv: "DOCTOR_TEST_KEY", ProxyAllow: []string{"api.anthropic.com:443"}}
	if err := policy.Save(policyPath, pol); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCTOR_TEST_KEY", "set")
	fake = &dockerx.Fake{ExistingImages: map[string]bool{
		pol.Images.Gateway:                               true,
		pol.Images.HarnessDockerAgent:                    true,
		pol.RunnerImageTag(runnerimage.ContentVersion()): true,
		policy.Default().Images.NodeRuntime:              true,
		policy.Default().Images.PythonRuntime:            true,
	}}
	prev := newDockerClient
	newDockerClient = func() dockerx.Client { return fake }
	t.Cleanup(func() { newDockerClient = prev })
	return policyPath, fake
}

// A configured machine with docker, every image and a reachable registry
// is "ready"; nothing is created or pulled along the way.
func TestDoctorMachineReady(t *testing.T) {
	policyPath, fake := doctorFixture(t)
	fr := newFakeRegistry(t)
	out, _, err := runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath)
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	for _, want := range []string{"ok    policy", "ok    model", "ok    docker", "ok    image:runner", "ok    registry", "ready", "ok    exec", "the commands every machine provides"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, op := range fake.Ops() {
		if strings.HasPrefix(op, "Pull") || strings.HasPrefix(op, "Build") {
			t.Fatalf("doctor must not change anything, but did %s", op)
		}
	}
}

// TestDoctorReportsTheExecCatalog: doctor lists the operator's exec
// catalog — command → package, and the mirror — so an author can see
// which declared commands this machine will honour.
func TestDoctorReportsTheExecCatalog(t *testing.T) {
	policyPath, _ := doctorFixture(t)
	pol, _, err := policy.Load(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	pol.Exec.Packages = map[string]string{"git": "git", "sort": "busybox", "openssl": "openssl=3.5.1-r0"}
	pol.Exec.APKMirror = "https://mirror.example/alpine"
	if err := policy.Save(policyPath, pol); err != nil {
		t.Fatal(err)
	}
	fr := newFakeRegistry(t)
	out, _, err := runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath)
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	for _, want := range []string{"ok    exec", "git → git", "openssl → openssl=3.5.1-r0", "sort → busybox", "mirror https://mirror.example/alpine"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// A local QuickAgent whose declared secret is unset, on a machine missing
// the runner image, fails both checks — naming the variable, never a value
// — and the JSON form carries the same report.
func TestDoctorLocalAgentReportsMissingImageAndSecret(t *testing.T) {
	policyPath, fake := doctorFixture(t)
	delete(fake.ExistingImages, policy.Default().RunnerImageTag(runnerimage.ContentVersion()))
	fr := newFakeRegistry(t)
	dir := writeTree(t, skillFiles("needs-secret", "capabilities:\n  secrets: [GITHUB_PAT]\n"))
	t.Setenv("GITHUB_PAT", "")

	out, _, err := runCLI(t, nil, "doctor", "--local", dir, "--registry", fr.url(), "--policy", policyPath)
	if err == nil || !strings.Contains(err.Error(), "2 check(s) failed") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if !strings.Contains(out, "FAIL  image:runner") || !strings.Contains(out, "FAIL  secrets") || !strings.Contains(out, "GITHUB_PAT") || !strings.Contains(out, "exit 11") {
		t.Fatalf("report:\n%s", out)
	}
	if !strings.Contains(out, "warn  profile") {
		t.Fatalf("expected a profile warning:\n%s", out)
	}

	t.Setenv("GITHUB_PAT", "value-that-must-not-print")
	out, _, err = runCLI(t, nil, "doctor", "--local", dir, "--registry", fr.url(), "--policy", policyPath, "--json")
	if err == nil || !strings.Contains(err.Error(), "1 check(s) failed") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	var rep doctorReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if strings.Contains(out, "value-that-must-not-print") {
		t.Fatal("secret value leaked into the report")
	}
	byName := map[string]doctorCheck{}
	for _, c := range rep.Checks {
		byName[c.Name] = c
	}
	if byName["secrets"].Status != "ok" || byName["image:runner"].Status != "fail" || byName["manifest"].Status != "ok" {
		t.Fatalf("checks = %+v", rep.Checks)
	}
}

// A registry ref resolves (registry reachable) but, before its first
// `run --prepare`, the install is only a warning — not a failure — so a
// fresh machine still gets a readable verdict.
func TestDoctorRegistryRefNotInstalledYet(t *testing.T) {
	policyPath, _ := doctorFixture(t)
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "hello", "autonomous", "hi").addVersion(t, "1.0.0", skillFiles("hello", ""))

	out, _, err := runCLI(t, nil, "doctor", "acme/hello", "--registry", fr.url(), "--policy", policyPath)
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "ok    resolve") || !strings.Contains(out, "acme/hello@1.0.0") || !strings.Contains(out, "warn  install") {
		t.Fatalf("report:\n%s", out)
	}
	out, _, err = runCLI(t, nil, "doctor", "acme/missing", "--registry", fr.url(), "--policy", policyPath)
	if err == nil || !strings.Contains(out, "FAIL  resolve") {
		t.Fatalf("unknown ref should fail resolve: %v\n%s", err, out)
	}
}

// No policy file: doctor points at setup and does not create one.
func TestDoctorWithoutPolicy(t *testing.T) {
	policyPath, _ := doctorFixture(t)
	fr := newFakeRegistry(t)
	missing := filepath.Join(filepath.Dir(policyPath), "absent.yaml")
	out, _, err := runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", missing)
	if err == nil || !strings.Contains(out, "FAIL  policy") || !strings.Contains(out, "agentry setup") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if _, _, err := policy.Load(missing); err == nil {
		t.Fatal("doctor created a policy file")
	}
}

// TestDoctorFlagsUncataloguedExecCommand: a local QuickAgent declaring a
// command the machine's exec catalog does not carry fails the exec check
// naming the command — what a run would refuse at plan time, said before
// the run; with the catalog entry it passes.
func TestDoctorFlagsUncataloguedExecCommand(t *testing.T) {
	policyPath, _ := doctorFixture(t)
	fr := newFakeRegistry(t)
	dir := writeTree(t, skillFiles("uses-git", "capabilities:\n  exec: { commands: [git] }\n"))
	out, _, _ := runCLI(t, nil, "doctor", "--local", dir, "--registry", fr.url(), "--policy", policyPath)
	if !strings.Contains(out, "FAIL  exec") || !strings.Contains(out, "git") || !strings.Contains(out, "exec.packages") {
		t.Fatalf("uncatalogued git not flagged:\n%s", out)
	}
	pol, _, err := policy.Load(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	pol.Exec.Packages = map[string]string{"git": "git"}
	if err := policy.Save(policyPath, pol); err != nil {
		t.Fatal(err)
	}
	out, _, _ = runCLI(t, nil, "doctor", "--local", dir, "--registry", fr.url(), "--policy", policyPath)
	if !strings.Contains(out, "ok    exec") || !strings.Contains(out, "git → git") {
		t.Fatalf("catalogued git not reported ok:\n%s", out)
	}
}

// doctor reports the policy's docker.runtime against the daemon's
// registered runtimes: a named failure with the fix when it is
// missing, ok when it is there, and nothing when the policy sets none.
func TestDoctorReportsTheDockerRuntime(t *testing.T) {
	policyPath, fake := doctorFixture(t)
	fr := newFakeRegistry(t)
	out, _, _ := runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath)
	if strings.Contains(out, "docker:runtime") {
		t.Fatalf("doctor mentions a runtime the policy never set:\n%s", out)
	}
	pol, _, err := policy.Load(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	pol.Docker.Runtime = "runsc"
	if err := policy.Save(policyPath, pol); err != nil {
		t.Fatal(err)
	}
	fake.RuntimeNames = []string{"io.containerd.runc.v2", "runc"}
	out, _, _ = runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath)
	if !strings.Contains(out, "FAIL  docker:runtime") || !strings.Contains(out, `"runsc"`) || !strings.Contains(out, "runc") {
		t.Fatalf("a missing runtime was not reported with the fix:\n%s", out)
	}
	fake.RuntimeNames = []string{"runc", "runsc"}
	out, _, err = runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath)
	if err != nil || !strings.Contains(out, "ok    docker:runtime") {
		t.Fatalf("a registered runtime was not reported ok (%v):\n%s", err, out)
	}

	// The gateway's runtime is reported by what it runs, not its
	// name: the same refusal ensure makes, with the fix.
	pol.Docker.GatewayRuntime = "runsc-uds"
	if err := policy.Save(policyPath, pol); err != nil {
		t.Fatal(err)
	}
	runsc := dockerx.Runtime{Path: "/usr/bin/runsc"}
	fake.RuntimeTable = map[string]dockerx.Runtime{"runc": {Path: "runc"}, "runsc": runsc,
		"runsc-uds": {Path: "/usr/bin/runsc", Args: []string{dockerx.HostUDSOpen}}}
	out, _, _ = runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath)
	if !strings.Contains(out, "ok    docker:gatewayRuntime") || !strings.Contains(out, "registered on the daemon as runsc with --host-uds=open") {
		t.Fatalf("a right gateway registration was not reported ok:\n%s", out)
	}
	fake.RuntimeTable["runsc-uds"] = dockerx.Runtime{Path: "runc", Args: []string{dockerx.HostUDSOpen}}
	out, _, _ = runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath)
	if !strings.Contains(out, "FAIL  docker:gatewayRuntime") || !strings.Contains(out, "runs runc, not /usr/bin/runsc") {
		t.Fatalf("a runsc-uds that runs runc was not reported:\n%s", out)
	}
	fake.RuntimeTable = nil
}

// TestDoctorNamesTheEngine: the docker check is the engine in the receipt's
// words, and each engine a run would be refused on is refused by name: no
// engine at all, podman with no API socket answering, an endpoint the run's
// docker filter cannot be put in front of, and a policy runtime on podman.
func TestDoctorNamesTheEngine(t *testing.T) {
	policyPath, fake := doctorFixture(t)
	fr := newFakeRegistry(t)
	fake.EngineInfo = &dockerx.Engine{Kind: dockerx.KindPodman, CLI: "podman", Version: "5.8.4", Rootless: true,
		Endpoint: "unix:///run/user/1000/podman/podman.sock", DaemonHost: hostnameOf(t), DaemonKernel: dockerx.HostKernel()}
	out, _, _ := runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath)
	if !strings.Contains(out, "ok    docker") || !strings.Contains(out, "podman 5.8.4, rootless, /run/user/1000/podman/podman.sock, docker filter ") {
		t.Errorf("the engine line:\n%s", out)
	}

	fake.EngineInfo = nil
	fake.EngineErr = errors.New("podman's API socket unix:///run/user/1000/podman/podman.sock does not answer: start it with `systemctl --user enable --now podman.socket`")
	out, _, _ = runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath)
	if !strings.Contains(out, "FAIL  docker") || !strings.Contains(out, "systemctl --user enable --now podman.socket") {
		t.Errorf("podman with no socket:\n%s", out)
	}

	fake.EngineErr = nil
	fake.EngineInfo = &dockerx.Engine{Kind: dockerx.KindDocker, CLI: "docker", Version: "29.7.2", Endpoint: "tcp://10.0.0.5:2376"}
	out, _, _ = runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath)
	if !strings.Contains(out, "FAIL  docker") || !strings.Contains(out, "tcp://10.0.0.5:2376 is not a Unix socket on this machine") {
		t.Errorf("a tcp:// context:\n%s", out)
	}

	fake.EngineInfo = &dockerx.Engine{Kind: dockerx.KindPodman, CLI: "podman", Version: "5.8.4", Endpoint: "unix:///run/podman/podman.sock"}
	pol, _, err := policy.Load(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	pol.Docker.Runtime = "runsc"
	if err := policy.Save(policyPath, pol); err != nil {
		t.Fatal(err)
	}
	out, _, _ = runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath)
	if !strings.Contains(out, "FAIL  docker:runtime") || !strings.Contains(out, `docker.runtime "runsc" is not held on podman`) || !strings.Contains(out, "ok    image:runner") {
		t.Errorf("a runtime on podman:\n%s", out)
	}
}

// With neither engine on the machine, the real client says so.
func TestDoctorWithNoEngine(t *testing.T) {
	policyPath, _ := doctorFixture(t)
	newDockerClient = func() dockerx.Client { return dockerx.New() }
	t.Setenv("PATH", t.TempDir())
	for _, k := range []string{dockerx.EnvEngine, dockerx.EnvDocker, dockerx.EnvPodman, "DOCKER_HOST", "DOCKER_CONTEXT"} {
		t.Setenv(k, "")
	}
	fr := newFakeRegistry(t)
	out, _, _ := runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath)
	if !strings.Contains(out, "FAIL  docker") || !strings.Contains(out, "no podman answers") || !strings.Contains(out, "install Docker Engine or podman") {
		t.Errorf("no engine:\n%s", out)
	}
}

func hostnameOf(t *testing.T) string {
	t.Helper()
	h, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// TestDoctorAdapterHarness: --adapter renders the harness probe's facts
// as checks — the resolved executable and version as ok, the endpoint
// against model.proxyAllow, and a harness that is not installed as the
// failure that names the fix. The probe lives in pkg/runner/harnessprobe
// so the desktop reads the same facts.
func TestDoctorAdapterHarness(t *testing.T) {
	policyPath, _ := doctorFixture(t)
	fr := newFakeRegistry(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\necho '2.1.263 (Claude Code)'\n"), 0o755); err != nil { //nolint:gosec // a test fixture
		t.Fatal(err)
	}
	pol, _, err := policy.Load(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	pol.Harness = map[string]policy.HarnessPolicy{"claude": {BaseURL: "https://api.anthropic.com/v1"}}
	if err := policy.Save(policyPath, pol); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", bin)
	out, _, err := runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath, "--adapter", "claude")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	for _, want := range []string{"ok    harness:claude", filepath.Join(bin, "claude") + " (2.1.263 (Claude Code))", "ok    harness:claude:endpoint", "api.anthropic.com:443 is in model.proxyAllow"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	t.Setenv("PATH", t.TempDir()) // nothing on it
	out, _, err = runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath, "--adapter", "claude")
	if err == nil {
		t.Fatalf("doctor passed with no claude installed:\n%s", out)
	}
	for _, want := range []string{"FAIL  harness:claude", "claude is not installed on this machine (not on PATH, and not in its usual install locations)", "set harness.claude.path in the policy"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	// Off PATH but in the native installer's directory (isolateEnv gave
	// this test its own HOME): found, and the line says where.
	known := filepath.Join(os.Getenv("HOME"), ".local", "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(known), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(known, []byte("#!/bin/sh\necho '2.1.263 (Claude Code)'\n"), 0o755); err != nil { //nolint:gosec // a test fixture
		t.Fatal(err)
	}
	out, _, err = runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath, "--adapter", "claude")
	if err != nil {
		t.Fatalf("doctor failed with claude in a usual location: %v\n%s", err, out)
	}
	if want := "ok    harness:claude     " + known + " (2.1.263 (Claude Code)) — a usual install location, not on this process's PATH"; !strings.Contains(out, want) {
		t.Errorf("missing %q in:\n%s", want, out)
	}

	// The containerised default names its image as the check, not a path.
	out, _, err = runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath, "--adapter", "docker-agent")
	if err != nil || !strings.Contains(out, "ok    adapter:docker-agent") {
		t.Fatalf("docker-agent: err=%v\n%s", err, out)
	}

	// An unknown adapter is the adapter check failing with the registry's list.
	out, _, err = runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath, "--adapter", "no-such")
	if err == nil || !strings.Contains(out, "FAIL  adapter") || !strings.Contains(out, "unknown adapter \"no-such\"") {
		t.Fatalf("unknown adapter: err=%v\n%s", err, out)
	}

	// codex, installed at another release, warns and does not fail; a
	// baseUrl the allowlist does not cover fails the endpoint check.
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\necho 'codex-cli 9.9.9'\n"), 0o755); err != nil { //nolint:gosec // a test fixture
		t.Fatal(err)
	}
	pol.Harness = map[string]policy.HarnessPolicy{"codex": {BaseURL: "https://api.openai.example:8443/v1"}}
	if err := policy.Save(policyPath, pol); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	out, _, err = runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath, "--adapter", "codex")
	if err == nil {
		t.Fatalf("an endpoint the proxy would deny must fail doctor:\n%s", out)
	}
	for _, want := range []string{"warn  harness:codex", "(codex-cli 9.9.9); this adapter was written against 0.153.4, so flags or event shapes may differ", "FAIL  harness:codex:endpoint", "points at api.openai.example:8443, which model.proxyAllow does not list"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	// A baseUrl that is not a URL fails the endpoint check by name.
	pol.Harness = map[string]policy.HarnessPolicy{"codex": {BaseURL: "not a url"}}
	if err := policy.Save(policyPath, pol); err != nil {
		t.Fatal(err)
	}
	out, _, _ = runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath, "--adapter", "codex")
	if !strings.Contains(out, "FAIL  harness:codex:endpoint") || !strings.Contains(out, "harness.codex.baseUrl \"not a url\" is not a URL") {
		t.Fatalf("not-a-url:\n%s", out)
	}
}

// TestDoctorModelKeyFromTheKeychain: with the variable unset and
// apiKeySource keyring, doctor asks the keychain — present is ok,
// absent is the failure that names the fix, unreachable is named.
func TestDoctorModelKeyFromTheKeychain(t *testing.T) {
	policyPath, _ := doctorFixture(t)
	fr := newFakeRegistry(t)
	pol, _, err := policy.Load(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	pol.Model.APIKeySource = "keyring"
	if err := policy.Save(policyPath, pol); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCTOR_TEST_KEY", "")
	old := modelKeyPresent
	t.Cleanup(func() { modelKeyPresent = old })

	modelKeyPresent = func(string) (bool, error) { return true, nil }
	out, _, err := runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath)
	if err != nil || !strings.Contains(out, "ok    model") || !strings.Contains(out, "OS keychain entry provider:anthropic") {
		t.Fatalf("present: err=%v\n%s", err, out)
	}
	modelKeyPresent = func(string) (bool, error) { return false, nil }
	out, _, err = runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath)
	if err == nil || !strings.Contains(out, "FAIL  model") || !strings.Contains(out, "has no entry provider:anthropic") || !strings.Contains(out, "agentry setup") {
		t.Fatalf("absent: err=%v\n%s", err, out)
	}
	modelKeyPresent = func(string) (bool, error) { return false, errStubDoctor("dbus: connection refused") }
	out, _, _ = runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath)
	if !strings.Contains(out, "FAIL  model") || !strings.Contains(out, "could not be asked") || !strings.Contains(out, "connection refused") {
		t.Fatalf("unreachable:\n%s", out)
	}
}

type errStubDoctor string

func (e errStubDoctor) Error() string { return string(e) }

// A policy whose key is a subscription token: doctor says which
// harness can use it before a run finds out.
func TestDoctorSaysASubscriptionTokenIsClaudeCodeOnly(t *testing.T) {
	policyPath, _ := doctorFixture(t)
	fr := newFakeRegistry(t)
	pol, _, err := policy.Load(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	pol.Model.APIKeyKind = policy.KeyKindSubscription
	if err := policy.Save(policyPath, pol); err != nil {
		t.Fatal(err)
	}
	out, _, _ := runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath)
	for _, want := range []string{"warn  model:kind", "subscription token", "--adapter claude", "container harness refuses it"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// A custom endpoint over plain http off this machine is a warning, never a
// failure.
func TestDoctorWarnsOnAPlainHTTPModelEndpoint(t *testing.T) {
	policyPath, _ := doctorFixture(t)
	fr := newFakeRegistry(t)
	pol, _, err := policy.Load(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	pol.Model.BaseURL = "http://gpu-box.lan:8000/v1"
	if err := policy.Save(policyPath, pol); err != nil {
		t.Fatal(err)
	}
	out, _, _ := runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath)
	for _, want := range []string{"warn  model:endpoint", "plain http", "Use https"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "FAIL  model:endpoint") {
		t.Errorf("plain http failed doctor:\n%s", out)
	}
}

// doctor reports each declared server at the gate's rating: a High the
// gate rates medium is a warning a run proceeds past, one it rates high
// fails, and a failed lookup whose last-known advisories are below the
// bar is never reported as "no known advisories".
func TestDoctorRatesDeclaredServersAsTheGate(t *testing.T) {
	policyPath, _ := doctorFixture(t)
	fr := newFakeRegistry(t)
	fr.scans = map[string]string{
		"@acme/medium": `{"status":"vulnerable","maxSeverity":"high","advisories":[{"id":"GHSA-71","severity":"high","gateSeverity":"medium","cvssScore":7.1,"summary":"s"}]}`,
		"@acme/high":   `{"status":"vulnerable","maxSeverity":"high","advisories":[{"id":"GHSA-87","severity":"high","gateSeverity":"high","cvssScore":8.7,"summary":"s","fixedIn":["2.0.0"]}]}`,
		"@acme/failed": `{"status":"error","maxSeverity":"high","lastError":"timeout","advisories":[{"id":"GHSA-72","severity":"high","gateSeverity":"medium","cvssScore":7.2,"summary":"s"}]}`,
	}
	dir := writeTree(t, skillFiles("servers", "capabilities:\n  mcpServers:\n"+
		"    - npm: \"@acme/medium\"\n      version: 1.0.0\n"+
		"    - npm: \"@acme/high\"\n      version: 1.0.0\n"+
		"    - npm: \"@acme/failed\"\n      version: 1.0.0\n"))
	out, _, _ := runCLI(t, nil, "doctor", "--local", dir, "--registry", fr.url(), "--policy", policyPath)
	for _, want := range []string{
		"npm:@acme/medium@1.0.0: an advisory the gate rates medium (GHSA-71 [advisory high, CVSS 7.1; the gate rates it medium]), below the refusal bar — a run proceeds",
		"npm:@acme/high@1.0.0: an advisory the gate rates high (GHSA-87 [advisory high, CVSS 8.7]) — a run refuses it; pin a fixed version (fixed in 2.0.0)",
		"npm:@acme/failed@1.0.0: the registry's last lookup failed; it last knew an advisory the gate rates medium",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("report lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "@acme/failed@1.0.0: no known advisories") {
		t.Fatalf("a failed lookup reported clean:\n%s", out)
	}
}

// A model sign-in is checked in the model sign-in store, never as a key
// variable: fail until one is kept, ok once it is, naming the vendor and
// never the account.
func TestDoctorChecksAModelSignInInItsStore(t *testing.T) {
	policyPath, _ := doctorFixture(t)
	pol := policy.Default()
	pol.Model = policy.Model{Provider: policy.ProviderOpenAI, Name: "gpt-test", APIKeyKind: policy.KeyKindChatGPTPlan, ProxyAllow: []string{"api.openai.com:443"}}
	if err := policy.Save(policyPath, pol); err != nil {
		t.Fatal(err)
	}
	fr := newFakeRegistry(t)
	out, _, _ := runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath)
	if !strings.Contains(out, "FAIL  model") || !strings.Contains(out, "none is kept on this computer") {
		t.Fatalf("not signed in:\n%s", out)
	}
	home, _ := runnerHome()
	if _, err := modelsignin.Open(home, nil).Put(modelsignin.ChatGPT, modelsignin.Record{Label: "person@example.com"}, modelsignin.Grant{RefreshToken: "rt"}); err != nil {
		t.Fatal(err)
	}
	out, _, _ = runCLI(t, nil, "doctor", "--registry", fr.url(), "--policy", policyPath)
	if !strings.Contains(out, "ok    model") || !strings.Contains(out, "model sign-in chatgpt") || strings.Contains(out, "person@example.com") {
		t.Fatalf("signed in:\n%s", out)
	}
}
