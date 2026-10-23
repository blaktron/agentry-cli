package persistent

// The agent's servers: those its built-in offers (offer.Server) and those
// the person added. An offered server is an entry of the roster like the
// person's, granted by the built-in, on by default when it needs nothing the
// person must give it. One that needs a stored secret or a git host account
// waits for it: on, it is "needs set-up" and no segment is given it until
// the secret or the account is there and on. An offered remote that can
// change things has a second entry, its writes, off by default; with it on
// the segment is given the read-write address.

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/persistent/offer"
)

// KindServerWrites is the kind of an offered server's writes entry.
const KindServerWrites EntryKind = "server-writes"

// writesSuffix ends the id of an offered server's writes entry.
const writesSuffix = ":writes"

// StateNeedsSetUp: an offered server that is on and waits for a stored
// secret or a git host account (Reason says which).
const StateNeedsSetUp EntryState = "needs-set-up"

// agentServer is one of the agent's servers.
type agentServer struct {
	// Server is the server as it is installed and named: an offered
	// server's read-only address.
	Server manifest.MCPServer
	// Offered is the built-in's offer, or nil for a server the person
	// added.
	Offered *offer.Server
}

// ID is the server's roster id.
func (s agentServer) ID() string { return ServerEntryID(s.Server) }

// offersOf are the servers the agent's built-in offers for its config: its
// own, but in a test.
var offersOf = func(a Agent, c Config) []offer.Server { return builtinOffers(a, c) }

// builtinOffers are the built-in's offers for the agent's accounts, less one
// drawn for an account whose address a QuickAgent could not declare (an
// address, an internal name): it is never on the roster.
func builtinOffers(a Agent, c Config) []offer.Server {
	hosts := make([]offer.GitHost, 0, len(c.Accounts))
	for _, g := range c.Accounts {
		hosts = append(hosts, offer.GitHost{Host: g.Host, Kind: string(g.Kind)})
	}
	var out []offer.Server
	for _, o := range a.Builtin.OfferedServers(hosts) {
		if o.Account != "" && checkServer(o.Server) != nil {
			continue
		}
		out = append(out, o)
	}
	return out
}

// agentServers are the agent's servers: the built-in's offers, then the
// person's, a person's server that is one of the offers left out.
func agentServers(a Agent, c Config) []agentServer {
	var out []agentServer
	offered := map[string]bool{}
	for _, o := range offersOf(a, c) {
		out = append(out, agentServer{Server: o.Server, Offered: &o})
		offered[ServerEntryID(o.Server)] = true
	}
	for _, srv := range c.Servers {
		if !offered[ServerEntryID(srv)] {
			out = append(out, agentServer{Server: srv})
		}
	}
	return out
}

// offeredServer is the agent's offered server with roster id id, for its
// config c.
func offeredServer(a Agent, c Config, id string) (offer.Server, bool) {
	for _, o := range offersOf(a, c) {
		if ServerEntryID(o.Server) == id {
			return o, true
		}
	}
	return offer.Server{}, false
}

// ServerWritesID is the roster id of an offered server's writes entry.
func ServerWritesID(srv manifest.MCPServer) string { return ServerEntryID(srv) + writesSuffix }

// switchedOn is the switch of the entry id as config.yaml has it, def when
// it has none.
func switchedOn(c Config, id string, def bool) bool {
	if v, ok := c.Switches[id]; ok {
		return v
	}
	return def
}

// setUp says an offered server has what it needs: its stored secret, kept
// and on, or an account on its host, kept and on. reason says what to do
// when it does not.
func setUp(a Agent, c Config, o offer.Server) (ok bool, reason string) {
	switch {
	case o.Secret != "":
		if slices.ContainsFunc(c.Secrets, func(s StoredSecret) bool { return s.Name == o.Secret }) && switchedOn(c, SecretEntryID(o.Secret), true) {
			return true, ""
		}
		return false, fmt.Sprintf("it needs your secret %s: agentry agent secret set %s %s", o.Secret, a.Name, o.Secret)
	case o.Account != "":
		if slices.ContainsFunc(c.Accounts, func(g GitAccount) bool { return g.Host == o.Account }) && switchedOn(c, AccountEntryID(o.Account), true) {
			return true, ""
		}
		return false, fmt.Sprintf("it needs an account on %s: agentry agent configure %s --account %s", o.Account, a.Name, o.Account)
	}
	return true, ""
}

// offeredEntries are an offered server's roster entries: the server, and its
// writes when it has them.
func offeredEntries(a Agent, c Config, o offer.Server) []RosterEntry {
	srv := o.Server
	line := serverLine(srv)
	if o.Account != "" {
		line = strings.Replace(line, ", given your secret "+srv.Secrets[0], ", signed in with its "+o.Account+" account's token", 1)
		if o.Writes != nil {
			line += ", read only"
		}
	}
	if o.Note != "" {
		line += "; " + o.Note
	}
	out := []RosterEntry{{ID: ServerEntryID(srv), Kind: KindServer, Name: o.Name, GrantedBy: GrantedByBuiltin, Line: line}}
	if o.Writes != nil {
		out = append(out, RosterEntry{ID: ServerWritesID(srv), Kind: KindServerWrites, Name: o.Name + ": changes", GrantedBy: GrantedByBuiltin,
			Line: "Lets " + o.Name + " change what its token can change on " + srv.RemoteHost() + " (" + o.Writes.URL + "), while " + o.Name + " is on"})
	}
	return out
}

// givenServer is the server a segment is given for s: an offered server's
// read-write address when its writes are on.
func givenServer(c Config, s agentServer) manifest.MCPServer {
	if o := s.Offered; o != nil && o.Writes != nil && switchedOn(c, ServerWritesID(s.Server), false) {
		return *o.Writes
	}
	return s.Server
}

// serverTokens are the account tokens the offered servers a segment is
// given are signed in with, by the secret each declares, read from keys.
func serverTokens(keys Secrets, c Config, given []agentServer) (map[string]string, error) {
	var out map[string]string
	for _, s := range given {
		o := s.Offered
		if o == nil || o.Account == "" {
			continue
		}
		i := slices.IndexFunc(c.Accounts, func(g GitAccount) bool { return g.Host == o.Account })
		if i < 0 {
			continue
		}
		if keys == nil {
			return nil, errors.New("the agent has a server on that signs in with its git host account, and this client cannot read the keychain")
		}
		g := c.Accounts[i]
		v, err := keys.Get(AccountKeychainAccount(g.Host, g.User))
		if err != nil {
			return nil, fmt.Errorf("%s signs in with the account on %s: %w (set its token again: agentry agent configure coder --account %s)", o.Name, g.Host, err, g.Host)
		}
		if err := CheckSecretValue(v); err != nil {
			return nil, fmt.Errorf("the token for %s in the keychain: %w", g.Host, err)
		}
		if out == nil {
			out = map[string]string{}
		}
		out[s.Server.Secrets[0]] = v
	}
	return out, nil
}

// checkPersonServer refuses a server the person adds that is one the agent
// offers, or that declares a secret beginning AGENTRY_: the runner's own
// names, the offered servers' account slots (offer.AccountSecretPrefix)
// among them.
func checkPersonServer(a Agent, c Config, srv manifest.MCPServer) error {
	if o, ok := offeredServer(a, c, ServerEntryID(srv)); ok {
		return fmt.Errorf("the %s agent comes with %s: switch it on (agentry agent grant %s %s on)", a.Name, o.Name, a.Name, ServerEntryID(srv))
	}
	for _, n := range srv.Secrets {
		if strings.HasPrefix(n, "AGENTRY_") {
			return fmt.Errorf("the server declares the secret %s: a name that begins AGENTRY_ is the runner's own", n)
		}
	}
	return nil
}

// suggestionsOf are the servers the agent's built-in suggests: its own, but
// in a test.
var suggestionsOf = func(a Agent) []offer.Suggestion { return a.Builtin.SuggestedServers() }

// Suggestions are the servers the agent suggests that it does not have.
func Suggestions(a Agent, c Config) []offer.Suggestion {
	var out []offer.Suggestion
	for _, s := range suggestionsOf(a) {
		if !slices.ContainsFunc(c.Servers, func(x manifest.MCPServer) bool { return ServerEntryID(x) == ServerEntryID(s.Server) }) {
			out = append(out, s)
		}
	}
	return out
}

// Suggested is the server the agent suggests under key, to add as the
// person's own.
func Suggested(a Agent, key string) (manifest.MCPServer, error) {
	var keys []string
	for _, s := range suggestionsOf(a) {
		if s.Key == key {
			return s.Server, nil
		}
		keys = append(keys, s.Key)
	}
	if len(keys) == 0 {
		return manifest.MCPServer{}, fmt.Errorf("the %s agent suggests no server", a.Name)
	}
	return manifest.MCPServer{}, fmt.Errorf("the %s agent suggests no server %q: it suggests %s", a.Name, key, strings.Join(keys, ", "))
}

// suggestionFor is the suggestion a server the person added is, if any.
func suggestionFor(a Agent, srv manifest.MCPServer) (offer.Suggestion, bool) {
	for _, s := range suggestionsOf(a) {
		if ServerEntryID(s.Server) == ServerEntryID(srv) {
			return s, true
		}
	}
	return offer.Suggestion{}, false
}
