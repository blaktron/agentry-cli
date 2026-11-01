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
// QuickAgent, and any persistent one under a policy that allows none, are
// refused before anything is installed.
func TestAgentInstallRefusesWhatIsNotAPersistentAgentHere(t *testing.T) {
	isolateEnv(t)
	home, policyPath := isolateRunnerHome(t)
	if _, _, err := runCLI(t, strings.NewReader(""), "--registry", noRegistry, "agent", "install", "--local", localFolder(t), "--allow-unsigned", "--yes", "--policy", policyPath); err == nil || !strings.Contains(err.Error(), "not a persistent QuickAgent") {
		t.Errorf("an autonomous QuickAgent: %v", err)
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
