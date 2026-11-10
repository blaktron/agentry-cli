package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/managedsettings"
	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/installed"
	"github.com/blaktron/agentry-cli/pkg/managed"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
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
	if opts.timeout != 300 || opts.maxTurns != 20 || len(opts.intake) != 0 || len(opts.orgIntake) != 1 || opts.orgIntake[0] != "topic=tax" {
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

// An organization's preset answer is used for a question the QuickAgent
// asks and the person did not answer; one it does not ask is passed over
// with a note, never refusing the run.
func TestAPresetAnswerToAQuestionNotAskedIsPassedOver(t *testing.T) {
	m := &manifest.Manifest{Intake: []manifest.IntakeQuestion{{ID: "topic", Prompt: "Topic?"}}}
	cmd := &cobra.Command{}
	var errOut bytes.Buffer
	cmd.SetErr(&errOut)
	cmd.SetIn(strings.NewReader(""))
	ans, err := collectIntake(cmd, m, runOpts{orgIntake: []string{"topic=tax", "region=emea"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ans == nil || ans.Values["topic"] != "tax" {
		t.Fatalf("answers %+v", ans)
	}
	if !strings.Contains(errOut.String(), "region, which this QuickAgent does not ask; passed over") {
		t.Fatalf("said %q", errOut.String())
	}
}

// A QuickAgent an organization's managed settings installed is asked about
// on its first run whatever its contract declares: nothing of the person's
// started the install, so even a contract an ordinary install would not
// confirm waits for their yes. Off a terminal it is refused without --yes.
func TestAnAssignedReleaseIsAskedAboutEvenWithANarrowContract(t *testing.T) {
	isolateEnv(t)
	var st installed.State
	key := installed.Key{Registry: "https://r.example", Namespace: "acme", Name: "quiet", Target: installdir.Target}
	st.UpsertAssigned(key, "1.0.0", true, "acme")
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader(""))
	cmd.SetErr(&bytes.Buffer{})
	next := installed.Snapshot(manifest.Capabilities{})
	err := confirmCapabilityChange(cmd, &bytes.Buffer{}, &st, key, "1.0.0", next, manifest.Capabilities{}, false)
	if err == nil || !strings.Contains(err.Error(), "installed by your organization's managed settings") {
		t.Fatalf("an assigned release with a narrow contract ran without consent: %v", err)
	}
	if err := confirmCapabilityChange(cmd, &bytes.Buffer{}, &st, key, "1.0.0", next, manifest.Capabilities{}, true); err != nil {
		t.Fatalf("the person's own --yes: %v", err)
	}
}

// presetModelPolicy is the default policy with a second saved model, GPT,
// on openai's gpt-5.5.
func presetModelPolicy(t *testing.T) (*policy.Policy, policy.SavedModel) {
	t.Helper()
	pol := policy.Default()
	if err := pol.AddProvider("openai", policy.Provider{APIKeyKind: policy.KeyKindAPI, APIKeyEnv: "OPENAI_API_KEY", ProxyAllow: []string{"api.openai.com:443"}}); err != nil {
		t.Fatal(err)
	}
	gpt, err := pol.AddModel(policy.SavedModel{Name: "GPT", Provider: "openai", Model: "gpt-5.5"})
	if err != nil {
		t.Fatal(err)
	}
	return pol, gpt
}

// A preset's model is the run's when the person names none, found among
// this computer's saved models; locked, a model the person names that is
// not the preset's is refused in the organization's name.
func TestAPresetModelIsTheRunsUnlessThePersonNamesOne(t *testing.T) {
	pol, gpt := presetModelPolicy(t)
	a := managedsettings.Assignment{Ref: "acme/demo", Version: "1.0.0", Mode: managedsettings.AssignRequired,
		Preset: &managedsettings.Preset{Model: &managedsettings.Model{Provider: "openai", Model: "gpt-5.5"}}}
	var warn bytes.Buffer
	opts := runOpts{}
	if err := applyPresetModel(&opts, "acme/demo@1.0.0", heldWith(t, a), pol, &warn); err != nil || opts.model != gpt.ID {
		t.Fatalf("the preset's model was not the run's: %q, %v", opts.model, err)
	}
	own := runOpts{model: pol.DefaultModel().Name}
	if err := applyPresetModel(&own, "acme/demo", heldWith(t, a), pol, &warn); err != nil || own.model != pol.DefaultModel().Name {
		t.Fatalf("the person's --model did not stand: %q, %v", own.model, err)
	}
	other := runOpts{}
	if err := applyPresetModel(&other, "acme/other", heldWith(t, a), pol, &warn); err != nil || other.model != "" {
		t.Fatalf("a release with no preset: %q, %v", other.model, err)
	}
	local := runOpts{local: "."}
	if err := applyPresetModel(&local, "", heldWith(t, a), pol, &warn); err != nil || local.model != "" {
		t.Fatalf("a --local run: %q, %v", local.model, err)
	}

	a.PresetLocked = true
	err := applyPresetModel(&runOpts{model: pol.DefaultModel().Name}, "acme/demo", heldWith(t, a), pol, &warn)
	if err == nil || !strings.Contains(err.Error(), "--model") || !strings.Contains(err.Error(), "acme's managed settings") ||
		!strings.Contains(err.Error(), "openai's gpt-5.5") || !strings.Contains(err.Error(), "(quickagents)") {
		t.Fatalf("another model under a locked preset: %v", err)
	}
	wantClassified(t, err, 11)
	same := runOpts{model: "GPT"}
	if err := applyPresetModel(&same, "acme/demo", heldWith(t, a), pol, &warn); err != nil || same.model != "GPT" {
		t.Fatalf("the preset's own model named under its lock: %q, %v", same.model, err)
	}
	if warn.Len() != 0 {
		t.Fatalf("warned %q", warn.String())
	}
}

// A preset never lifts the managed model rows: a preset model they refuse
// is refused with their sentence, locked or not.
func TestAPresetModelTheModelRowsRefuseIsRefused(t *testing.T) {
	pol, _ := presetModelPolicy(t)
	a := managedsettings.Assignment{Ref: "acme/demo", Version: "1.0.0", Mode: managedsettings.AssignRequired,
		Preset: &managedsettings.Preset{Model: &managedsettings.Model{Provider: "openai", Model: "gpt-5.5"}}}
	held := heldWith(t, a)
	providers, err := json.Marshal([]string{"anthropic"})
	if err != nil {
		t.Fatal(err)
	}
	held.Documents[0].Document.Settings[managedsettings.ModelProviders] = managedsettings.Setting{Value: providers, Locked: true}
	opts := runOpts{}
	err = applyPresetModel(&opts, "acme/demo", held, pol, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "model.providers allows anthropic") || opts.model != "" {
		t.Fatalf("a preset model outside model.providers: %q, %v", opts.model, err)
	}
	wantClassified(t, err, 11)
}

// A preset model no saved model on this computer is refuses a locked
// preset's run, naming the fix, and leaves a default's run on the person's
// own choice with a warning.
func TestAPresetModelWithNoSavedModel(t *testing.T) {
	pol := policy.Default()
	a := managedsettings.Assignment{Ref: "acme/demo", Version: "1.0.0", Mode: managedsettings.AssignRequired,
		Preset: &managedsettings.Preset{Model: &managedsettings.Model{Provider: "openai", Model: "gpt-5.5"}}}
	var warn bytes.Buffer
	opts := runOpts{}
	if err := applyPresetModel(&opts, "acme/demo", heldWith(t, a), pol, &warn); err != nil || opts.model != "" {
		t.Fatalf("a default preset model with no saved model: %q, %v", opts.model, err)
	}
	if !strings.Contains(warn.String(), "acme's managed settings suggest openai's gpt-5.5 for acme/demo") {
		t.Fatalf("warned %q", warn.String())
	}
	a.PresetLocked = true
	err := applyPresetModel(&runOpts{}, "acme/demo", heldWith(t, a), pol, &warn)
	if err == nil || !strings.Contains(err.Error(), "agentry models add") || !strings.Contains(err.Error(), "(quickagents)") {
		t.Fatalf("a locked preset model with no saved model: %v", err)
	}
	wantClassified(t, err, 11)
}
