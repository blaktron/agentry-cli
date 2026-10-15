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
	yaml := "schemaVersion: \"2\"\nname: t\nnamespace: d\nmodes: [autonomous]\nversion: 1.0.0\nsummary: \"t\"\n" +
		capsBlock + "publish:\n  include: [agent.yaml, AGENT.md]\n"
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
	// The line is the declared contract, compiled; an author's own grant
	// is replaced, never kept, and stays visible as a comment naming
	// what was dropped.
	t.Run("author allowed-tools is replaced by the contract", func(t *testing.T) {
		in := "---\nname: mine\nallowed-tools: Bash\ndescription: d\n---\n\nbody\n"
		got := string(SkillFrontmatterWithTools([]byte(in), "n", "s", tools))
		if a := activeAllowedTools(got); a != "Read, WebFetch(domain:x.y)" {
			t.Errorf("active allowed-tools = %q:\n%s", a, got)
		}
		if !strings.Contains(got, "# allowed-tools: Bash\n") || !strings.Contains(got, "name: mine\n") || !strings.Contains(got, "description: d\n---\n\nbody\n") {
			t.Errorf("author line not kept as a comment, or frontmatter lost:\n%s", got)
		}
	})
	t.Run("a list-form author grant is replaced whole", func(t *testing.T) {
		in := "---\nname: mine\nallowed-tools:\n  - Bash\n  - Read\ndescription: d\n---\nbody\n"
		got := string(SkillFrontmatterWithTools([]byte(in), "n", "s", tools))
		if a := activeAllowedTools(got); a != "Read, WebFetch(domain:x.y)" {
			t.Errorf("active allowed-tools = %q:\n%s", a, got)
		}
		if strings.Contains(got, "\n  - Bash") || !strings.Contains(got, "#   - Bash\n") || !strings.Contains(got, "description: d\n") {
			t.Errorf("list items survived as grants, or were not kept as comments:\n%s", got)
		}
	})
	t.Run("no grants removes the author's line", func(t *testing.T) {
		in := "---\nname: mine\nallowed-tools: Bash, Write\n---\n\nbody\n"
		got := string(SkillFrontmatterWithTools([]byte(in), "n", "s", nil))
		if a := activeAllowedTools(got); a != "" {
			t.Errorf("a pure-text contract kept a grant %q:\n%s", a, got)
		}
		if !strings.Contains(got, "# allowed-tools: Bash, Write\n") || !strings.HasSuffix(got, "---\n\nbody\n") {
			t.Errorf("dropped line not named, or body changed:\n%s", got)
		}
	})
	t.Run("an author line equal to the contract is left as written", func(t *testing.T) {
		in := "---\nname: mine\nallowed-tools: Read, WebFetch(domain:x.y)\n---\n\nbody\n"
		if got := string(SkillFrontmatterWithTools([]byte(in), "n", "s", tools)); got != in {
			t.Errorf("an agreeing line was rewritten:\n%s", got)
		}
	})
	t.Run("CRLF frontmatter is handled the same", func(t *testing.T) {
		in := "---\r\nname: mine\r\nallowed-tools: Bash\r\n---\r\n\r\nbody\r\n"
		got := string(SkillFrontmatterWithTools([]byte(in), "n", "s", tools))
		if a := activeAllowedTools(got); a != "Read, WebFetch(domain:x.y)" {
			t.Errorf("active allowed-tools = %q:\n%q", a, got)
		}
		if !strings.HasSuffix(got, "---\r\n\r\nbody\r\n") {
			t.Errorf("body or fence changed:\n%q", got)
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

// TestAgentsMDSectionNoNote pins the note-free wrapper: same section, no
// trailing Tools line — the shape pre-v2 callers still emit.
func TestAgentsMDSectionNoNote(t *testing.T) {
	got := AgentsMDSection("acme", "helper", "1.0.0", "Helps out", "agents/helper")
	want := AgentsMDSectionNote("acme", "helper", "1.0.0", "Helps out", "agents/helper", "")
	if got != want {
		t.Errorf("AgentsMDSection = %q, want the note-free AgentsMDSectionNote %q", got, want)
	}
	for _, must := range []string{"## acme/helper (v1.0.0)", "Helps out", "Files: agents/helper"} {
		if !strings.Contains(got, must) {
			t.Errorf("section missing %q:\n%s", must, got)
		}
	}
	if strings.Contains(got, "Tools (") {
		t.Errorf("note-free section carries a tools note:\n%s", got)
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

// activeAllowedTools is the value of the frontmatter's top-level
// allowed-tools key, "" when there is none (comments do not count).
func activeAllowedTools(content string) string {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	if len(lines) == 0 || lines[0] != "---" {
		return ""
	}
	for _, l := range lines[1:] {
		if l == "---" {
			return ""
		}
		if v, ok := strings.CutPrefix(l, "allowed-tools:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
