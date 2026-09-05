// `agentry init` (§11): scaffold a new agent workspace with a per-kind
// agent.yaml matching PLAN §5.2, README.md, a LICENSE placeholder, and the
// kind's entrypoint file. --from imports an existing SKILL.md or MCP
// server.json instead (cmd_init_from.go).
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/blaktron/agentry/pkg/manifest"
	"github.com/spf13/cobra"
)

// newInitCmd builds the init command.
func newInitCmd() *cobra.Command {
	var modes, kind, name, namespace string
	var ff fromFlags
	cmd := &cobra.Command{
		Use:   "init [dir]",
		Short: "Scaffold a new BriefAgent workspace (agent.yaml, README, LICENSE, AGENT.md)",
		Long: "Scaffold a new BriefAgent workspace: a manifest schema v2 agent.yaml declaring the\n" +
			"run modes, README.md, a LICENSE placeholder note, and the AGENT.md instruction file.\n\n" +
			"--modes lists the run modes the BriefAgent supports: autonomous (runs from its\n" +
			"files, writes its output and receipts, and ends), interactive (a chat session in\n" +
			"the terminal or the desktop app under exactly the same controls), or both,\n" +
			"comma-separated. Interactive adds an interactive: block with the opening message.\n\n" +
			"--from PATH imports an existing source instead of scaffolding: an Agent Skills\n" +
			"SKILL.md (with its sibling files) or an MCP registry server.json (declared under\n" +
			"capabilities.mcpServers with an exact pin and its secret names). The capability\n" +
			"contract is never guessed from the source: on a terminal the wizard asks, with\n" +
			"hints from the source as defaults; otherwise pass --egress and --secrets\n" +
			"(\"none\" allowed; --exec, --fs-read, --fs-write optional).",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("kind") {
				return errors.New("--kind was withdrawn with manifest schema v2 (kinds became run modes): pass --modes autonomous, --modes interactive, or --modes autonomous,interactive")
			}
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			if ff.from != "" {
				return runInitFrom(cmd, dir, name, namespace, ff)
			}
			return runInit(cmd, dir, modes, name, namespace)
		},
	}
	cmd.Flags().StringVar(&modes, "modes", string(manifest.ModeAutonomous), "run modes the BriefAgent supports: autonomous | interactive | autonomous,interactive")
	// The v1 flag stays parseable so an old habit gets a pointer to --modes
	// rather than cobra's "unknown flag".
	cmd.Flags().StringVar(&kind, "kind", "", "withdrawn: use --modes")
	_ = cmd.Flags().MarkHidden("kind")
	cmd.Flags().StringVar(&ff.from, "from", "", "import an existing SKILL.md or server.json (file or directory) instead of scaffolding")
	cmd.Flags().StringVar(&ff.egress, "egress", "", "with --from: egress hosts (comma-separated, \"*\", or none)")
	cmd.Flags().StringVar(&ff.secrets, "secrets", "", "with --from: secret env var NAMES (comma-separated, or none)")
	cmd.Flags().StringVar(&ff.exec, "exec", "", "with --from: commands the agent executes (comma-separated; default none)")
	cmd.Flags().StringVar(&ff.fsRead, "fs-read", "none", "with --from: filesystem read: none | workspace")
	cmd.Flags().StringVar(&ff.fsWrite, "fs-write", "none", "with --from: filesystem write: none | workspace")
	cmd.Flags().StringVar(&name, "name", "", "BriefAgent name (default: the directory basename)")
	cmd.Flags().StringVar(&namespace, "namespace", "", "owning namespace handle (default: derived from $USER)")
	return cmd
}

// runInit performs the scaffold.
func runInit(cmd *cobra.Command, dir, modesFlag, name, namespace string) error {
	modes, err := parseModes(modesFlag)
	if err != nil {
		return err
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", dir, err)
	}
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", absDir, err)
	}
	if _, err := os.Stat(filepath.Join(absDir, "agent.yaml")); err == nil {
		return fmt.Errorf("%s already contains an agent.yaml — refusing to overwrite", absDir)
	}

	if name == "" {
		name = filepath.Base(absDir)
	}
	if !manifest.ValidName(name) {
		return fmt.Errorf("agent name %q is invalid (lowercase [a-z0-9-], 1-64 chars); pass --name", name)
	}
	if namespace == "" {
		namespace = defaultNamespace()
	}
	if !manifest.ValidNamespace(namespace) {
		return fmt.Errorf("namespace %q is invalid (lowercase [a-z0-9-], 1-38 chars); pass --namespace", namespace)
	}

	files := scaffoldFiles(modes, name, namespace)
	var created []string
	for _, f := range files {
		full := filepath.Join(absDir, filepath.FromSlash(f.path))
		if _, err := os.Stat(full); err == nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "keeping existing %s\n", f.path)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(f.content), 0o644); err != nil { //nolint:gosec // scaffold content is world-readable by design
			return fmt.Errorf("write %s: %w", f.path, err)
		}
		created = append(created, f.path)
	}

	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "initialized BriefAgent %s/%s (modes: %s) in %s\n", namespace, name, modesLabel(modes), absDir)
	for _, p := range created {
		_, _ = fmt.Fprintf(out, "  created %s\n", p)
	}
	_, _ = fmt.Fprintln(out, "next steps:")
	_, _ = fmt.Fprintln(out, "  1. edit agent.yaml (summary, capabilities) and the instructions in AGENT.md")
	_, _ = fmt.Fprintln(out, "  2. replace LICENSE with the full license text")
	_, _ = fmt.Fprintln(out, "  3. agentry validate "+dir)
	return nil
}

// invalidHandleChars strips anything outside the handle grammar.
var invalidHandleChars = regexp.MustCompile(`[^a-z0-9-]+`)

// defaultNamespace derives a plausible namespace from $USER, falling back to
// "example". It is a scaffold default only; validate/publish enforce the
// real ownership rules.
func defaultNamespace() string {
	u := strings.ToLower(os.Getenv("USER"))
	u = invalidHandleChars.ReplaceAllString(u, "")
	u = strings.Trim(u, "-")
	if manifest.ValidNamespace(u) {
		return u
	}
	return "example"
}

// scaffoldFile is one file the scaffold writes.
type scaffoldFile struct {
	path    string
	content string
}

// scaffoldFiles returns the scaffold set (PLAN §5.1/§5.2 layout, manifest
// schema v2): the manifest, the README, the LICENSE note, and AGENT.md.
func scaffoldFiles(modes []manifest.Mode, name, namespace string) []scaffoldFile {
	return []scaffoldFile{
		{"agent.yaml", agentYAML(modes, name, namespace)},
		{"README.md", readmeMD(name, namespace)},
		{"LICENSE", licenseNote},
		{manifest.DefaultInstructionFile, agentMD(name)},
	}
}

// todoSummary is the placeholder summary; AGENT.md's frontmatter repeats it
// so V9 (frontmatter vs manifest) is quiet until the author edits both.
const todoSummary = "TODO one line (at most 140 chars) saying what this BriefAgent does"

// agentYAML renders the schema v2 manifest template (PLAN §5.2): every mode
// field is present, commented out where the chosen modes do not use it.
func agentYAML(modes []manifest.Mode, name, namespace string) string {
	var b strings.Builder
	_, _ = fmt.Fprintf(&b, `schemaVersion: "2"
name: %s
namespace: %s
# Run modes this BriefAgent supports, one or both:
#   autonomous  — runs from its files (after any intake questions), writes
#                 its output and receipts, and ends
#   interactive — the same plan and the same controls, run as a chat session
#                 in the terminal or the desktop app: the agent can ask, the
#                 user can answer and add read-only inputs, and nobody can
#                 widen the capabilities declared below
modes: [%s]
version: 0.1.0
summary: %q
license: Apache-2.0

# The instruction file (AGENT.md when omitted). A leading YAML frontmatter
# block is stripped; the body is what the harness runs.
entrypoints:
  instruction: AGENT.md

# Questions asked before a run starts, in either mode; the answers are
# staged read-only for the agent at /inputs/intake.json. At most 20:
# intake:
#   - id: dataset
#     prompt: "Path to the dataset to analyse"
#     type: file            # text (default) | secret | choice | file
#     required: true
`, name, namespace, strings.Join(modeStrings(modes), ", "), todoSummary)
	if slices.Contains(modes, manifest.ModeInteractive) {
		b.WriteString(`
# Hints for interactive sessions. Budgets and timeouts are the operator's
# runner policy, which a manifest can only tighten.
interactive:
  opening: "What should I look at?"
  # idleTimeoutSeconds: 900
`)
	} else {
		b.WriteString(`
# With interactive in modes, an interactive block hints the session:
# interactive:
#   opening: "What should I look at?"
#   idleTimeoutSeconds: 900
`)
	}
	b.WriteString(`
# The agentry install targets this package supports, independent of mode;
# the first listed is the default (claude-skill: ~/.claude/skills/<name>,
# agents-md: a section in ./AGENTS.md).
runtime:
  interfaces: [claude-skill, agents-md]

# capabilities is the security contract (closed vocabulary, PLAN 5.4).
# Nothing declared means "pure text": no network, filesystem, exec, or
# secrets. Declare only what the agent really needs, for example:
# capabilities:
#   network:
#     egress: [api.example.com]
#   secrets: [EXAMPLE_TOKEN]

publish:
  include: [agent.yaml, README.md, AGENT.md, LICENSE]
`)
	return b.String()
}

// readmeMD renders the scaffold README (required for public publish, V7).
func readmeMD(name, namespace string) string {
	return fmt.Sprintf(`# %s

TODO describe this BriefAgent. This file renders on the BriefAgent page and is
required for public publishing (validation rule V7).

## Install

    agentry install %s/%s
`, name, namespace, name)
}

// licenseNote is the LICENSE placeholder written by init.
const licenseNote = `PLACEHOLDER LICENSE NOTE

agent.yaml declares the SPDX id "Apache-2.0". Replace this file with the
full text of the license you actually chose before publishing publicly
(rule V7 requires a LICENSE file consistent with the declared SPDX id).
`

// agentMD renders the instruction file with Agent Skills-style frontmatter
// (name must match the manifest, V9) so the same file serves the
// claude-skill install target unchanged.
func agentMD(name string) string {
	return fmt.Sprintf(`---
name: %s
description: %s
---

# %s

TODO write the instructions the agent follows. It receives one task, does one
job, and finishes; in an interactive session it can ask for what it needs.
`, name, todoSummary, name)
}

// skillMD renders an Agent Skills-shaped SKILL.md for an import whose
// source ships none (server.json); the manifest names it as the
// instruction file.
func skillMD(name string) string {
	return fmt.Sprintf(`---
name: %s
description: TODO describe when a consuming agent should use this skill
---

# %s

TODO write the instructions the consuming agent will follow.
`, name, name)
}
