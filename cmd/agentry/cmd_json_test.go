package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/installed"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// The machine-readable shapes of the commands, each decoded from the
// command's stdout: list, uninstall --plan / --json, validate, setup, and
// the run envelope's exit class and result file.

func TestListJSONMarksMissingDirectories(t *testing.T) {
	isolateEnv(t)
	here := t.TempDir()
	var st installed.State
	st.Upsert(installed.Key{Registry: "https://r.example", Namespace: "acme", Name: "here", Target: "claude-skill", Dir: here}, "1.0.0", installed.CapSnapshot{}, true)
	st.Upsert(installed.Key{Registry: "runner", Namespace: "acme", Name: "gone", Target: "runner", Dir: filepath.Join(here, "no-such-dir")}, "1.0.0", installed.CapSnapshot{}, false)
	if err := installed.Save(st); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runCLI(t, nil, "--json", "list")
	if err != nil {
		t.Fatal(err)
	}
	var out listOutput
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("list is not JSON: %v\n%s", err, stdout)
	}
	if len(out.Records) != 2 || out.Path == "" {
		t.Fatalf("list: %+v", out)
	}
	byName := map[string]bool{}
	for _, r := range out.Records {
		byName[r.Name] = r.Present
	}
	if !byName["here"] || byName["gone"] {
		t.Fatalf("present flags wrong: %+v", byName)
	}
	// The text form marks the missing one and changes nothing.
	text, _, err := runCLI(t, nil, "list")
	if err != nil || !strings.Contains(text, "(directory missing)") {
		t.Fatalf("text list: %v\n%s", err, text)
	}
	after, _ := installed.Load()
	if len(after.Records) != 2 {
		t.Fatal("list changed the state file")
	}
}

func TestUninstallPlanJSONRemovesNothing(t *testing.T) {
	isolateEnv(t)
	home := t.TempDir()
	t.Setenv("AGENTRY_RUNNER_HOME", home)
	inst := filepath.Join(home, "agents", "acme", "demo", "1.0.0-abcdef123456")
	if err := os.MkdirAll(inst, 0o755); err != nil {
		t.Fatal(err)
	}
	var st installed.State
	st.Upsert(installed.Key{Registry: "runner", Namespace: "acme", Name: "demo", Target: "runner", Dir: filepath.Join(home, "agents", "acme", "demo")}, "1.0.0", installed.CapSnapshot{}, false)
	if err := installed.Save(st); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := runCLI(t, nil, "--json", "uninstall", "acme/demo", "--plan")
	if err != nil {
		t.Fatal(err)
	}
	var plan uninstallOutput
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil {
		t.Fatalf("plan is not JSON: %v\n%s", err, stdout)
	}
	if !plan.Plan || len(plan.Installs) != 1 || plan.Installs[0] != inst || !plan.ConsentRecord {
		t.Fatalf("plan: %+v", plan)
	}
	if _, err := os.Stat(inst); err != nil {
		t.Fatal("--plan removed the install")
	}
	if after, _ := installed.Load(); len(after.Records) != 1 {
		t.Fatal("--plan removed the consent record")
	}

	stdout, _, err = runCLI(t, nil, "--json", "uninstall", "acme/demo")
	if err != nil {
		t.Fatal(err)
	}
	var done uninstallOutput
	if err := json.Unmarshal([]byte(stdout), &done); err != nil {
		t.Fatal(err)
	}
	if done.Plan || len(done.Installs) != 1 || !done.ConsentRecord {
		t.Fatalf("result: %+v", done)
	}
	if _, err := os.Stat(inst); !os.IsNotExist(err) {
		t.Fatal("uninstall left the install")
	}
	if after, _ := installed.Load(); len(after.Records) != 0 {
		t.Fatal("uninstall left the consent record")
	}
}

func TestValidateJSONReport(t *testing.T) {
	isolateEnv(t)
	files := skillFiles("demo", "")
	files["helper.md"] = string([]byte{0x7f, 0x45, 0x4c, 0x46, 0x02, 0x01}) // ELF magic behind a .md name
	files["agent.yaml"] = strings.Replace(files["agent.yaml"],
		"include: [agent.yaml, SKILL.md]", "include: [agent.yaml, SKILL.md, helper.md]", 1)
	dir := writeTree(t, files)

	stdout, _, err := runCLI(t, nil, "--json", "validate", dir)
	if err == nil {
		t.Fatal("an ELF in the include set must still fail")
	}
	var rep validateOutput
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("report is not JSON: %v\n%s", err, stdout)
	}
	if rep.Passed || rep.Counts.InventoryFailing != 1 || len(rep.Inventory) != 1 || rep.Inventory[0].Scanner != "S1/binary-executable" {
		t.Fatalf("report: %+v", rep)
	}
	if rep.Manifest.Name != "demo" || rep.Bundle.Version == "" || rep.FileCount != 3 || rep.Counts.Errors < 2 {
		t.Fatalf("report facts: %+v", rep)
	}
	for _, is := range rep.Issues {
		if is.Rule == "" || is.Severity == "" || is.Message == "" {
			t.Fatalf("issue lacks a field: %+v", is)
		}
	}

	// A workspace that passes (V7 wants LICENSE and README.md at the root;
	// V9 wants the frontmatter description to match the summary).
	files = skillFiles("demo", "")
	files["LICENSE"] = "Apache-2.0\n"
	files["README.md"] = "# demo\n"
	files["SKILL.md"] = strings.Replace(files["SKILL.md"], "description: test", "description: test fixture skill", 1)
	clean := writeTree(t, files)
	stdout, _, err = runCLI(t, nil, "--json", "validate", clean)
	if err != nil {
		t.Fatalf("clean workspace failed: %v\n%s", err, stdout)
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil || !rep.Passed || len(rep.Findings) != 0 || len(rep.Issues) != 0 {
		t.Fatalf("clean report: %v %+v", err, rep)
	}
}

func TestSetupJSONNamesTheKeyVariableOnly(t *testing.T) {
	isolateEnv(t)
	t.Setenv("OPENAI_API_KEY", "sk-never-printed")
	t.Setenv("AGENTRY_RUNNER_HOME", t.TempDir())
	policyPath := filepath.Join(t.TempDir(), "policy.yaml")
	stdout, _, err := runCLI(t, nil, "--json", "setup", "--yes", "--provider", "openai", "--skip-images", "--policy", policyPath)
	if err != nil {
		t.Fatal(err)
	}
	var out setupOutput
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("setup is not JSON: %v\n%s", err, stdout)
	}
	if out.Provider != "openai" || out.APIKeyEnv != "OPENAI_API_KEY" || out.Images != "skipped" || out.PolicyPath != policyPath {
		t.Fatalf("setup: %+v", out)
	}
	if strings.Contains(stdout, "sk-never-printed") {
		t.Fatal("the key value reached stdout")
	}
}

func TestRunEnvelopeNamesExitClassAndResultFile(t *testing.T) {
	env := &receipt.Envelope{Status: "error", ExitCode: 13, OutDir: "/tmp/out"}
	b, err := env.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back["exitClass"] != "gateway" || back["resultFile"] != "/tmp/out/result.json" {
		t.Fatalf("envelope: %v", back)
	}
	ok := &receipt.Envelope{Status: "ok", ExitCode: 0}
	b, _ = ok.Marshal()
	if !strings.Contains(string(b), `"exitClass": "ok"`) || strings.Contains(string(b), "resultFile") {
		t.Fatalf("ok envelope: %s", b)
	}
}
