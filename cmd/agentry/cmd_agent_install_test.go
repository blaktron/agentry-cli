package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/installed"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
)

// persistentFolder is a persistent QuickAgent's folder with a persona.
func persistentFolder(t *testing.T, ns string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"agent.yaml": "schemaVersion: \"2\"\nname: helper\nnamespace: " + ns + "\nmodes: [persistent]\nversion: 1.0.0\nsummary: Keeps notes\n" +
			"persona:\n  name: Ada\npublish:\n  include: [agent.yaml, AGENT.md, PERSONA.md]\n",
		"AGENT.md":   "You keep notes for the person.\n",
		"PERSONA.md": "Brisk, warm and exact.\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestAgentInstallLocalThenListShowUninstall: an author's folder installs as
// a persistent agent, enabled and keyed by its reference; two publishers'
// agents of one name list side by side; show and list name each by its
// reference with its persona; uninstall keeps its data unless asked; and
// agentry uninstall refuses a persistent agent's release.
func TestAgentInstallLocalThenListShowUninstall(t *testing.T) {
	isolateEnv(t)
	home, policyPath := isolateRunnerHome(t)
	prev := agentDocker
	agentDocker = func() dockerx.Client { return &dockerx.Fake{} }
	t.Cleanup(func() { agentDocker = prev })

	for _, ns := range []string{"alice", "bob"} {
		if _, _, err := runCLI(t, strings.NewReader(""), "--registry", noRegistry, "agent", "install", "--local", persistentFolder(t, ns), "--yes", "--policy", policyPath); err == nil || !strings.Contains(err.Error(), "--allow-unsigned") {
			t.Fatalf("installed an unsigned folder without --allow-unsigned: %v", err)
		}
		stdout, _, err := runCLI(t, strings.NewReader(""), "--registry", noRegistry, "agent", "install", "--local", persistentFolder(t, ns), "--allow-unsigned", "--yes", "--policy", policyPath)
		if err != nil {
			t.Fatalf("agent install --local (%s): %v", ns, err)
		}
		if !strings.Contains(stdout, ns+"/helper@1.0.0 is installed as a persistent agent, enabled. It goes by Ada.") {
			t.Fatalf("install said %q", stdout)
		}
	}
	listed, _, err := runCLI(t, strings.NewReader(""), "--json", "agent", "list")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Agents []agentState `json:"agents"`
	}
	if err := json.Unmarshal([]byte(listed), &list); err != nil {
		t.Fatal(err)
	}
	refs := map[string]agentState{}
	for _, a := range list.Agents {
		refs[a.Ref] = a
	}
	for _, ref := range []string{"agentry/assistant", "alice/helper", "bob/helper"} {
		if _, ok := refs[ref]; !ok {
			t.Errorf("agent list has no %s: %s", ref, listed)
		}
	}
	if a := refs["alice/helper"]; a.Agent != "alice.helper" || a.State != agentStopped || a.Source != persistent.SourceLocal || a.DisplayName != "Ada" {
		t.Errorf("alice/helper listed as %+v", a)
	}
	if _, _, err := runCLI(t, strings.NewReader(""), "agent", "show", "helper"); err == nil || !strings.Contains(err.Error(), "alice/helper") {
		t.Errorf("show helper: %v, want the ambiguity named", err)
	}
	shown, _, err := runCLI(t, strings.NewReader(""), "agent", "show", "bob/helper")
	if err != nil || !strings.Contains(shown, "Ada (bob/helper): stopped") || !strings.Contains(shown, "from a folder on this computer (unsigned)") || !strings.Contains(shown, "persona:    Ada") {
		t.Fatalf("show bob/helper: %q, %v", shown, err)
	}

	if _, _, err := runCLI(t, strings.NewReader(""), "--registry", noRegistry, "install", "--local", persistentFolder(t, "alice"), "--allow-unsigned", "--yes"); err == nil || !strings.Contains(err.Error(), "agentry agent install") {
		t.Errorf("a plain install over a persistent agent's release: %v", err)
	}
	if _, _, err := runCLI(t, strings.NewReader(""), "uninstall", "alice/helper"); err == nil || !strings.Contains(err.Error(), "agentry agent uninstall") {
		t.Errorf("agentry uninstall of a persistent agent's release: %v", err)
	}
	if _, _, err := runCLI(t, strings.NewReader(""), "agent", "uninstall", "assistant"); err == nil {
		t.Error("a built-in was uninstalled")
	}
	if err := persistent.SaveConfig(home, "alice.helper", persistent.Config{Version: persistent.ConfigVersion, Enabled: true, DisplayName: "Mine"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, strings.NewReader(""), "agent", "uninstall", "alice/helper"); err != nil {
		t.Fatalf("agent uninstall: %v", err)
	}
	if c, _ := persistent.LoadConfig(home, "alice.helper"); c.DisplayName != "Mine" {
		t.Errorf("uninstall without --remove-data dropped the agent's configuration: %+v", c)
	}
	if st, _ := installed.Load(); st.Find(installdir.ConsentKey(home, "alice", "helper")) != nil {
		t.Error("uninstall kept the consent record")
	}
	if _, err := os.Stat(installdir.AgentDir(home, "alice", "helper")); !os.IsNotExist(err) {
		t.Errorf("uninstall kept the release: %v", err)
	}
	if _, _, err := runCLI(t, strings.NewReader(""), "agent", "uninstall", "bob/helper", "--remove-data"); err == nil {
		t.Error("--remove-data went on with no terminal and no --confirm")
	}
	if _, _, err := runCLI(t, strings.NewReader(""), "agent", "uninstall", "bob/helper", "--remove-data", "--confirm", "bob/helper"); err != nil {
		t.Fatalf("agent uninstall --remove-data: %v", err)
	}
	if _, err := os.Stat(persistent.Dir(home, "bob.helper")); !os.IsNotExist(err) {
		t.Errorf("--remove-data kept the agent's folder: %v", err)
	}
}

// TestAgentInstallRefusesWhatIsNotAPersistentAgentHere: an autonomous
// QuickAgent, a persistent one that declares filesystem access beside its
// folders, and any persistent one under a policy that allows none, are
// refused before anything is installed.
func TestAgentInstallRefusesWhatIsNotAPersistentAgentHere(t *testing.T) {
	isolateEnv(t)
	home, policyPath := isolateRunnerHome(t)
	if _, _, err := runCLI(t, strings.NewReader(""), "--registry", noRegistry, "agent", "install", "--local", localFolder(t), "--allow-unsigned", "--yes", "--policy", policyPath); err == nil || !strings.Contains(err.Error(), "not a persistent QuickAgent") {
		t.Errorf("an autonomous QuickAgent: %v", err)
	}
	reads := persistentFolder(t, "alice")
	manifestPath := filepath.Join(reads, "agent.yaml")
	text, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte(strings.Replace(string(text), "persona:", "capabilities:\n  filesystem:\n    read: workspace\n    write: none\npersona:", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, strings.NewReader(""), "--registry", noRegistry, "agent", "install", "--local", reads, "--allow-unsigned", "--yes", "--policy", policyPath); err == nil || !strings.Contains(err.Error(), "declares filesystem access") {
		t.Errorf("a persistent QuickAgent that declares filesystem access: %v", err)
	}
	raw, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policyPath, []byte(strings.Replace(string(raw), "allowWildcardEgress: false", "allowWildcardEgress: false\nallowPersistentReleases: false", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = runCLI(t, strings.NewReader(""), "--registry", noRegistry, "agent", "install", "--local", persistentFolder(t, "alice"), "--allow-unsigned", "--yes", "--policy", policyPath)
	if err == nil || !strings.Contains(err.Error(), "allowPersistentReleases: false") {
		t.Fatalf("under a policy that allows none: %v", err)
	}
	wantClassified(t, err, engine.ExitPolicy)
	if agents := persistent.AgentsIn(home); len(agents) != len(persistent.Agents()) {
		t.Errorf("a refused install left an agent: %v", agents)
	}
}

// persistentRelease is a persistent release's files at version, with a
// locked persona, two persistent keys and two subagents: acme/digest, which
// resolves, and acme/gone, which is not in the registry.
func persistentRelease(version, extraKeys string) map[string]string {
	return map[string]string{
		"agent.yaml": "schemaVersion: \"2\"\nname: helper\nnamespace: acme\nmodes: [persistent]\nversion: " + version + "\nsummary: Keeps notes\n" +
			"capabilities:\n  persistent:\n    clock: true\n    memory: true\n" + extraKeys +
			"persona:\n  name: Ada\n  editable: false\n" +
			"dependencies:\n  agents:\n" +
			"    - ref: acme/digest\n      version: 1.0.0\n      why: Summarises the notes you share\n" +
			"    - ref: acme/gone\n      version: ^1.0.0\n      why: Fetches pages\n" +
			"publish:\n  include: [agent.yaml, AGENT.md, PERSONA.md]\n",
		"AGENT.md":   "You keep notes for the person.\n",
		"PERSONA.md": "Brisk, warm and exact.\n",
	}
}

// TestAgentInstallPlanPreviewsThePinsAndWritesNothing: agent install --plan
// --json answers the consent facts of a persistent release (its persistent
// keys, its locked persona, each subagent pinned with its version, reach and
// digest, or why it resolves to none) and installs, records and pins
// nothing; after an install, the plan of a newer version names the
// installed release and what the update widens.
func TestAgentInstallPlanPreviewsThePinsAndWritesNothing(t *testing.T) {
	isolateEnv(t)
	home, policyPath := isolateRunnerHome(t)
	prev := agentDocker
	agentDocker = func() dockerx.Client { return &dockerx.Fake{} }
	t.Cleanup(func() { agentDocker = prev })
	fr := newFakeRegistry(t)
	fr.enableSigning(t)
	fa := fr.addAgent("acme", "helper", "persistent", "Keeps notes")
	fa.addVersion(t, "1.0.0", persistentRelease("1.0.0", "")).attMode = attSigned
	digest := fr.addAgent("acme", "digest", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("digest", "capabilities:\n  network:\n    egress: [api.example.com]\n"))
	digest.attMode = attSigned

	plan := func(ref string) installPlan {
		t.Helper()
		stdout, _, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "--json", "agent", "install", ref, "--plan", "--policy", policyPath)
		if err != nil {
			t.Fatalf("agent install --plan %s: %v", ref, err)
		}
		var p installPlan
		if err := json.Unmarshal([]byte(stdout), &p); err != nil {
			t.Fatalf("plan is not JSON: %v\n%s", err, stdout)
		}
		if p.Agent == nil {
			t.Fatalf("the plan has no agent block: %s", stdout)
		}
		return p
	}
	p := plan("acme/helper@1.0.0")
	a := p.Agent
	if p.Ref != "acme/helper@1.0.0" || p.Refusal != "" || a.Agent != "acme.helper" || !a.RunsSubagents || a.Installed != nil || p.Installed != nil {
		t.Fatalf("plan: %+v, agent %+v", p, a)
	}
	if strings.Join(a.Persistent, ",") != "clock,memory" {
		t.Errorf("persistent keys = %v", a.Persistent)
	}
	if a.Persona == nil || a.Persona.Name != "Ada" || a.Persona.File != "PERSONA.md" || !a.Persona.Locked {
		t.Errorf("persona = %+v", a.Persona)
	}
	if len(a.Subagents) != 2 {
		t.Fatalf("subagents = %+v", a.Subagents)
	}
	if s := a.Subagents[0]; s.Ref != "acme/digest" || s.Version != "1.0.0" || s.Digest != digest.digest || !strings.Contains(s.Reach, "network egress") || s.Why != "Summarises the notes you share" || s.Unresolved != "" {
		t.Errorf("acme/digest pinned as %+v", s)
	}
	if s := a.Subagents[1]; s.Ref != "acme/gone" || s.Version != "" || s.Digest != "" || s.Unresolved == "" {
		t.Errorf("acme/gone pinned as %+v", s)
	}
	if !strings.Contains(string(p.Contract), `"clock":true`) {
		t.Errorf("contract lacks the persistent block: %s", p.Contract)
	}
	if _, err := persistent.LoadRelease(home, "acme.helper"); err == nil {
		t.Error("--plan recorded the agent")
	}
	if st, _ := installed.Load(); len(st.Records) != 0 {
		t.Errorf("--plan wrote the install state: %+v", st.Records)
	}
	if _, err := os.Stat(installdir.AgentDir(home, "acme", "helper")); !os.IsNotExist(err) {
		t.Errorf("--plan laid the release down: %v", err)
	}
	if agents := persistent.AgentsIn(home); len(agents) != len(persistent.Agents()) {
		t.Errorf("--plan left an agent: %v", agents)
	}

	if _, _, err := runCLI(t, strings.NewReader(""), "--registry", fr.url(), "agent", "install", "acme/helper@1.0.0", "--yes", "--policy", policyPath); err != nil {
		t.Fatalf("agent install: %v", err)
	}
	fa.addVersion(t, "1.1.0", persistentRelease("1.1.0", "    skills: true\n")).attMode = attSigned
	p = plan("acme/helper@1.1.0")
	if in := p.Agent.Installed; in == nil || in.Version != "1.0.0" || in.Source != persistent.SourceRegistry || len(in.Subagents) != 2 || in.Subagents[0].Digest != digest.digest {
		t.Fatalf("the update's plan names the installed release as %+v", p.Agent.Installed)
	}
	if p.Installed == nil || p.Installed.Version != "1.0.0" || !p.NeedsConfirmation || !strings.Contains(strings.Join(p.Diff.Widened, "; "), "skills") {
		t.Errorf("the update's widening: installed %+v, diff %+v, asks %t", p.Installed, p.Diff, p.NeedsConfirmation)
	}
	if r, err := persistent.LoadRelease(home, "acme.helper"); err != nil || r.Version != "1.0.0" {
		t.Errorf("the update's plan moved the agent: %+v, %v", r, err)
	}
}

// TestAgentInstallPlanOfAFolderRunsNoSubagents: the plan of a folder
// installed with --local says it runs none of its subagents and installs
// nothing, without --allow-unsigned, whose absence is its refusal.
func TestAgentInstallPlanOfAFolderRunsNoSubagents(t *testing.T) {
	isolateEnv(t)
	home, policyPath := isolateRunnerHome(t)
	dir := t.TempDir()
	for name, body := range persistentRelease("1.0.0", "") {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stdout, _, err := runCLI(t, strings.NewReader(""), "--registry", noRegistry, "--json", "agent", "install", "--local", dir, "--plan", "--policy", policyPath)
	if err != nil {
		t.Fatalf("agent install --local --plan: %v", err)
	}
	var p installPlan
	if err := json.Unmarshal([]byte(stdout), &p); err != nil {
		t.Fatalf("plan is not JSON: %v\n%s", err, stdout)
	}
	if a := p.Agent; a == nil || a.RunsSubagents || len(a.Subagents) != 2 || a.Subagents[0].Version != "" || !strings.Contains(a.Subagents[0].Unresolved, "runs none") {
		t.Fatalf("agent block: %+v", p.Agent)
	}
	if !strings.Contains(p.Refusal, "--allow-unsigned") {
		t.Errorf("refusal = %q", p.Refusal)
	}
	if _, err := persistent.LoadRelease(home, "acme.helper"); err == nil {
		t.Error("--plan recorded the agent")
	}
}
