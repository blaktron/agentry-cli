package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/installed"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// noRegistry is an address nothing answers at: a local install must never
// need one.
const noRegistry = "http://127.0.0.1:1"

// localFolder copies the pure-text fixture somewhere a test may edit it.
func localFolder(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := repoPath(t, "testdata/runner/pure-text")
	for _, name := range []string{"agent.yaml", "AGENT.md"} {
		b, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A local folder is unsigned by definition: installing it needs
// --allow-unsigned, and it goes only into the runner home.
func TestInstallLocalGates(t *testing.T) {
	isolateEnv(t)
	isolateRunnerHome(t)
	dir := localFolder(t)

	_, _, err := runCLI(t, strings.NewReader(""), "--registry", noRegistry, "install", "--local", dir, "--yes")
	if err == nil || !strings.Contains(err.Error(), "--allow-unsigned") {
		t.Fatalf("installed without --allow-unsigned: %v", err)
	}
	wantClassified(t, err, engine.ExitVerify)
	if _, _, err := runCLI(t, strings.NewReader(""), "--registry", noRegistry, "install", "--local", dir, "--allow-unsigned", "--yes", "--target", "claude-skill"); err == nil {
		t.Fatal("a local folder was installed to a coding-agent target")
	}
	if _, _, err := runCLI(t, strings.NewReader(""), "--registry", noRegistry, "install", "--local", dir, "acme/other"); err == nil {
		t.Fatal("--local with a ref was accepted")
	}
}

// Installed from a folder, a QuickAgent lists as local and unsigned, runs
// by name with no registry, asks --allow-unsigned on every run, and is
// refused once its installed files change.
func TestInstallLocalThenRunByName(t *testing.T) {
	isolateEnv(t)
	home, policyPath := isolateRunnerHome(t)
	dir := localFolder(t)

	stdout, _, err := runCLI(t, strings.NewReader(""), "--registry", noRegistry, "install", "--local", dir, "--allow-unsigned", "--yes")
	if err != nil {
		t.Fatalf("install --local: %v", err)
	}
	if !strings.Contains(stdout, "installed fixtures/pure-text@1.0.0 (local, unsigned)") {
		t.Fatalf("install said %q", stdout)
	}
	listed, _, err := runCLI(t, strings.NewReader(""), "list", "--check")
	if err != nil || !strings.Contains(listed, "fixtures/pure-text@1.0.0") || !strings.Contains(listed, "(local, unsigned)") || strings.Contains(listed, "could not check") {
		t.Fatalf("list --check: %q, %v", listed, err)
	}

	run := []string{"--registry", noRegistry, "run", "fixtures/pure-text", "--dry-run", "--policy", policyPath}
	_, stderr, err := runCLI(t, strings.NewReader(""), append(run, "--allow-unsigned")...)
	if err != nil || !strings.Contains(stderr, "installed from a local folder, unsigned") {
		t.Fatalf("run by name: %v (stderr %q)", err, stderr)
	}
	_, _, err = runCLI(t, strings.NewReader(""), run...)
	if err == nil || !strings.Contains(err.Error(), "--allow-unsigned") {
		t.Fatalf("a local install ran without --allow-unsigned: %v", err)
	}
	wantClassified(t, err, engine.ExitVerify)

	st, _ := installed.Load()
	rec := st.Find(installdir.ConsentKey(home, "fixtures", "pure-text"))
	if rec == nil || !rec.Local() {
		t.Fatalf("after runs the record is %+v, want still local", rec)
	}
	releaseDir := installdir.ReleaseDir(home, "fixtures", "pure-text", "1.0.0", rec.Digest)
	if err := os.WriteFile(filepath.Join(releaseDir, "workspace", "AGENT.md"), []byte("Do something else.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = runCLI(t, strings.NewReader(""), append(run, "--allow-unsigned")...)
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("a changed local install ran: %v", err)
	}
	wantClassified(t, err, engine.ExitVerify)
}

// A --local run of an edited folder does not overwrite the agent's local
// install record: `run ns/name` still finds the install.
func TestARunOfAnotherTreeKeepsTheLocalInstall(t *testing.T) {
	isolateEnv(t)
	home, policyPath := isolateRunnerHome(t)
	dir := localFolder(t)
	if _, _, err := runCLI(t, strings.NewReader(""), "--registry", noRegistry, "install", "--local", dir, "--allow-unsigned", "--yes"); err != nil {
		t.Fatal(err)
	}
	st, _ := installed.Load()
	want := st.Find(installdir.ConsentKey(home, "fixtures", "pure-text")).Digest

	if err := os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("Answer in one line.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, strings.NewReader(""), "--registry", noRegistry, "run", "--local", dir, "--dry-run", "--allow-unsigned", "--yes", "--policy", policyPath); err != nil {
		t.Fatalf("run --local: %v", err)
	}
	st, _ = installed.Load()
	if rec := st.Find(installdir.ConsentKey(home, "fixtures", "pure-text")); rec == nil || !rec.Local() || rec.Digest != want {
		t.Fatalf("the local install's record became %+v", rec)
	}
}

// --plan reports the unsigned refusal rather than stopping on it, and
// --json answers in the registry install's shapes (stdout holds only
// the JSON document).
func TestInstallLocalPlanAndJSON(t *testing.T) {
	isolateEnv(t)
	isolateRunnerHome(t)
	dir := localFolder(t)

	stdout, _, err := runCLI(t, strings.NewReader(""), "--json", "--registry", noRegistry, "install", "--local", dir, "--plan")
	if err != nil {
		t.Fatalf("install --local --plan: %v", err)
	}
	var plan installPlan
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil {
		t.Fatalf("plan is not one JSON document: %v\n%s", err, stdout)
	}
	if plan.Ref != "fixtures/pure-text@1.0.0" || plan.Attestation != "local" || plan.Signed || !strings.Contains(plan.Refusal, "--allow-unsigned") {
		t.Fatalf("plan %+v", plan)
	}
	stdout, _, err = runCLI(t, strings.NewReader(""), "--json", "--registry", noRegistry, "install", "--local", dir, "--allow-unsigned", "--yes")
	if err != nil {
		t.Fatalf("install --local --json: %v", err)
	}
	var res installResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil || res.Attestation != "local" || !res.Recorded || res.Digest != plan.Digest {
		t.Fatalf("result %+v (%v)\n%s", res, err, stdout)
	}
}

// run --plan previews the local install a run would use, from a copy in
// its temp home: nothing is written into the installed release, which a
// run then still finds unchanged. A --pin that is not the local install's
// digest is refused, and @beta asks the registry.
func TestRunPlanPinAndChannelOnALocalInstall(t *testing.T) {
	isolateEnv(t)
	home, policyPath := isolateRunnerHome(t)
	dir := localFolder(t)
	if _, _, err := runCLI(t, strings.NewReader(""), "--registry", noRegistry, "install", "--local", dir, "--allow-unsigned", "--yes"); err != nil {
		t.Fatal(err)
	}
	st, _ := installed.Load()
	rec := st.Find(installdir.ConsentKey(home, "fixtures", "pure-text"))
	releaseDir := installdir.ReleaseDir(home, "fixtures", "pure-text", "1.0.0", rec.Digest)

	stdout, _, err := runCLI(t, strings.NewReader(""), "--registry", noRegistry, "run", "fixtures/pure-text", "--plan", "--policy", policyPath)
	if err != nil || !strings.Contains(stdout, "attestation: local") || !strings.Contains(stdout, "installed: unchanged since it was installed") || !strings.Contains(stdout, "--allow-unsigned") {
		t.Fatalf("run --plan did not preview the local install: %v\n%s", err, stdout)
	}
	if _, err := os.Stat(filepath.Join(releaseDir, "plan")); !os.IsNotExist(err) {
		t.Fatal("run --plan wrote into the installed local release")
	}
	if _, err := installdir.Inspect(releaseDir, rec.Digest); err != nil {
		t.Fatalf("the local install changed under run --plan: %v", err)
	}

	_, _, err = runCLI(t, strings.NewReader(""), "--registry", noRegistry, "run", "fixtures/pure-text", "--dry-run", "--allow-unsigned", "--pin", "sha256:0000", "--policy", policyPath)
	if err == nil || !strings.Contains(err.Error(), "--pin mismatch") {
		t.Fatalf("a --pin that is not the local install's digest: %v", err)
	}
	wantClassified(t, err, engine.ExitVerify)

	_, stderr, err := runCLI(t, strings.NewReader(""), "--registry", noRegistry, "run", "fixtures/pure-text@beta", "--dry-run", "--allow-unsigned", "--policy", policyPath)
	if err == nil || strings.Contains(stderr, "installed from a local folder") {
		t.Fatalf("@beta ran the local install: %v (stderr %q)", err, stderr)
	}
}

// A local install whose files changed is refused with the folder as the
// way back, and `install --local --repair` lays it down again from it;
// doctor names it local and unsigned.
func TestRepairAndDoctorOnALocalInstall(t *testing.T) {
	isolateEnv(t)
	home, policyPath := isolateRunnerHome(t)
	dir := localFolder(t)
	install := []string{"--registry", noRegistry, "install", "--local", dir, "--allow-unsigned", "--yes"}
	if _, _, err := runCLI(t, strings.NewReader(""), install...); err != nil {
		t.Fatal(err)
	}
	if _, _, err := policy.EnsureDefault(policyPath); err != nil {
		t.Fatal(err)
	}
	stdout, _, _ := runCLI(t, strings.NewReader(""), "--registry", noRegistry, "doctor", "fixtures/pure-text", "--policy", policyPath)
	if !strings.Contains(stdout, "installed from a local folder, unsigned") {
		t.Fatalf("doctor said %q", stdout)
	}

	st, _ := installed.Load()
	rec := st.Find(installdir.ConsentKey(home, "fixtures", "pure-text"))
	releaseDir := installdir.ReleaseDir(home, "fixtures", "pure-text", "1.0.0", rec.Digest)
	if err := os.WriteFile(filepath.Join(releaseDir, "workspace", "AGENT.md"), []byte("Do something else.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := []string{"--registry", noRegistry, "run", "fixtures/pure-text", "--dry-run", "--allow-unsigned", "--policy", policyPath}
	_, _, err := runCLI(t, strings.NewReader(""), run...)
	if err == nil || !strings.Contains(err.Error(), "install --local") || strings.Contains(err.Error(), "fresh download") {
		t.Fatalf("the refusal must name the folder as the way back: %v", err)
	}
	if _, _, err := runCLI(t, strings.NewReader(""), install...); err == nil {
		t.Fatal("an install over a changed copy of the same release was not refused")
	}
	if _, _, err := runCLI(t, strings.NewReader(""), append(install, "--repair")...); err != nil {
		t.Fatalf("install --local --repair: %v", err)
	}
	if _, _, err := runCLI(t, strings.NewReader(""), run...); err != nil {
		t.Fatalf("after repair the local install does not run: %v", err)
	}
}
