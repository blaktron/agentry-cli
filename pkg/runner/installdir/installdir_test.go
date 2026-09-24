package installdir

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

func stageTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The layout is the run's: <home>/agents/<ns>/<name>/<version>-<12 of the
// digest>, the agent's directory above it stable across versions.
func TestDirLayout(t *testing.T) {
	home := "/rh"
	if got := AgentDir(home, "acme", "demo"); got != filepath.Join("/rh", "agents", "acme", "demo") {
		t.Fatalf("AgentDir = %q", got)
	}
	got := ReleaseDir(home, "acme", "demo", "1.2.0", "sha256:0123456789abcdefXYZ")
	if got != filepath.Join("/rh", "agents", "acme", "demo", "1.2.0-0123456789ab") {
		t.Fatalf("ReleaseDir = %q", got)
	}
	if DigestSuffix("sha256:abc") != "abc" || DigestSuffix("0123456789abcdef") != "0123456789ab" {
		t.Fatal("DigestSuffix")
	}
}

// Write lands workspace/ and install.json atomically, skipping a local
// tree's .git and agentry-out; Load answers only for the digest asked.
func TestWriteAndLoad(t *testing.T) {
	src := stageTree(t, map[string]string{"agent.yaml": "name: demo\n", "prompts/main.md": "hi", ".git/HEAD": "ref", "agentry-out/x": "old"})
	dir := filepath.Join(t.TempDir(), "agents", "acme", "demo", "1.0.0-abc")
	meta := &Meta{Ref: "acme/demo@1.0.0", Namespace: "acme", Name: "demo", Version: "1.0.0", Digest: "sha256:abc", Source: "registry",
		Attestation: receipt.Attestation{State: "verified", KeyID: "k1"}}
	if err := Write(dir, src, meta); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"workspace/agent.yaml", "workspace/prompts/main.md", "install.json"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(want))); err != nil {
			t.Fatalf("%s: %v", want, err)
		}
	}
	for _, skipped := range []string{"workspace/.git", "workspace/agentry-out"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(skipped))); !os.IsNotExist(err) {
			t.Fatalf("%s was copied", skipped)
		}
	}
	if _, err := os.Stat(dir + ".partial"); !os.IsNotExist(err) {
		t.Fatal("the partial directory was left behind")
	}
	got, ok := Load(dir, "sha256:abc")
	if !ok || got.Ref != "acme/demo@1.0.0" || got.Attestation.KeyID != "k1" {
		t.Fatalf("Load = %+v %v", got, ok)
	}
	if _, ok := Load(dir, "sha256:other"); ok {
		t.Fatal("Load answered for another digest")
	}
	// The JSON shape is the one runs have read since the cache existed.
	raw, _ := os.ReadFile(filepath.Join(dir, "install.json"))
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	for _, k := range []string{"ref", "namespace", "name", "version", "digest", "source", "attestation"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("install.json lacks %q:\n%s", k, raw)
		}
	}
	if _, ok := m["export"]; ok {
		t.Fatalf("export is written only when set:\n%s", raw)
	}
	// A second Write replaces a stale tree entirely.
	src2 := stageTree(t, map[string]string{"agent.yaml": "name: demo\n"})
	if err := Write(dir, src2, meta); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "workspace", "prompts", "main.md")); !os.IsNotExist(err) {
		t.Fatal("a stale file survived the rewrite")
	}
}

// The export style (plan desktop-one-click-install.md D2): the coding
// agent the policy names, else the provider, else AGENTS.md.
func TestStyleFor(t *testing.T) {
	cases := []struct {
		name string
		pol  *policy.Policy
		want string
	}{
		{"nil policy", nil, manifest.InterfaceAgentsMD},
		{"claude harness", &policy.Policy{Harness: map[string]policy.HarnessPolicy{"claude": {}}}, manifest.InterfaceClaudeSkill},
		{"codex harness", &policy.Policy{Harness: map[string]policy.HarnessPolicy{"codex": {}}}, manifest.InterfaceAgentsMD},
		{"both: claude wins", &policy.Policy{Harness: map[string]policy.HarnessPolicy{"codex": {}, "claude": {}}}, manifest.InterfaceClaudeSkill},
		{"anthropic provider", &policy.Policy{Model: policy.Model{Provider: "anthropic"}}, manifest.InterfaceClaudeSkill},
		{"openai provider", &policy.Policy{Model: policy.Model{Provider: "openai"}}, manifest.InterfaceAgentsMD},
		{"custom provider", &policy.Policy{Model: policy.Model{Provider: "custom"}}, manifest.InterfaceAgentsMD},
		{"google provider", &policy.Policy{Model: policy.Model{Provider: "google"}}, manifest.InterfaceAgentsMD},
		{"codex over anthropic", &policy.Policy{Model: policy.Model{Provider: "anthropic"}, Harness: map[string]policy.HarnessPolicy{"codex": {}}}, manifest.InterfaceAgentsMD},
	}
	for _, c := range cases {
		if got := StyleFor(c.pol); got != c.want {
			t.Errorf("%s: StyleFor = %q, want %q", c.name, got, c.want)
		}
	}
}

// WriteExport lays the coding-agent shape under <dir>/export from the
// verified workspace: the Claude skill as ~/.claude/skills would hold it,
// AGENTS.md with the agent's section as a project root would.
func TestWriteExport(t *testing.T) {
	m := &manifest.Manifest{Name: "demo", Namespace: "acme", Version: "1.0.0", Summary: "reads things"}
	m.Capabilities.Filesystem.Read = manifest.FSAccess{Mode: manifest.FSModeWorkspace}
	for _, style := range []string{manifest.InterfaceClaudeSkill, manifest.InterfaceAgentsMD} {
		src := stageTree(t, map[string]string{"agent.yaml": "name: demo\n", "SKILL.md": "# Demo\n", "prompts/main.md": "hi"})
		dir := filepath.Join(t.TempDir(), "1.0.0-abc")
		meta := &Meta{Ref: "acme/demo@1.0.0", Namespace: "acme", Name: "demo", Version: "1.0.0", Digest: "sha256:abc", Source: "registry"}
		if err := Write(dir, src, meta); err != nil {
			t.Fatal(err)
		}
		if err := WriteExport(dir, style, m, meta); err != nil {
			t.Fatalf("%s: %v", style, err)
		}
		got, ok := Load(dir, "sha256:abc")
		if !ok || got.Export != style {
			t.Fatalf("%s: export not recorded: %+v", style, got)
		}
		switch style {
		case manifest.InterfaceClaudeSkill:
			skill, err := os.ReadFile(filepath.Join(dir, "export", "demo", "SKILL.md"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(skill), "allowed-tools: Read, Grep, Glob") || !strings.Contains(string(skill), "# Demo") {
				t.Fatalf("SKILL.md:\n%s", skill)
			}
		case manifest.InterfaceAgentsMD:
			agents, err := os.ReadFile(filepath.Join(dir, "export", "AGENTS.md"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(agents), "acme/demo") {
				t.Fatalf("AGENTS.md:\n%s", agents)
			}
			if _, err := os.Stat(filepath.Join(dir, "export", "agents", "demo", "prompts", "main.md")); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// An instruction-only release (a prompt agent: AGENT.md, no SKILL.md — the
// shape of the catalogue on dev) exports as a Claude skill too, its
// SKILL.md made from the instruction (agentry-cli#193): a chosen Claude
// Code no longer fails the runner-home install of such a release.
func TestWriteExportInstructionOnlyClaudeSkill(t *testing.T) {
	m := &manifest.Manifest{SchemaVersion: "2", Name: "changelog-from-git", Namespace: "agentry", Version: "1.2.1", Summary: "Drafts a changelog from git history."}
	m.Entrypoints.Instruction = "AGENT.md"
	m.Capabilities.Exec.Commands = []string{"git"}
	src := stageTree(t, map[string]string{"agent.yaml": "name: changelog-from-git\n", "AGENT.md": "# Changelog\n\nRead the log.\n", "README.md": "readme\n"})
	dir := filepath.Join(t.TempDir(), "1.2.1-abc")
	meta := &Meta{Ref: "agentry/changelog-from-git@1.2.1", Namespace: "agentry", Name: "changelog-from-git", Version: "1.2.1", Digest: "sha256:abc", Source: "registry"}
	if err := Write(dir, src, meta); err != nil {
		t.Fatal(err)
	}
	pol := policy.Default()
	pol.Harness = map[string]policy.HarnessPolicy{"claude": {}}
	if style := StyleFor(pol); style != manifest.InterfaceClaudeSkill {
		t.Fatalf("StyleFor with claude chosen: %s", style)
	}
	if err := WriteExport(dir, manifest.InterfaceClaudeSkill, m, meta); err != nil {
		t.Fatalf("the Claude-skill export of an instruction-only release: %v", err)
	}
	skill, err := os.ReadFile(filepath.Join(dir, "export", "changelog-from-git", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"name: changelog-from-git", "description: Drafts a changelog from git history.", "allowed-tools: Bash(git:*)", "# Changelog\n\nRead the log.\n"} {
		if !strings.Contains(string(skill), want) {
			t.Fatalf("SKILL.md lacks %q:\n%s", want, skill)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "export", "changelog-from-git", "AGENT.md")); err != nil {
		t.Fatalf("the instruction file must stay beside the skill: %v", err)
	}
	if got, ok := Load(dir, "sha256:abc"); !ok || got.Export != manifest.InterfaceClaudeSkill {
		t.Fatalf("install.json names what was written: %+v", got)
	}
}

// RemoveOtherVersions leaves the named release and takes every sibling.
func TestRemoveOtherVersions(t *testing.T) {
	home := t.TempDir()
	keep := ReleaseDir(home, "acme", "demo", "1.1.0", "sha256:bbb")
	old := ReleaseDir(home, "acme", "demo", "1.0.0", "sha256:aaa")
	for _, d := range []string{keep, old, filepath.Join(AgentDir(home, "acme", "demo"), "1.0.0-aaa.partial")} {
		if err := os.MkdirAll(filepath.Join(d, "workspace"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveOtherVersions(AgentDir(home, "acme", "demo"), keep); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("the kept release went")
	}
	entries, _ := os.ReadDir(AgentDir(home, "acme", "demo"))
	if len(entries) != 1 || entries[0].Name() != filepath.Base(keep) {
		t.Fatalf("left: %v", entries)
	}
}

// ReleaseName is the one spelling of a release's directory name, the one
// ReleaseDir and every client use (agentry-cli#192).
func TestReleaseName(t *testing.T) {
	if got := ReleaseName("1.2.0", "sha256:0123456789abcdefXYZ"); got != "1.2.0-0123456789ab" {
		t.Fatalf("ReleaseName = %q", got)
	}
	if filepath.Base(ReleaseDir("/rh", "acme", "demo", "1.2.0", "sha256:0123456789abcdefXYZ")) != ReleaseName("1.2.0", "sha256:0123456789abcdefXYZ") {
		t.Fatal("ReleaseDir does not end in ReleaseName")
	}
}

// StyleForHome reads the runner home's policy for the export style with
// its one honest outcome for each case (agentry-cli#192): no policy is the
// generic shape and nothing to say; a policy is StyleFor's answer; a
// policy that cannot be read is the generic shape and a warning naming it.
func TestStyleForHome(t *testing.T) {
	home := t.TempDir()
	if style, warn := StyleForHome(home); style != manifest.InterfaceAgentsMD || warn != "" {
		t.Fatalf("no policy: %q %q", style, warn)
	}
	pol := policy.Default()
	pol.Harness = map[string]policy.HarnessPolicy{"claude": {}}
	path := policy.PathIn(home)
	if err := policy.Save(path, pol); err != nil {
		t.Fatal(err)
	}
	if style, warn := StyleForHome(home); style != manifest.InterfaceClaudeSkill || warn != "" {
		t.Fatalf("claude policy: %q %q", style, warn)
	}
	if err := os.WriteFile(path, []byte("not: [a policy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	style, warn := StyleForHome(home)
	if style != manifest.InterfaceAgentsMD || !strings.Contains(warn, path) || !strings.Contains(warn, "AGENTS.md") {
		t.Fatalf("unreadable policy: %q %q", style, warn)
	}
}

// Install is the one lay-down both clients call: the release written,
// then its export in the style given, recorded in install.json.
func TestInstall(t *testing.T) {
	m := &manifest.Manifest{Name: "demo", Namespace: "acme", Version: "1.0.0", Summary: "reads things"}
	src := stageTree(t, map[string]string{"agent.yaml": "name: demo\n", "SKILL.md": "# Demo\n"})
	dir := filepath.Join(t.TempDir(), ReleaseName("1.0.0", "sha256:abc"))
	meta := &Meta{Ref: "acme/demo@1.0.0", Namespace: "acme", Name: "demo", Version: "1.0.0", Digest: "sha256:abc", Source: "registry"}
	if err := Install(dir, src, m, meta, manifest.InterfaceClaudeSkill); err != nil {
		t.Fatal(err)
	}
	got, ok := Load(dir, "sha256:abc")
	if !ok || got.Export != manifest.InterfaceClaudeSkill {
		t.Fatalf("install.json: %+v ok=%v", got, ok)
	}
	for _, want := range []string{"workspace/SKILL.md", "export/demo/SKILL.md"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(want))); err != nil {
			t.Fatalf("%s: %v", want, err)
		}
	}
	if err := Install(dir, src, m, meta, "bogus"); err == nil {
		t.Fatal("an unknown style was accepted")
	}
	other := filepath.Join(t.TempDir(), ReleaseName("1.0.0", "sha256:abc"))
	consented := *meta
	consented.Attestation.Consent = "allow-unsigned"
	if err := Install(other, src, m, &consented, manifest.InterfaceAgentsMD); err == nil {
		t.Fatal("a consent was written into install.json")
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatalf("the refused Install wrote %s", other)
	}
}

// A keep that is not a release directly under the agent's directory is
// ErrNotARelease, so a caller that warns on a failed replace still stops
// on a path it built wrong.
func TestRemoveOtherVersionsRefusalIsErrNotARelease(t *testing.T) {
	agentDir := t.TempDir()
	if err := RemoveOtherVersions(agentDir, filepath.Join(agentDir, "a", "b")); !errors.Is(err, ErrNotARelease) {
		t.Fatalf("err = %v, want ErrNotARelease", err)
	}
}
