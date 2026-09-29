// `agentry init --from` (agentry#71): imports populate the manifest from
// what the source states and never guess the capability contract — the
// non-interactive path refuses without explicit answers, the wizard asks
// with hints as defaults, and every result validates under the real
// toolchain.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"
)

// scriptPrompter answers Ask in order ("" keeps the default).
type scriptPrompter struct {
	answers []string
	asked   []string
	said    []string
}

func (p *scriptPrompter) Ask(prompt, def string) (string, error) {
	p.asked = append(p.asked, prompt+" ["+def+"]")
	if len(p.answers) == 0 {
		return "", nil
	}
	a := p.answers[0]
	p.answers = p.answers[1:]
	return a, nil
}
func (p *scriptPrompter) AskSecret(string) (string, error)       { return "", nil }
func (p *scriptPrompter) Confirm(_ string, d bool) (bool, error) { return d, nil }
func (p *scriptPrompter) Say(t string)                           { p.said = append(p.said, t) }

func writeSkillSource(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "issue-triage")
	if err := os.MkdirAll(filepath.Join(src, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"SKILL.md":          "---\nname: issue-triage\ndescription: Triage new GitHub issues by label and severity, then post a summary comment.\nlicense: MIT\nallowed-tools: [Bash, Read]\nmetadata:\n  version: 2.1.0\n---\n\n# Issue triage\n\nCall https://api.github.com/repos/{owner}/{repo}/issues with GITHUB_TOKEN.\nRun `gh` to comment.\n",
		"scripts/triage.sh": "#!/bin/sh\necho triage\n",
		"README.md":         "# issue-triage (upstream readme)\n",
	}
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(src, n), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return src
}

// Non-interactive without answers: refuse, naming the flags and the hints.
func TestInitFromRefusesToGuess(t *testing.T) {
	isolateEnv(t)
	src := writeSkillSource(t)
	dir := filepath.Join(t.TempDir(), "out")
	_, stderr, err := runCLI(t, strings.NewReader(""), "init", dir, "--from", src, "--namespace", "acme")
	if err == nil || !strings.Contains(err.Error(), "--egress and --secrets") {
		t.Fatalf("expected a refusal naming the flags: %v", err)
	}
	if !strings.Contains(stderr, "api.github.com") || !strings.Contains(stderr, "GITHUB_TOKEN") || !strings.Contains(stderr, "commands mentioned: gh") {
		t.Errorf("hints not shown:\n%s", stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "agent.yaml")); err == nil {
		t.Fatal("refusal must write nothing")
	}
}

// Explicit answers: the source's frontmatter and files seed the workspace,
// the answers become the contract, and the result validates.
func TestInitFromSkillWithAnswers(t *testing.T) {
	isolateEnv(t)
	src := writeSkillSource(t)
	dir := filepath.Join(t.TempDir(), "out")
	stdout, stderr, err := runCLI(t, strings.NewReader(""), "init", dir, "--from", src, "--namespace", "acme",
		"--egress", "api.github.com", "--secrets", "GITHUB_TOKEN", "--exec", "gh", "--fs-read", "workspace")
	if err != nil {
		t.Fatalf("init --from: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "imported QuickAgent acme/issue-triage") {
		t.Errorf("banner:\n%s", stdout)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "agent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse(raw)
	if err != nil {
		t.Fatalf("imported manifest does not parse: %v\n%s", err, raw)
	}
	if m.Name != "issue-triage" || m.Version != "2.1.0" || m.License != "MIT" || !strings.HasPrefix(m.Summary, "Triage new GitHub issues") {
		t.Errorf("mapped fields: name=%s version=%s license=%s summary=%q", m.Name, m.Version, m.License, m.Summary)
	}
	if len(m.Capabilities.Secrets) != 1 || m.Capabilities.Secrets[0] != "GITHUB_TOKEN" {
		t.Errorf("secrets = %v", m.Capabilities.Secrets)
	}
	if !strings.Contains(string(raw), "egress: [api.github.com]") || !strings.Contains(string(raw), "exec: { commands: [gh] }") || !strings.Contains(string(raw), "read: workspace") {
		t.Errorf("contract not rendered from answers:\n%s", raw)
	}
	for _, f := range []string{"SKILL.md", "scripts/triage.sh", "README.md", "LICENSE"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err != nil {
			t.Errorf("workspace lacks %s", f)
		}
	}
	readme, _ := os.ReadFile(filepath.Join(dir, "README.md"))
	if !strings.Contains(string(readme), "upstream readme") {
		t.Error("the source README must be imported, not replaced")
	}
	if !strings.Contains(string(raw), "scripts/**") {
		t.Errorf("publish.include must carry the imported directory:\n%s", raw)
	}
	vout, _, err := runCLI(t, nil, "validate", dir)
	if err != nil || !strings.Contains(vout, "validate: PASSED") {
		t.Errorf("imported workspace does not validate: %v\n%s", err, vout)
	}
}

// The wizard asks every question with the source's hints as defaults; an
// empty answer confirms the hint, a word replaces it.
func TestAskCapabilitiesUsesHintsAsDefaults(t *testing.T) {
	imp, err := readSkill(filepath.Join(writeSkillSource(t), "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	p := &scriptPrompter{answers: []string{"", "none", "", "workspace", ""}}
	a, err := askCapabilities(p, imp)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.asked) != 5 {
		t.Fatalf("asked %d questions: %v", len(p.asked), p.asked)
	}
	if !strings.Contains(p.asked[0], "[api.github.com]") || !strings.Contains(p.asked[1], "[GITHUB_TOKEN]") || !strings.Contains(p.asked[2], "[gh]") {
		t.Errorf("hints must be the defaults: %v", p.asked)
	}
	if len(a.Egress) != 1 || a.Egress[0] != "api.github.com" || a.Secrets != nil || len(a.Exec) != 1 || a.FSRead != "workspace" || a.FSWrite != "none" {
		t.Errorf("answers = %+v", a)
	}
}

// server.json imports as a skill declaring the package under mcpServers:
// exact pin, secret names from the secret-flagged env vars, egress from the
// answer scoped to the server; a version range or a non-npm/PyPI package
// is refused.
func TestInitFromServerJSON(t *testing.T) {
	isolateEnv(t)
	srcDir := t.TempDir()
	sj := `{
  "$schema": "https://static.modelcontextprotocol.io/schemas/2025-12-11/server.schema.json",
  "name": "io.github.example/github-issues",
  "description": "Read and comment on GitHub issues.",
  "version": "1.4.0",
  "packages": [{
    "registryType": "npm", "identifier": "@example/mcp-github-issues", "version": "1.4.0",
    "transport": {"type": "stdio"},
    "environmentVariables": [
      {"name": "GITHUB_PAT", "isSecret": true, "isRequired": true},
      {"name": "LOG_LEVEL", "isSecret": false}
    ]
  }]
}`
	if err := os.WriteFile(filepath.Join(srcDir, "server.json"), []byte(sj), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "out")
	stdout, stderr, err := runCLI(t, strings.NewReader(""), "init", dir, "--from", srcDir, "--namespace", "acme",
		"--egress", "api.github.com", "--secrets", "none")
	if err != nil {
		t.Fatalf("init --from server.json: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "imported QuickAgent acme/mcp-github-issues") {
		t.Errorf("banner:\n%s", stdout)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "agent.yaml"))
	m, err := manifest.Parse(raw)
	if err != nil {
		t.Fatalf("does not parse: %v\n%s", err, raw)
	}
	if len(m.Capabilities.MCPServers) != 1 {
		t.Fatalf("mcpServers = %+v", m.Capabilities.MCPServers)
	}
	srv := m.Capabilities.MCPServers[0]
	if srv.NPM != "@example/mcp-github-issues" || srv.Version != "1.4.0" || len(srv.Secrets) != 1 || srv.Secrets[0] != "GITHUB_PAT" || len(srv.Egress) != 1 || srv.Egress[0] != "api.github.com" {
		t.Errorf("server = %+v", srv)
	}
	if len(m.Capabilities.Secrets) != 0 || m.Version != "1.4.0" {
		t.Errorf("agent-level secrets=%v version=%s", m.Capabilities.Secrets, m.Version)
	}
	vout, _, err := runCLI(t, nil, "validate", dir)
	if err != nil || !strings.Contains(vout, "validate: PASSED") {
		t.Errorf("imported workspace does not validate: %v\n%s", err, vout)
	}

	// Refusals: a range, and nothing installable.
	bad := filepath.Join(t.TempDir(), "server.json")
	_ = os.WriteFile(bad, []byte(strings.Replace(sj, `"version": "1.4.0",
    "transport"`, `"version": "^1.4.0",
    "transport"`, 1)), 0o644)
	if _, _, err := runCLI(t, strings.NewReader(""), "init", filepath.Join(t.TempDir(), "o"), "--from", bad, "--namespace", "acme", "--egress", "none", "--secrets", "none"); err == nil || !strings.Contains(err.Error(), "exact version") {
		t.Errorf("range must be refused: %v", err)
	}
	_ = os.WriteFile(bad, []byte(strings.Replace(sj, `"registryType": "npm"`, `"registryType": "oci"`, 1)), 0o644)
	if _, _, err := runCLI(t, strings.NewReader(""), "init", filepath.Join(t.TempDir(), "o"), "--from", bad, "--namespace", "acme", "--egress", "none", "--secrets", "none"); err == nil || !strings.Contains(err.Error(), "no npm or PyPI package") {
		t.Errorf("oci-only must be refused: %v", err)
	}
}

// Guards: the withdrawn --kind, an unrecognized source, an existing agent.yaml.
func TestInitFromGuards(t *testing.T) {
	isolateEnv(t)
	src := writeSkillSource(t)
	out := filepath.Join(t.TempDir(), "o")
	if _, _, err := runCLI(t, strings.NewReader(""), "init", out, "--from", src, "--kind", "skill", "--namespace", "acme", "--egress", "none", "--secrets", "none"); err == nil || !strings.Contains(err.Error(), "--modes") {
		t.Errorf("withdrawn --kind: %v", err)
	}
	if _, _, err := runCLI(t, strings.NewReader(""), "init", out, "--from", t.TempDir(), "--namespace", "acme", "--egress", "none", "--secrets", "none"); err == nil || !strings.Contains(err.Error(), "no SKILL.md or server.json") {
		t.Errorf("empty dir: %v", err)
	}
	if _, _, err := runCLI(t, strings.NewReader(""), "init", src, "--from", src, "--namespace", "acme", "--egress", "none", "--secrets", "none"); err != nil {
		t.Fatalf("in-place import: %v", err)
	}
	if _, _, err := runCLI(t, strings.NewReader(""), "init", src, "--from", src, "--namespace", "acme", "--egress", "none", "--secrets", "none"); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("second import: %v", err)
	}
}
