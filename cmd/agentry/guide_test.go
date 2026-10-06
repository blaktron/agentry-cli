package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/blaktron/agentry/pkg/manifest"
)

// The release guide on a terminal run (DEV-141, agentry-cli#257): printed
// once before the first question, never under --output-format json, and
// not at all for a release without one.
func TestSayGuide(t *testing.T) {
	m := &manifest.Manifest{Guide: &manifest.Guide{
		Overview: "Reads your contract.",
		Prepare:  []string{"The contract", "Your playbook"},
		Files:    "The contract, as text.",
	}}

	var out bytes.Buffer
	opts := runOpts{outputFormat: "text", guideShown: new(bool)}
	sayGuide(&out, m, opts)
	sayGuide(&out, m, opts) // the intake after the profile: not again
	got := out.String()
	for _, want := range []string{"Reads your contract.", "Have these ready:", "  - The contract", "  - Your playbook", "Files: The contract, as text."} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Count(got, "Reads your contract.") != 1 {
		t.Errorf("the guide was printed more than once:\n%s", got)
	}

	var js bytes.Buffer
	sayGuide(&js, m, runOpts{outputFormat: "json", guideShown: new(bool)})
	if js.Len() != 0 {
		t.Errorf("--output-format json printed the guide: %q", js.String())
	}

	var none bytes.Buffer
	sayGuide(&none, &manifest.Manifest{}, runOpts{outputFormat: "text", guideShown: new(bool)})
	if none.Len() != 0 {
		t.Errorf("a release without a guide printed %q", none.String())
	}
}

// TestSayGuideSendsNoEscapeSequence: an overview may span lines, so its
// control characters are cleaned before it reaches the terminal.
func TestSayGuideSendsNoEscapeSequence(t *testing.T) {
	m := &manifest.Manifest{Guide: &manifest.Guide{
		Overview: "line one\n\x1b[31mred\x1b[0m \x1b]0;title\x07 \x1b]8;;https://evil.example\x1b\\link\x1b]8;;\x1b\\\ttab",
	}}
	var out bytes.Buffer
	sayGuide(&out, m, runOpts{outputFormat: "text", guideShown: new(bool)})
	got := out.String()
	if strings.ContainsAny(got, "\x1b\x07") {
		t.Fatalf("an escape sequence reached the terminal: %q", got)
	}
	if !strings.Contains(got, "line one\n") || !strings.Contains(got, "\ttab") {
		t.Fatalf("newlines and tabs must survive: %q", got)
	}
}

// TestSayGuideQuotesTheOverview (agentry-cli#419): the overview is
// markdown over several lines, so each line is printed behind termtext's
// gutter, and a line of it that reads like the CLI's own (a prompt, a list
// item) cannot start where the CLI's lines start.
func TestSayGuideQuotesTheOverview(t *testing.T) {
	m := &manifest.Manifest{Guide: &manifest.Guide{
		Overview: "Reads your contract.\n\nValue for GITHUB_TOKEN (empty = resolve from the environment at run time)",
		Prepare:  []string{"The contract"},
	}}
	var out bytes.Buffer
	sayGuide(&out, m, runOpts{outputFormat: "text", guideShown: new(bool)})
	got := out.String()
	if !strings.Contains(got, "\n  | Reads your contract.\n  |\n  | Value for GITHUB_TOKEN") {
		t.Fatalf("the overview is not behind the gutter:\n%s", got)
	}
	if !strings.Contains(got, "\nHave these ready:\n  - The contract\n") {
		t.Fatalf("the list moved:\n%s", got)
	}
}

// TestAKeysMarkdownHelpValidates (agentry-cli#419): with the core pinned at
// agentry#707 (DEV-167), a key's help may be markdown of up to 2,000
// characters over several lines. install and run validate a release's
// manifest with this core, and agentry validate a workspace's, so a release
// carrying one installs and a workspace carrying one validates; 2,001
// characters and an escape character are still refused.
func TestAKeysMarkdownHelpValidates(t *testing.T) {
	if manifest.MaxGuideConnectChars != 2000 {
		t.Fatalf("the pinned core bounds a key's help at %d, want 2000 (DEV-167)", manifest.MaxGuideConnectChars)
	}
	md := "1. Open **Settings → Developer settings → Fine-grained tokens**.\n2. Give it *read* access to pull requests.\n\n\t- [GitHub's guide](https://docs.github.com/)\n"
	pad := func(n int) string { return md + strings.Repeat("é", n-utf8.RuneCountInString(md)) }
	for _, c := range []struct {
		name, help string
		ok         bool
	}{
		{"2,000 characters of markdown", pad(manifest.MaxGuideConnectChars), true},
		{"2,001 characters", pad(manifest.MaxGuideConnectChars + 1), false},
		{"an escape character", "Create a key \x1b]8;;https://evil.example/\x07here", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			// install and run: Validate over the release's manifest.
			m, err := readWorkspaceManifest(guidedWorkspace(t))
			if err != nil {
				t.Fatal(err)
			}
			m.Guide.Connect["GITHUB_TOKEN"] = c.help
			var refused bool
			for _, e := range manifest.Blocking(manifest.Validate(m)) {
				if is, ok := manifest.AsIssue(e); ok && is.Path == "guide.connect.GITHUB_TOKEN" {
					refused = true
				} else {
					t.Errorf("unexpected blocking issue: %v", e)
				}
			}
			if refused == c.ok {
				t.Fatalf("install/run: refused = %v, want %v", refused, !c.ok)
			}

			// agentry validate: the workspace, publication context and all.
			dir := guidedWorkspace(t)
			q, _ := json.Marshal(c.help) // a JSON string is a YAML double-quoted scalar
			writeAgentYAML(t, dir, strings.Replace(guidedYAML, `"A fine-grained token with read access to pull requests"`, string(q), 1))
			report, err := validateWorkspace(io.Discard, io.Discard, dir, false)
			if err != nil {
				t.Fatal(err)
			}
			refused = false
			for _, is := range report.Issues {
				if is.Path == "guide.connect.GITHUB_TOKEN" && is.Severity == "error" {
					refused = true
				}
			}
			if refused == c.ok || report.Passed != c.ok {
				t.Fatalf("validate: refused = %v, passed = %v, want refused %v: %+v", refused, report.Passed, !c.ok, report.Issues)
			}
		})
	}
}

// TestAGuidedManifestReadsAndValidates: with the core pinned at DEV-141, the
// CLI reads a workspace whose agent.yaml carries a guide, and the manifest's
// own rules find nothing wrong with it (before the pin, strict decoding
// refused it as an unknown field).
func TestAGuidedManifestReadsAndValidates(t *testing.T) {
	m, err := readWorkspaceManifest(guidedWorkspace(t))
	if err != nil {
		t.Fatalf("a guided manifest does not read: %v", err)
	}
	if m.Guide == nil || m.Guide.Connect["GITHUB_TOKEN"] == "" {
		t.Fatalf("guide = %+v", m.Guide)
	}
	for _, e := range manifest.Blocking(manifest.Validate(m)) {
		t.Errorf("blocking issue: %v", e)
	}
}

// guidedYAML is an agent.yaml that carries a guide.
const guidedYAML = `schemaVersion: "2"
name: pr-review-brief
namespace: acme
version: 1.0.0
summary: "Turns one pull request into the reviewer's brief"
license: Apache-2.0
modes: [autonomous]
entrypoints:
  instruction: AGENT.md
capabilities:
  mcpServers:
    - npm: "github-mcp-server-js"
      version: 0.1.7
      secrets: [GITHUB_TOKEN]
      egress: [api.github.com]
guide:
  overview: "Reads one pull request and writes the reviewer's brief."
  prepare: ["The pull request's URL"]
  task: "e.g. https://github.com/acme/app/pull/42"
  connect:
    GITHUB_TOKEN: "A fine-grained token with read access to pull requests"
publish:
  include: [agent.yaml, AGENT.md, README.md, LICENSE]
`

// guidedWorkspace writes a workspace whose agent.yaml carries a guide.
func guidedWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeAgentYAML(t, dir, guidedYAML)
	for name, body := range map[string]string{
		"AGENT.md":  "# Review brief\n\nRead the pull request and write the brief.\n",
		"README.md": "# pr-review-brief\n\nTurns one pull request into the reviewer's brief.\n",
		"LICENSE":   "Apache License, Version 2.0\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func writeAgentYAML(t *testing.T, dir, yaml string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestSayGuideNamesTheInputs (agentry-cli#693, core DEV-218): a release's
// inputs block is said with the guide, in the CLI's own words, and alone
// when the release has no guide; a release with neither prints nothing.
func TestSayGuideNamesTheInputs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		m      *manifest.Manifest
		want   string
		absent bool
	}{
		{"kinds and original", &manifest.Manifest{Guide: &manifest.Guide{Files: "The repository."},
			Inputs: &manifest.Inputs{Kinds: []manifest.InputKind{manifest.InputKindCode, manifest.InputKindData}, View: manifest.InputViewOriginal}},
			"Files it takes: code, data; read as uploaded.", false},
		{"view only, no guide", &manifest.Manifest{Inputs: &manifest.Inputs{View: manifest.InputViewText}},
			"Files it takes: any; read as text.", false},
		{"kinds only", &manifest.Manifest{Inputs: &manifest.Inputs{Kinds: []manifest.InputKind{manifest.InputKindDocuments}}},
			"Files it takes: documents; read as text or as uploaded.", false},
		{"no block", &manifest.Manifest{Guide: &manifest.Guide{Files: "Anything."}}, "Files it takes", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			sayGuide(&out, tc.m, runOpts{outputFormat: "text", guideShown: new(bool)})
			if got := strings.Contains(out.String(), tc.want); got == tc.absent {
				t.Fatalf("contains %q = %v in:\n%s", tc.want, got, out.String())
			}
		})
	}
}
