package persistent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/persistent/offer"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// The stand-in offers: a server on by default, one that needs a stored
// secret, and a remote signed in with the github.com account, with writes.
var (
	docsOffer = offer.Server{Name: "Docs", On: true,
		Server: manifest.MCPServer{NPM: "@example/docs-mcp", Version: "1.0.0", Egress: []string{"docs.example.com"}}}
	searchOffer = offer.Server{Name: "Search", Secret: "SEARCH_KEY",
		Server: manifest.MCPServer{NPM: "@example/search-mcp", Version: "2.0.0", Egress: []string{"api.search.example.com"}, Secrets: []string{"SEARCH_KEY"}}}
	hubOffer = offer.Server{Name: "GitHub", Account: "github.com",
		Server: manifest.MCPServer{URL: "https://api.githubcopilot.com/mcp/readonly", Auth: manifest.AuthSecret, Secrets: []string{"AGENTRY_GIT_ACCOUNT_GITHUB"}},
		Writes: &manifest.MCPServer{URL: "https://api.githubcopilot.com/mcp/", Auth: manifest.AuthSecret, Secrets: []string{"AGENTRY_GIT_ACCOUNT_GITHUB"}}}
)

// TestMain gives the package's tests built-ins that offer and suggest no
// servers, so a test of the roster, the installs or a segment sees only
// what it sets up; a test of the offers sets them (withOffers), and
// TestOfferedServerEgressIsDeclared reads the built-ins' own.
func TestMain(m *testing.M) {
	offersOf = func(Agent) []offer.Server { return nil }
	suggestionsOf = func(Agent) []offer.Suggestion { return nil }
	os.Exit(m.Run())
}

// withOffers makes the stand-ins the agents' offers for the test.
func withOffers(t *testing.T, offers ...offer.Server) {
	t.Helper()
	if err := offer.CheckAll(offers, ServerEntryID); err != nil {
		t.Fatal(err)
	}
	old := offersOf
	offersOf = func(Agent) []offer.Server { return offers }
	t.Cleanup(func() { offersOf = old })
}

func TestAnOfferedServerIsAnEntryOfTheBuiltins(t *testing.T) {
	withOffers(t, docsOffer)
	home := shortHome(t)
	a := coderAgent(t)
	c := Config{Enabled: true}
	r := BuildRoster(a, c, nil)
	id := ServerEntryID(docsOffer.Server)
	e, ok := r.Entry(id)
	if !ok || e.Kind != KindServer || !e.On || e.GrantedBy != GrantedByBuiltin || e.Name != "Docs" || !strings.Contains(e.Line, "reaching docs.example.com alone") {
		t.Fatalf("the entry %+v", e)
	}
	// It is agreed to like any other entry that is on.
	if !slices.ContainsFunc(Unconsented(r, c), func(x RosterEntry) bool { return x.ID == id }) {
		t.Error("an offered server on by default asks no agreement")
	}
	if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: id, On: false}}); err != nil {
		t.Fatal(err)
	}
	if e, _ := BuildRoster(a, c, nil).Entry(id); e.On {
		t.Error("switched off, it is on")
	}
	if _, err := ApplyChange(home, a, &c, Change{RemoveServer: id}); err == nil || !strings.Contains(err.Error(), "switch it off") {
		t.Errorf("removing an offered server: %v", err)
	}
	srv := docsOffer.Server
	if _, err := ApplyChange(home, a, &c, Change{AddServer: &srv, Consent: true}); err == nil || !strings.Contains(err.Error(), "comes with Docs") {
		t.Errorf("adding an offered server as the person's: %v", err)
	}
	own := wordServer
	own.Secrets = []string{"AGENTRY_GIT_ACCOUNT_GITHUB"}
	if _, err := ApplyChange(home, a, &c, Change{AddServer: &own, Consent: true}); err == nil || !strings.Contains(err.Error(), "AGENTRY_") {
		t.Errorf("a person's server declaring a runner's secret: %v", err)
	}
}

// TestAnOfferedServerWaitsForItsSecret: off by default; switched on with no
// stored secret it needs set-up and no segment is given it; with the secret
// stored and on it is given, its slot filled from the stored secret.
func TestAnOfferedServerWaitsForItsSecret(t *testing.T) {
	withOffers(t, searchOffer)
	t.Setenv("HOME", t.TempDir())
	home := shortHome(t)
	a := assistantAgent(t)
	record(t, serversDir(home, a.Name), searchOffer.Server)
	id := ServerEntryID(searchOffer.Server)
	c := Config{Enabled: true}
	if e, _ := BuildRoster(a, c, nil).Entry(id); e.On {
		t.Fatal("a server that needs a secret is on by default")
	}
	if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: id, On: true}, Consent: true}); err != nil {
		t.Fatal(err)
	}
	r := BuildRoster(a, c, nil)
	if e, _ := r.Entry(id); e.State != StateNeedsSetUp || !strings.Contains(e.Reason, "agentry agent secret set assistant SEARCH_KEY") {
		t.Fatalf("without its secret: %+v", e)
	}
	g, applied, done, err := Apply(context.Background(), home, a, c, r, nil, nil, fakeSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	done()
	if len(g.Servers) != 0 || slices.Contains(applied.On, id) {
		t.Fatalf("a server waiting for its secret was given: %+v %v", g.Servers, applied.On)
	}
	const key = "search-key-0123456789"
	if _, err := ApplyChange(home, a, &c, Change{SetSecret: &StoredSecret{Name: "SEARCH_KEY", Set: time.Now()}, Consent: true}); err != nil {
		t.Fatal(err)
	}
	r = BuildRoster(a, c, nil)
	if e, _ := r.Entry(id); e.State != StateOn {
		t.Fatalf("with its secret: %+v", e)
	}
	g, applied, done, err = Apply(context.Background(), home, a, c, r, nil, nil, fakeSecrets{SecretAccount(a.Name, "SEARCH_KEY"): key})
	if err != nil {
		t.Fatal(err)
	}
	done()
	if len(g.Servers) != 1 || g.Servers[0].NPM != "@example/search-mcp" || g.Secrets["SEARCH_KEY"] != key || !slices.Contains(applied.On, id) {
		t.Fatalf("with its secret it was not given: %+v %v", g.Servers, applied.On)
	}
}

// TestAnAccountServerIsSignedInWithItsAccount: an offered remote that signs
// in with the github.com account waits for one; with it, its slot alone is
// given the account's token, never the Coding agent's programs, and its
// writes entry switches it to its read-write address. No token is in the
// plan.
func TestAnAccountServerIsSignedInWithItsAccount(t *testing.T) {
	withOffers(t, hubOffer)
	t.Setenv("HOME", t.TempDir())
	home := shortHome(t)
	a := coderAgent(t)
	id, writes := ServerEntryID(hubOffer.Server), ServerWritesID(hubOffer.Server)
	c := Config{Enabled: true}
	for _, ch := range []Change{{Switch: &Switch{ID: id, On: true}}} {
		ch.Consent = true
		if _, err := ApplyChange(home, a, &c, ch); err != nil {
			t.Fatal(err)
		}
	}
	if e, _ := BuildRoster(a, c, nil).Entry(id); e.State != StateNeedsSetUp || !strings.Contains(e.Reason, "--account github.com") {
		t.Fatalf("without an account: %+v", e)
	}
	g := botAccount()
	if _, err := ApplyChange(home, a, &c, Change{SetAccount: &g, Consent: true}); err != nil {
		t.Fatal(err)
	}
	keys := fakeSecrets{AccountKeychainAccount("github.com", "coder-bot"): ghToken}
	for _, withWrites := range []bool{false, true} {
		if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: writes, On: withWrites}, Consent: true}); err != nil {
			t.Fatal(err)
		}
		r := BuildRoster(a, c, nil)
		gr, applied, done, err := Apply(context.Background(), home, a, c, r, nil, nil, keys)
		if err != nil {
			t.Fatal(err)
		}
		done()
		want := "https://api.githubcopilot.com/mcp/readonly"
		if withWrites {
			want = "https://api.githubcopilot.com/mcp/"
		}
		if len(gr.Servers) != 1 || gr.Servers[0].URL != want || gr.ServerSecrets["AGENTRY_GIT_ACCOUNT_GITHUB"] != ghToken || slices.Contains(applied.On, writes) != withWrites {
			t.Fatalf("writes %v: servers %+v, applied %v", withWrites, gr.Servers, applied.On)
		}
		if _, ok := gr.Secrets["AGENTRY_GIT_ACCOUNT_GITHUB"]; ok {
			t.Fatal("the account's token is a stored secret of the programs")
		}
		sp, err := localrun.Segment{Builtin: a.Builtin, Home: home, Policy: policy.Default(), PolicySHA: "s", Build: "test", Grants: gr}.Plan(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		ts, err := os.ReadFile(filepath.Join(sp.Dir, "gateway", "toolspec.json"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(ts), "AGENTRY_GIT_ACCOUNT_GITHUB") {
			t.Error("the tools server is given the server's slot")
		}
		err = filepath.Walk(sp.Dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			b, err := os.ReadFile(path) //nolint:gosec // the plan's own files
			if err == nil && strings.Contains(string(b), ghToken) {
				return errors.New(path + " holds the token")
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Switched off, the account takes the server with it.
	if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: AccountEntryID("github.com"), On: false}}); err != nil {
		t.Fatal(err)
	}
	r := BuildRoster(a, c, nil)
	gr, _, done, err := Apply(context.Background(), home, a, c, r, nil, nil, keys)
	if err != nil {
		t.Fatal(err)
	}
	done()
	if len(gr.Servers) != 0 || len(gr.ServerSecrets) != 0 {
		t.Fatalf("with the account off: %+v %v", gr.Servers, gr.ServerSecrets)
	}
}

// TestOfferedServerEgressIsDeclared: every server a built-in offers reaches
// the hosts it names and no other, and the offers hold to their own rules.
func TestOfferedServerEgressIsDeclared(t *testing.T) {
	for _, b := range localrun.PersistentBuiltins() {
		if err := offer.CheckAll(b.OfferedServers(), ServerEntryID); err != nil {
			t.Errorf("%s: %v", b.Ref(), err)
		}
		for _, o := range b.OfferedServers() {
			if slices.Contains(o.Server.Egress, "*") {
				t.Errorf("%s offers %s with open egress", b.Ref(), o.Name)
			}
			// Each is a server a QuickAgent could declare.
			if err := checkServer(o.Server); err != nil {
				t.Errorf("%s offers %s: %v", b.Ref(), o.Name, err)
			}
			if o.Writes != nil {
				if err := checkServer(*o.Writes); err != nil {
					t.Errorf("%s offers %s's writes: %v", b.Ref(), o.Name, err)
				}
			}
		}
		for _, sg := range b.SuggestedServers() {
			if err := sg.Check(); err != nil {
				t.Errorf("%s suggests %s: %v", b.Ref(), sg.Name, err)
			}
			if err := checkServer(sg.Server); err != nil {
				t.Errorf("%s suggests %s: %v", b.Ref(), sg.Name, err)
			}
		}
	}
	for name, bad := range map[string]offer.Server{
		"open egress":          {Name: "x", On: true, Server: manifest.MCPServer{NPM: "x", Version: "1.0.0", Egress: []string{"*"}}},
		"a wildcard host":      {Name: "x", On: true, Server: manifest.MCPServer{NPM: "x", Version: "1.0.0", Egress: []string{"*.example.com"}}},
		"no host":              {Name: "x", On: true, Server: manifest.MCPServer{NPM: "x", Version: "1.0.0"}},
		"keyed and on":         {Name: "x", On: true, Secret: "K", Server: manifest.MCPServer{NPM: "x", Version: "1.0.0", Egress: []string{"a.example.com"}, Secrets: []string{"K"}}},
		"an undeclared secret": {Name: "x", Secret: "K", Server: manifest.MCPServer{NPM: "x", Version: "1.0.0", Egress: []string{"a.example.com"}}},
		"an account slot":      {Name: "x", Account: "github.com", Server: manifest.MCPServer{URL: "https://a.example.com/mcp", Auth: manifest.AuthSecret, Secrets: []string{"TOKEN"}}},
		"an address":           {Name: "x", On: true, Server: manifest.MCPServer{NPM: "x", Version: "1.0.0", Egress: []string{"169.254.169.254"}}},
		"writes elsewhere": {Name: "x", Account: "github.com",
			Server: manifest.MCPServer{URL: "https://a.example.com/mcp/readonly", Auth: manifest.AuthSecret, Secrets: []string{"AGENTRY_GIT_ACCOUNT_X"}},
			Writes: &manifest.MCPServer{URL: "https://b.example.com/mcp", Auth: manifest.AuthSecret, Secrets: []string{"AGENTRY_GIT_ACCOUNT_X"}}},
	} {
		if err := bad.Check(); err == nil {
			t.Errorf("%s: offered", name)
		}
	}
}

// TestASuggestedServerIsAddedAsThePersonsOwn: a suggestion is on no roster
// until it is added; added, it is the person's own server, asked for like
// one, named and noted as its suggestion; removed, it is suggested again.
func TestASuggestedServerIsAddedAsThePersonsOwn(t *testing.T) {
	sg := offer.Suggestion{Key: "scan", Name: "Scanner", Note: "it is sent the code it scans",
		Server: manifest.MCPServer{PyPI: "example-scan", Version: "1.0.0", Egress: []string{"scan.example.com"}}}
	old := suggestionsOf
	suggestionsOf = func(Agent) []offer.Suggestion { return []offer.Suggestion{sg} }
	t.Cleanup(func() { suggestionsOf = old })
	home := shortHome(t)
	a := coderAgent(t)
	c := Config{Enabled: true}
	id := ServerEntryID(sg.Server)
	if _, ok := BuildRoster(a, c, nil).Entry(id); ok {
		t.Fatal("a suggestion is on the roster before it is added")
	}
	if got := Suggestions(a, c); len(got) != 1 || got[0].Key != "scan" {
		t.Fatalf("suggestions %+v", got)
	}
	if _, err := Suggested(a, "nope"); err == nil || !strings.Contains(err.Error(), "it suggests scan") {
		t.Errorf("an unknown key: %v", err)
	}
	srv, err := Suggested(a, "scan")
	if err != nil {
		t.Fatal(err)
	}
	var ce *ConsentError
	if _, err := ApplyChange(home, a, &c, Change{AddServer: &srv}); !errors.As(err, &ce) {
		t.Fatalf("added unagreed: %v", err)
	}
	if _, err := ApplyChange(home, a, &c, Change{AddServer: &srv, Consent: true}); err != nil {
		t.Fatal(err)
	}
	e, ok := BuildRoster(a, c, nil).Entry(id)
	if !ok || e.GrantedBy != GrantedByYou || !e.On || e.Name != "Scanner (pypi:example-scan@1.0.0)" || !strings.HasSuffix(e.Line, "; it is sent the code it scans") {
		t.Fatalf("added: %+v", e)
	}
	if len(Suggestions(a, c)) != 0 {
		t.Error("an added suggestion is still suggested")
	}
	if _, err := ApplyChange(home, a, &c, Change{RemoveServer: id}); err != nil {
		t.Fatal(err)
	}
	if len(Suggestions(a, c)) != 1 {
		t.Error("a removed suggestion is not suggested again")
	}
}

// TestRosterReachesEveryGateWithTheDependencyScan: the Coding agent's scan,
// on with its programs, reaches the tools spec, the catalogue's read-only
// mount of OSV's databases, the plan's line and the System Prompt; with its
// entry or its programs off, none of them.
func TestRosterReachesEveryGateWithTheDependencyScan(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home := shortHome(t)
	a := coderAgent(t)
	for _, tc := range []struct {
		name           string
		scan, commands bool
	}{{"on", true, true}, {"its entry off", false, true}, {"its programs off", true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{Enabled: true}
			for _, sw := range []Switch{{ID: EntryDependencyScan, On: tc.scan}, {ID: EntryCommands, On: tc.commands}} {
				if _, err := ApplyChange(home, a, &c, Change{Switch: &sw, Consent: true}); err != nil {
					t.Fatal(err)
				}
			}
			gr, _, done, err := Apply(context.Background(), home, a, c, BuildRoster(a, c, nil), nil, nil, fakeSecrets{})
			if err != nil {
				t.Fatal(err)
			}
			done()
			want := tc.scan && tc.commands
			// On by default, it asks for the person's agreement.
			if fresh := (Config{Enabled: true}); !slices.ContainsFunc(Unconsented(BuildRoster(a, fresh, nil), fresh), func(e RosterEntry) bool { return e.ID == EntryDependencyScan }) {
				t.Error("the dependency scan asks no agreement")
			}
			if gr.Coder.DependencyScan != want {
				t.Fatalf("the grant's scan %v, want %v", gr.Coder.DependencyScan, want)
			}
			sp, err := localrun.Segment{Builtin: a.Builtin, Home: home, Policy: policy.Default(), PolicySHA: "s", Build: "test", Grants: gr}.Plan(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			read := func(name string) string {
				b, err := os.ReadFile(filepath.Join(sp.Dir, "gateway", name))
				if err != nil {
					t.Fatal(err)
				}
				return string(b)
			}
			ts, cat := read("toolspec.json"), read("catalog.yaml")
			if strings.Contains(ts, `"dependencyScan"`) != want || strings.Contains(cat, "@@RUN_DEPENDENCY_DB@@:/run/agentry/osv-db:ro") != want {
				t.Errorf("the tools server's scan: spec %v, mount %v, want %v", strings.Contains(ts, `"dependencyScan"`), strings.Contains(cat, "@@RUN_DEPENDENCY_DB@@"), want)
			}
			if _, has := sp.Plan.Enforcement["persistent.dependencyScan"]; has != want {
				t.Errorf("the plan's line: %v, want %v", has, want)
			}
			if slices.Contains(sp.Plan.Tools, "dependency_scan") != want {
				t.Errorf("the plan's tools %v", sp.Plan.Tools)
			}
		})
	}
}

// TestTheCodingAgentSuggestsSemgrep: Semgrep is suggested to the Coding
// agent alone, as the semgrep package's `semgrep mcp`, reaching
// semgrep.dev alone with its metrics and OpenTelemetry export off, given
// no secret and only the tools that work on code passed in the call (its
// plain scan reads files from disk, its rule schema is fetched elsewhere); its sentence names the args and
// says no rules ship and it never gets the projects. It reads the
// built-ins' own suggestions, as TestOfferedServerEgressIsDeclared does.
func TestTheCodingAgentSuggestsSemgrep(t *testing.T) {
	old := suggestionsOf
	suggestionsOf = func(a Agent) []offer.Suggestion { return a.Builtin.SuggestedServers() }
	t.Cleanup(func() { suggestionsOf = old })
	coder, err := Lookup("coder")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := Suggested(coder, "semgrep")
	if err != nil {
		t.Fatal(err)
	}
	if srv.PyPI != "semgrep" || !slices.Equal(srv.Args, []string{"mcp"}) || !slices.Equal(srv.Egress, []string{"semgrep.dev"}) ||
		len(srv.Secrets) != 0 || srv.Env["SEMGREP_SEND_METRICS"] != "off" {
		t.Fatalf("semgrep suggestion = %+v", srv)
	}
	if !slices.Equal(srv.Tools, []string{"semgrep_scan_with_custom_rule", "get_supported_languages", "get_abstract_syntax_tree"}) {
		t.Errorf("the suggestion gives %q: only the tools that work on code passed in the call", srv.Tools)
	}
	if srv.Env["OTEL_SDK_DISABLED"] != "true" {
		t.Errorf("its OpenTelemetry export is not switched off: %v", srv.Env)
	}
	sg, ok := suggestionFor(coder, srv)
	if !ok {
		t.Fatal("the added server is not its suggestion")
	}
	line := serverLine(srv) + "; " + sg.Note
	for _, want := range []string{"started with the arguments mcp", "reaching semgrep.dev alone", "Agentry ships no Semgrep rules", "never the agent's projects"} {
		if !strings.Contains(line, want) {
			t.Errorf("sentence %q lacks %q", line, want)
		}
	}
	for _, name := range []string{"assistant", "main"} {
		a, err := Lookup(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Suggested(a, "semgrep"); err == nil {
			t.Errorf("the %s agent suggests Semgrep", name)
		}
	}
}
