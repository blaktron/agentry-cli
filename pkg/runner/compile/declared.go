package compile

import (
	"sort"

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// DeclaredServerGatewayFiles renders the gateway's catalog.yaml,
// registry.yaml, config.yaml and tools.yaml for declared MCP servers alone,
// each catalog entry exactly as a run renders it (mcpServerEntry), keyed by
// file name. It is for a tool that launches declared servers the way a run
// does with no QuickAgent around them: the MCP server survey
// (scripts/mcp-server-survey, agentry-cli#658). The volume still names
// spec.PHServers; the caller substitutes its server store, as the engine
// does.
func DeclaredServerGatewayFiles(servers []spec.MCPServerSpec) (map[string][]byte, error) {
	files := map[string][]byte{}
	catalog := yamlMap{}
	enabled := yamlMap{}
	tools := yamlMap{}
	sorted := append([]spec.MCPServerSpec(nil), servers...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, srv := range sorted {
		catalog.set(srv.Name, mcpServerEntry(srv))
		e := yamlMap{}
		e.set("ref", strNode(""))
		enabled.set(srv.Name, e.node())
		if len(srv.Tools) > 0 {
			tools.set(srv.Name, strListNode(sortedCopy(srv.Tools)))
		}
	}
	doc := yamlMap{}
	doc.set("registry", catalog.node())
	b, err := marshalYAML("# generated — gateway server catalog, declared servers only\n", doc.node())
	if err != nil {
		return nil, err
	}
	files["catalog.yaml"] = b
	reg := yamlMap{}
	reg.set("registry", enabled.node())
	if b, err = marshalYAML("# generated — enabled servers\n", reg.node()); err != nil {
		return nil, err
	}
	files["registry.yaml"] = b
	files["config.yaml"] = []byte("# generated — per-server config values (none)\n{}\n")
	if b, err = marshalYAML("# generated — per-server tool allowlist\n", tools.node()); err != nil {
		return nil, err
	}
	files["tools.yaml"] = b
	return files, nil
}
