package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
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
	for _, want := range []string{"ok    policy", "ok    model", "ok    docker", "ok    image:runner", "ok    registry", "ready"} {
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

// A local BriefAgent whose declared secret is unset, on a machine missing
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
	fr.addAgent("acme", "hello", "skill", "hi").addVersion(t, "1.0.0", skillFiles("hello", ""))

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
