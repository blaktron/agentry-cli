package persistent

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/persistent/offer"
)

// lookupOffer needs SEARCH_KEY, for which the key kept for the "brave" web
// search provider may stand in.
var lookupOffer = offer.Server{Name: "Lookup", Secret: "SEARCH_KEY", SearchKey: "brave",
	Server: manifest.MCPServer{NPM: "@example/lookup-mcp", Version: "1.0.0", Egress: []string{"api.lookup.example.com"}, Secrets: []string{"SEARCH_KEY"}}}

const keptSearchKey = "kept-search-key-0123456789"

// withSearchKey makes the key the person keeps for web search provider's:
// held when key is not empty. reads counts the times it was asked for.
func withSearchKey(t *testing.T, provider, key string) (reads *int) {
	t.Helper()
	n := 0
	oldHeld, oldValue := searchKeyHeld, searchKeyValue
	searchKeyHeld = func(p string) bool { n++; return p == provider && key != "" }
	searchKeyValue = func(p string) (string, error) {
		if p != provider {
			return "", nil
		}
		return key, nil
	}
	t.Cleanup(func() { searchKeyHeld, searchKeyValue = oldHeld, oldValue })
	return &n
}

// lookupOn is the Assistant with lookupOffer switched on and agreed to.
func lookupOn(t *testing.T, home string) (Agent, Config) {
	t.Helper()
	a := assistantAgent(t)
	record(t, serversDir(home, a.Name), lookupOffer.Server)
	c := Config{Enabled: true}
	if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: ServerEntryID(lookupOffer.Server), On: true}, Consent: true}); err != nil {
		t.Fatal(err)
	}
	return a, c
}

// TestTheSearchKeyStandsInForAnOfferedServersSecret: with no secret stored
// and the key kept for its search provider, the offered server is set up,
// and the segment gives it that key in its own slot, not among the stored
// secrets the Coding agent's programs would see.
func TestTheSearchKeyStandsInForAnOfferedServersSecret(t *testing.T) {
	withOffers(t, lookupOffer)
	withSearchKey(t, "brave", keptSearchKey)
	t.Setenv("HOME", t.TempDir())
	home := shortHome(t)
	a, c := lookupOn(t, home)
	id := ServerEntryID(lookupOffer.Server)
	r := BuildRoster(a, c, nil)
	e, _ := r.Entry(id)
	if e.State != StateOn || !strings.Contains(e.Line, "given your secret SEARCH_KEY or, while you store none, the Brave key you keep for web search") {
		t.Fatalf("the entry: %+v", e)
	}
	g, applied, done, err := Apply(context.Background(), home, a, c, r, nil, nil, fakeSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	done()
	if len(g.Servers) != 1 || !slices.Contains(applied.On, id) {
		t.Fatalf("the server was not given: %+v %v", g.Servers, applied.On)
	}
	if g.ServerSecrets["SEARCH_KEY"] != keptSearchKey || g.Secrets["SEARCH_KEY"] != "" {
		t.Fatalf("server secrets %v, stored secrets %v", maskedKeys(g.ServerSecrets), maskedKeys(g.Secrets))
	}
}

// maskedKeys names a map's keys, never its values.
func maskedKeys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestWithoutTheSearchKeyTheServerNeedsSetUp: no key kept, the server waits,
// its reason naming both ways to give it one.
func TestWithoutTheSearchKeyTheServerNeedsSetUp(t *testing.T) {
	withOffers(t, lookupOffer)
	withSearchKey(t, "brave", "")
	t.Setenv("HOME", t.TempDir())
	home := shortHome(t)
	a, c := lookupOn(t, home)
	e, _ := BuildRoster(a, c, nil).Entry(ServerEntryID(lookupOffer.Server))
	if e.State != StateNeedsSetUp || !strings.Contains(e.Reason, "agentry agent secret set assistant SEARCH_KEY") || !strings.Contains(e.Reason, "a Brave key for web search") {
		t.Fatalf("the entry: %+v", e)
	}
}

// TestAStoredSecretWinsOverTheSearchKey: a stored secret of the name is
// given instead; switched off, the server waits, and the search key does
// not stand in for a secret the person withheld.
func TestAStoredSecretWinsOverTheSearchKey(t *testing.T) {
	withOffers(t, lookupOffer)
	withSearchKey(t, "brave", keptSearchKey)
	t.Setenv("HOME", t.TempDir())
	home := shortHome(t)
	a, c := lookupOn(t, home)
	id := ServerEntryID(lookupOffer.Server)
	const stored = "stored-secret-0123456789"
	if _, err := ApplyChange(home, a, &c, Change{SetSecret: &StoredSecret{Name: "SEARCH_KEY", Set: time.Now()}, Consent: true}); err != nil {
		t.Fatal(err)
	}
	r := BuildRoster(a, c, nil)
	g, _, done, err := Apply(context.Background(), home, a, c, r, nil, nil, fakeSecrets{SecretAccount(a.Name, "SEARCH_KEY"): stored})
	if err != nil {
		t.Fatal(err)
	}
	done()
	if g.Secrets["SEARCH_KEY"] != stored || g.ServerSecrets["SEARCH_KEY"] != "" {
		t.Fatalf("server secrets %v, stored secrets %v", maskedKeys(g.ServerSecrets), maskedKeys(g.Secrets))
	}
	if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: SecretEntryID("SEARCH_KEY"), On: false}}); err != nil {
		t.Fatal(err)
	}
	if e, _ := BuildRoster(a, c, nil).Entry(id); e.State != StateNeedsSetUp {
		t.Fatalf("with its stored secret off: %+v", e)
	}
}

// TestAPersonsServerDeclaringTheSecretKeepsTheSearchKeyOut: the key would
// reach the person's server too, so it does not stand in.
func TestAPersonsServerDeclaringTheSecretKeepsTheSearchKeyOut(t *testing.T) {
	withOffers(t, lookupOffer)
	withSearchKey(t, "brave", keptSearchKey)
	t.Setenv("HOME", t.TempDir())
	home := shortHome(t)
	a, c := lookupOn(t, home)
	own := wordServer
	own.Secrets = []string{"SEARCH_KEY"}
	if _, err := ApplyChange(home, a, &c, Change{AddServer: &own, Consent: true}); err != nil {
		t.Fatal(err)
	}
	if e, _ := BuildRoster(a, c, nil).Entry(ServerEntryID(lookupOffer.Server)); e.State != StateNeedsSetUp || strings.Contains(e.Reason, "web search") {
		t.Fatalf("the entry: %+v", e)
	}
}

// TestAnAgreementToTheEarlierSentenceCounts: the person who agreed to the
// sentence that named the stored secret alone is not asked again.
func TestAnAgreementToTheEarlierSentenceCounts(t *testing.T) {
	withOffers(t, lookupOffer)
	withSearchKey(t, "brave", keptSearchKey)
	t.Setenv("HOME", t.TempDir())
	home := shortHome(t)
	a, c := lookupOn(t, home)
	id := ServerEntryID(lookupOffer.Server)
	c.Consented[id] = serverLine(lookupOffer.Server)
	if slices.ContainsFunc(Unconsented(BuildRoster(a, c, nil), c), func(e RosterEntry) bool { return e.ID == id }) {
		t.Error("an agreement to the earlier sentence asks again")
	}
	c.Consented[id] = "something else"
	if !slices.ContainsFunc(Unconsented(BuildRoster(a, c, nil), c), func(e RosterEntry) bool { return e.ID == id }) {
		t.Error("an agreement to another sentence counts")
	}
}

// TestAnOffServerDoesNotLookForTheSearchKey: the policy and the keychain are
// read only for an entry that is on.
func TestAnOffServerDoesNotLookForTheSearchKey(t *testing.T) {
	withOffers(t, lookupOffer)
	reads := withSearchKey(t, "brave", keptSearchKey)
	t.Setenv("HOME", t.TempDir())
	a := assistantAgent(t)
	BuildRoster(a, Config{Enabled: true}, nil)
	if *reads != 0 {
		t.Errorf("an off server looked for the search key %d times", *reads)
	}
}
