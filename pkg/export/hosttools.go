// Embedded-mode tool mapping (RUNNER-PLAN §R5, RQ2): when an agent is
// installed INTO an existing coding agent instead of run standalone, the
// host's built-in tools stand in for the runner's companion server, and
// the manifest capability contract compiles to the host's tool-allowlist
// vocabulary. Nothing is bundled either way — the contract just changes
// dialect. Enforcement here is the host's; the runner's docs say so
// (honest-posture rule).
package export

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/blaktron/agentry/pkg/manifest"
)

// HostToolGrants maps the capability contract to Claude Code's built-in
// tool grant vocabulary (the claude-skill target's `allowed-tools`).
// Returns nil for a pure-text contract: a skill that declares no
// capabilities needs no tool grants, and emitting an empty list would
// depend on host semantics we do not control.
func HostToolGrants(c manifest.Capabilities) []string {
	var tools []string
	if c.Filesystem.Read.Mode == manifest.FSModeWorkspace || c.Filesystem.Read.Mode == manifest.FSModePaths {
		tools = append(tools, "Read", "Grep", "Glob")
	}
	if c.Filesystem.Write.Mode == manifest.FSModeWorkspace || c.Filesystem.Write.Mode == manifest.FSModePaths {
		tools = append(tools, "Write", "Edit")
	}
	if !c.Exec.IsZero() && !c.Exec.None {
		cmds := append([]string(nil), c.Exec.Commands...)
		sort.Strings(cmds)
		for _, cmd := range cmds {
			tools = append(tools, fmt.Sprintf("Bash(%s:*)", cmd))
		}
	}
	eg := c.Network.Egress
	if !eg.IsZero() && !eg.None {
		hosts := append([]string(nil), eg.Hosts...)
		sort.Strings(hosts)
		for _, h := range hosts {
			if h == "*" {
				tools = append(tools, "WebFetch")
				continue
			}
			tools = append(tools, fmt.Sprintf("WebFetch(domain:%s)", h))
		}
	}
	return tools
}

// EnsureSkillAllowedTools inserts an `allowed-tools` line into a SKILL.md
// frontmatter block that lacks one. Content without frontmatter is
// returned unchanged (EnsureSkillFrontmatter runs first and owns that
// case); an existing allowed-tools line is the author's and wins — the
// exporter narrows nothing it did not write.
func EnsureSkillAllowedTools(content []byte, tools []string) []byte {
	if len(tools) == 0 {
		return content
	}
	if !bytes.HasPrefix(content, []byte("---\n")) && !bytes.HasPrefix(content, []byte("---\r\n")) {
		return content
	}
	head := content[bytes.IndexByte(content, '\n')+1:]
	end := bytes.Index(head, []byte("\n---"))
	if end < 0 {
		return content
	}
	block := head[:end]
	for _, line := range bytes.Split(block, []byte("\n")) {
		if bytes.HasPrefix(bytes.TrimSpace(line), []byte("allowed-tools:")) {
			return content
		}
	}
	insert := "allowed-tools: " + strings.Join(tools, ", ") + "\n"
	var out bytes.Buffer
	out.Write(content[:len(content)-len(head)+end+1]) // through the last frontmatter line
	out.WriteString(insert)
	out.Write(head[end+1:])
	return out.Bytes()
}

// SkillFrontmatterWithTools is EnsureSkillFrontmatter plus the embedded
// tool grants: generated frontmatter carries allowed-tools directly, and
// author-supplied frontmatter gains the key only when absent.
func SkillFrontmatterWithTools(content []byte, name, summary string, tools []string) []byte {
	if bytes.HasPrefix(content, []byte("---\n")) || bytes.HasPrefix(content, []byte("---\r\n")) {
		return EnsureSkillAllowedTools(content, tools)
	}
	return EnsureSkillAllowedTools(EnsureSkillFrontmatter(content, name, summary), tools)
}

// HostToolsNote renders the embedded-tools contract for prose targets
// (the agents-md section): what the host should grant, in one line.
func HostToolsNote(c manifest.Capabilities) string {
	tools := HostToolGrants(c)
	if len(tools) == 0 {
		return "Tools: none declared (pure-text agent)."
	}
	return "Tools (host built-ins per the declared capability contract): " + strings.Join(tools, ", ") + "."
}
