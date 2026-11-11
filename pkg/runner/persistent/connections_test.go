package persistent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/connection"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// fakeConnections hands out one token per kept key and counts each ask.
type fakeConnections struct {
	tokens map[connection.Key]string
	broken map[connection.Key]string
	asked  []connection.Key
}

func (f *fakeConnections) Connected(k connection.Key) bool {
	_, ok := f.tokens[k]
	return ok && f.broken[k] == ""
}

func (f *fakeConnections) Token(_ context.Context, k connection.Key) (engine.ConnectionToken, error) {
	f.asked = append(f.asked, k)
	if why, ok := f.broken[k]; ok {
		return engine.ConnectionToken{}, &connection.BrokenError{Reason: why}
	}
	t, ok := f.tokens[k]
	if !ok {
		return engine.ConnectionToken{}, connection.ErrNone
	}
	return engine.ConnectionToken{AccessToken: t, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

var crmServer = manifest.MCPServer{URL: "https://mcp.crm.example/mcp", Auth: manifest.AuthOAuth}

func withOAuthServer(t *testing.T) (Agent, Config) {
	t.Helper()
	a := assistantAgent(t)
	c := Config{Enabled: true, Servers: []manifest.MCPServer{crmServer}}
	return a, c
}

// The key of an agent's connection names the agent, not a release, so an
// update of the agent keeps it; no release's ns/name@version can be it.
func TestAnAgentsConnectionIsKeyedToTheAgent(t *testing.T) {
	k := ConnectionKey("assistant", crmServer)
	if k.Ref != "agent:assistant" || k.Server != crmServer.URL || k.Digest == "" {
		t.Fatalf("key %+v", k)
	}
	if name, ok := IsAgentConnection(&connection.Connection{Ref: k.Ref, Digest: k.Digest, Server: k.Server}); !ok || name != "assistant" {
		t.Fatalf("IsAgentConnection = %q, %v", name, ok)
	}
	if _, ok := IsAgentConnection(&connection.Connection{Ref: "acme/crm@1.0.0", Digest: "sha256:x"}); ok {
		t.Fatal("a release's connection read as an agent's")
	}
}

// A segment is given an on OAuth server with the access token of the
// person's connection, keyed by its address, and a renewal that asks for
// the same connection again; nothing else of the connection.
func TestASegmentIsGivenTheServersAccessTokenAlone(t *testing.T) {
	home := shortHome(t)
	a, c := withOAuthServer(t)
	conns := &fakeConnections{tokens: map[connection.Key]string{ConnectionKey(a.Name, crmServer): "tok-1"}}
	g, applied, done, err := ApplyWith(context.Background(), home, a, c, BuildRoster(a, c, nil), Openers{Connections: conns}, nil, fakeSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	id := ServerEntryID(crmServer)
	if !containsID(applied.On, id) || len(applied.NeedsSignIn) != 0 {
		t.Fatalf("applied %+v", applied)
	}
	if len(g.Servers) != 1 || g.Servers[0].URL != crmServer.URL {
		t.Fatalf("servers %+v", g.Servers)
	}
	if g.Connections[crmServer.URL].AccessToken != "tok-1" || g.RenewConnection == nil {
		t.Fatalf("connections %+v", g.Connections)
	}
	if _, err := g.RenewConnection(context.Background(), crmServer.URL); err != nil {
		t.Fatal(err)
	}
	if len(conns.asked) != 2 || conns.asked[1] != ConnectionKey(a.Name, crmServer) {
		t.Fatalf("asked %+v", conns.asked)
	}
	if _, err := g.RenewConnection(context.Background(), "https://elsewhere.example/mcp"); err == nil {
		t.Fatal("a renewal was answered for a server the segment was not given")
	}
	b, _ := json.Marshal(applied)
	if strings.Contains(string(b), "tok-1") {
		t.Fatal("the token is in what the segment records")
	}
}

// With no connection, or one the vendor refused, the server is left out of
// the segment, and the roster says it needs a sign-in and how to make it.
func TestAServerWithNoWorkingConnectionIsLeftOut(t *testing.T) {
	home := shortHome(t)
	a, c := withOAuthServer(t)
	id := ServerEntryID(crmServer)
	for name, conns := range map[string]*fakeConnections{
		"none":    {},
		"refused": {tokens: map[connection.Key]string{ConnectionKey(a.Name, crmServer): "x"}, broken: map[connection.Key]string{ConnectionKey(a.Name, crmServer): "invalid_grant"}},
	} {
		r := BuildRoster(a, c, nil)
		g, applied, done, err := ApplyWith(context.Background(), home, a, c, r, Openers{Connections: conns}, nil, fakeSecrets{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		done()
		if containsID(applied.On, id) || len(g.Servers) != 0 || len(g.Connections) != 0 {
			t.Fatalf("%s: given %+v %+v", name, applied, g.Servers)
		}
		why := applied.NeedsSignIn[id]
		if !strings.Contains(why, "agentry agent connect "+a.Name) {
			t.Fatalf("%s: reason %q", name, why)
		}
		if name == "refused" && !strings.Contains(why, "invalid_grant") {
			t.Fatalf("refused: reason %q", why)
		}
		out := leftOut(r, c, applied, func(s string) bool { return containsID(applied.On, s) })
		e, _ := out.Entry(id)
		if e.State != StateNeedsSignIn || e.Reason != why {
			t.Fatalf("%s: roster entry %+v", name, e)
		}
		// The segment's envelope says the server was not given.
		for _, re := range out.Receipt() {
			if re.ID == id && re.State != string(StateNeedsSignIn) {
				t.Fatalf("%s: the envelope records %+v", name, re)
			}
		}
	}
}

// A client that cannot read connections gives none of those servers.
func TestNoConnectionsGivesNoSignInServer(t *testing.T) {
	a, c := withOAuthServer(t)
	_, applied, done, err := ApplyWith(context.Background(), shortHome(t), a, c, BuildRoster(a, c, nil), Openers{}, nil, fakeSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	done()
	if _, ok := applied.NeedsSignIn[ServerEntryID(crmServer)]; !ok {
		t.Fatalf("applied %+v", applied)
	}
}

// A connection made while the agent runs changes the server's signature,
// so the next turn gives the server.
func TestAConnectionMadeLaterChangesTheSignature(t *testing.T) {
	a, c := withOAuthServer(t)
	r := BuildRoster(a, c, nil)
	conns := &fakeConnections{}
	before := connectionSignatures(a.Name, a, c, r, conns)
	conns.tokens = map[connection.Key]string{ConnectionKey(a.Name, crmServer): "t"}
	after := connectionSignatures(a.Name, a, c, r, conns)
	if got := before.changed(after); len(got) != 1 || got[0] != ServerEntryID(crmServer) {
		t.Fatalf("changed %v (%v → %v)", got, before, after)
	}
}

// A failed refresh says the agent tries again, and repeats nothing of
// what the failure carried.
func TestARefreshThatFailsSaysItTriesAgain(t *testing.T) {
	why := signInReason("assistant", errors.New("invalid_request: token=SECRET-VALUE"))
	if strings.Contains(why, "SECRET-VALUE") || !strings.Contains(why, "next starts") {
		t.Fatal(why)
	}
}

// One agent's connection is never another's, nor a release's: each key is
// its own record.
func TestAnAgentsConnectionIsItsOwn(t *testing.T) {
	a, b := ConnectionKey("assistant", crmServer), ConnectionKey("coder", crmServer)
	if a.ID() == b.ID() {
		t.Fatal("two agents share a connection")
	}
	conns := &fakeConnections{tokens: map[connection.Key]string{a: "t"}}
	if conns.Connected(b) {
		t.Fatal("the coder reads the assistant's connection")
	}
	release := connection.Key{Ref: "acme/crm@1.0.0", Digest: "sha256:" + strings.Repeat("a", 64), Server: crmServer.URL}
	if release.ID() == a.ID() {
		t.Fatal("a release shares an agent's connection")
	}
}

// An OAuth server is not used before the person agrees to its sentence,
// which says it signs in with their connection: an agreement recorded to
// another sentence does not cover it.
func TestAnOAuthServerAsksForAgreementFirst(t *testing.T) {
	a, c := withOAuthServer(t)
	id := ServerEntryID(crmServer)
	c.Consented = map[string]string{id: "Uses the MCP server https://mcp.crm.example/mcp (remote), all its tools"}
	pending := Unconsented(BuildRoster(a, c, nil), c)
	if !slices.ContainsFunc(pending, func(e RosterEntry) bool { return e.ID == id }) {
		t.Fatalf("an OAuth server is used unagreed: pending %+v", pending)
	}
	RecordConsent(BuildRoster(a, c, nil), &c)
	if pending := Unconsented(BuildRoster(a, c, nil), c); len(pending) != 0 {
		t.Fatalf("agreed, still pending %+v", pending)
	}
}

func containsID(ids []string, id string) bool {
	for _, s := range ids {
		if s == id {
			return true
		}
	}
	return false
}

// A stopped agent's roster says an on server with no connection needs a
// sign-in, and one with a connection is on as it is.
func TestTheRosterSaysWhichServersNeedASignIn(t *testing.T) {
	a, c := withOAuthServer(t)
	id := ServerEntryID(crmServer)
	e, _ := WithSignIns(BuildRoster(a, c, nil), a, c, &fakeConnections{}).Entry(id)
	if e.State != StateNeedsSignIn || !strings.Contains(e.Reason, "agentry agent connect assistant") {
		t.Fatalf("unconnected: %+v", e)
	}
	conns := &fakeConnections{tokens: map[connection.Key]string{ConnectionKey(a.Name, crmServer): "t"}}
	if e, _ := WithSignIns(BuildRoster(a, c, nil), a, c, conns).Entry(id); e.State == StateNeedsSignIn {
		t.Fatalf("connected: %+v", e)
	}
}

// A running agent whose OAuth server has no connection starts without it
// and says it needs a sign-in; a connection made meanwhile gives the server
// to the segment the next turn starts, with its token.
func TestARunningAgentTakesAConnectionAtItsNextTurn(t *testing.T) {
	home := shortHome(t)
	t.Setenv("HOME", t.TempDir())
	a := assistantAgent(t)
	c := Config{Enabled: true, Servers: []manifest.MCPServer{crmServer}}
	RecordConsent(BuildRoster(a, c, nil), &c)
	if err := SaveConfig(home, "assistant", c); err != nil {
		t.Fatal(err)
	}
	conns := &fakeConnections{}
	st := &starter{}
	sv, err := Start(context.Background(), Options{
		Home: home, Agent: a, Client: ClientCLI, Policy: policy.Default(),
		Docker: &dockerx.Fake{}, StartSegment: st.start, Connections: conns,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = sv.Stop(context.Background()) }()
	id := ServerEntryID(crmServer)
	if seg := st.segments[0]; len(seg.Grants.Servers) != 0 || len(seg.Grants.Connections) != 0 {
		t.Fatalf("first segment given %+v", seg.Grants.Servers)
	}
	if e, _ := sv.Roster().Entry(id); e.State != StateNeedsSignIn || !strings.Contains(e.Reason, "agentry agent connect assistant") {
		t.Fatalf("roster %+v", e)
	}
	conns.tokens = map[connection.Key]string{ConnectionKey("assistant", crmServer): "tok-2"}
	if err := sv.Send(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	n := len(st.segments)
	last := st.segments[n-1]
	st.mu.Unlock()
	if n != 2 || len(last.Grants.Servers) != 1 || last.Grants.Connections[crmServer.URL].AccessToken != "tok-2" {
		t.Fatalf("%d segments; last given %+v %+v", n, last.Grants.Servers, last.Grants.Connections)
	}
	if e, _ := sv.Roster().Entry(id); e.State == StateNeedsSignIn {
		t.Fatalf("roster after connecting %+v", e)
	}
}
