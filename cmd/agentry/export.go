// Interop exporters (§5.6): pure functions of (archive, manifest, target)
// that emit the claude-skill, mcp, and agents-md install formats. Keeping
// them side-effect free makes them table-testable without a registry.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry/pkg/manifest"
)

// ensureSkillFrontmatter returns content guaranteed to start with YAML
// frontmatter. When the skill file already opens with a frontmatter fence it
// is returned unchanged; otherwise `name` and `description` (mapped from the
// manifest name and summary per §5.6) are prepended.
func ensureSkillFrontmatter(content []byte, name, summary string) []byte {
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

// mcpServerEntry is one server stanza in a client's .mcp.json.
type mcpServerEntry struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// mcpEntryFromManifest builds the .mcp.json stanza for an installed
// mcp-server: command and args from entrypoints.mcp, and empty-value env
// placeholders for every declared secret (§5.6). Args naming archive files
// are re-rooted under extractedRel (slash-separated, "" = config file sits
// next to the extracted files).
func mcpEntryFromManifest(m *manifest.Manifest, extractedRel string, archiveFiles map[string]bool) (mcpServerEntry, error) {
	if m.Entrypoints.MCP == nil {
		return mcpServerEntry{}, errors.New("manifest declares no mcp entrypoint")
	}
	entry := mcpServerEntry{Command: m.Entrypoints.MCP.Command}
	for _, a := range m.Entrypoints.MCP.Args {
		if extractedRel != "" && archiveFiles[a] {
			a = path.Join(extractedRel, a)
		}
		entry.Args = append(entry.Args, a)
	}
	if len(m.Capabilities.Secrets) > 0 {
		entry.Env = make(map[string]string, len(m.Capabilities.Secrets))
		for _, s := range m.Capabilities.Secrets {
			entry.Env[s] = ""
		}
	}
	return entry, nil
}

// mergeMCPConfig upserts one server entry into a .mcp.json document,
// preserving every existing server and any unknown top-level keys. The
// output is deterministic (sorted keys, two-space indent, trailing newline),
// so repeated merges are byte-idempotent.
func mergeMCPConfig(existing []byte, name string, entry mcpServerEntry) ([]byte, error) {
	root := map[string]any{}
	if len(bytes.TrimSpace(existing)) > 0 {
		if err := json.Unmarshal(existing, &root); err != nil {
			return nil, fmt.Errorf("existing .mcp.json is not valid JSON (refusing to overwrite): %w", err)
		}
	}
	servers, _ := root["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return nil, fmt.Errorf("encode server entry: %w", err)
	}
	var entryDoc map[string]any
	if err := json.Unmarshal(raw, &entryDoc); err != nil {
		return nil, fmt.Errorf("normalize server entry: %w", err)
	}
	servers[name] = entryDoc
	root["mcpServers"] = servers
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode .mcp.json: %w", err)
	}
	return append(out, '\n'), nil
}

// agentsMDSection renders the AGENTS.md section for one installed
// prompt-agent (§5.6 agents-md target).
func agentsMDSection(ns, name, version, summary, installPath string) string {
	var b strings.Builder
	_, _ = fmt.Fprintf(&b, "## %s/%s (v%s)\n\n", ns, name, version)
	if strings.TrimSpace(summary) != "" {
		b.WriteString(summary + "\n\n")
	}
	_, _ = fmt.Fprintf(&b, "Files: %s\n", installPath)
	return b.String()
}

// upsertAgentsMDSection appends the agent's section to an AGENTS.md document,
// replacing any existing section for the same ns/name (any version) so
// reinstalls and upgrades stay idempotent.
func upsertAgentsMDSection(existing []byte, ns, name, version, summary, installPath string) []byte {
	section := strings.TrimRight(agentsMDSection(ns, name, version, summary, installPath), "\n")
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
