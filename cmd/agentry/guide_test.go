package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// TestAGuidedManifestReadsAndValidates: with the core pinned at DEV-141, the
// CLI reads a workspace whose agent.yaml carries a guide, and the manifest's
// own rules find nothing wrong with it (before the pin, strict decoding
// refused it as an unknown field).
func TestAGuidedManifestReadsAndValidates(t *testing.T) {
	dir := t.TempDir()
	yaml := `schemaVersion: "2"
name: pr-review-brief
namespace: acme
version: 1.0.0
summary: "Turns one pull request into the reviewer's brief"
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
  include: [agent.yaml, AGENT.md]
`
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := readWorkspaceManifest(dir)
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
