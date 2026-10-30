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
// in its sites. Nil when the manifest does not parse, which validate
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
	for _, miss := range toolgrant.Check(m.Capabilities, instructions) {
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
		out = append(out, fmt.Sprintf("the instructions: %s need %s: %s Add %s (%s).",
			strings.Join(n.tools, ", "), path, n.reason, path, grantGuide(n.grant)))
	}
	return out
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
	return "manifest/manifest.md"
}
