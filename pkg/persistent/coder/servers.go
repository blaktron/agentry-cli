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
// adds as their own: Semgrep, whose rules Agentry may not ship, so it runs
// under the person's own use of Semgrep's terms. It is a subcommand of the
// semgrep package's CLI and reaches semgrep.dev alone, its metrics and its
// OpenTelemetry export switched off (their hosts are refused either way).
// It joins a network, so it is never given the agent's projects, and it is
// given only the tools that work on code passed in the call: a scan with a
// rule the agent writes, the languages, and a syntax tree. Its plain scan
// and supply-chain scan read files from disk, its rule schema is fetched
// from a host it does not reach, and its findings need a Semgrep account.
func SuggestedServers() []offer.Suggestion {
	return []offer.Suggestion{
		{
			Key:  "semgrep",
			Name: "Code scanning (Semgrep)",
			Server: manifest.MCPServer{
				PyPI: "semgrep", Version: "1.182.0", Args: []string{"mcp"},
				Egress: []string{"semgrep.dev"},
				Env:    map[string]string{"SEMGREP_SEND_METRICS": "off", "OTEL_SDK_DISABLED": "true"},
				Tools:  []string{"semgrep_scan_with_custom_rule", "get_supported_languages", "get_abstract_syntax_tree"},
			},
			Note: "Agentry ships no Semgrep rules: it scans with the rules the agent writes, under Semgrep's terms, and it is sent the code the agent passes to its tools, never the agent's projects",
		},
	}
}
