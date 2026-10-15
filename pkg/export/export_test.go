package export

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"
)

func TestEnsureSkillFrontmatter(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "adds frontmatter when missing",
			content: "# Skill\n\nDo the thing.\n",
			want:    "---\nname: incident-summarizer\ndescription: Summarizes incidents\n---\n\n# Skill\n\nDo the thing.\n",
		},
		{
			name:    "leaves existing frontmatter untouched",
			content: "---\nname: custom\n---\n\nBody.\n",
			want:    "---\nname: custom\n---\n\nBody.\n",
		},
		{
			name:    "empty content still gets frontmatter",
			content: "",
			want:    "---\nname: incident-summarizer\ndescription: Summarizes incidents\n---\n\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(EnsureSkillFrontmatter([]byte(tt.content), "incident-summarizer", "Summarizes incidents"))
			if got != tt.want {
				t.Errorf("got:\n%q\nwant:\n%q", got, tt.want)
			}
		})
	}
}

func TestEnsureSkillFrontmatterQuotesSpecialChars(t *testing.T) {
	got := string(EnsureSkillFrontmatter([]byte("body\n"), "my-skill", "does: things, with #chars"))
	if !strings.HasPrefix(got, "---\n") {
		t.Fatalf("missing frontmatter fence: %q", got)
	}
	// The summary must survive a YAML round-trip; a colon-bearing value has
	// to be quoted by the marshaller.
	if !strings.Contains(got, "does: things, with #chars") {
		t.Errorf("summary text lost: %q", got)
	}
}

func TestEnsureSkillFrontmatterEmptySummaryFallsBackToName(t *testing.T) {
	got := string(EnsureSkillFrontmatter([]byte("body\n"), "my-skill", "  "))
	if !strings.Contains(got, "description: my-skill\n") {
		t.Errorf("empty summary should fall back to the name:\n%q", got)
	}
}

func TestUpsertAgentsMDSectionAppendAndReplace(t *testing.T) {
	// Fresh file.
	first := UpsertAgentsMDSection(nil, "acme", "helper", "1.0.0", "Helps out", "agents/helper")
	want := "## acme/helper (v1.0.0)\n\nHelps out\n\nFiles: agents/helper\n"
	if string(first) != want {
		t.Fatalf("fresh section:\ngot  %q\nwant %q", first, want)
	}

	// Replace on upgrade (same heading prefix, new version) — idempotent.
	second := UpsertAgentsMDSection(first, "acme", "helper", "1.1.0", "Helps more", "agents/helper")
	if strings.Contains(string(second), "v1.0.0") {
		t.Errorf("old section survived the upgrade:\n%s", second)
	}
	if !strings.Contains(string(second), "## acme/helper (v1.1.0)") || !strings.Contains(string(second), "Helps more") {
		t.Errorf("new section missing:\n%s", second)
	}
	third := UpsertAgentsMDSection(second, "acme", "helper", "1.1.0", "Helps more", "agents/helper")
	if string(second) != string(third) {
		t.Errorf("upsert is not idempotent:\n%s\nvs\n%s", second, third)
	}
}

func TestUpsertAgentsMDSectionPreservesOtherContent(t *testing.T) {
	existing := []byte(`# Project agents

Intro prose.

## acme/helper (v0.9.0)

Old text.

## other/agent (v2.0.0)

Keep me.
`)
	got := string(UpsertAgentsMDSection(existing, "acme", "helper", "1.0.0", "New text", "agents/helper"))
	for _, want := range []string{"# Project agents", "Intro prose.", "## other/agent (v2.0.0)", "Keep me.", "## acme/helper (v1.0.0)", "New text"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "v0.9.0") || strings.Contains(got, "Old text.") {
		t.Errorf("old section not replaced:\n%s", got)
	}
	// The replaced section must stay separated from the next heading.
	if strings.Contains(got, "Files: agents/helper\n## ") {
		t.Errorf("missing blank line before next section:\n%s", got)
	}
}

// TestWriteClaudeSkillTreeBody: what the exported skill's body is read
// from — the named skill file; for a release with no skill entrypoint,
// its instruction as the runner reads it, else a SKILL.md it ships;
// always from the verified release, never from what an earlier install
// left in the destination — and how a release with nothing to make a
// skill from is refused.
func TestWriteClaudeSkillTreeBody(t *testing.T) {
	caps := manifest.Capabilities{}
	caps.Filesystem.Read.Mode = manifest.FSModeWorkspace
	v2 := func(name string, instruction string) *manifest.Manifest {
		m := &manifest.Manifest{SchemaVersion: "2", Name: name, Summary: "Drafts a changelog from git history.", Capabilities: caps}
		m.Entrypoints.Instruction = instruction
		return m
	}
	v1 := func(kind, name, skill string) *manifest.Manifest {
		m := &manifest.Manifest{SchemaVersion: "1", Kind: kind, Name: name, Summary: "s", Capabilities: caps}
		m.Entrypoints.Skill = skill
		return m
	}
	const fm = "---\nname: changelog-from-git\ndescription: Drafts a changelog from git history.\nallowed-tools: Read, Grep, Glob\n---\n\n"
	cases := []struct {
		name     string
		m        *manifest.Manifest
		release  map[string]string
		dest     map[string]string // left in the destination by an earlier install
		want     string            // SKILL.md's whole content
		contains []string          // or these, when the frontmatter is the author's
		refusal  string
	}{
		{name: "a v2 prompt agent: SKILL.md from the named instruction", m: v2("changelog-from-git", "AGENT.md"),
			release: map[string]string{"AGENT.md": "# Changelog\n\nRead the log.\n", "README.md": "readme\n"},
			want:    fm + "# Changelog\n\nRead the log.\n"},
		{name: "the v2 default instruction file, unnamed", m: v2("changelog-from-git", ""),
			release: map[string]string{"AGENT.md": "body\n"}, want: fm + "body\n"},
		{name: "a stale SKILL.md in the destination is not the source", m: v2("changelog-from-git", "AGENT.md"),
			release: map[string]string{"AGENT.md": "new body\n"}, dest: map[string]string{"SKILL.md": "---\nname: old\n---\n\nold body\n"},
			want: fm + "new body\n"},
		{name: "no skill entrypoint: the instruction wins over a shipped SKILL.md, as the runner reads it", m: v2("changelog-from-git", ""),
			release: map[string]string{"AGENT.md": "the instruction\n", "SKILL.md": "a stray skill\n"}, want: fm + "the instruction\n"},
		{name: "a v2 release that ships only SKILL.md keeps exporting", m: v2("changelog-from-git", ""),
			release: map[string]string{"SKILL.md": "skill body\n"}, want: fm + "skill body\n"},
		{name: "an instruction with its own frontmatter keeps it and gains the tools", m: v2("changelog-from-git", "AGENT.md"),
			release:  map[string]string{"AGENT.md": "---\nname: own\ndescription: own words\n---\n\nown body\n"},
			contains: []string{"description: own words", "allowed-tools: Read, Grep, Glob", "\n\nown body\n"}},
		{name: "a v1 prompt agent falls back to PROMPT.md", m: v1("prompt-agent", "changelog-from-git", ""),
			release: map[string]string{"PROMPT.md": "prompt body\n"}, contains: []string{"name: changelog-from-git", "allowed-tools: Read, Grep, Glob", "---\n\nprompt body\n"}},
		{name: "a v1 skill exports its named file as before", m: v1("skill", "changelog-from-git", "SKILL.md"),
			release:  map[string]string{"SKILL.md": "---\nname: own\ndescription: own words\n---\n\nskill body\n"},
			contains: []string{"description: own words", "allowed-tools: Read, Grep, Glob", "skill body\n"}},
		{name: "a named skill file the release does not carry is refused by name", m: v1("skill", "liar", "skills/LIAR.md"),
			release: map[string]string{"AGENT.md": "body\n"}, refusal: "names skill entrypoint skills/LIAR.md but does not carry it"},
		{name: "nothing to make a skill from is refused naming what was looked for", m: v2("empty", ""),
			release: map[string]string{"README.md": "readme\n"}, refusal: "looked for AGENT.md, SKILL.md"},
		{name: "an instruction that resolves outside the release is refused", m: v2("escape", "../../outside.md"),
			release: map[string]string{"AGENT.md": "body\n"}, refusal: "resolves outside the release"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			staging := t.TempDir()
			for name, body := range c.release {
				if err := os.WriteFile(filepath.Join(staging, name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			dest := filepath.Join(t.TempDir(), c.m.Name)
			for name, body := range c.dest {
				if err := os.MkdirAll(dest, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dest, name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err := WriteClaudeSkillTree(staging, dest, c.m)
			if c.refusal != "" {
				if err == nil || !strings.Contains(err.Error(), c.refusal) {
					t.Fatalf("want a refusal containing %q, got %v", c.refusal, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			skill, err := os.ReadFile(filepath.Join(dest, "SKILL.md"))
			if err != nil {
				t.Fatalf("SKILL.md: %v", err)
			}
			if c.want != "" && string(skill) != c.want {
				t.Fatalf("SKILL.md:\n%s\nwant:\n%s", skill, c.want)
			}
			for _, want := range c.contains {
				if !strings.Contains(string(skill), want) {
					t.Fatalf("SKILL.md lacks %q:\n%s", want, skill)
				}
			}
			for name := range c.release {
				if _, err := os.Stat(filepath.Join(dest, name)); err != nil {
					t.Fatalf("the release's %s must stay in the tree: %v", name, err)
				}
			}
		})
	}
}
