package coder

import (
	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/persistent/offer"
)

// The account secrets GitHub's and GitLab's servers are signed in with.
const (
	githubAccountSecret = offer.AccountSecretPrefix + "GITHUB"
	gitlabAccountSecret = offer.AccountSecretPrefix + "GITLAB"
)

// OfferedServers are the MCP servers the agent comes with, each an entry
// of its roster: library documentation on by default, and the git hosts'
// servers and web search off until it has an account or a key.
func OfferedServers() []offer.Server {
	return []offer.Server{
		{
			Name:   "Library docs (Context7)",
			Server: manifest.MCPServer{NPM: "@upstash/context7-mcp", Version: "4.4.4", Egress: []string{"context7.com"}},
			On:     true,
			Note:   "it sends the libraries and questions it looks up to Context7 (Upstash), whose documentation comes from the libraries' communities and is not checked by Agentry; Context7's free limits apply",
		},
		{
			Name:    "GitHub",
			Server:  manifest.MCPServer{URL: "https://api.githubcopilot.com/mcp/readonly", Auth: manifest.AuthSecret, Secrets: []string{githubAccountSecret}},
			Writes:  &manifest.MCPServer{URL: "https://api.githubcopilot.com/mcp/", Auth: manifest.AuthSecret, Secrets: []string{githubAccountSecret}},
			Account: "github.com",
			Note:    "GitHub's own server, which reaches what the account's token reaches",
		},
		{
			Name:    "GitLab",
			Server:  manifest.MCPServer{URL: "https://gitlab.com/api/v4/mcp", Auth: manifest.AuthSecret, Secrets: []string{gitlabAccountSecret}},
			Account: "gitlab.com",
			Note:    "GitLab's own server, which can read and change what the account's token can: it takes a fine-grained token with the MCP server's Execute permission, and refuses a legacy token",
		},
		offer.BraveSearch(),
	}
}

// SuggestedServers are the MCP servers the agent suggests, which the person
// adds as their own.
func SuggestedServers() []offer.Suggestion { return nil }
