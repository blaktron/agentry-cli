// Package offer is the shape of an MCP server a persistent built-in comes
// with: an offered server, named in the built-in's own Go value rather than
// its manifest, so the manifest's contract is unchanged. Each one is an
// entry of the agent's roster, switched, agreed to and receipted as a
// server the person added is, on by default only when it needs nothing the
// person must give it; one that needs a stored secret or a git host account
// waits, off, until the person sets that up.
package offer

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/blaktron/agentry/pkg/manifest"
)

// AccountSecretPrefix begins the secret slot an account-bound server is
// given its account's token in, one a server of each account: a prefix the
// runner sets alone, which a server the person adds may not declare, so no
// other server can be given the token.
const AccountSecretPrefix = "AGENTRY_GIT_ACCOUNT_"

// Server is one server a built-in offers.
type Server struct {
	// Name is how the roster names it ("Weather (Open-Meteo)").
	Name string
	// Server is the server, as a manifest declares one: an npm or PyPI
	// package at an exact version, or a remote address.
	Server manifest.MCPServer
	// Writes, when set, is the same server with its changes allowed (a
	// remote's read-write address): a switch of the entry of its own,
	// off by default, chooses it.
	Writes *manifest.MCPServer
	// On is its default: true only when Secret and Account are empty.
	On bool
	// Secret is the stored secret it needs, one of the secrets Server
	// declares: until the person stores it and has it on, the server
	// waits for it.
	Secret string
	// Account is the git host whose account's token the server is given,
	// in the one secret it declares, which begins AccountSecretPrefix:
	// until the agent has an account there and has it on, the server waits
	// for it.
	Account string
	// Note is what its consent sentence adds after what it reaches: the
	// provider's terms, what it is sent.
	Note string
}

// GitHost is one of the agent's git host accounts as a built-in draws an
// offer for it: its host (host[:port], lower case) and its kind ("github",
// "gitlab", "gitea" or "git").
type GitHost struct {
	Host string
	Kind string
}

// Check refuses a server a built-in may not offer (what a QuickAgent may
// not declare at all, the manifest's validation refuses):
//   - a package that reaches any host ("*"), or none it names;
//   - a remote that names hosts beside its own;
//   - a default of on for one that needs a secret or an account;
//   - a secret it needs and does not declare;
//   - an account server that does not declare exactly one secret, beginning
//     AccountSecretPrefix;
//   - a Writes that is another server.
func (s Server) Check() error {
	srv := s.Server
	if s.Name == "" {
		return errors.New("an offered server must have a name")
	}
	if err := checkEgress(srv); err != nil {
		return fmt.Errorf("%s: %w", s.Name, err)
	}
	if s.On && (s.Secret != "" || s.Account != "") {
		return fmt.Errorf("%s needs a secret or an account, so it is off until it has one", s.Name)
	}
	if s.Secret != "" && !slices.Contains(srv.Secrets, s.Secret) {
		return fmt.Errorf("%s needs the secret %s, which it does not declare", s.Name, s.Secret)
	}
	if s.Secret != "" && s.Account != "" {
		return fmt.Errorf("%s needs a secret or an account, not both", s.Name)
	}
	if s.Account != "" && (len(srv.Secrets) != 1 || !strings.HasPrefix(srv.Secrets[0], AccountSecretPrefix)) {
		return fmt.Errorf("%s is given its account's token in the one secret it declares, which begins %s", s.Name, AccountSecretPrefix)
	}
	if s.Account == "" && slices.ContainsFunc(srv.Secrets, func(n string) bool { return strings.HasPrefix(n, AccountSecretPrefix) }) {
		return fmt.Errorf("%s declares an account's secret and has no account", s.Name)
	}
	if w := s.Writes; w != nil {
		if !w.IsRemote() || !srv.IsRemote() || w.RemoteHost() != srv.RemoteHost() || !slices.Equal(w.Secrets, srv.Secrets) || w.Auth != srv.Auth {
			return fmt.Errorf("%s's writes are the same remote server at another address on its host", s.Name)
		}
	}
	return nil
}

// numericTLD says a name ends in a number: an address, not a host name.
func numericTLD(name string) bool {
	last := name[strings.LastIndexByte(name, '.')+1:]
	return strings.Trim(last, "0123456789") == ""
}

var hostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// checkEgress refuses a server whose reach is not a list of named hosts: a
// remote reaches its own host, a package the hosts it declares, each a host
// name with no wildcard.
func checkEgress(srv manifest.MCPServer) error {
	if srv.IsRemote() {
		if len(srv.Egress) > 0 {
			return errors.New("a remote server reaches its own host alone")
		}
		return nil
	}
	if len(srv.Egress) == 0 {
		return errors.New("an offered package server names the hosts it reaches")
	}
	for _, h := range srv.Egress {
		name := h
		if host, port, ok := strings.Cut(h, ":"); ok && port != "" {
			name = host
		}
		if strings.Contains(h, "*") || !hostRE.MatchString(name) || numericTLD(name) {
			return fmt.Errorf("its egress %q is not a host name", h)
		}
	}
	return nil
}

// CheckAll refuses a built-in's offered servers when one is refused, when
// two are the same server, or when two are given the same account secret.
func CheckAll(servers []Server, id func(manifest.MCPServer) string) error {
	ids, slots := map[string]bool{}, map[string]bool{}
	for _, s := range servers {
		if err := s.Check(); err != nil {
			return err
		}
		if ids[id(s.Server)] {
			return fmt.Errorf("%s is offered twice", s.Name)
		}
		ids[id(s.Server)] = true
		if s.Account != "" {
			if slots[s.Server.Secrets[0]] {
				return fmt.Errorf("%s is given the same account secret as another server", s.Name)
			}
			slots[s.Server.Secrets[0]] = true
		}
	}
	return nil
}

// Suggestion is a server a built-in suggests rather than offers: not on its
// roster until the person adds it, in one step, as a server of their own,
// with its install and agreement as for any server they add. Removing it
// takes it off the roster; it stays suggested. A suggestion is for a server
// whose terms or reach suit a person's own choice, not a default: one whose
// rules may not be shipped, or whose free service asks that a product not
// send its users there.
type Suggestion struct {
	// Key is how the person names it to add it ("weather").
	Key string
	// Name is how it is listed ("Weather (Open-Meteo)").
	Name string
	// Server is the server, as a manifest declares one.
	Server manifest.MCPServer
	// Note is what its sentence adds once added: whose terms apply, what
	// it is sent.
	Note string
}

var keyRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// Check refuses a suggestion a built-in may not make: one with no key or
// name, or that reaches any host or none it names, as an offered server.
func (s Suggestion) Check() error {
	if !keyRE.MatchString(s.Key) || s.Name == "" {
		return fmt.Errorf("a suggested server has a key of lower-case letters, digits and hyphens and a name (%q, %q)", s.Key, s.Name)
	}
	if err := checkEgress(s.Server); err != nil {
		return fmt.Errorf("%s: %w", s.Name, err)
	}
	return nil
}

// BraveSearch is web search with Brave, offered to either agent: off until
// the person stores their own Brave Search API key for it.
func BraveSearch() Server {
	return Server{
		Name:   "Web search (Brave)",
		Server: manifest.MCPServer{NPM: "@brave/brave-search-mcp-server", Version: "2.1.4", Egress: []string{"api.search.brave.com"}, Secrets: []string{"BRAVE_API_KEY"}},
		Secret: "BRAVE_API_KEY",
		Note:   "it sends its searches to Brave with your own Brave Search API key, under Brave's terms for that key",
	}
}
