// Package export holds the interop exporters (§5.6): pure functions of
// (archive, manifest, target) that emit the claude-skill and agents-md
// install formats. Keeping them side-effect free makes them table-testable
// without a registry, and lets any client — the CLI, the desktop app —
// produce byte-identical installs from the same inputs.
package export

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// EnsureSkillFrontmatter returns content guaranteed to start with YAML
// frontmatter. When the skill file already opens with a frontmatter fence it
// is returned unchanged; otherwise `name` and `description` (mapped from the
// manifest name and summary per §5.6) are prepended.
func EnsureSkillFrontmatter(content []byte, name, summary string) []byte {
	if bytes.HasPrefix(content, []byte("---\n")) || bytes.HasPrefix(content, []byte("---\r\n")) {
		return content
	}
	description := summary
	if strings.TrimSpace(description) == "" {
		description = name
	}
	fm, err := yaml.Marshal(struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}{Name: name, Description: description})
	if err != nil {
		// Two plain strings cannot fail to marshal; keep the content usable
		// regardless.
		return content
	}
	var out bytes.Buffer
	out.WriteString("---\n")
	out.Write(fm)
	out.WriteString("---\n\n")
	out.Write(content)
	return out.Bytes()
}

// AgentsMDSection renders the AGENTS.md section for one installed
// BriefAgent (§5.6 agents-md target).
func AgentsMDSection(ns, name, version, summary, installPath string) string {
	return AgentsMDSectionNote(ns, name, version, summary, installPath, "")
}

// AgentsMDSectionNote is AgentsMDSection plus an optional trailing note
// line — the embedded-mode tools contract (RUNNER-PLAN §R5) rides here.
func AgentsMDSectionNote(ns, name, version, summary, installPath, note string) string {
	var b strings.Builder
	_, _ = fmt.Fprintf(&b, "## %s/%s (v%s)\n\n", ns, name, version)
	if strings.TrimSpace(summary) != "" {
		b.WriteString(summary + "\n\n")
	}
	_, _ = fmt.Fprintf(&b, "Files: %s\n", installPath)
	if strings.TrimSpace(note) != "" {
		b.WriteString("\n" + note + "\n")
	}
	return b.String()
}

// UpsertAgentsMDSection appends the agent's section to an AGENTS.md document,
// replacing any existing section for the same ns/name (any version) so
// reinstalls and upgrades stay idempotent.
func UpsertAgentsMDSection(existing []byte, ns, name, version, summary, installPath string) []byte {
	return UpsertAgentsMDSectionNote(existing, ns, name, version, summary, installPath, "")
}

// UpsertAgentsMDSectionNote is UpsertAgentsMDSection with the note line.
func UpsertAgentsMDSectionNote(existing []byte, ns, name, version, summary, installPath, note string) []byte {
	section := strings.TrimRight(AgentsMDSectionNote(ns, name, version, summary, installPath, note), "\n")
	prefix := "## " + ns + "/" + name + " ("

	content := strings.TrimRight(string(existing), "\n")
	var out []string
	replaced := false
	if content != "" {
		lines := strings.Split(content, "\n")
		for i := 0; i < len(lines); {
			if !strings.HasPrefix(lines[i], prefix) {
				out = append(out, lines[i])
				i++
				continue
			}
			// Skip the old section: everything up to the next "## " heading.
			j := i + 1
			for j < len(lines) && !strings.HasPrefix(lines[j], "## ") {
				j++
			}
			if !replaced {
				out = append(out, section)
				if j < len(lines) {
					out = append(out, "")
				}
				replaced = true
			}
			i = j
		}
	}
	if !replaced {
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, section)
	}
	return []byte(strings.Join(out, "\n") + "\n")
}
