package assistant

import (
	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/persistent/offer"
)

// userAgent names the agent to the public services it calls, with a way to
// reach its makers, as their policies ask.
const userAgent = "Agentry-Assistant (https://secureagentry.net)"

// OfferedServers are the MCP servers the agent comes with, each an entry
// of its roster: Wikipedia on by default, web search off until it has a
// key.
func OfferedServers() []offer.Server {
	return []offer.Server{
		{
			Name: "Wikipedia",
			Server: manifest.MCPServer{NPM: "@cyanheads/wikipedia-mcp-server", Version: "0.2.6", Egress: []string{"en.wikipedia.org"},
				Env: map[string]string{"WIKIPEDIA_BASE_URL": "https://en.wikipedia.org", "WIKIPEDIA_USER_AGENT": userAgent}},
			On:   true,
			Note: "it reads English Wikipedia, whose articles are its editors' and not checked by Agentry",
		},
		offer.BraveSearch(),
	}
}

// SuggestedServers are the MCP servers the agent suggests, which the person
// adds as their own: free public services whose terms suit a person's own
// use, not a product's default for all its users.
func SuggestedServers() []offer.Suggestion {
	return []offer.Suggestion{
		{
			Key:    "weather",
			Name:   "Weather (Open-Meteo)",
			Server: manifest.MCPServer{NPM: "open-meteo-mcp-server", Version: "2.5.4", Egress: []string{"api.open-meteo.com", "geocoding-api.open-meteo.com"}},
			Note:   "Open-Meteo's free service is for non-commercial use, so this is for your own use, under its terms",
		},
		{
			Key:  "maps",
			Name: "Maps (OpenStreetMap)",
			Server: manifest.MCPServer{NPM: "@cyanheads/openstreetmap-mcp-server", Version: "0.6.1", Egress: []string{"nominatim.openstreetmap.org", "overpass-api.de"},
				Env: map[string]string{"OSM_USER_AGENT": userAgent}},
			Note: "OpenStreetMap's public servers ask for light use (at most one search a second) and credit to OpenStreetMap's contributors for what it returns",
		},
	}
}
