package export

import (
	"strings"
	"testing"
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
