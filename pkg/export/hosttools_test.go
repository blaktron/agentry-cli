// Embedded-mode mapping tests (RUNNER-PLAN §R5): capability contract →
// host tool grants, and the frontmatter insertion rules.
package export

import (
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"
)

func capsFromYAML(t *testing.T, capsBlock string) manifest.Capabilities {
	t.Helper()
	yaml := "schemaVersion: \"1\"\nname: t\nnamespace: d\nkind: prompt-agent\nversion: 1.0.0\nsummary: \"t\"\n" +
		capsBlock + "publish:\n  include: [agent.yaml]\n"
	m, err := manifest.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return m.Capabilities
}

func TestHostToolGrants(t *testing.T) {
	tests := []struct {
		name string
		caps string
		want string
	}{
		{"pure text", "", ""},
		{"read only", "capabilities:\n  filesystem:\n    read: workspace\n", "Read, Grep, Glob"},
		{"read write", "capabilities:\n  filesystem:\n    read: workspace\n    write: workspace\n", "Read, Grep, Glob, Write, Edit"},
		{"exec", "capabilities:\n  exec: { commands: [jq, git] }\n", "Bash(git:*), Bash(jq:*)"},
		{"egress", "capabilities:\n  network:\n    egress: [b.example, a.example]\n", "WebFetch(domain:a.example), WebFetch(domain:b.example)"},
		{"wildcard egress", "capabilities:\n  network:\n    egress: \"*\"\n", "WebFetch"},
		{"declared none", "capabilities:\n  filesystem:\n    read: none\n  exec: none\n  network:\n    egress: none\n", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(HostToolGrants(capsFromYAML(t, tc.caps)), ", ")
			if got != tc.want {
				t.Errorf("grants = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSkillFrontmatterWithTools(t *testing.T) {
	tools := []string{"Read", "WebFetch(domain:x.y)"}

	t.Run("generated frontmatter gains allowed-tools", func(t *testing.T) {
		got := string(SkillFrontmatterWithTools([]byte("body\n"), "n", "s", tools))
		if !strings.Contains(got, "allowed-tools: Read, WebFetch(domain:x.y)\n") {
			t.Errorf("missing allowed-tools:\n%s", got)
		}
		if !strings.HasPrefix(got, "---\n") {
			t.Errorf("frontmatter missing:\n%s", got)
		}
	})
	t.Run("existing frontmatter without the key gains it", func(t *testing.T) {
		in := "---\nname: mine\ndescription: d\n---\n\nbody\n"
		got := string(SkillFrontmatterWithTools([]byte(in), "n", "s", tools))
		if !strings.Contains(got, "allowed-tools: Read, WebFetch(domain:x.y)\n---\n") {
			t.Errorf("key not inserted before the fence:\n%s", got)
		}
		if !strings.Contains(got, "name: mine") {
			t.Errorf("author frontmatter lost:\n%s", got)
		}
	})
	t.Run("author allowed-tools wins", func(t *testing.T) {
		in := "---\nname: mine\nallowed-tools: Read\n---\n\nbody\n"
		got := string(SkillFrontmatterWithTools([]byte(in), "n", "s", tools))
		if got != in {
			t.Errorf("author allowed-tools overridden:\n%s", got)
		}
	})
	t.Run("no grants leaves content alone", func(t *testing.T) {
		in := "---\nname: mine\n---\n\nbody\n"
		if got := string(SkillFrontmatterWithTools([]byte(in), "n", "s", nil)); got != in {
			t.Errorf("pure-text content changed:\n%s", got)
		}
	})
}

func TestHostToolsNote(t *testing.T) {
	if got := HostToolsNote(capsFromYAML(t, "")); !strings.Contains(got, "none declared") {
		t.Errorf("pure text note = %q", got)
	}
	got := HostToolsNote(capsFromYAML(t, "capabilities:\n  filesystem:\n    read: workspace\n"))
	if !strings.Contains(got, "Read, Grep, Glob") {
		t.Errorf("note = %q", got)
	}
}

func TestUpsertAgentsMDSectionNoteCarriesTools(t *testing.T) {
	got := string(UpsertAgentsMDSectionNote(nil, "acme", "helper", "1.0.0", "Helps", "agents/helper",
		"Tools (host built-ins per the declared capability contract): Read."))
	if !strings.Contains(got, "Tools (host built-ins") {
		t.Errorf("note missing:\n%s", got)
	}
	// Idempotent upsert with the note present.
	again := string(UpsertAgentsMDSectionNote([]byte(got), "acme", "helper", "1.0.0", "Helps", "agents/helper",
		"Tools (host built-ins per the declared capability contract): Read."))
	if again != got {
		t.Errorf("upsert with note not idempotent:\n%s\nvs\n%s", got, again)
	}
}
