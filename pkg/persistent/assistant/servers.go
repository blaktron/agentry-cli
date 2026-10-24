package assistant

import (
	"embed"
	"strings"

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

// MS365Key is the Microsoft 365 server's key: its roster entry is
// server:ms365 and its volume ends -ms365.
const MS365Key = "ms365"

// MS365Home is where the Microsoft 365 server keeps its state, its token
// cache sealed under MS365CacheKey's value, on the server's own volume.
const MS365Home = "/persist/ms365"

// MS365CacheKey is the slot the server's cache keeper reads its key from.
const MS365CacheKey = "MS365_CACHE_KEY"

// MS365Egress are the hosts the Microsoft 365 server reaches: Microsoft's
// sign-in, for its tokens' refresh, and Microsoft Graph.
var MS365Egress = []string{"login.microsoftonline.com", "graph.microsoft.com"}

// ms365Tools holds the Microsoft 365 server's allow-lists, one tool a line:
// read.txt the tools it is given read only (mail, calendar, contacts and
// OneDrive, reading), changes.txt those "Allow changes" adds (drafts, moving
// and deleting mail, calendar events and replies to invitations, contacts),
// send.txt those "Allow sending mail" adds (sending, replying, forwarding,
// and forwarding an event, which mails it to new people). Every other tool
// of the server, mail rules, mailbox settings, calendar permissions,
// OneDrive's writes and shares, uploads and downloads among them, is in none.
//
//go:embed ms365/*.txt
var ms365Tools embed.FS

// ms365List is one allow-list's tools.
func ms365List(name string) []string {
	b, err := ms365Tools.ReadFile("ms365/" + name + ".txt")
	if err != nil {
		panic(err)
	}
	return strings.Fields(string(b))
}

// MS365Tools are the tools the Microsoft 365 server is given with changes
// and sending as switched: the read list, then changes', then sending's.
func MS365Tools(changes, send bool) []string {
	out := ms365List("read")
	if changes {
		out = append(out, ms365List("changes")...)
		if send {
			out = append(out, ms365List("send")...)
		}
	}
	return out
}

// ms365Args are the server's arguments for those tools: an anchored
// alternation of their names, which the server matches whole, and
// --read-only without changes, which also keeps its sign-in's scopes to
// reading.
func ms365Args(changes, send bool) []string {
	args := []string{"--enabled-tools", "^(" + strings.Join(MS365Tools(changes, send), "|") + ")$"}
	if !changes {
		args = append([]string{"--read-only"}, args...)
	}
	return args
}

// ImageServers are the servers the agent comes with that run from an image:
// Google Workspace through gog's MCP mode, from gog (the policy's
// images.gog), off until the person signs it in with their own Google Cloud
// client, read-only unless "Allow changes" is on, sending mail a switch of
// its own given only with changes, since gog refuses it alone; and
// Microsoft 365 and Outlook.com through Softeria's server, from ms365 (the
// policy's images.ms365), off until the person signs it in with their own
// Entra client, given the tools of its switches by name.
func ImageServers(gog, ms365 string) []offer.ImageServer {
	return []offer.ImageServer{{
		Key:          GogKey,
		Name:         "Google Workspace (gog)",
		Image:        gog,
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
	}, {
		Key:          MS365Key,
		Name:         "Microsoft 365 and Outlook.com",
		Image:        ms365,
		Egress:       MS365Egress,
		Secrets:      []string{MS365CacheKey},
		VolumeTarget: MS365Home,
		Switches: []offer.ImageSwitch{
			{Key: "writes", Name: "Microsoft 365: allow changes", SignedFor: true,
				Line: "Lets Microsoft 365 and Outlook.com change what your Microsoft account holds: make mail drafts, move, file and delete mail, make, change and cancel calendar events and answer invitations, and change your contacts, while it is on; a calendar change can email invitations or updates to the people invited"},
			{Key: "send", Name: "Microsoft 365: allow sending mail", Requires: "writes", SignedFor: true,
				Line: "Lets Microsoft 365 and Outlook.com send mail, drafts, replies and forwards from your Microsoft account, and forward calendar events, while it and its changes are on"},
		},
		Profiles: map[string][]string{
			"":            ms365Args(false, false),
			"writes":      ms365Args(true, false),
			"writes+send": ms365Args(true, true),
		},
		Note:       "Softeria's Microsoft 365 server, signed in with your own Entra client: it reads your mail, calendar, contacts and OneDrive, read only unless you allow changes; it reaches nothing of Teams, SharePoint, OneNote or To Do, writes nothing to OneDrive, and keeps its sign-in sealed under a key in your keychain",
		SignIn:     "it needs your Microsoft sign-in: agentry agent configure assistant --microsoft-client <client id>, then --microsoft-sign-in <email>",
		ClientEnv:  "MS365_MCP_CLIENT_ID",
		TenantEnv:  "MS365_MCP_TENANT_ID",
		SignoffArg: "--message-signoff-suffix",
	}}
}

// MailGoogle is the Google mail provider's key: gog's Google Workspace.
const MailGoogle = "google"

// MailMicrosoft is the Microsoft mail provider's key: Microsoft 365 and
// Outlook.com.
const MailMicrosoft = "microsoft"

// MailProviders are the services whose mail and calendar the agent reaches,
// each through its own image servers; the person enables the ones they use.
func MailProviders() []offer.MailProvider {
	return []offer.MailProvider{
		{Key: MailGoogle, Name: "Google", ImageServers: []string{GogKey}},
		{Key: MailMicrosoft, Name: "Microsoft", ImageServers: []string{MS365Key}},
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
