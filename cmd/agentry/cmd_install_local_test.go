package main

import (
	"crypto/sha256"
	"encoding/hex"
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

	// Only the runner home takes a local folder, and the consent is
	// --allow-unsigned, not --allow-unscanned.
	for _, tc := range []struct {
		flag, value, want string
	}{
		{"--provider", "claude", "--local installs into the runner home only"},
		{"--dir", t.TempDir(), "--local installs into the runner home only"},
		{"--target", "agents-md", "--local installs into the runner home only"},
		{"--allow-unscanned", "", "--allow-unscanned is for registry releases"},
	} {
		args := []string{"--registry", noRegistry, "install", "--local", dir, "--allow-unsigned", "--yes", tc.flag}
		if tc.value != "" {
			args = append(args, tc.value)
		}
		if _, _, err := runCLI(t, strings.NewReader(""), args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.flag, err, tc.want)
		}
	}
	file := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(file, []byte("name: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, strings.NewReader(""), "--registry", noRegistry, "install", "--local", file, "--allow-unsigned", "--yes"); err == nil || err.Error() != "--local "+file+": not a directory" {
		t.Errorf("a file taken for a folder: %v", err)
	}
	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, "agent.yaml"), []byte("schemaVersion: \"2\"\nname: broken\nnamespace: fixtures\nmodes: [autonomous]\nversion: not-semver\nsummary: s\nentrypoints:\n  instruction: AGENT.md\npublish:\n  include: [agent.yaml]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = runCLI(t, strings.NewReader(""), "--registry", noRegistry, "install", "--local", broken, "--allow-unsigned", "--yes")
	wantClassified(t, err, engine.ExitCompile)

	// A folder that declares network egress asks, and --yes answers.
	wide := localFolder(t)
	raw, err := os.ReadFile(filepath.Join(wide, "agent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wide, "agent.yaml"), append(raw, []byte("\ncapabilities:\n  network:\n    egress: [api.example.com]\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, strings.NewReader(""), "--registry", noRegistry, "install", "--local", wide, "--allow-unsigned"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("a folder declaring egress installed without consent: %v", err)
	}
	// Install state that cannot be read is said before the install goes on.
	statePath, err := installed.StatePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte("records: [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// It goes on as a registry install does: the record is rewritten and
	// names the release just laid down.
	if _, stderr, err := runCLI(t, strings.NewReader(""), "--registry", noRegistry, "install", "--local", wide, "--allow-unsigned", "--yes"); err != nil || !strings.Contains(stderr, "treating this as a first install") {
		t.Fatalf("install over unreadable state: %v (stderr %q), want the warning and an install", err, stderr)
	}
	rewritten, err := installed.Load()
	if err != nil {
		t.Fatalf("the record after an install over unreadable state: %v, want it rewritten", err)
	}
	if len(rewritten.Records) != 1 || rewritten.Records[0].Namespace != "fixtures" || rewritten.Records[0].Name != "pure-text" || !rewritten.Records[0].Local() {
		t.Fatalf("the rewritten record = %+v, want the one local release just installed", rewritten.Records)
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}

	// The runner target is the one a local folder is installed into.
	if _, _, err := runCLI(t, strings.NewReader(""), "--registry", noRegistry, "install", "--local", dir, "--allow-unsigned", "--yes", "--target", "runner"); err != nil {
		t.Fatalf("--target runner: %v", err)
	}
}

// Installed from a folder, a QuickAgent lists as local and unsigned, runs
// by name with no registry, asks --allow-unsigned on every run, and is
// refused once its installed files change.
func TestInstallLocalThenRunByName(t *testing.T) {
	isolateEnv(t)
	home, policyPath := isolateRunnerHome(t)
	dir := localFolder(t)

	// A QuickAgent of the same name installed from a registry is replaced,
	// and the install says so.
	fr := newFakeRegistry(t)
	published := skillFiles("pure-text", "")
	published["agent.yaml"] = strings.Replace(published["agent.yaml"], "namespace: acme", "namespace: fixtures", 1)
	fr.addAgent("fixtures", "pure-text", "autonomous", "s").addVersion(t, "1.0.0", published)
	if _, stderr, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "install", "fixtures/pure-text@1.0.0", "--allow-unsigned", "--yes"); err != nil {
		t.Fatalf("install from the registry: %v (stderr %q)", err, stderr)
	}

	stdout, stderr, err := runCLI(t, strings.NewReader(""), "--registry", noRegistry, "install", "--local", dir, "--allow-unsigned", "--yes")
	if err != nil {
		t.Fatalf("install --local: %v", err)
	}
	if !strings.Contains(stderr, "note: this replaces fixtures/pure-text@1.0.0 installed from a registry\n") {
		t.Errorf("the replacement of a registry install is not said: %q", stderr)
	}
	if !strings.Contains(stdout, "installed fixtures/pure-text@1.0.0 (local, unsigned)") {
		t.Fatalf("install said %q", stdout)
	}
	// It says what it installed, from where, and how to run it.
	for _, want := range []string{
		"fixtures/pure-text@1.0.0 (autonomous), local and unsigned, from " + dir + " (sha256:",
		"capabilities:\n  none declared (pure text agent)\n",
		"run it with: agentry run fixtures/pure-text --allow-unsigned\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("install output lacks %q:\n%s", want, stdout)
		}
	}
	listed, _, err := runCLI(t, strings.NewReader(""), "list", "--check")
	if err != nil || !strings.Contains(listed, "fixtures/pure-text@1.0.0") || !strings.Contains(listed, "(local, unsigned)") || strings.Contains(listed, "could not check") || strings.Contains(listed, "up to date") || strings.Contains(listed, "update available") {
		t.Fatalf("list --check: %q, %v", listed, err)
	}

	run := []string{"--registry", noRegistry, "run", "fixtures/pure-text", "--dry-run", "--policy", policyPath}
	_, stderr, err = runCLI(t, strings.NewReader(""), append(run, "--allow-unsigned")...)
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
	home, _ := isolateRunnerHome(t)
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
	if plan.Target != "runner" || plan.Dir != installdir.AgentDir(home, "fixtures", "pure-text") || !plan.Managed || plan.NeedsConfirmation {
		t.Fatalf("plan placement %+v", plan)
	}
	stdout, _, err = runCLI(t, strings.NewReader(""), "--json", "--registry", noRegistry, "install", "--local", dir, "--allow-unsigned", "--yes")
	if err != nil {
		t.Fatalf("install --local --json: %v", err)
	}
	var res installResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil || res.Attestation != "local" || !res.Recorded || res.Digest != plan.Digest {
		t.Fatalf("result %+v (%v)\n%s", res, err, stdout)
	}
	sum := sha256.Sum256(res.Contract)
	if res.Ref != "fixtures/pure-text@1.0.0" || res.Namespace != "fixtures" || res.Name != "pure-text" || res.Version != "1.0.0" || res.Target != "runner" ||
		res.Dir != installdir.AgentDir(home, "fixtures", "pure-text") || !res.Managed || res.Signed || res.Repaired ||
		string(res.Contract) != "{}" || res.ContractDigest != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("result %+v", res)
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
	_, _, err = runCLI(t, strings.NewReader(""), install...)
	if err == nil {
		t.Fatal("an install over a changed copy of the same release was not refused")
	}
	wantClassified(t, err, engine.ExitVerify)
	if _, _, err := runCLI(t, strings.NewReader(""), append(install, "--repair")...); err != nil {
		t.Fatalf("install --local --repair: %v", err)
	}
	// The result of a repair says it was one.
	stdout, _, err = runCLI(t, strings.NewReader(""), append([]string{"--json"}, append(install, "--repair")...)...)
	var repaired installResult
	if err != nil || json.Unmarshal([]byte(stdout), &repaired) != nil || !repaired.Repaired || repaired.Attestation != "local" {
		t.Fatalf("install --local --repair --json: %v %+v\n%s", err, repaired, stdout)
	}
	if _, _, err := runCLI(t, strings.NewReader(""), run...); err != nil {
		t.Fatalf("after repair the local install does not run: %v", err)
	}
}
