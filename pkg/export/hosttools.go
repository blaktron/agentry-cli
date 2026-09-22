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

// SetSkillAllowedTools makes a SKILL.md frontmatter's top-level
// allowed-tools line the compiled grants: the declared capability
// contract, never the author's (agentry-cli#195, RUNNER-PLAN §R5). An
// author's own line (or list) that differs is replaced and kept as a
// comment naming what was dropped; with no grants it is removed, so the
// host pre-approves nothing the manifest did not declare. Content without
// frontmatter is returned unchanged (EnsureSkillFrontmatter runs first and
// owns that case).
func SetSkillAllowedTools(content []byte, tools []string) []byte {
	nl := "\n"
	switch {
	case bytes.HasPrefix(content, []byte("---\r\n")):
		nl = "\r\n"
	case !bytes.HasPrefix(content, []byte("---\n")):
		return content
	}
	lines := strings.SplitAfter(string(content), "\n")
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r\n") == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return content
	}
	want := ""
	if len(tools) > 0 {
		want = "allowed-tools: " + strings.Join(tools, ", ") + nl
	}
	start, stop := -1, -1
	for i := 1; i < end; i++ {
		if strings.HasPrefix(lines[i], "allowed-tools:") {
			start, stop = i, i+1
			for stop < end && yamlContinuation(lines[stop]) {
				stop++
			}
			break
		}
	}
	var out strings.Builder
	if start < 0 {
		if want == "" {
			return content
		}
		out.WriteString(strings.Join(lines[:end], ""))
		out.WriteString(want)
		out.WriteString(strings.Join(lines[end:], ""))
		return []byte(out.String())
	}
	if strings.Join(lines[start:stop], "") == want {
		return content
	}
	out.WriteString(strings.Join(lines[:start], ""))
	out.WriteString("# The skill file's own allowed-tools, replaced by the grants compiled" + nl)
	out.WriteString("# from the declared capability contract:" + nl)
	for _, l := range lines[start:stop] {
		out.WriteString("# " + l)
	}
	out.WriteString(want)
	out.WriteString(strings.Join(lines[stop:], ""))
	return []byte(out.String())
}

// yamlContinuation reports whether a frontmatter line continues the key
// above it: an indented line, or a block-sequence item at column 0.
func yamlContinuation(line string) bool {
	return strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "-")
}

// SkillFrontmatterWithTools is EnsureSkillFrontmatter plus the embedded
// tool grants: the allowed-tools line is the compiled contract whether the
// frontmatter is generated or the author's.
func SkillFrontmatterWithTools(content []byte, name, summary string, tools []string) []byte {
	if bytes.HasPrefix(content, []byte("---\n")) || bytes.HasPrefix(content, []byte("---\r\n")) {
		return SetSkillAllowedTools(content, tools)
	}
	return SetSkillAllowedTools(EnsureSkillFrontmatter(content, name, summary), tools)
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
