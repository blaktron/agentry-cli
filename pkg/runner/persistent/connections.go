package persistent

// A persistent agent's remote MCP servers that sign in by OAuth. Each one
// signs in with the person's own connection, made on a terminal with
// `agentry agent connect` and kept for the agent (not for a release, as a
// QuickAgent's is), so it outlives the agent's updates. A segment is given
// the server's access token alone: never the refresh token or a client
// secret, which stay in the connection store on the host. The runner
// refreshes the token before the segment starts and renews it while the
// segment runs, as it does for a run (engine.Params.RenewConnection).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/runner/connection"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
)

// agentConnectionPrefix starts the Ref of an agent's connection, so that
// no release's ns/name@version can name it.
const agentConnectionPrefix = "agent:"

// agentConnectionDigest stands where a release's connection has its
// archive digest: an agent's connection is the agent's, whatever release
// it runs.
const agentConnectionDigest = "persistent"

// SignsIn says whether srv is a remote server that signs in by OAuth, the
// kind a persistent agent signs in to with the person's connection. A
// package server with an oauth block is not given to a persistent agent.
func SignsIn(srv manifest.MCPServer) bool {
	return srv.IsRemote() && srv.Auth == manifest.AuthOAuth
}

// packageSignsIn says whether srv is a package server that signs in by
// OAuth, which a persistent agent is never given.
func packageSignsIn(srv manifest.MCPServer) bool {
	return !srv.IsRemote() && (srv.Auth == manifest.AuthOAuth || srv.OAuth != nil)
}

// ConnectionKey is the key of the connection agent name's server srv
// signs in with.
func ConnectionKey(name string, srv manifest.MCPServer) connection.Key {
	return connection.Key{Ref: agentConnectionPrefix + name, Digest: agentConnectionDigest, Server: srv.URL}
}

// IsAgentConnection says whether c is a persistent agent's, and names the
// agent.
func IsAgentConnection(c *connection.Connection) (agent string, ok bool) {
	if c == nil || c.Digest != agentConnectionDigest {
		return "", false
	}
	name, ok := strings.CutPrefix(c.Ref, agentConnectionPrefix)
	return name, ok && name != ""
}

// SignInServers are the remote servers of agent a under config c that sign
// in by OAuth, on or off: what `agentry agent connect` connects.
func SignInServers(a Agent, c Config) []manifest.MCPServer {
	var out []manifest.MCPServer
	for _, as := range agentServers(a, c) {
		if srv := givenServer(c, as); SignsIn(srv) {
			out = append(out, srv)
		}
	}
	return out
}

// Connections are the person's connections a persistent agent's servers
// sign in with.
type Connections interface {
	// Connected says whether k has a kept connection the vendor has not
	// refused. It reads the record alone, never a secret.
	Connected(k connection.Key) bool
	// Token refreshes k's connection when its token is due and returns
	// the access token.
	Token(ctx context.Context, k connection.Key) (engine.ConnectionToken, error)
}

// StoreConnections are the connections kept in the runner home, their
// secrets in the OS keychain (or the store's file where it has none).
func StoreConnections(home string) Connections {
	return storeConnections{
		store:   connection.Open(home, connection.OSKeychain{}),
		clients: connection.OpenClients(home, connection.OSKeychain{}),
		http:    mcpauth.Guard{}.Client(),
	}
}

type storeConnections struct {
	store   *connection.Store
	clients *connection.Clients
	http    *http.Client
}

func (s storeConnections) Connected(k connection.Key) bool {
	c, err := s.store.Get(k)
	return err == nil && c.Broken == ""
}

func (s storeConnections) Token(ctx context.Context, k connection.Key) (engine.ConnectionToken, error) {
	a, _, err := s.store.Use(ctx, k, s.http, s.clients)
	if err != nil {
		return engine.ConnectionToken{}, err
	}
	return engine.ConnectionToken{AccessToken: a.Token, ExpiresAt: a.ExpiresAt}, nil
}

// connectFix is the sentence that says how to make agent name's
// connections.
func connectFix(name string) string {
	return "connect it on a terminal: agentry agent connect " + name
}

// signInReason is why an on server that signs in was not given to a
// segment: it has no connection, its vendor refused the last refresh, or
// the refresh could not be made now. A failed refresh is not quoted: what
// a vendor answers is not this client's to repeat in a roster, which the
// segment's envelope records.
func signInReason(name string, err error) string {
	var broken *connection.BrokenError
	switch {
	case errors.Is(err, connection.ErrNone):
		return "it has no connection yet: " + connectFix(name)
	case errors.As(err, &broken):
		return "its vendor no longer honours the connection (" + broken.Reason + "): " + connectFix(name)
	default:
		return "its connection could not be refreshed: the vendor could not be reached or answered something unexpected; it is tried again when the agent next starts"
	}
}

// signInTokens fetches the access token of each server of servers that is
// on and signs in, keyed by its address as engine.Params.Connections is.
// A server whose token cannot be had is named in left with the reason.
// The renewal a segment is given asks conns again for the same key.
func signInTokens(ctx context.Context, name string, servers []agentServer, c Config, on func(string) bool, conns Connections) (tokens map[string]engine.ConnectionToken, renew func(context.Context, string) (engine.ConnectionToken, error), left map[string]string) {
	keys := map[string]connection.Key{}
	for _, as := range servers {
		srv := givenServer(c, as)
		if !on(as.ID()) || !SignsIn(srv) {
			continue
		}
		if left == nil {
			left = map[string]string{}
		}
		if conns == nil {
			left[as.ID()] = "this client cannot read your connections"
			continue
		}
		k := ConnectionKey(name, srv)
		t, err := conns.Token(ctx, k)
		if err != nil {
			left[as.ID()] = signInReason(name, err)
			continue
		}
		if tokens == nil {
			tokens = map[string]engine.ConnectionToken{}
		}
		tokens[srv.URL] = t
		keys[srv.URL] = k
	}
	if len(keys) > 0 {
		renew = func(ctx context.Context, server string) (engine.ConnectionToken, error) {
			k, ok := keys[server]
			if !ok {
				return engine.ConnectionToken{}, fmt.Errorf("the segment has no connection for %s", server)
			}
			return conns.Token(ctx, k)
		}
	}
	return tokens, renew, left
}

// connectionSignatures are each on server's that signs in: when its
// connection works ("connected") or not ("none"). A connection made or
// broken while the agent runs changes it, so the next turn gives the
// server anew; a replaced one is read at the token's next renewal.
func connectionSignatures(name string, a Agent, c Config, r Roster, conns Connections) Signatures {
	out := Signatures{}
	if conns == nil {
		return out
	}
	for _, as := range agentServers(a, c) {
		srv := givenServer(c, as)
		if e, ok := r.Entry(as.ID()); !ok || !e.On || !SignsIn(srv) {
			continue
		}
		out[as.ID()] = "none"
		if conns.Connected(ConnectionKey(name, srv)) {
			out[as.ID()] = "connected"
		}
	}
	return out
}

// RevokeTimeout bounds one connection's revocation at its vendor: one that
// does not answer in time is recorded as a failed revocation and the
// connection is removed all the same.
const RevokeTimeout = 30 * time.Second

// Removal is what RemoveAgentConnections removes with: the store, the
// person's own OAuth clients and the HTTP client the vendors are asked
// through.
type Removal struct {
	Store   *connection.Store
	Clients *connection.Clients
	HTTP    *http.Client
}

// StoreRemoval removes from the runner home's store, its secrets in the OS
// keychain.
func StoreRemoval(home string) Removal {
	return Removal{
		Store:   connection.Open(home, connection.OSKeychain{}),
		Clients: connection.OpenClients(home, connection.OSKeychain{}),
		HTTP:    mcpauth.Guard{}.Client(),
	}
}

// RemoveAgentConnections removes the connections agent name's servers sign
// in with (only the one to the server at address only, when it is set),
// each revoked at its vendor where the vendor offers it, and says which
// servers' connections went.
func RemoveAgentConnections(ctx context.Context, rm Removal, name, only, why string) ([]string, error) {
	list, err := rm.Store.List()
	if err != nil {
		return nil, err
	}
	var gone []string
	var errs []error
	for _, c := range list {
		if n, ok := IsAgentConnection(c); !ok || n != name || (only != "" && c.Server != only) {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, RevokeTimeout)
		_, rerr := rm.Store.Remove(rctx, c.Key(), rm.HTTP, rm.Clients, why)
		cancel()
		if rerr != nil {
			errs = append(errs, fmt.Errorf("remove the connection to %s: %w", c.Server, rerr))
			continue
		}
		gone = append(gone, c.Server)
	}
	return gone, errors.Join(errs...)
}

// resetConnections removes an agent's connections at a full reset;
// tests replace it.
var resetConnections = func(ctx context.Context, home, name string) error {
	_, err := RemoveAgentConnections(ctx, StoreRemoval(home), name, "", "the agent was reset")
	return err
}

// WithSignIns marks each server of r that is on, signs in by OAuth and has
// no working connection in conns as needing a sign-in, saying how to make
// one: the roster of an agent that is not running, as `agentry agent show`
// and Agentry Desktop show it.
func WithSignIns(r Roster, a Agent, c Config, conns Connections) Roster {
	if conns == nil {
		return r
	}
	byID := map[string]manifest.MCPServer{}
	for _, as := range agentServers(a, c) {
		byID[as.ID()] = givenServer(c, as)
	}
	out := slices.Clone(r)
	for i, e := range out {
		srv, ok := byID[e.ID]
		if e.Kind != KindServer || !e.On || !ok || !SignsIn(srv) || conns.Connected(ConnectionKey(a.Name, srv)) {
			continue
		}
		out[i].State, out[i].Reason = StateNeedsSignIn, signInReason(a.Name, connection.ErrNone)
	}
	return out
}
