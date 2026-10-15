// Package export holds the interop exporters: pure functions of (archive,
// manifest, target) that emit the claude-skill and agents-md install
// formats, and the two tree writers that lay them on disk from an
// extracted archive. Keeping the shaping side-effect free makes it
// table-testable without a registry, and lets any client — the CLI, the
// desktop app — produce byte-identical installs from the same inputs.
package export

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/blaktron/agentry/pkg/manifest"
	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry-cli/pkg/pathguard"
)

// EnsureSkillFrontmatter returns content guaranteed to start with YAML
// frontmatter. When the skill file already opens with a frontmatter fence it
// is returned unchanged; otherwise `name` and `description` (mapped from the
// manifest name and summary) are prepended.
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
// QuickAgent (the agents-md target).
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

// WriteClaudeSkillTree copies the release's files from staging into dest —
// the skill's directory, as ~/.claude/skills/<name> holds it — and gives
// the skill entrypoint its frontmatter. Embedded mode (RUNNER-PLAN §R5):
// the host coding agent supplies the tools, so the capability contract
// compiles to its allowed-tools vocabulary here — the same contract the
// standalone runner enforces through the gateway. The entrypoint is a
// manifest string; the file it names must be inside dest, whatever the
// string.
func WriteClaudeSkillTree(staging, dest string, m *manifest.Manifest) error {
	if err := CopyTree(staging, dest); err != nil {
		return err
	}
	skillRel := m.Entrypoints.Skill
	if skillRel == "" {
		skillRel = "SKILL.md"
	}
	skillPath := filepath.Join(dest, filepath.FromSlash(skillRel))
	if !pathguard.Under(dest, skillPath) {
		return fmt.Errorf("skill entrypoint %q resolves outside the install directory — refusing to write it", skillRel)
	}
	content, err := skillBody(staging, m, skillRel)
	if err != nil {
		return err
	}
	withFM := SkillFrontmatterWithTools(content, m.Name, m.Summary, HostToolGrants(m.Capabilities))
	if err := os.WriteFile(skillPath, withFM, 0o644); err != nil { //nolint:gosec // skill files are world-readable content
		return fmt.Errorf("write skill entrypoint: %w", err)
	}
	return nil
}

// skillBody is the skill's content, read from the verified release
// (staging) and never from a destination an earlier install may have left
// a file in: the skill file the manifest names; or, for a release with no
// skill entrypoint — a prompt agent, whose instruction is AGENT.md — its
// instruction as the runner reads it (the first of the manifest's
// InstructionCandidates it carries), else a SKILL.md it ships anyway. A
// release that names a skill file it does not carry is refused by that
// name, never patched over from something else; one with nothing to make a
// skill from is refused naming what was looked for.
func skillBody(staging string, m *manifest.Manifest, skillRel string) ([]byte, error) {
	if m.Entrypoints.Skill != "" {
		content, err := readUnder(staging, skillRel)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("the release names skill entrypoint %s but does not carry it", skillRel)
		}
		if err != nil {
			return nil, fmt.Errorf("read skill entrypoint %s: %w", skillRel, err)
		}
		return content, nil
	}
	var tried []string
	for _, rel := range append(m.InstructionCandidates(), skillRel) {
		if slices.Contains(tried, rel) {
			continue
		}
		tried = append(tried, rel)
		content, err := readUnder(staging, rel)
		if err == nil {
			return content, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("read instruction %s: %w", rel, err)
		}
	}
	return nil, fmt.Errorf("the release carries no instruction file and no skill file (looked for %s) to make a Claude skill from", strings.Join(tried, ", "))
}

// readUnder reads rel under root, refusing a rel that resolves outside
// it: an entrypoint is a manifest string, and the file it names must be
// inside the release whatever the string says.
func readUnder(root, rel string) ([]byte, error) {
	p := filepath.Join(root, filepath.FromSlash(rel))
	if !pathguard.Under(root, p) {
		return nil, fmt.Errorf("%q resolves outside the release — refusing to read it", rel)
	}
	return os.ReadFile(p) //nolint:gosec // under root, checked just above
}

// WriteAgentsMDTree copies the release's files from staging into dest —
// <project>/agents/<name> — and upserts the agent's section, with the note
// naming which host built-ins the declared contract corresponds to
// (embedded mode, RUNNER-PLAN §R5), into the AGENTS.md at agentsMDPath.
func WriteAgentsMDTree(staging, dest, agentsMDPath string, m *manifest.Manifest, ns, version string) error {
	if err := CopyTree(staging, dest); err != nil {
		return err
	}
	existing, err := os.ReadFile(agentsMDPath) //nolint:gosec // the project's own AGENTS.md
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", agentsMDPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(agentsMDPath), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(agentsMDPath), err)
	}
	updated := UpsertAgentsMDSectionNote(existing, ns, m.Name, version, m.Summary, dest, HostToolsNote(m.Capabilities))
	if err := os.WriteFile(agentsMDPath, updated, 0o644); err != nil { //nolint:gosec // project doc, not a secret
		return fmt.Errorf("write %s: %w", agentsMDPath, err)
	}
	return nil
}

// CopyTree copies src into dest, preserving each file's permission bits
// (the archive normalizes modes).
func CopyTree(src, dest string) error { return CopyTreeSkipping(src, dest, nil) }

// CopyTreeSkipping is CopyTree leaving out every directory skipDir names
// (a local tree's .git, a run's own output folder).
func CopyTreeSkipping(src, dest string, skipDir func(name string) bool) error {
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if skipDir != nil && info.IsDir() && p != src && skipDir(info.Name()) {
			return filepath.SkipDir
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dest, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, rerr := os.ReadFile(p) //nolint:gosec // reading the staging dir
		if rerr != nil {
			return rerr
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
	if err != nil {
		return fmt.Errorf("copy files to %s: %w", dest, err)
	}
	return nil
}
