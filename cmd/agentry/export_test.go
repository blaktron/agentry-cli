package main

import (
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
			got := string(ensureSkillFrontmatter([]byte(tt.content), "incident-summarizer", "Summarizes incidents"))
			if got != tt.want {
				t.Errorf("got:\n%q\nwant:\n%q", got, tt.want)
			}
		})
	}
}

func TestEnsureSkillFrontmatterQuotesSpecialChars(t *testing.T) {
	got := string(ensureSkillFrontmatter([]byte("body\n"), "my-skill", "does: things, with #chars"))
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
	got := string(ensureSkillFrontmatter([]byte("body\n"), "my-skill", "  "))
	if !strings.Contains(got, "description: my-skill\n") {
		t.Errorf("empty summary should fall back to the name:\n%q", got)
	}
}

func TestMergeMCPConfigFresh(t *testing.T) {
	entry := mcpServerEntry{Command: "node", Args: []string{"src/server.js"}, Env: map[string]string{"TOKEN": ""}}
	got, err := mergeMCPConfig(nil, "my-server", entry)
	if err != nil {
		t.Fatal(err)
	}
	// Keys sort alphabetically: the whole document round-trips through maps
	// so repeated merges are byte-identical.
	want := `{
  "mcpServers": {
    "my-server": {
      "args": [
        "src/server.js"
      ],
      "command": "node",
      "env": {
        "TOKEN": ""
      }
    }
  }
}
`
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestMergeMCPConfigPreservesExistingEntriesAndKeys(t *testing.T) {
	existing := `{
  "mcpServers": {
    "other": {"command": "python", "args": ["a.py"]}
  },
  "somethingElse": {"keep": true}
}`
	got, err := mergeMCPConfig([]byte(existing), "mine", mcpServerEntry{Command: "node"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"other"`, `"python"`, `"somethingElse"`, `"keep": true`, `"mine"`} {
		if !strings.Contains(string(got), want) {
			t.Errorf("merged config lost %s:\n%s", want, got)
		}
	}
}

func TestMergeMCPConfigIdempotent(t *testing.T) {
	entry := mcpServerEntry{Command: "node", Args: []string{"srv.js"}}
	once, err := mergeMCPConfig(nil, "s", entry)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := mergeMCPConfig(once, "s", entry)
	if err != nil {
		t.Fatal(err)
	}
	if string(once) != string(twice) {
		t.Errorf("merge is not idempotent:\nfirst:\n%s\nsecond:\n%s", once, twice)
	}
}

func TestMergeMCPConfigRejectsInvalidJSON(t *testing.T) {
	if _, err := mergeMCPConfig([]byte("{nope"), "s", mcpServerEntry{Command: "node"}); err == nil {
		t.Error("invalid existing JSON must not be clobbered")
	}
}

func TestMCPEntryFromManifest(t *testing.T) {
	m := &manifest.Manifest{
		Entrypoints: manifest.Entrypoints{
			MCP: &manifest.MCPEntrypoint{Command: "node", Args: []string{"src/server.js", "--verbose"}},
		},
		Capabilities: manifest.Capabilities{Secrets: []string{"API_TOKEN"}},
	}
	archiveFiles := map[string]bool{"src/server.js": true}

	entry, err := mcpEntryFromManifest(m, "agents/my-server", archiveFiles)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Command != "node" {
		t.Errorf("command = %q", entry.Command)
	}
	if got, want := strings.Join(entry.Args, " "), "agents/my-server/src/server.js --verbose"; got != want {
		t.Errorf("args = %q, want %q (file args re-rooted, flags untouched)", got, want)
	}
	if v, ok := entry.Env["API_TOKEN"]; !ok || v != "" {
		t.Errorf("env = %v, want empty placeholder for API_TOKEN", entry.Env)
	}

	// Extracting next to the config file keeps args untouched.
	entry, err = mcpEntryFromManifest(m, "", archiveFiles)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Args[0] != "src/server.js" {
		t.Errorf("args with empty rel = %v", entry.Args)
	}
}

func TestMCPEntryFromManifestNoEntrypoint(t *testing.T) {
	if _, err := mcpEntryFromManifest(&manifest.Manifest{}, "", nil); err == nil {
		t.Error("missing mcp entrypoint must error")
	}
}

func TestUpsertAgentsMDSectionAppendAndReplace(t *testing.T) {
	// Fresh file.
	first := upsertAgentsMDSection(nil, "acme", "helper", "1.0.0", "Helps out", "agents/helper")
	want := "## acme/helper (v1.0.0)\n\nHelps out\n\nFiles: agents/helper\n"
	if string(first) != want {
		t.Fatalf("fresh section:\ngot  %q\nwant %q", first, want)
	}

	// Replace on upgrade (same heading prefix, new version) — idempotent.
	second := upsertAgentsMDSection(first, "acme", "helper", "1.1.0", "Helps more", "agents/helper")
	if strings.Contains(string(second), "v1.0.0") {
		t.Errorf("old section survived the upgrade:\n%s", second)
	}
	if !strings.Contains(string(second), "## acme/helper (v1.1.0)") || !strings.Contains(string(second), "Helps more") {
		t.Errorf("new section missing:\n%s", second)
	}
	third := upsertAgentsMDSection(second, "acme", "helper", "1.1.0", "Helps more", "agents/helper")
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
	got := string(upsertAgentsMDSection(existing, "acme", "helper", "1.0.0", "New text", "agents/helper"))
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
