// `agentry init` (§11): scaffold a new agent workspace with a per-kind
// agent.yaml matching PLAN §5.2, README.md, a LICENSE placeholder, and the
// kind's entrypoint file. Import wizards (--from=path|skill|mcp) are
// deferred to a later milestone.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/manifest"
)

// newInitCmd builds the init command.
func newInitCmd() *cobra.Command {
	var kind, name, namespace string
	cmd := &cobra.Command{
		Use:   "init [dir]",
		Short: "Scaffold a new agent workspace (agent.yaml, README, LICENSE, entrypoint)",
		Long: "Scaffold a new agent workspace: a per-kind agent.yaml (PLAN §5.2), README.md,\n" +
			"a LICENSE placeholder note, and the kind's entrypoint file.\n\n" +
			"Import wizards (--from=path|skill|mcp) are deferred to a later milestone.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			return runInit(cmd, dir, kind, name, namespace)
		},
	}
	cmd.Flags().StringVar(&kind, "kind", string(manifest.KindSkill), "agent kind: skill | prompt-agent | mcp-server")
	cmd.Flags().StringVar(&name, "name", "", "agent name (default: the directory basename)")
	cmd.Flags().StringVar(&namespace, "namespace", "", "owning namespace handle (default: derived from $USER)")
	return cmd
}

// runInit performs the scaffold.
func runInit(cmd *cobra.Command, dir, kind, name, namespace string) error {
	k := manifest.Kind(kind)
	switch k {
	case manifest.KindSkill, manifest.KindPromptAgent, manifest.KindMCPServer:
	default:
		return fmt.Errorf("unknown --kind %q (want skill, prompt-agent, or mcp-server)", kind)
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

	files := scaffoldFiles(k, name, namespace)
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
	_, _ = fmt.Fprintf(out, "initialized %s agent %s/%s in %s\n", k, namespace, name, absDir)
	for _, p := range created {
		_, _ = fmt.Fprintf(out, "  created %s\n", p)
	}
	_, _ = fmt.Fprintln(out, "next steps:")
	_, _ = fmt.Fprintln(out, "  1. edit agent.yaml (summary, capabilities) and the entrypoint content")
	_, _ = fmt.Fprintln(out, "  2. replace LICENSE with the full license text")
	_, _ = fmt.Fprintln(out, "  3. agentry validate "+dir)
	_, _ = fmt.Fprintln(out, "note: import wizards (--from=path|skill|mcp) are deferred to a later milestone")
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

// scaffoldFiles returns the per-kind scaffold set (PLAN §5.1/§5.2 layout).
func scaffoldFiles(k manifest.Kind, name, namespace string) []scaffoldFile {
	files := []scaffoldFile{
		{"agent.yaml", agentYAML(k, name, namespace)},
		{"README.md", readmeMD(name, namespace)},
		{"LICENSE", licenseNote},
	}
	switch k {
	case manifest.KindSkill:
		files = append(files, scaffoldFile{"SKILL.md", skillMD(name)})
	case manifest.KindPromptAgent:
		files = append(files, scaffoldFile{"prompts/main.md", promptMainMD(name)})
	case manifest.KindMCPServer:
		files = append(files,
			scaffoldFile{"src/server.js", serverJS(name)},
			scaffoldFile{"package-lock.json", packageLockJSON(name)},
		)
	}
	return files
}

// agentYAML renders the per-kind manifest template (PLAN §5.2).
func agentYAML(k manifest.Kind, name, namespace string) string {
	header := fmt.Sprintf(`schemaVersion: "1"
name: %s
namespace: %s
kind: %s
version: 0.1.0
summary: "TODO one line (at most 140 chars) saying what this agent does"
license: Apache-2.0
`, name, namespace, k)

	capabilities := `
# capabilities is the security contract (closed vocabulary, PLAN 5.4).
# Nothing declared means "pure text": no network, filesystem, exec, or
# secrets. Declare only what the agent really needs, for example:
# capabilities:
#   network:
#     egress: [api.example.com]
#   secrets: [EXAMPLE_TOKEN]
`

	switch k {
	case manifest.KindPromptAgent:
		return header + `runtime:
  interfaces: [agents-md]
` + capabilities + `
publish:
  include: [agent.yaml, README.md, prompts/**, LICENSE]
`
	case manifest.KindMCPServer:
		return header + `entrypoints:
  mcp:
    command: node
    args: [src/server.js]
    transport: stdio
runtime:
  interfaces: [mcp]
` + capabilities + `
publish:
  include: [agent.yaml, README.md, src/**, package-lock.json, LICENSE]
`
	default: // skill
		return header + `entrypoints:
  skill: SKILL.md
runtime:
  interfaces: [claude-skill]
` + capabilities + `
publish:
  include: [agent.yaml, README.md, SKILL.md, LICENSE]
`
	}
}

// readmeMD renders the scaffold README (required for public publish, V7).
func readmeMD(name, namespace string) string {
	return fmt.Sprintf(`# %s

TODO describe this agent. This file renders on the agent page and is
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

// skillMD renders the kind=skill entrypoint with agentskills-style
// frontmatter (name must match the installed directory name, §5.6).
func skillMD(name string) string {
	return fmt.Sprintf(`---
name: %s
description: TODO describe when a consuming agent should use this skill
---

# %s

TODO write the instructions the consuming agent will follow.
`, name, name)
}

// promptMainMD renders the kind=prompt-agent entrypoint.
func promptMainMD(name string) string {
	return fmt.Sprintf(`# %s — main prompt

You are %s, a single-purpose agent.

TODO write the prompt.
`, name, name)
}

// serverJS renders a dependency-free stdio MCP server stub (kind=mcp-server).
func serverJS(name string) string {
	return fmt.Sprintf(`// Minimal MCP stdio server stub scaffolded by `+"`agentry init`"+`.
// Replace with a real implementation; dependencies must be covered by
// package-lock.json (validation rule V6).
const readline = require("node:readline");

const rl = readline.createInterface({ input: process.stdin });
rl.on("line", (line) => {
  let msg;
  try {
    msg = JSON.parse(line);
  } catch {
    return;
  }
  if (msg.method === "initialize") {
    process.stdout.write(
      JSON.stringify({
        jsonrpc: "2.0",
        id: msg.id,
        result: { capabilities: {}, serverInfo: { name: %q, version: "0.1.0" } },
      }) + "\n",
    );
  }
});
`, name)
}

// packageLockJSON renders the mandatory lockfile for kind=mcp-server (V6).
func packageLockJSON(name string) string {
	return fmt.Sprintf("{\"name\":%q,\"lockfileVersion\":3,\"packages\":{}}\n", name)
}
