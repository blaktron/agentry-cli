package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/installed"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// TestInstallRunnerTargetIsTheDefault (agentry-cli#190, DR-28): `install`
// with no --target lays the release down in the runner home — the
// directory a run reads, with install.json and the pristine workspace —
// writes the coding-agent shape beside it in the style the policy implies
// (no policy: AGENTS.md), and records it in installed.yaml under the run's
// own consent key, managed.
func TestInstallRunnerTargetIsTheDefault(t *testing.T) {
	isolateEnv(t)
	home, _ := isolateRunnerHome(t)
	fr := newFakeRegistry(t)
	fv := fr.addAgent("acme", "demo", "autonomous", "reads things").addVersion(t, "1.0.0", skillFiles("demo", "capabilities:\n  filesystem:\n    read: workspace\n"))

	stdout, stderr, err := runCLI(t, nil, "--registry", fr.url(), "install", "acme/demo@1.0.0", "--allow-unsigned")
	if err != nil {
		t.Fatalf("install: %v (stderr %q)", err, stderr)
	}
	agentDir := installdir.AgentDir(home, "acme", "demo")
	releaseDir := installdir.ReleaseDir(home, "acme", "demo", "1.0.0", fv.digest)
	if !strings.Contains(stdout, "into the runner home: "+releaseDir) || !strings.Contains(stdout, "(export: agents-md)") {
		t.Fatalf("stdout:\n%s", stdout)
	}
	meta, ok := installdir.Load(releaseDir, fv.digest)
	if !ok || meta.Export != "agents-md" || meta.Attestation.State != "unsigned" || meta.Attestation.Consent != "" {
		t.Fatalf("install.json: %+v ok=%v (the cache holds the verdict, never the consent)", meta, ok)
	}
	for _, want := range []string{"workspace/SKILL.md", "export/AGENTS.md", "export/agents/demo/SKILL.md"} {
		if _, err := os.Stat(filepath.Join(releaseDir, filepath.FromSlash(want))); err != nil {
			t.Fatalf("%s: %v", want, err)
		}
	}
	st, err := installed.Load()
	if err != nil || len(st.Records) != 1 {
		t.Fatalf("records: %+v err %v", st.Records, err)
	}
	rec := st.Records[0]
	if rec.Registry != "runner" || rec.Target != "runner" || rec.Dir != agentDir || rec.Version != "1.0.0" || !rec.Managed {
		t.Fatalf("record: %+v", rec)
	}

	// --dir is for the coding-agent targets.
	if _, _, err := runCLI(t, nil, "--registry", fr.url(), "install", "acme/demo@1.0.0", "--allow-unsigned", "--dir", t.TempDir()); err == nil || !strings.Contains(err.Error(), "--dir") {
		t.Fatalf("--dir with the runner target: %v", err)
	}
}

// TestInstallRunnerTargetStyleFollowsThePolicy: the policy's coding agent,
// then its provider, decide the export written beside the release (D2).
func TestInstallRunnerTargetStyleFollowsThePolicy(t *testing.T) {
	isolateEnv(t)
	home, policyPath := isolateRunnerHome(t)
	fr := newFakeRegistry(t)
	fv := fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))
	releaseDir := installdir.ReleaseDir(home, "acme", "demo", "1.0.0", fv.digest)

	for _, c := range []struct {
		name, provider, harness, want, file string
	}{
		{"openai", "openai", "", "agents-md", "export/AGENTS.md"},
		{"anthropic", "anthropic", "", "claude-skill", "export/demo/SKILL.md"},
		{"codex over anthropic", "anthropic", "codex", "agents-md", "export/AGENTS.md"},
		{"claude over openai", "openai", "claude", "claude-skill", "export/demo/SKILL.md"},
	} {
		pol := policy.Default()
		pol.Model.Provider = c.provider
		if c.harness != "" {
			pol.Harness = map[string]policy.HarnessPolicy{c.harness: {}}
		}
		if err := policy.Save(policyPath, pol); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, err := runCLI(t, nil, "--registry", fr.url(), "install", "acme/demo@1.0.0", "--allow-unsigned")
		if err != nil {
			t.Fatalf("%s: install: %v (stderr %q)", c.name, err, stderr)
		}
		if !strings.Contains(stdout, "(export: "+c.want+")") {
			t.Fatalf("%s: stdout %s", c.name, stdout)
		}
		if _, err := os.Stat(filepath.Join(releaseDir, filepath.FromSlash(c.file))); err != nil {
			t.Fatalf("%s: %s missing", c.name, c.file)
		}
	}
}

// TestInstallRunnerTargetThenRunFetchesNothing: a run after the install
// finds the release by digest and downloads nothing more; the consent the
// install recorded covers the run (one record).
func TestInstallRunnerTargetThenRunFetchesNothing(t *testing.T) {
	isolateEnv(t)
	home, policyPath := isolateRunnerHome(t)
	fr := newFakeRegistry(t)
	fr.enableSigning(t)
	fv := fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", "capabilities:\n  network:\n    egress: [api.example.com]\n"))
	fv.attMode = attSigned

	if _, stderr, err := runCLI(t, nil, "--registry", fr.url(), "install", "acme/demo@1.0.0", "--yes"); err != nil {
		t.Fatalf("install: %v (stderr %q)", err, stderr)
	}
	downloads := len(fr.downloadAuths)
	if downloads != 1 {
		t.Fatalf("the install downloaded %d times", downloads)
	}
	// The cache says the verdict was a verified signature the way a run's
	// fetch does: a later run's receipt reads it verbatim.
	if meta, ok := installdir.Load(installdir.ReleaseDir(home, "acme", "demo", "1.0.0", fv.digest), fv.digest); !ok || meta.Attestation.State != "verified" || !meta.Attestation.Verified || meta.Attestation.Consent != "" {
		t.Fatalf("install.json attestation: %+v ok=%v", meta, ok)
	}
	// A dry run materializes into the real home (a --plan uses a throwaway
	// one and always fetches): the release is found by digest, nothing is
	// downloaded, and the consent the install recorded covers the run —
	// off a TTY with no --yes, an unconsented egress contract would refuse.
	_, stderr, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/demo@1.0.0", "--dry-run", "--policy", policyPath)
	if err != nil && strings.Contains(err.Error(), "--yes") {
		t.Fatalf("the install's consent did not cover the run: %v", err)
	}
	if len(fr.downloadAuths) != downloads {
		t.Fatalf("the run downloaded again: %d (stderr %q)", len(fr.downloadAuths)-downloads, stderr)
	}
	st, _ := installed.Load()
	if len(st.Records) != 1 || !st.Records[0].Managed {
		t.Fatalf("records after run: %+v", st.Records)
	}
}

// TestInstallRunnerTargetReplacesAndUninstalls: a newer version replaces
// the older directory (D9) and keeps one record; widened capabilities are
// asked about against the installed snapshot; uninstall removes the
// agent's directory and the record.
func TestInstallRunnerTargetReplacesAndUninstalls(t *testing.T) {
	isolateEnv(t)
	home, _ := isolateRunnerHome(t)
	fr := newFakeRegistry(t)
	fa := fr.addAgent("acme", "demo", "autonomous", "s")
	v1 := fa.addVersion(t, "1.0.0", skillFiles("demo", ""))
	v2 := fa.addVersion(t, "1.1.0", skillFiles("demo", "capabilities:\n  network:\n    egress: [api.example.com]\n"))

	if _, stderr, err := runCLI(t, nil, "--registry", fr.url(), "install", "acme/demo@1.0.0", "--allow-unsigned"); err != nil {
		t.Fatalf("install 1.0.0: %v (stderr %q)", err, stderr)
	}
	// The widening asks; off a TTY without --yes it refuses, and 1.0.0 stays.
	if _, _, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "install", "acme/demo@1.1.0", "--allow-unsigned"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("widened egress installed without consent: %v", err)
	}
	old := installdir.ReleaseDir(home, "acme", "demo", "1.0.0", v1.digest)
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("the refused update removed 1.0.0: %v", err)
	}
	stdout, stderr, err := runCLI(t, nil, "--registry", fr.url(), "install", "acme/demo@1.1.0", "--allow-unsigned", "--yes")
	if err != nil {
		t.Fatalf("install 1.1.0: %v (stderr %q)", err, stderr)
	}
	if !strings.Contains(stdout, "capability diff vs 1.0.0") {
		t.Fatalf("no diff against the installed version:\n%s", stdout)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("1.0.0's directory survived the update")
	}
	if _, ok := installdir.Load(installdir.ReleaseDir(home, "acme", "demo", "1.1.0", v2.digest), v2.digest); !ok {
		t.Fatal("1.1.0 is not installed")
	}
	st, _ := installed.Load()
	if len(st.Records) != 1 || st.Records[0].Version != "1.1.0" {
		t.Fatalf("records: %+v", st.Records)
	}

	stdout, _, err = runCLI(t, nil, "uninstall", "acme/demo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(installdir.AgentDir(home, "acme", "demo")); !os.IsNotExist(err) {
		t.Fatalf("uninstall left the agent's directory (stdout %s)", stdout)
	}
	if st, _ := installed.Load(); len(st.Records) != 0 {
		t.Fatalf("uninstall left the record: %+v", st.Records)
	}
}

// TestListCheckMarksUpdates (agentry-cli#190): --check asks the registry
// for the newest version per record — the configured registry for a
// runner record — and marks the ones an install would update; a registry
// that cannot be reached is that record's line, not the listing's failure.
func TestListCheckMarksUpdates(t *testing.T) {
	isolateEnv(t)
	isolateRunnerHome(t)
	fr := newFakeRegistry(t)
	fa := fr.addAgent("acme", "demo", "autonomous", "s")
	fa.addVersion(t, "1.0.0", skillFiles("demo", ""))
	if _, stderr, err := runCLI(t, nil, "--registry", fr.url(), "install", "acme/demo@1.0.0", "--allow-unsigned"); err != nil {
		t.Fatalf("install: %v (stderr %q)", err, stderr)
	}
	// Nothing newer yet.
	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "list", "--check")
	if err != nil || !strings.Contains(stdout, "up to date") || strings.Contains(stdout, "update available") {
		t.Fatalf("list --check with nothing newer: %v\n%s", err, stdout)
	}
	// A newer version is published.
	fa.addVersion(t, "1.1.0", skillFiles("demo", ""))
	stdout, _, err = runCLI(t, nil, "--registry", fr.url(), "--json", "list", "--check")
	if err != nil {
		t.Fatal(err)
	}
	var out listOutput
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if len(out.Records) != 1 || !out.Records[0].UpdateAvailable || out.Records[0].Latest != "1.1.0" || out.Records[0].CheckError != "" {
		t.Fatalf("records: %+v", out.Records)
	}
	stdout, _, _ = runCLI(t, nil, "--registry", fr.url(), "list", "--check")
	if !strings.Contains(stdout, "update available: 1.1.0") {
		t.Fatalf("text listing:\n%s", stdout)
	}
	// A record whose registry cannot be reached says so, and the listing
	// goes on with exit 0.
	st, _ := installed.Load()
	st.Upsert(installed.Key{Registry: "https://127.0.0.1:1", Namespace: "acme", Name: "gone", Target: "claude-skill", Dir: t.TempDir()}, "0.1.0", installed.CapSnapshot{}, true)
	if err := installed.Save(st); err != nil {
		t.Fatal(err)
	}
	stdout, _, err = runCLI(t, nil, "--registry", fr.url(), "--json", "list", "--check")
	if err != nil {
		t.Fatalf("an unreachable registry failed the listing: %v", err)
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatal(err)
	}
	var demo, gone *listedRecord
	for i := range out.Records {
		switch out.Records[i].Name {
		case "demo":
			demo = &out.Records[i]
		case "gone":
			gone = &out.Records[i]
		}
	}
	if demo == nil || !demo.UpdateAvailable || gone == nil || gone.CheckError == "" || gone.UpdateAvailable {
		t.Fatalf("records: %+v", out.Records)
	}
}

// TestInstallRunnerTargetWritesNothingBeforeTheGates: a refused
// attestation, a refused consent, and a --plan each leave the runner home
// without an agents directory — nothing lands before the digest, the
// signature and the consent (ARCHITECTURE §9).
func TestInstallRunnerTargetWritesNothingBeforeTheGates(t *testing.T) {
	isolateEnv(t)
	home, _ := isolateRunnerHome(t)
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", "capabilities:\n  network:\n    egress: [api.example.com]\n"))
	agents := filepath.Join(home, "agents")

	// Unsigned without --allow-unsigned: refused at the attestation.
	if _, _, err := runCLI(t, nil, "--registry", fr.url(), "install", "acme/demo@1.0.0", "--yes"); err == nil || !strings.Contains(err.Error(), "--allow-unsigned") {
		t.Fatalf("unsigned installed: %v", err)
	}
	// Egress without consent off a TTY: refused at the gate.
	if _, _, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "install", "acme/demo@1.0.0", "--allow-unsigned"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("egress installed without consent: %v", err)
	}
	// --plan: the preview, nothing written.
	if _, _, err := runCLI(t, nil, "--registry", fr.url(), "--json", "install", "acme/demo@1.0.0", "--plan", "--allow-unsigned"); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err := os.Stat(agents); !os.IsNotExist(err) {
		t.Fatalf("something wrote into the runner home before the gates: %v", err)
	}
	if st, _ := installed.Load(); len(st.Records) != 0 {
		t.Fatalf("a refusal recorded: %+v", st.Records)
	}
}

// TestListCheckAsksTheRecordsOwnRegistry: a coding-agent record names its
// registry, and --check asks that one — without the configured registry's
// credential — while a runner record asks the configured registry.
func TestListCheckAsksTheRecordsOwnRegistry(t *testing.T) {
	isolateEnv(t)
	isolateRunnerHome(t)
	configured := newFakeRegistry(t)
	configured.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))
	other := newFakeRegistry(t)
	oa := other.addAgent("acme", "demo", "autonomous", "s")
	oa.addVersion(t, "1.0.0", skillFiles("demo", ""))
	oa.addVersion(t, "2.0.0", skillFiles("demo", ""))

	if _, stderr, err := runCLI(t, nil, "--registry", configured.url(), "install", "acme/demo@1.0.0", "--allow-unsigned"); err != nil {
		t.Fatalf("runner install: %v (stderr %q)", err, stderr)
	}
	if _, stderr, err := runCLI(t, nil, "--registry", other.url(), "install", "acme/demo@1.0.0", "--allow-unsigned", "--target", "claude-skill"); err != nil {
		t.Fatalf("coding-agent install: %v (stderr %q)", err, stderr)
	}
	t.Setenv("AGENTRY_TOKEN", "configured-secret")
	stdout, _, err := runCLI(t, nil, "--registry", configured.url(), "--json", "list", "--check")
	if err != nil {
		t.Fatal(err)
	}
	var out listOutput
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if len(out.Records) != 2 {
		t.Fatalf("records: %+v", out.Records)
	}
	for _, r := range out.Records {
		switch r.Target {
		case "runner":
			if r.Latest != "1.0.0" || r.UpdateAvailable || r.CheckError != "" {
				t.Fatalf("runner record asked the wrong registry: %+v", r)
			}
		case "claude-skill":
			if r.Latest != "2.0.0" || !r.UpdateAvailable || r.CheckError != "" {
				t.Fatalf("coding-agent record did not ask its own registry: %+v", r)
			}
		}
	}
	for _, auth := range other.resolveAuths {
		if strings.Contains(auth, "configured-secret") {
			t.Fatal("the configured registry's credential went to another registry")
		}
	}
}

// TestInstallRunnerTargetOfAnInstructionOnlyReleaseWithClaudeChosen
// (agentry-cli#193): a prompt agent — AGENT.md, no SKILL.md, the shape of
// the catalogue on dev — installs into the runner home with Claude Code
// chosen, its export a Claude skill whose SKILL.md is made from the
// instruction; and `--target claude-skill` on the same release writes the
// same skill into ~/.claude/skills. Before this, both failed on the
// SKILL.md the release does not carry.
func TestInstallRunnerTargetOfAnInstructionOnlyReleaseWithClaudeChosen(t *testing.T) {
	isolateEnv(t)
	home, policyPath := isolateRunnerHome(t)
	fr := newFakeRegistry(t)
	fv := fr.addAgent("acme", "pa", "autonomous", "s").addVersion(t, "1.0.0", promptAgentFiles("pa"))
	pol := policy.Default()
	pol.Harness = map[string]policy.HarnessPolicy{"claude": {}}
	if err := policy.Save(policyPath, pol); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := runCLI(t, nil, "--registry", fr.url(), "install", "acme/pa@1.0.0", "--allow-unsigned", "--yes")
	if err != nil {
		t.Fatalf("install: %v (stderr %q)", err, stderr)
	}
	if !strings.Contains(stdout, "(export: claude-skill)") {
		t.Fatalf("stdout %s", stdout)
	}
	releaseDir := installdir.ReleaseDir(home, "acme", "pa", "1.0.0", fv.digest)
	skill, err := os.ReadFile(filepath.Join(releaseDir, "export", "pa", "SKILL.md"))
	if err != nil {
		t.Fatalf("the synthesised skill: %v", err)
	}
	if !strings.Contains(string(skill), "name: pa") || !strings.Contains(string(skill), "allowed-tools: Bash(jq:*)") || !strings.HasSuffix(string(skill), "# main\n\nDo it.\n") {
		t.Fatalf("SKILL.md:\n%s", skill)
	}

	stdout, stderr, err = runCLI(t, nil, "--registry", fr.url(), "install", "acme/pa@1.0.0", "--target", "claude-skill", "--allow-unsigned", "--yes")
	if err != nil {
		t.Fatalf("--target claude-skill: %v (stderr %q)", err, stderr)
	}
	if !strings.Contains(stdout, "as Claude skill") {
		t.Fatalf("stdout %s", stdout)
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(userHome, ".claude", "skills", "pa", "SKILL.md")); err != nil {
		t.Fatalf("~/.claude/skills/pa/SKILL.md: %v", err)
	}
	if _, err := os.Stat(filepath.Join(userHome, ".claude", "skills", "pa", "AGENT.md")); err != nil {
		t.Fatalf("the instruction file stays beside the skill: %v", err)
	}
}
