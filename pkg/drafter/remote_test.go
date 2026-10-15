package drafter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
	"github.com/blaktron/agentry/pkg/manifest"
)

// A remote MCP server and a package server's OAuth sign-in in a draft. A
// remote server is held by the core's V5 and noted with what its first
// run asks; a package's oauth block must be a sign-in the guide lists,
// copied exactly.

const (
	linearRemote = `{"url": "https://mcp.linear.app/mcp", "auth": "oauth", "scopes": ["read"], "tools": ["list_issues"]}`
	githubRemote = `{"url": "https://api.githubcopilot.com/mcp/readonly", "auth": "secret", "secrets": ["GITHUB_TOKEN"]}`
	asanaRemote  = `{"url": "https://mcp.asana.com/v2/mcp", "auth": "oauth"}`
	otherRemote  = `{"url": "https://mcp.example.com/mcp", "auth": "none"}`
)

// declaredAs is a listed sign-in as a draft declares it, with the given
// change applied before encoding.
func declaredAs(t *testing.T, si SignInEntry, change func(*manifest.MCPServer)) string {
	t.Helper()
	oauth := si.OAuth
	oauth.Files = append([]manifest.MCPOAuthFile(nil), si.OAuth.Files...)
	srv := manifest.MCPServer{Version: si.Versions, Egress: append([]string(nil), si.Egress...), Env: map[string]string{}, OAuth: &oauth}
	if si.Ecosystem == "npm" {
		srv.NPM = si.Package
	} else {
		srv.PyPI = si.Package
	}
	for k, v := range si.Env {
		srv.Env[k] = v
	}
	if change != nil {
		change(&srv)
	}
	b, err := json.Marshal(srv)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// listedSignIn is the guide's workspace-mcp sign-in as a draft declares
// it, changed by change.
func listedSignIn(t *testing.T, change func(*manifest.MCPServer)) string {
	t.Helper()
	l, err := LoadServerList(FS())
	if err != nil {
		t.Fatal(err)
	}
	si, ok := l.SignIn("pypi", "workspace-mcp", "2.0.1")
	if !ok {
		t.Fatal("the guide lists no workspace-mcp 2.0.1 sign-in")
	}
	return declaredAs(t, si, change)
}

// manifestWith is a passing v2 manifest that declares the servers, a JSON
// array's members.
func manifestWith(servers string) string {
	return `{"schemaVersion": "2", "name": "x", "namespace": "acme", "version": "0.1.0", "summary": "s", "modes": ["autonomous"],
		"entrypoints": {"instruction": "AGENT.md"}, "publish": {"include": ["agent.yaml", "AGENT.md"]},
		"capabilities": {"network": {"egress": "none"}, "mcpServers": [` + servers + `]}}`
}

func TestARemoteServerIsNotedWithWhatItsFirstRunAsks(t *testing.T) {
	f := &fakeMeta{}
	d := checked(t, f, linearRemote+", "+githubRemote+", "+asanaRemote+", "+otherRemote)
	if len(d.serverProblems) != 0 || f.calls != 0 {
		t.Fatalf("a remote server is no package to resolve or refuse: problems %v, calls %d", d.serverProblems, f.calls)
	}
	all := strings.Join(d.serversAdmitted, "\n")
	for _, want := range []string{
		"https://mcp.linear.app/mcp: a remote server, run by its vendor; Agentry checks its address and what it asks for, not the vendor's code. It signs in with OAuth: the first run of this version asks whoever runs it to sign in at the vendor and allow the scopes read, and a run gets an access token, nothing else. Seen to answer through Agentry (a consent and two hosted runs, 2026-10-07).",
		"https://api.githubcopilot.com/mcp/readonly: a remote server, run by its vendor; Agentry checks its address and what it asks for, not the vendor's code. It signs in with the key GITHUB_TOKEN, sent to the vendor as a bearer token. Seen to answer through Agentry (a local run, 2026-10-05).",
		"allow the scopes the vendor asks for",
		"Its sign-in was read from Asana's own metadata, not seen to run here (vendor metadata read 2026-10-09). Asana registers no client for Agentry, so whoever runs it brings their own OAuth client; the run setup says how.",
		"https://mcp.example.com/mcp: a remote server, run by its vendor; Agentry checks its address and what it asks for, not the vendor's code. It signs in with nothing. It is on no list, so it was judged with the drafting guide.",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("the notes do not say %q:\n%s", want, all)
		}
	}
	if strings.Contains(strings.ToLower(all), "safe") {
		t.Errorf("the notes call something safe:\n%s", all)
	}
}

// The line indexes the manifest's own server list, not the package-only
// list, so a remote server declared first does not send the repair turn to
// the wrong entry.
func TestARulesLineNamesAServerByItsOwnIndex(t *testing.T) {
	d := checked(t, &fakeMeta{}, linearRemote+", "+markitdown)
	if len(d.serverProblems) != 1 || !strings.HasPrefix(d.serverProblems[0], "capabilities.mcpServers[1]: pypi:markitdown-mcp@0.0.1a7: does not run here") {
		t.Fatalf("the line names the manifest's own index: %v", d.serverProblems)
	}
}

func TestAPackagesSignInIsHeldToTheListedOne(t *testing.T) {
	d := checked(t, &fakeMeta{}, listedSignIn(t, nil))
	if len(d.serverProblems) != 0 {
		t.Fatalf("the listed sign-in, copied exactly, passes: %v", d.serverProblems)
	}
	if len(d.serversAdmitted) != 1 || strings.Contains(d.serversAdmitted[0], "on neither list") {
		t.Errorf("a listed sign-in is its package's one admission, not \"on neither list\": %q", d.serversAdmitted)
	}
	if all := strings.Join(d.serversAdmitted, "\n"); !strings.Contains(all,
		"pypi:workspace-mcp@2.0.1: signs in with OAuth at https://accounts.google.com with the sign-in the drafting guide lists (a local run, and its renewal 65 minutes later, 2026-10-06): the first run of this version asks whoever runs it to allow the scopes https://www.googleapis.com/auth/gmail.readonly, and the server gets an access token, never a refresh token. The vendor registers no client for Agentry") {
		t.Errorf("the notes do not say what the listed sign-in asks:\n%s", all)
	}

	cases := map[string]struct {
		change func(*manifest.MCPServer)
		want   string
	}{
		"a template of its own": {func(s *manifest.MCPServer) { s.OAuth.Files[0].Template = `{"token": "{access_token}"}` }, "oauth.files are not the listed files, path and template"},
		"a wider scope":         {func(s *manifest.MCPServer) { s.OAuth.Scopes = []string{"https://mail.google.com/"} }, "oauth.scopes are not https://www.googleapis.com/auth/gmail.readonly"},
		"its host left out":     {func(s *manifest.MCPServer) { s.Egress = nil }, "egress lacks gmail.googleapis.com"},
		"a setting changed":     {func(s *manifest.MCPServer) { s.Env["USER_GOOGLE_EMAIL"] = "someone@example.com" }, "env.USER_GOOGLE_EMAIL is not me@agentry.invalid"},
		"another version":       {func(s *manifest.MCPServer) { s.Version = "2.0.2" }, "its oauth block is not a sign-in the drafting guide lists for this version"},
	}
	for name, c := range cases {
		d := checked(t, &fakeMeta{}, listedSignIn(t, c.change))
		if len(d.serverProblems) != 1 || !strings.Contains(d.serverProblems[0], c.want) || !strings.HasPrefix(d.serverProblems[0], "capabilities.mcpServers[0]: pypi:workspace-mcp@") {
			t.Errorf("%s: want one line naming %q, got %v", name, c.want, d.serverProblems)
		}
		if strings.Contains(strings.Join(d.serverProblems, ""), "someone@example.com") {
			t.Errorf("%s: a value the draft wrote reached the rules line", name)
		}
	}

	made := `{"npm": "made-up-calendar-mcp", "version": "1.0.0", "egress": ["www.googleapis.com"], "oauth": {"issuer": "https://accounts.google.com", "env": {"TOKEN": "{access_token}"}}}`
	d = checked(t, &fakeMeta{}, made)
	if len(d.serverProblems) != 1 || !strings.Contains(d.serverProblems[0], unlistedSignIn) {
		t.Fatalf("a sign-in the guide does not list is a rules line: %v", d.serverProblems)
	}
}

// A first answer with a sign-in of its own gets the repair turn with the
// line, and a second that copies the listed sign-in is kept, its notes
// saying what the first run asks.
func TestADraftsOwnSignInGetsTheRepairTurn(t *testing.T) {
	out := t.TempDir()
	var tasks []string
	own := listedSignIn(t, func(s *manifest.MCPServer) { s.OAuth.Files[0].Template = `{"token": "{access_token}"}` })
	p := PostStep{
		OutDir: out, Description: "Summarise my unread Gmail each morning.", Task: "Summarise my unread Gmail each morning.",
		Scan: webscan.Result{Verdict: webscan.Pass}, Ref: Namespace + "/" + Name + "@" + Version, Checker: &postChecker{},
		Servers: (&fakeMeta{}).resolve,
		Repair: func(_ context.Context, rt RepairTurn) (*receipt.Envelope, error) {
			tasks = append(tasks, rt.Task)
			if err := os.MkdirAll(filepath.Join(rt.OutDir, "reports"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(rt.OutDir, "reports", instructionsReport), []byte("# Gmail\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return &receipt.Envelope{Status: "ok", Result: json.RawMessage(withServers(postRepaired, listedSignIn(t, nil)))}, nil
		},
	}
	d, _, err := p.Run(context.Background(), firstTurn(t, out, withServers(passingDraft, own)), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "- capabilities.mcpServers[0]: pypi:workspace-mcp@2.0.1: its sign-in is not the one the drafting guide lists for it: oauth.files are not the listed files, path and template; " + copyTheListedSignIn
	if len(tasks) != 1 || !strings.Contains(tasks[0], want) {
		t.Fatalf("the repair task does not carry %q:\n%.2000s", want, strings.Join(tasks, "\n---\n"))
	}
	if !d.IsRepaired() || len(d.serverProblems) != 0 || !strings.Contains(d.Notes, "with the sign-in the drafting guide lists") {
		t.Fatalf("the draft: repaired %v, problems %v, notes %q", d.IsRepaired(), d.serverProblems, d.Notes)
	}
}

// Every sign-in the guide lists is one the core's rules take, as a draft
// copies it: a draft that copies it exactly cannot be refused for its
// shape.
func TestEveryListedSignInPassesTheCore(t *testing.T) {
	l, err := LoadServerList(FS())
	if err != nil {
		t.Fatal(err)
	}
	if len(l.SignIns) == 0 || len(l.Remote) == 0 {
		t.Fatal("the guide lists no sign-in or no remote server")
	}
	for _, si := range l.SignIns {
		m, err := manifest.Parse([]byte(manifestWith(declaredAs(t, si, nil))))
		if err != nil {
			t.Fatalf("%s: %v", si.Package, err)
		}
		if errs := manifest.Blocking(manifest.Validate(m)); len(errs) > 0 {
			t.Errorf("%s's listed sign-in fails the core: %v", si.Package, errs)
		}
	}
	for _, r := range l.Remote {
		srv := map[string]any{"url": r.URL, "auth": r.Auth, "scopes": r.Scopes}
		if r.Auth == manifest.AuthSecret {
			srv["secrets"] = []string{"VENDOR_TOKEN"}
		}
		b, _ := json.Marshal(srv)
		m, err := manifest.Parse([]byte(manifestWith(string(b))))
		if err != nil {
			t.Fatalf("%s: %v", r.URL, err)
		}
		if errs := manifest.Blocking(manifest.Validate(m)); len(errs) > 0 {
			t.Errorf("%s (%s) fails the core: %v", r.URL, r.Auth, errs)
		}
	}
}

func TestTheListRefusesABadRemoteOrSignInEntry(t *testing.T) {
	entry := func(section, body string) fstest.MapFS {
		return fstest.MapFS{ServerListFile: {Data: []byte("servers: []\n" + section + ":\n" + body)}}
	}
	cases := map[string]fstest.MapFS{
		"an http address":           entry("remote", "- {vendor: V, url: http://x.example/mcp, auth: none, observed: o, evidence: e, date: 2026-10-09}\n"),
		"an auth outside the three": entry("remote", "- {vendor: V, url: https://x.example/mcp, auth: basic, observed: o, evidence: e, date: 2026-10-09}\n"),
		"oauth with no client":      entry("remote", "- {vendor: V, url: https://x.example/mcp, auth: oauth, observed: o, evidence: e, date: 2026-10-09}\n"),
		"scopes on a key":           entry("remote", "- {vendor: V, url: https://x.example/mcp, auth: secret, scopes: [a], observed: o, evidence: e, date: 2026-10-09}\n"),
		"no evidence":               entry("remote", "- {vendor: V, url: https://x.example/mcp, auth: none, observed: o, date: 2026-10-09}\n"),
		"a sign-in with a range":    entry("signins", "- {ecosystem: pypi, package: p, versions: '>=1', client: own, oauth: {issuer: https://a.example}, observed: o, evidence: e, date: 2026-10-09}\n"),
		"a sign-in with no issuer":  entry("signins", "- {ecosystem: pypi, package: p, versions: 1.0.0, client: own, observed: o, evidence: e, date: 2026-10-09}\n"),
		"a sign-in with no client":  entry("signins", "- {ecosystem: pypi, package: p, versions: 1.0.0, oauth: {issuer: https://a.example}, observed: o, evidence: e, date: 2026-10-09}\n"),
	}
	for name, fsys := range cases {
		if _, err := LoadServerList(fsys); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}

// The bench: a fixture can expect a remote server at a host and a
// package carrying the listed sign-in; a remote server counts its
// address's host and is never "not pinned".
func TestTheBenchHoldsADraftToItsRemoteServersAndSignIns(t *testing.T) {
	shape := func(servers string, e Expect) string {
		t.Helper()
		d := &Draft{Manifest: json.RawMessage(manifestWith(servers)), Instructions: "## Role\nx\n## Inputs\nx\n## Steps\nx\n## Output\nx\n## Background\nx\n## Checks\nx\n"}
		yml, err := manifestYAML(d.Manifest)
		if err != nil {
			t.Fatal(err)
		}
		m, err := manifest.Parse(yml)
		if err != nil {
			t.Fatal(err)
		}
		e.Modes, e.MaxServers = []string{"autonomous"}, 1
		return strings.Join(CheckShape(m, d, e), "; ")
	}
	if got := shape(linearRemote, Expect{Hosts: []string{"mcp.linear.app"}, Remote: map[string]string{"mcp.linear.app": "oauth"}}); got != "" {
		t.Fatalf("a draft with the remote server its fixture expects: %s", got)
	}
	if got := shape(linearRemote, Expect{Hosts: []string{"api.linear.app"}}); !strings.Contains(got, "reaches mcp.linear.app, not among") {
		t.Fatalf("a remote server's host is not counted: %q", got)
	}
	if got := shape(githubRemote, Expect{Hosts: []string{"api.githubcopilot.com"}, MaxSecrets: 1, Remote: map[string]string{"api.githubcopilot.com": "secret"}}); got != "" {
		t.Fatalf("a key-signed remote server its fixture expects: %s", got)
	}
	if got := shape(githubRemote, Expect{Hosts: []string{"api.githubcopilot.com"}, MaxSecrets: 1, Remote: map[string]string{"api.githubcopilot.com": "oauth"}}); !strings.Contains(got, "no remote server at api.githubcopilot.com signed in with oauth") {
		t.Fatalf("a key-signed remote server is taken for an OAuth one: %q", got)
	}
	if got := shape(listedSignIn(t, nil), Expect{Hosts: []string{"gmail.googleapis.com"}, SignIns: []string{"pypi:workspace-mcp"}}); got != "" {
		t.Fatalf("a draft carrying the listed sign-in: %s", got)
	}
	own := listedSignIn(t, func(s *manifest.MCPServer) { s.OAuth.Files[0].Template = `{"token": "{access_token}"}` })
	if got := shape(own, Expect{Hosts: []string{"gmail.googleapis.com"}, SignIns: []string{"pypi:workspace-mcp"}}); !strings.Contains(got, "pypi:workspace-mcp does not carry the sign-in the drafting guide lists") {
		t.Fatalf("a sign-in of the draft's own passes the bench: %q", got)
	}
}

// A remote server signed in to by OAuth names the scopes its task needs,
// where the vendor offers more than one.
func TestARemoteServerNamesItsScopes(t *testing.T) {
	d := checked(t, &fakeMeta{}, otherRemote+", "+`{"url": "https://api.githubcopilot.com/mcp/readonly", "auth": "oauth"}`)
	if len(d.serverProblems) != 1 || !strings.HasPrefix(d.serverProblems[0], "capabilities.mcpServers[1]: https://api.githubcopilot.com/mcp/readonly: it signs in with OAuth and names no scopes") ||
		!strings.Contains(d.serverProblems[0], "from those GitHub offers (repo, read:org") {
		t.Fatalf("a GitHub OAuth server with no scopes: %v", d.serverProblems)
	}
	if d := checked(t, &fakeMeta{}, asanaRemote); len(d.serverProblems) != 0 {
		t.Fatalf("a vendor that offers one scope needs none named: %v", d.serverProblems)
	}
	if d := checked(t, &fakeMeta{}, `{"url": "https://mcp.unknown.example/mcp", "auth": "oauth"}`); len(d.serverProblems) != 0 {
		t.Fatalf("a vendor the list does not know is not held to its scopes: %v", d.serverProblems)
	}
}

// A package on the core's unsupported list is refused whatever its
// block: it has no listed sign-in, and the core's V5 names it too.
func TestAnUnsupportedPackageCannotSignIn(t *testing.T) {
	gsuite := `{"pypi": "mcp-gsuite", "version": "0.4.1", "egress": ["gmail.googleapis.com"], "oauth": {"issuer": "https://accounts.google.com", "env": {"TOKEN": "{access_token}"}}}`
	d := checked(t, &fakeMeta{}, gsuite)
	if len(d.serverProblems) != 1 || !strings.Contains(d.serverProblems[0], unlistedSignIn) {
		t.Fatalf("mcp-gsuite's sign-in: %v", d.serverProblems)
	}
	m, err := manifest.Parse([]byte(manifestWith(gsuite)))
	if err != nil {
		t.Fatal(err)
	}
	if errs := manifest.Blocking(manifest.Validate(m)); len(errs) == 0 || !strings.Contains(errs[0].Error(), "declare pypi:workspace-mcp instead") {
		t.Fatalf("the core does not name the alternative: %v", errs)
	}
}

// A repair that drops a remote server the report does not name is a drop:
// a remote server's identity is its address.
func TestARepairThatDropsARemoteServerIsCaught(t *testing.T) {
	first := &Draft{Manifest: json.RawMessage(manifestWith(linearRemote))}
	second := &Draft{Manifest: json.RawMessage(manifestWith("")), Validation: json.RawMessage(`{"passed":true}`)}
	if got := droppedUnnamed(first, second, RepairReport{}); len(got) == 0 {
		t.Fatal("a dropped remote server is not caught")
	}
}

// The bench counts the Drafter's own bullets, not the server check's: a
// draft of five bullets with a remote server's note is within the cap.
func TestTheBenchCountsOnlyTheDraftersBullets(t *testing.T) {
	d := checked(t, &fakeMeta{}, linearRemote)
	d.ownNotes = strings.Repeat("- a bullet\n", MaxNoteBullets)
	d.noteServerProblems()
	if !strings.Contains(d.Notes, serversAdmittedHeading) {
		t.Fatalf("the notes carry no server section: %q", d.Notes)
	}
	if n := noteBullets(d.Notes); n != MaxNoteBullets {
		t.Fatalf("counted %d bullets, want the Drafter's %d", n, MaxNoteBullets)
	}
}
