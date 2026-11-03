// The next-steps block: what an install's run line carries for a release
// with questions and files, that it is absent for a coding-agent export,
// and that no command writes it under --json.
package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"
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

	// A release with neither questions nor files says where its task comes
	// from; one with files and no questions does not.
	fr.addAgent("acme", "plain", "autonomous", "plain").addVersion(t, "1.0.0", skillFiles("plain", ""))
	fr.addAgent("acme", "files", "autonomous", "files").addVersion(t, "1.0.0", skillFiles("files", "inputs:\n  kinds: [code]\n"))
	_, stderr, err = runCLI(t, nil, "--registry", fr.url(), "install", "acme/plain@1.0.0", "--allow-unsigned")
	if err != nil {
		t.Fatalf("install plain: %v (stderr %q)", err, stderr)
	}
	if want := "next steps:\n  1. agentry run acme/plain --allow-unsigned\n     the task comes from --input FILE or piped stdin, else it runs on its own instruction\n" +
		"  2. agentry info acme/plain\n     its questions in full, the files it takes, its keys and the publisher's guide\n"; !strings.HasSuffix(stderr, want) {
		t.Errorf("next steps of a release with no questions or files lack\n%s\nin:\n%s", want, stderr)
	}
	_, stderr, err = runCLI(t, nil, "--registry", fr.url(), "install", "acme/files@1.0.0", "--allow-unsigned")
	if err != nil {
		t.Fatalf("install files: %v (stderr %q)", err, stderr)
	}
	if !strings.Contains(stderr, "  1. agentry run acme/files --allow-unsigned --file <path>\n     Files it takes: code; read as text or as uploaded. --file is repeatable.\n  2. agentry info acme/files\n") || strings.Contains(stderr, "the task comes from") {
		t.Errorf("next steps of a release that takes files:\n%s", stderr)
	}

	// A question's prompt is the publisher's text: cleaned for the terminal
	// (the manifest rules refuse control characters in it, but the block does
	// not rely on that), trimmed, and cut to a line.
	long := strings.Repeat("w", 450)
	hostile := &manifest.Manifest{Intake: []manifest.IntakeQuestion{{ID: "ask", Prompt: "  Which \x1b[2J" + long + "  "}}}
	steps := installNextSteps("acme", "prompts", hostile, "--allow-unsigned")
	if len(steps) != 2 || len(steps[0].notes) != 1 {
		t.Fatalf("steps = %+v", steps)
	}
	note := steps[0].notes[0]
	if strings.Contains(note, "\x1b") || !strings.HasPrefix(note, "ask (optional): Which ") || !strings.HasSuffix(note, "w…") || strings.Count(note, "w") != 390 {
		t.Errorf("the question's note = %q (%d w's), want the prompt without control characters, trimmed, cut to %d characters", note, strings.Count(note, "w"), maxInfoLine)
	}
}

// TestNextStepsBlockNamesWhatFollows: the block follows the commands that
// end with one, numbered, with each step's notes under it, and names the next
// command after a sign-in, a setup and a clean validate.
func TestNextStepsBlockNamesWhatFollows(t *testing.T) {
	var buf bytes.Buffer
	printNextSteps(&buf)
	if buf.Len() != 0 {
		t.Errorf("a block with no steps printed %q", buf.String())
	}
	for _, c := range []struct {
		name  string
		steps []nextStep
		want  string
	}{
		{"a sign-in", loginNextSteps(), "next steps:\n" +
			"  1. agentry setup\n     choose the model provider QuickAgents run on, and prepare the container engine\n" +
			"  2. agentry doctor\n     check that this machine is ready to run one\n"},
		{"a setup", setupNextSteps(), "next steps:\n" +
			"  1. agentry doctor\n     check the key variable, the container engine and the registry\n" +
			"  2. agentry search <words>\n     find a QuickAgent to run\n" +
			"  3. agentry run <ns/name>\n     run one; it is installed on first use\n"},
		{"a clean validate", validateNextSteps("./demo"), "next steps:\n" +
			"  1. agentry run --local ./demo --allow-unsigned\n     try it on this machine first\n" +
			"  2. agentry publish ./demo\n     submit it to the registry and follow the pipeline to its verdict\n"},
	} {
		buf.Reset()
		printNextSteps(&buf, c.steps...)
		if buf.String() != c.want {
			t.Errorf("%s:\n%s\nwant:\n%s", c.name, buf.String(), c.want)
		}
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
