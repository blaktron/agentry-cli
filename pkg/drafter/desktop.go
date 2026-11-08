package drafter

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/blaktron/agentry/pkg/manifest"
)

// The Drafter's rule on an application on the person's computer: a run
// reaches one only through capabilities.desktop, naming the application by
// its id, which Agentry Desktop answers on the person's computer. A
// declared MCP server for such an application runs in the sandbox, where
// the application is not, so a draft never declares one.

// desktopServer is a well-known MCP server for an application on the
// person's computer: the application, the id a release names it by, and
// words in its packages' names or its address.
type desktopServer struct {
	application, id string
	words           []string
}

// desktopServers are the servers the Drafter recognizes when a draft
// declares one as a sandboxed server.
var desktopServers = []desktopServer{
	{"Blender", "blender", []string{"mcp-for-blender", "blender-mcp", "blendermcp"}},
	{"Unreal Engine", "unreal", []string{"unreal-mcp", "unrealmcp", "unreal-engine-mcp"}},
}

// desktopServerProblems is one line per declared MCP server that is a
// server for an application on the person's computer, by its package or its
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
		for _, d := range desktopServers {
			for _, w := range d.words {
				if strings.Contains(name, w) {
					out = append(out, fmt.Sprintf("capabilities.mcpServers: %s is a server for %s, which runs on the person's computer; a declared server runs in the sandbox and never reaches it. Take it out and declare capabilities.desktop.servers: [{\"id\": %q}] instead (manifest/desktop.md).",
						serverLabel(srv), d.application, d.id))
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
