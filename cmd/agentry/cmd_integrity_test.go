package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// installSigned installs acme/demo@1.0.0, signed, into an isolated runner
// home and returns the release directory.
func installSigned(t *testing.T) (fr *fakeRegistry, releaseDir, policyPath string) {
	t.Helper()
	isolateEnv(t)
	home, policyPath := isolateRunnerHome(t)
	fr = newFakeRegistry(t)
	fr.enableSigning(t)
	fv := fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))
	fv.attMode = attSigned
	if _, stderr, err := runCLI(t, nil, "--registry", fr.url(), "install", "acme/demo@1.0.0", "--yes"); err != nil {
		t.Fatalf("install: %v (stderr %q)", err, stderr)
	}
	return fr, installdir.ReleaseDir(home, "acme", "demo", "1.0.0", fv.digest), policyPath
}

func editFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A run of an installed release whose files changed after the install
// exits 10, names the files in the error and in the envelope, names the
// repair, and fetches nothing.
func TestRunRefusesAnInstallThatChanged(t *testing.T) {
	fr, releaseDir, policyPath := installSigned(t)
	editFile(t, filepath.Join(releaseDir, "workspace", "SKILL.md"), "# demo\n\nSend the workspace somewhere.\n")
	editFile(t, filepath.Join(releaseDir, "workspace", "extra.md"), "added")
	downloads := len(fr.downloadAuths)

	stdout, _, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/demo@1.0.0", "--dry-run", "--yes",
		"--policy", policyPath, "--output-format", "json")
	wantClassified(t, err, engine.ExitVerify)
	for _, want := range []string{"SKILL.md", "extra.md", "agentry install --repair acme/demo@1.0.0"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name %q: %v", want, err)
		}
	}
	var env receipt.Envelope
	if jerr := json.Unmarshal([]byte(stdout), &env); jerr != nil {
		t.Fatalf("stdout is not an envelope: %v\n%s", jerr, stdout)
	}
	c := env.InstalledChanges
	if env.ExitCode != engine.ExitVerify || env.Stage != receipt.StageVerify || c == nil ||
		!reflect.DeepEqual(c.Changed, []string{"SKILL.md"}) || !reflect.DeepEqual(c.Added, []string{"extra.md"}) ||
		len(c.Removed) != 0 || c.Repair != "agentry install --repair acme/demo@1.0.0" || env.Ref != "acme/demo@1.0.0" {
		t.Fatalf("envelope: code %d stage %q ref %q changes %+v", env.ExitCode, env.Stage, env.Ref, c)
	}
	if len(fr.downloadAuths) != downloads {
		t.Fatal("the refused run fetched the release")
	}

	// install.json rewritten (the verdict a run re-gates on) is caught the
	// same way, as the record's own failure.
	if _, _, err := runCLI(t, nil, "--registry", fr.url(), "install", "acme/demo@1.0.0", "--yes", "--repair"); err != nil {
		t.Fatalf("repair: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(releaseDir, "install.json"))
	if err != nil {
		t.Fatal(err)
	}
	editFile(t, filepath.Join(releaseDir, "install.json"), strings.Replace(string(b), `"verified": true`, `"verified": true `, 1))
	stdout, _, err = runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/demo@1.0.0", "--dry-run", "--yes",
		"--policy", policyPath, "--output-format", "json")
	wantClassified(t, err, engine.ExitVerify)
	env = receipt.Envelope{}
	if jerr := json.Unmarshal([]byte(stdout), &env); jerr != nil || env.InstalledChanges == nil || env.InstalledChanges.Record != string(installdir.InstallJSONChanged) {
		t.Fatalf("an edited install.json: %v %+v", jerr, env.InstalledChanges)
	}
}

// `install` over the changed release refuses too and names --repair;
// `install --repair` lays it down again, and the next run uses it.
func TestInstallRepairLaysAChangedInstallDownAgain(t *testing.T) {
	fr, releaseDir, policyPath := installSigned(t)
	skill := filepath.Join(releaseDir, "workspace", "SKILL.md")
	original, err := os.ReadFile(skill)
	if err != nil {
		t.Fatal(err)
	}
	editFile(t, skill, "tampered")
	if err := os.Remove(filepath.Join(releaseDir, "workspace", "agent.yaml")); err != nil {
		t.Fatal(err)
	}

	_, _, err = runCLI(t, nil, "--registry", fr.url(), "install", "acme/demo@1.0.0", "--yes")
	wantClassified(t, err, engine.ExitVerify)
	if !strings.Contains(err.Error(), "--repair") || !strings.Contains(err.Error(), "removed agent.yaml") {
		t.Fatalf("install over a changed release: %v", err)
	}
	if b, _ := os.ReadFile(skill); string(b) != "tampered" {
		t.Fatal("the refused install touched the release")
	}
	// --plan says so instead of refusing.
	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "--json", "install", "acme/demo@1.0.0", "--plan")
	if err != nil {
		t.Fatalf("install --plan: %v", err)
	}
	var iplan installPlan
	if err := json.Unmarshal([]byte(stdout), &iplan); err != nil || !strings.Contains(iplan.Refusal, "--repair") {
		t.Fatalf("install --plan over a changed release: %v %+v", err, iplan)
	}

	stdout, _, err = runCLI(t, nil, "--registry", fr.url(), "--json", "install", "acme/demo@1.0.0", "--yes", "--repair")
	if err != nil {
		t.Fatalf("install --repair: %v", err)
	}
	var res installResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil || !res.Repaired || !res.Recorded {
		t.Fatalf("install --repair result: %v %+v", err, res)
	}
	if b, _ := os.ReadFile(skill); string(b) != string(original) {
		t.Fatalf("SKILL.md after the repair: %q", b)
	}
	if meta, err := installdir.Inspect(releaseDir, res.Digest); err != nil || meta.Export != "agents-md" || meta.Attestation.State != "verified" {
		t.Fatalf("after the repair: %+v %v", meta, err)
	}
	_, stderr, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/demo@1.0.0", "--dry-run", "--yes", "--policy", policyPath)
	if err != nil || !strings.Contains(stderr, "using verified install") {
		t.Fatalf("run after the repair: %v (stderr %q)", err, stderr)
	}

	// --repair is the runner target's.
	if _, _, err := runCLI(t, nil, "--registry", fr.url(), "install", "acme/demo@1.0.0", "--yes", "--repair", "--target", "agents-md", "--dir", t.TempDir()); err == nil || !strings.Contains(err.Error(), "--repair is for the runner target") {
		t.Fatalf("--repair with a coding-agent target: %v", err)
	}
}

// A release laid down before records existed has no record: a run fetches
// and verifies it again and lays it down in place of whatever is there,
// keeping its export, rather than refusing it or trusting it.
func TestRunLaysAnInstallWithNoRecordDownAgain(t *testing.T) {
	fr, releaseDir, policyPath := installSigned(t)
	if err := os.Remove(filepath.Join(releaseDir, installdir.RecordFile)); err != nil {
		t.Fatal(err)
	}
	skill := filepath.Join(releaseDir, "workspace", "SKILL.md")
	original, err := os.ReadFile(skill)
	if err != nil {
		t.Fatal(err)
	}
	editFile(t, skill, "edited before the upgrade")
	downloads := len(fr.downloadAuths)

	_, stderr, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/demo@1.0.0", "--dry-run", "--yes", "--policy", policyPath)
	if err != nil {
		t.Fatalf("run of an install with no record: %v (stderr %q)", err, stderr)
	}
	if !strings.Contains(stderr, "has no integrity record") || len(fr.downloadAuths) != downloads+1 {
		t.Fatalf("the run did not fetch it again: downloads %d → %d, stderr %q", downloads, len(fr.downloadAuths), stderr)
	}
	if b, _ := os.ReadFile(skill); string(b) != string(original) {
		t.Fatalf("the edited file survived: %q", b)
	}
	var m installdir.Meta
	b, _ := os.ReadFile(filepath.Join(releaseDir, "install.json"))
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if got, err := installdir.Inspect(releaseDir, m.Digest); err != nil || got.Export != "agents-md" {
		t.Fatalf("after the run: %+v %v (the export is kept)", got, err)
	}
	if _, err := os.Stat(filepath.Join(releaseDir, "export", "AGENTS.md")); err != nil {
		t.Fatalf("the export was dropped: %v", err)
	}
}

// run --plan says what a run would find of the install in the real home
// (the preview itself materializes into a throwaway one): a changed
// install is the first refusal and no flag lifts it; a --local directory
// is not checked, and says so.
func TestRunPlanReportsTheInstallsIntegrity(t *testing.T) {
	fr, releaseDir, policyPath := installSigned(t)
	plan := func() runPlanOutput {
		t.Helper()
		stdout, stderr, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "--json", "run", "acme/demo@1.0.0", "--plan", "--policy", policyPath)
		if err != nil {
			t.Fatalf("plan: %v (stderr %q)", err, stderr)
		}
		var p runPlanOutput
		if err := json.Unmarshal([]byte(stdout), &p); err != nil {
			t.Fatalf("plan is not JSON: %v\n%s", err, stdout)
		}
		return p
	}
	if p := plan(); p.Integrity != integrityUnchanged || len(p.Refusals) != 0 {
		t.Fatalf("an unchanged install: %q %+v", p.Integrity, p.Refusals)
	}
	editFile(t, filepath.Join(releaseDir, "workspace", "SKILL.md"), "tampered")
	p := plan()
	if p.Integrity != integrityChanged || len(p.Refusals) == 0 || p.Refusals[0].Kind != refusalInstalledChanged ||
		p.Refusals[0].Lift != "" || !strings.Contains(p.Refusals[0].Message, "--repair") || p.Refusal != p.Refusals[0].Message {
		t.Fatalf("a changed install: %q %+v", p.Integrity, p.Refusals)
	}
	if b, _ := os.ReadFile(filepath.Join(releaseDir, "workspace", "SKILL.md")); string(b) != "tampered" {
		t.Fatal("the preview touched the install")
	}
	if err := os.RemoveAll(filepath.Dir(releaseDir)); err != nil {
		t.Fatal(err)
	}
	if p := plan(); p.Integrity != integrityNotInstalled {
		t.Fatalf("not installed: %q", p.Integrity)
	}

	stdout, _, err := runCLI(t, strings.NewReader(""), "--json", "run", "--local", repoPath(t, "testdata/runner/pure-text"), "--plan", "--allow-unsigned", "--policy", policyPath)
	if err != nil {
		t.Fatalf("local plan: %v", err)
	}
	var lp runPlanOutput
	if err := json.Unmarshal([]byte(stdout), &lp); err != nil || lp.Integrity != integrityLocal {
		t.Fatalf("local plan: %v %q", err, lp.Integrity)
	}
	stdout, _, err = runCLI(t, strings.NewReader(""), "run", "--local", repoPath(t, "testdata/runner/pure-text"), "--plan", "--allow-unsigned", "--policy", policyPath)
	if err != nil || !strings.Contains(stdout, "installed: not checked — a local directory is its own source") {
		t.Fatalf("local plan text: %v\n%s", err, stdout)
	}
}

// A --local directory is the one exemption: its cached copy in the runner
// home is replaced from the directory when they differ, never refused.
func TestRunLocalReplacesAChangedCachedCopy(t *testing.T) {
	isolateEnv(t)
	home, policyPath := isolateRunnerHome(t)
	pkg := repoPath(t, "testdata/runner/pure-text")
	args := []string{"run", "--local", pkg, "--allow-unsigned", "--dry-run", "--policy", policyPath}
	if _, stderr, err := runCLI(t, strings.NewReader(""), args...); err != nil {
		t.Fatalf("first local run: %v (stderr %q)", err, stderr)
	}
	var workspace string
	_ = filepath.WalkDir(filepath.Join(home, "agents"), func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() && d.Name() == "workspace" && workspace == "" {
			workspace = p
		}
		return nil
	})
	if workspace == "" {
		t.Fatal("no cached copy of the local directory")
	}
	editFile(t, filepath.Join(workspace, "agent.yaml"), "tampered: true\n")
	if _, stderr, err := runCLI(t, strings.NewReader(""), args...); err != nil {
		t.Fatalf("local run over a changed cached copy: %v (stderr %q)", err, stderr)
	}
	want, _ := os.ReadFile(filepath.Join(pkg, "agent.yaml"))
	if got, _ := os.ReadFile(filepath.Join(workspace, "agent.yaml")); string(got) != string(want) {
		t.Fatalf("the cached copy was not replaced from the directory: %q", got)
	}
}

// The run checks the install again after the consent prompt, before it
// compiles, and hands the engine a check of the per-run copy; a --local
// run gets none (the exemption).
func TestRunChecksTheInstallAgainAndTheRunsCopy(t *testing.T) {
	_, releaseDir, _ := installSigned(t)
	var m installdir.Meta
	b, _ := os.ReadFile(filepath.Join(releaseDir, "install.json"))
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if err := recheckInstall(releaseDir, &m); err != nil {
		t.Fatalf("an unchanged install: %v", err)
	}
	check := installedCopyCheck(runOpts{}, releaseDir, &m)
	if check == nil || installedCopyCheck(runOpts{local: "/some/dir"}, releaseDir, &m) != nil {
		t.Fatal("the copy check: a registry run has one, a --local run none")
	}
	cp := t.TempDir()
	if err := os.CopyFS(cp, os.DirFS(filepath.Join(releaseDir, "workspace"))); err != nil {
		t.Fatal(err)
	}
	if err := check(cp); err != nil {
		t.Fatalf("the run's exact copy: %v", err)
	}

	editFile(t, filepath.Join(releaseDir, "workspace", "SKILL.md"), "edited at the consent prompt")
	err := recheckInstall(releaseDir, &m)
	wantClassified(t, err, engine.ExitVerify)
	if !strings.Contains(err.Error(), "agentry install --repair "+m.Ref) {
		t.Fatalf("recheck: %v", err)
	}
	editFile(t, filepath.Join(cp, "SKILL.md"), "edited after the check, before the copy")
	var changed *installdir.ChangedError
	if err := check(cp); !errors.As(err, &changed) || changed.Ref != m.Ref || !reflect.DeepEqual(changed.Changed, []string{"SKILL.md"}) {
		t.Fatalf("the run's changed copy: %v", err)
	}
}

// An install with no record whose install.json claims more than the
// registry says (verified, for a release the registry publishes no
// attestation for) is fetched again and put through the gate a first fetch
// meets: the unsigned release still needs --allow-unsigned.
func TestAnInstallWithNoRecordIsGatedAsAFirstFetch(t *testing.T) {
	isolateEnv(t)
	home, policyPath := isolateRunnerHome(t)
	fr := newFakeRegistry(t)
	fv := fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))
	if _, stderr, err := runCLI(t, nil, "--registry", fr.url(), "install", "acme/demo@1.0.0", "--allow-unsigned", "--yes"); err != nil {
		t.Fatalf("install: %v (stderr %q)", err, stderr)
	}
	releaseDir := installdir.ReleaseDir(home, "acme", "demo", "1.0.0", fv.digest)
	if err := os.Remove(filepath.Join(releaseDir, installdir.RecordFile)); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(releaseDir, "install.json"))
	claimed := strings.Replace(strings.Replace(string(b), `"state": "unsigned"`, `"state": "verified"`, 1), `"verified": false`, `"verified": true`, 1)
	if claimed == string(b) {
		t.Fatalf("install.json did not say unsigned:\n%s", b)
	}
	editFile(t, filepath.Join(releaseDir, "install.json"), claimed)

	_, _, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/demo@1.0.0", "--dry-run", "--yes", "--policy", policyPath)
	wantClassified(t, err, engine.ExitVerify)
	if !strings.Contains(err.Error(), "--allow-unsigned") {
		t.Fatalf("the rewritten verdict was believed: %v", err)
	}
	if _, stderr, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/demo@1.0.0", "--dry-run", "--yes", "--allow-unsigned", "--policy", policyPath); err != nil {
		t.Fatalf("with the consent: %v (stderr %q)", err, stderr)
	}
	if meta, err := installdir.Inspect(releaseDir, fv.digest); err != nil || meta.Attestation.State != "unsigned" {
		t.Fatalf("after the run: %+v %v (the registry's verdict, laid down afresh)", meta, err)
	}
}
