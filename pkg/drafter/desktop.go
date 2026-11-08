package drafter

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/blaktron/agentry/pkg/appserver"
	"github.com/blaktron/agentry/pkg/manifest"
)

// The Drafter's rule on an application on the person's computer: a run
// reaches one only through capabilities.desktop, naming the catalogue's id,
// which Agentry Desktop answers on the person's computer. A declared MCP
// server for such an application runs in the sandbox, where the
// application is not, so a draft never declares one.

// desktopServerWords are names, or parts of a package's name or a remote
// server's address, of servers for the catalogue's applications, beside
// each catalogue entry's own package name.
var desktopServerWords = map[string][]string{
	"blender": {"blender-mcp", "mcp-for-blender", "blendermcp"},
	"unreal":  {"unreal-mcp", "unrealmcp", "unreal-engine-mcp"},
}

// desktopServerProblems is one line per declared MCP server that is a
// server for an application of the catalogue, by its package or its
// address: it names the application and the line to declare instead. Nil
// when there is none, or the manifest does not parse (validate says so).
func desktopServerProblems(raw json.RawMessage) []string {
	m, err := manifest.Parse(raw)
	if err != nil {
		return nil
	}
	var out []string
	for _, srv := range m.Capabilities.MCPServers {
		name := strings.ToLower(srv.NPM + " " + srv.PyPI + " " + srv.URL + " " + srv.SourceID())
		for _, e := range appserver.Builtin().All() {
			words := append([]string{strings.ToLower(e.Package.Name)}, desktopServerWords[e.ID]...)
			for _, w := range words {
				if w != "" && strings.Contains(name, w) {
					out = append(out, fmt.Sprintf("capabilities.mcpServers: %s is a server for %s, which runs on the person's computer; a declared server runs in the sandbox and never reaches it. Take it out and declare capabilities.desktop.servers: [{\"id\": %q}] instead (manifest/desktop.md).",
						serverLabel(srv), e.Application, e.ID))
					break
				}
			}
		}
	}
	return out
}

// serverLabel names a declared server for a line: its package, else its
// address, else its name.
func serverLabel(srv manifest.MCPServer) string {
	switch {
	case srv.NPM != "":
		return "npm:" + srv.NPM
	case srv.PyPI != "":
		return "pypi:" + srv.PyPI
	case srv.URL != "":
		return srv.URL
	}
	return srv.SourceID()
}
