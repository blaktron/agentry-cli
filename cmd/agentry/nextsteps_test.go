// The next-steps block: what an install's run line carries for a release
// with questions and files, that it is absent for a coding-agent export,
// and that no command writes it under --json.
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestInstallNextStepsNameTheQuestionsAndFiles: a release that asks
// questions and declares its inputs is installed, and the run line carries
// --intake for each question but a secret, which the run asks for on the
// terminal, and --file, with a note per question in the publisher's words.
func TestInstallNextStepsNameTheQuestionsAndFiles(t *testing.T) {
	isolateEnv(t)
	isolateRunnerHome(t)
	fr := newFakeRegistry(t)
	caps := `intake:
  - id: spec
    prompt: The spec
    type: url
    required: true
  - id: depth
    prompt: How deep
    type: choice
    choices: [quick, full]
  - id: access
    prompt: The access token
    type: secret
inputs:
  kinds: [code]
`
	fr.addAgent("acme", "audit", "autonomous", "audits").addVersion(t, "1.0.0", skillFiles("audit", caps))

	_, stderr, err := runCLI(t, nil, "--registry", fr.url(), "install", "acme/audit@1.0.0", "--allow-unsigned")
	if err != nil {
		t.Fatalf("install: %v (stderr %q)", err, stderr)
	}
	for _, want := range []string{
		"next steps:\n  1. agentry run acme/audit --allow-unsigned --intake spec=<value> --intake depth=<value> --file <path>\n",
		"     spec (required): The spec\n",
		`     depth (optional): How deep; one of "quick", "full"` + "\n",
		"     access (optional, a secret the run asks for on the terminal): The access token\n",
		"     Files it takes: code; read as text or as uploaded. --file is repeatable.\n",
		"  2. agentry info acme/audit\n",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("next steps lack %q:\n%s", want, stderr)
		}
	}
	// A coding-agent export has no run line.
	_, stderr, err = runCLI(t, nil, "--registry", fr.url(), "install", "acme/audit@1.0.0", "--allow-unsigned", "--target", "claude-skill", "--dir", t.TempDir())
	if err != nil {
		t.Fatalf("install --target claude-skill: %v (stderr %q)", err, stderr)
	}
	if strings.Contains(stderr, "next steps:") {
		t.Errorf("a claude-skill install printed next steps:\n%s", stderr)
	}
}

// TestNextStepsAreSilentUnderJSON: with --json stdout is one JSON document
// and the block is not written anywhere.
func TestNextStepsAreSilentUnderJSON(t *testing.T) {
	isolateEnv(t)
	isolateRunnerHome(t)
	fr := newFakeRegistry(t)
	fr.enableSigning(t)
	fr.addAgent("acme", "demo", "autonomous", "reads things").addVersion(t, "1.0.0", skillFiles("demo", ""))
	dir := writeTree(t, publishableSkillFiles("demo", ""))
	for name, args := range map[string][]string{
		"install":  {"--registry", fr.url(), "--json", "install", "acme/demo@1.0.0", "--allow-unsigned"},
		"validate": {"--json", "validate", dir},
		"setup":    {"--json", "setup", "--skip-images", "--policy", t.TempDir() + "/policy.yaml", "--yes", "--provider", "openai"},
	} {
		stdout, stderr, err := runCLI(t, strings.NewReader(""), args...)
		if err != nil {
			t.Fatalf("%s --json: %v (stderr %q)", name, err, stderr)
		}
		var doc map[string]any
		if jerr := json.Unmarshal([]byte(stdout), &doc); jerr != nil {
			t.Errorf("%s --json: stdout is not one JSON document (%v):\n%s", name, jerr, stdout)
		}
		if strings.Contains(stdout+stderr, "next steps:") {
			t.Errorf("%s --json wrote next steps:\nstdout %q\nstderr %q", name, stdout, stderr)
		}
	}
}
