package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/managedsettings"

	"github.com/blaktron/agentry-cli/pkg/installed"
	"github.com/blaktron/agentry-cli/pkg/managed"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/trust"
)

// assignRow is the quickagents row listing the assignments, locked.
func assignRow(t *testing.T, list ...managedsettings.Assignment) map[string]managedsettings.Setting {
	t.Helper()
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]managedsettings.Setting{managedsettings.QuickAgents: {Value: raw, Locked: true}}
}

func required(ref, version string) managedsettings.Assignment {
	return managedsettings.Assignment{Ref: ref, Version: version, Mode: managedsettings.AssignRequired}
}

const egressContract = "capabilities:\n  network:\n    egress: [api.example.com]\n"

// A required QuickAgent is installed when the settings are fetched, through
// the install flow, and the record it leaves holds no consent: the first run
// is headed by the organization, asks, and is refused without an answer.
func TestARequiredQuickAgentIsInstalledWithoutConsent(t *testing.T) {
	isolateEnv(t)
	home, policyPath := isolateRunnerHome(t)
	fr := newFakeRegistry(t)
	fr.enableSigning(t)
	fv := fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", egressContract))
	fv.attMode = attSigned
	holdManagedFrom(t, fr, assignRow(t, required("acme/demo", "1.0.0")))

	releaseDir := installdir.ReleaseDir(home, "acme", "demo", "1.0.0", fv.digest)
	if _, err := installdir.Inspect(releaseDir, fv.digest); err != nil {
		t.Fatalf("the required release is not installed: %v", err)
	}
	st, err := installed.Load()
	if err != nil || len(st.Records) != 1 {
		t.Fatalf("records: %+v err %v", st.Records, err)
	}
	rec := st.Records[0]
	if rec.AssignedBy != "acme" || !rec.Capabilities.Empty() || rec.Version != "1.0.0" {
		t.Fatalf("record: %+v; want assigned by acme, no capabilities", rec)
	}

	// No path treats the assignment as consent: a run off a terminal and
	// without --yes stops at the consent, naming the organization.
	_, stderr, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/demo@1.0.0", "--dry-run", "--policy", policyPath)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("run without consent: %v (stderr %q)", err, stderr)
	}
	if !strings.Contains(stderr, "Required by acme") {
		t.Errorf("the consent is not headed by the organization:\n%s", stderr)
	}
	if st, _ := installed.Load(); !st.Records[0].Capabilities.Empty() {
		t.Fatalf("a refused run recorded a consent: %+v", st.Records[0])
	}

	// The person's own --yes consents, and the record is theirs.
	if _, stderr, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/demo@1.0.0", "--dry-run", "--yes", "--policy", policyPath); err != nil {
		t.Fatalf("run with --yes: %v (stderr %q)", err, stderr)
	}
	st, _ = installed.Load()
	if rec := st.Records[0]; rec.AwaitsConsent() || rec.Capabilities.Empty() {
		t.Fatalf("after the person's consent: %+v", rec)
	}
}

// A required QuickAgent that fails a check of the install installs nothing,
// says why in one line and does not fail the command that fetched it.
func TestARequiredQuickAgentThatFailsACheckIsNotInstalled(t *testing.T) {
	isolateEnv(t)
	home, _ := isolateRunnerHome(t)
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", "")) // unsigned
	fr.enableSigning(t)
	fr.orgs = []map[string]any{{"handle": "acme", "role": "member", "state": "active"}}
	t.Setenv(envToken, "agy_test")
	if err := trust.SavePinned(fr.url(), trust.KeySet{Keys: []trust.SigningKey{{KeyID: fr.keyID, Algorithm: "ecdsa-p256-sha256", PublicKey: fr.pubPEM}}}); err != nil {
		t.Fatal(err)
	}
	fr.serveManaged(5, assignRow(t, required("acme/demo", "1.0.0")))
	_, stderr, err := runCLI(t, nil, "--registry", fr.url(), "managed", "refresh")
	if err != nil {
		t.Fatalf("the failed install failed the refresh: %v", err)
	}
	if !strings.Contains(stderr, "acme/demo, required by acme, was not installed: unsigned") || strings.Count(stderr, "\n") != 1 {
		t.Errorf("stderr:\n%s", stderr)
	}
	if st, _ := installed.Load(); len(st.Records) != 0 {
		t.Errorf("records: %+v", st.Records)
	}
	if _, err := os.Stat(installdir.AgentDir(home, "acme", "demo")); err == nil {
		t.Errorf("a release is laid down")
	}
}

// A refresh installs nothing again for a version already held, and follows a
// pin the organization moves; an update keeps what the person accepted and
// records no more.
func TestARequiredQuickAgentFollowsItsPinOnly(t *testing.T) {
	isolateEnv(t)
	home, policyPath := isolateRunnerHome(t)
	fr := newFakeRegistry(t)
	fr.enableSigning(t)
	v1 := fr.addAgent("acme", "demo", "autonomous", "s")
	fv1 := v1.addVersion(t, "1.0.0", skillFiles("demo", egressContract))
	fv1.attMode = attSigned
	holdManagedFrom(t, fr, assignRow(t, required("acme/demo", "1.0.0")))
	downloads := len(fr.downloadAuths)

	fr.serveManaged(6, assignRow(t, required("acme/demo", "1.0.0")))
	if _, stderr, err := runCLI(t, nil, "--registry", fr.url(), "managed", "refresh"); err != nil {
		t.Fatalf("refresh: %v (%s)", err, stderr)
	}
	if len(fr.downloadAuths) != downloads {
		t.Fatalf("a held version was downloaded again")
	}

	// The person consents to 1.0.0.
	if _, stderr, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/demo@1.0.0", "--dry-run", "--yes", "--policy", policyPath); err != nil {
		t.Fatalf("run: %v (%s)", err, stderr)
	}
	fv2 := v1.addVersion(t, "1.1.0", skillFiles("demo", "capabilities:\n  network:\n    egress: [api.example.com, other.example.com]\n"))
	fv2.attMode = attSigned
	fr.serveManaged(7, assignRow(t, required("acme/demo", "1.1.0")))
	if _, stderr, err := runCLI(t, nil, "--registry", fr.url(), "managed", "refresh"); err != nil {
		t.Fatalf("refresh: %v (%s)", err, stderr)
	}
	if _, err := installdir.Inspect(installdir.ReleaseDir(home, "acme", "demo", "1.1.0", fv2.digest), fv2.digest); err != nil {
		t.Fatalf("the moved pin is not installed: %v", err)
	}
	st, _ := installed.Load()
	if len(st.Records) != 1 || st.Records[0].Version != "1.1.0" {
		t.Fatalf("records: %+v", st.Records)
	}
	// The widened contract asks again, as an update always does.
	_, stderr, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "run", "acme/demo@1.1.0", "--dry-run", "--policy", policyPath)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("the widened contract was not asked about: %v (%s)", err, stderr)
	}
}

// A required QuickAgent cannot be uninstalled while a document requires it
// (exit 11, naming the organization and the setting); once none does it is
// the person's own and goes.
func TestARequiredQuickAgentIsNotUninstalledWhileRequired(t *testing.T) {
	isolateEnv(t)
	isolateRunnerHome(t)
	fr := newFakeRegistry(t)
	fr.enableSigning(t)
	fv := fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))
	fv.attMode = attSigned
	holdManagedFrom(t, fr, assignRow(t, required("acme/demo", "1.0.0")))

	_, _, err := runCLI(t, nil, "uninstall", "acme/demo")
	if err == nil || !strings.Contains(err.Error(), "required by acme's managed settings (quickagents)") {
		t.Fatalf("uninstall while required: %v", err)
	}
	wantClassified(t, err, 11)
	if st, _ := installed.Load(); len(st.Records) != 1 {
		t.Fatalf("the refused uninstall removed the record: %+v", st.Records)
	}

	// Offered, it is not required.
	offered := assignRow(t, managedsettings.Assignment{Ref: "acme/demo", Version: managedsettings.VersionLatest, Mode: managedsettings.AssignOffered})
	fr.serveManaged(6, offered)
	if _, stderr, err := runCLI(t, nil, "--registry", fr.url(), "managed", "refresh"); err != nil {
		t.Fatalf("refresh: %v (%s)", err, stderr)
	}
	if _, stderr, err := runCLI(t, nil, "uninstall", "acme/demo"); err != nil {
		t.Fatalf("uninstall once not required: %v (%s)", err, stderr)
	}
}

// `search --org` lists the offered and required assignments from the held
// settings, with no registry asked.
func TestSearchOrgListsTheAssignments(t *testing.T) {
	isolateEnv(t)
	isolateRunnerHome(t)
	fr := newFakeRegistry(t)
	holdManagedFrom(t, fr, assignRow(t,
		managedsettings.Assignment{Ref: "acme/offer", Version: "2.0.0", Mode: managedsettings.AssignOffered},
		managedsettings.Assignment{Ref: "acme/never-installs", Version: managedsettings.VersionLatest, Mode: managedsettings.AssignRequired},
	))
	fr.srv.Close()
	stdout, stderr, err := runCLI(t, nil, "search", "--org")
	if err != nil {
		t.Fatalf("search --org: %v (%s)", err, stderr)
	}
	for _, want := range []string{"acme/offer", "2.0.0", "offered", "acme/never-installs", "latest", "required", "acme"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q:\n%s", want, stdout)
		}
	}
	stdout, _, err = runCLI(t, nil, "--json", "search", "--org", "OFFER")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Results []assignedView `json:"results"`
		Total   int            `json:"total"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil || doc.Total != 1 || doc.Results[0].Ref != "acme/offer" || doc.Results[0].Organization != "acme" {
		t.Fatalf("json: %s (%v)", stdout, err)
	}
}

func heldWith(t *testing.T, a managedsettings.Assignment) managed.Effective {
	t.Helper()
	d := managedsettings.Document{Org: "acme", Settings: assignRow(t, a)}
	return managed.Effective{Documents: []managed.State{{Registry: "https://reg.example", Document: d}}}
}

// A preset is a default the person's flags override; locked, a conflicting
// flag is refused naming the organization and the setting.
func TestAPresetIsADefaultUnlessLocked(t *testing.T) {
	a := managedsettings.Assignment{Ref: "acme/demo", Version: "1.0.0", Mode: managedsettings.AssignRequired,
		Preset: &managedsettings.Preset{TimeoutSeconds: 300, MaxTurns: 20, Inputs: map[string]string{"topic": "tax"}}}
	opts := runOpts{maxTurns: -1}
	if err := applyPreset(&opts, "acme/demo@1.0.0", heldWith(t, a)); err != nil {
		t.Fatal(err)
	}
	if opts.timeout != 300 || opts.maxTurns != 20 || len(opts.intake) != 1 || opts.intake[0] != "topic=tax" {
		t.Fatalf("defaults not applied: %+v", opts)
	}
	own := runOpts{timeout: 60, maxTurns: 5, intake: []string{"topic=law"}}
	if err := applyPreset(&own, "acme/demo", heldWith(t, a)); err != nil || own.timeout != 60 || own.maxTurns != 5 || own.intake[0] != "topic=law" {
		t.Fatalf("the person's flags did not stand: %+v %v", own, err)
	}
	if err := applyPreset(&runOpts{maxTurns: -1}, "acme/other", heldWith(t, a)); err != nil {
		t.Fatal(err)
	}

	a.PresetLocked = true
	err := applyPreset(&runOpts{timeout: 60, maxTurns: -1}, "acme/demo", heldWith(t, a))
	if err == nil || !strings.Contains(err.Error(), "--timeout") || !strings.Contains(err.Error(), "acme's managed settings") || !strings.Contains(err.Error(), "(quickagents)") {
		t.Fatalf("a conflicting flag under a lock: %v", err)
	}
	wantClassified(t, err, 11)
	if err := applyPreset(&runOpts{timeout: 300, maxTurns: 20, intake: []string{"topic=tax"}}, "acme/demo", heldWith(t, a)); err != nil {
		t.Fatalf("flags that agree with a lock: %v", err)
	}
	if err := applyPreset(&runOpts{maxTurns: -1, intake: []string{"topic=law"}}, "acme/demo", heldWith(t, a)); err == nil {
		t.Fatal("a conflicting answer under a lock was taken")
	}
}
