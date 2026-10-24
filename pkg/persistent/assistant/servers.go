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

// GogKey is the Google Workspace server's key: its roster entry is
// server:gog and its volume ends -gog.
const GogKey = "gog"

// GogHome is where gog keeps its state, its sign-ins and its encrypted
// keyring, on the server's own volume.
const GogHome = "/persist/gogcli"

// GogKeyringSecret is the slot gog reads its file keyring's password from.
const GogKeyringSecret = "GOG_KEYRING_PASSWORD"

// GogEgress are the hosts gog's MCP tools reach: the token endpoint, Gmail,
// Calendar and Drive (www.googleapis.com), Docs and Sheets.
var GogEgress = []string{"oauth2.googleapis.com", "gmail.googleapis.com", "www.googleapis.com", "docs.googleapis.com", "sheets.googleapis.com"}

// GogServices are the Google services the sign-in asks for: those gog's MCP
// mode serves.
var GogServices = []string{"gmail", "calendar", "drive", "docs", "sheets"}

// ImageServers are the servers the agent comes with that run from an image:
// Google Workspace through gog's MCP mode, from image (the policy's
// images.gog), off until the person signs it in with their own Google Cloud
// client. Read-only unless "Allow changes" is on; sending mail is a switch
// of its own, given only with changes, since gog refuses it alone.
func ImageServers(image string) []offer.ImageServer {
	return []offer.ImageServer{{
		Key:          GogKey,
		Name:         "Google Workspace (gog)",
		Image:        image,
		Command:      []string{"mcp"},
		Env:          map[string]string{"GOG_HOME": GogHome, "GOG_KEYRING_BACKEND": "file"},
		Egress:       GogEgress,
		Secrets:      []string{GogKeyringSecret},
		VolumeTarget: GogHome,
		Switches: []offer.ImageSwitch{
			{Key: "writes", Name: "Google Workspace: allow changes", Args: []string{"--allow-write"}, SignedFor: true,
				Line: "Lets Google Workspace (gog) change what your Google account holds: write Docs and Sheets, make Gmail drafts, label, archive, trash and restore mail, while it is on"},
			{Key: "send", Name: "Google Workspace: allow sending mail", Args: []string{"--allow-gmail-send"}, Requires: "writes", SignedFor: true,
				Line: "Lets Google Workspace (gog) send mail and drafts from your Gmail account, while it and its changes are on"},
		},
		Note:       "gog's MCP server, signed in with your own Google Cloud client: it reads Gmail, Drive, Docs, Sheets and your Calendar's events, read only unless you allow changes; it serves nothing of Contacts, Tasks, Slides, Forms or Calendar changes",
		SignIn:     "it needs your Google sign-in: agentry agent configure assistant --google-client <client file>, then --google-sign-in <email>",
		AccountEnv: "GOG_ACCOUNT",
	}}
}

// MailGoogle is the Google mail provider's key: gog's Google Workspace.
const MailGoogle = "google"

// MailProviders are the services whose mail and calendar the agent reaches,
// each through its own image servers; the person enables the ones they use.
func MailProviders() []offer.MailProvider {
	return []offer.MailProvider{{Key: MailGoogle, Name: "Google", ImageServers: []string{GogKey}}}
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
