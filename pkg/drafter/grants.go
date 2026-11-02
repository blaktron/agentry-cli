package drafter

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/blaktron/agentry/pkg/manifest"
	"github.com/blaktron/agentry/pkg/toolgrant"
)

// grantProblems is the Drafter's rule on the runner tools a draft's
// instructions call, one line per grant the manifest lacks: a tool whose
// grant is not declared (the core's toolgrant reading, which publication
// holds as a high finding) is absent from the run, so an instruction that
// depends on it describes a run that cannot happen. Each line names the
// tools that need the grant, the line of the instructions that first calls
// each, and the manifest line to add, so the repair turn adds the grant: a
// browser's instructions need capabilities.browser with the url question
// in its sites. A persistent agent's instructions are read with its own
// tools too (toolgrant.CheckRelease): a tool of a capabilities.persistent
// key needs that key, and the subagent tools need a subagent in
// dependencies.agents. Nil when the manifest does not parse, which validate
// reports.
func grantProblems(raw json.RawMessage, instructions string) []string {
	m, err := manifest.Parse(raw)
	if err != nil {
		return nil
	}
	type need struct {
		tools  []string
		reason string
		grant  toolgrant.Grant
	}
	var order []string
	byPath := map[string]*need{}
	for _, miss := range toolgrant.CheckRelease(m, instructions) {
		n, ok := byPath[miss.Path]
		if !ok {
			n = &need{reason: miss.Reason, grant: miss.Grant}
			byPath[miss.Path] = n
			order = append(order, miss.Path)
		}
		n.tools = append(n.tools, fmt.Sprintf("%s (line %d)", miss.Tool, miss.Line))
	}
	out := make([]string, 0, len(order))
	for _, path := range order {
		n := byPath[path]
		out = append(out, fmt.Sprintf("the instructions: %s need %s: %s %s",
			strings.Join(n.tools, ", "), path, n.reason, grantFix(n.grant, path)))
	}
	return out
}

// grantFix is the sentence that ends a grant's line: the manifest line to
// add and the guide's page on it. A persistent agent's key may as well be
// left out with the tools that need it, and a subagent is not added without
// the registry search the repair turn lacks, so those lines say so.
func grantFix(g toolgrant.Grant, path string) string {
	switch {
	case g == toolgrant.GrantSubagents:
		return fmt.Sprintf("Declare a subagent at %s (%s) only if its ref, constraint and reason are already in the draft; otherwise take the subagent tools out of the instructions.",
			path, grantGuide(g))
	case strings.HasPrefix(path, "capabilities.persistent."):
		return fmt.Sprintf("Add %s (%s), or take the tools that need it out of the instructions if the task does not need them.", path, grantGuide(g))
	}
	return fmt.Sprintf("Add %s (%s).", path, grantGuide(g))
}

// grantGuide is the drafting guide's page on a grant.
func grantGuide(g toolgrant.Grant) string {
	switch g {
	case toolgrant.GrantBrowser:
		return "manifest/browser.md"
	case toolgrant.GrantExec:
		return "manifest/exec.md"
	case toolgrant.GrantEgress:
		return "manifest/egress-and-fetch.md"
	case toolgrant.GrantTargetHosts:
		return "manifest/target-hosts.md"
	case toolgrant.GrantMCPServer:
		return "servers/mcp-servers.md"
	}
	if strings.HasPrefix(string(g), "persistent.") || g == toolgrant.GrantSubagents {
		return "manifest/persistent.md"
	}
	return "manifest/manifest.md"
}
