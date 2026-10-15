package main

// Connections at Start, against a fake vendor: consent once, then later
// runs without it; no terminal is exit 11 naming the server; a refused
// refresh asks again; and that a probe refresh token and client secret
// reach nothing a run is given or leaves behind but the store's own secret
// file.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/mcpauth/mcpauthtest"
	"github.com/blaktron/agentry-cli/pkg/runner/connection"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
)

const probeClientSecret = "PROBE-SECRET-d8-condition"

// connectionFixture wires the seams to a fake vendor that issues probe
// refresh tokens and, to a client that cannot be public, a probe secret.
// opened counts the consents the browser was opened for.
func connectionFixture(t *testing.T) (*mcpauthtest.Vendor, *manifest.Manifest, *installdir.Meta, *int) {
	t.Helper()
	isolateEnv(t)
	t.Setenv("AGENTRY_RUNNER_HOME", t.TempDir())
	t.Setenv("AGENTRY_NO_BROWSER", "")
	v := mcpauthtest.New(t)
	v.DCR, v.NoPublic, v.ClientSecret = true, true, probeClientSecret
	opened := 0
	browser := v.Browser()
	prevHTTP, prevKeys, prevTTY, prevOpen, prevCIMD := vendorHTTP, connectionKeys, consentTTY, openBrowser, nativeCIMD
	vendorHTTP = func() *http.Client { return v.Guard().Client() }
	connectionKeys = func() connection.Keychain { return nil }
	consentTTY = func(io.Reader) bool { return true }
	openBrowser = func(u string) error { opened++; browser(u); return nil }
	nativeCIMD = ""
	t.Cleanup(func() {
		vendorHTTP, connectionKeys, consentTTY, openBrowser, nativeCIMD = prevHTTP, prevKeys, prevTTY, prevOpen, prevCIMD
	})
	m := &manifest.Manifest{}
	m.Capabilities.MCPServers = []manifest.MCPServer{{URL: v.ServerURL(), Auth: manifest.AuthOAuth, Scopes: []string{"read"}}}
	meta := &installdir.Meta{Ref: "acme/mail@1.0.0", Namespace: "acme", Name: "mail", Version: "1.0.0", Digest: "sha256:" + strings.Repeat("a", 64)}
	return v, m, meta, &opened
}

func testCmd(stderr io.Writer) *cobra.Command {
	c := &cobra.Command{}
	c.SetContext(context.Background())
	c.SetIn(strings.NewReader(""))
	c.SetOut(io.Discard)
	c.SetErr(stderr)
	return c
}

func TestAConnectionIsAskedForOnceAndReusedByTheNextRun(t *testing.T) {
	v, m, meta, opened := connectionFixture(t)
	home, err := runnerHome()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	first, err := ensureConnections(testCmd(&stderr), home, meta, m)
	if err != nil {
		t.Fatalf("first run: %v\n%s", err, stderr.String())
	}
	if *opened != 1 || first.tokens[v.ServerURL()].AccessToken == "" {
		t.Fatalf("first run: opened %d, tokens %v", *opened, first.tokens)
	}
	second, err := ensureConnections(testCmd(&stderr), home, meta, m)
	if err != nil {
		t.Fatal(err)
	}
	if *opened != 1 {
		t.Fatal("the second run of the same version asked for consent again")
	}
	if tok := second.tokens[v.ServerURL()].AccessToken; tok == "" || tok == first.tokens[v.ServerURL()].AccessToken {
		t.Fatalf("the second run's access token %q is not a fresh one", tok)
	}
	if !strings.Contains(second.line, v.ServerURL()+" (issuer "+v.URL+", scopes read, client dcr)") || !strings.Contains(second.line, "an access token alone") {
		t.Fatalf("receipt line = %q", second.line)
	}

	// Another version of the same QuickAgent has no connection: it asks.
	other := *meta
	other.Ref, other.Version = "acme/mail@1.0.1", "1.0.1"
	if _, err := ensureConnections(testCmd(&stderr), home, &other, m); err != nil {
		t.Fatal(err)
	}
	if *opened != 2 {
		t.Fatalf("another version reused a connection (opened %d)", *opened)
	}

	// Every refresh token the vendor issued, and the client secret, reach
	// none of what a run is given (the tokens map, the receipt line),
	// nothing said on the terminal, and no file under the runner home but
	// the store's own secret files: searched raw and base64.
	probes := append([]string{probeClientSecret}, v.Issued...)
	given := stderr.String() + first.line + second.line
	for _, tok := range []map[string]engine.ConnectionToken{first.tokens, second.tokens} {
		for _, t := range tok {
			given += " " + t.AccessToken
		}
	}
	assertNoProbe(t, "what the run is given and the terminal", given, probes)
	err = filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if filepath.Dir(path) == connection.Dir(home) && strings.HasSuffix(path, ".secret") {
			return nil // the store's 0600 secret half, where no keychain answers
		}
		b, rerr := os.ReadFile(path) //nolint:gosec // the test's runner home
		if rerr != nil {
			return rerr
		}
		assertNoProbe(t, path, string(b), probes)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A package server's oauth block is signed in to at its issuer, with no
// resource, keyed by the package's id; the next run of the same version
// asks nothing; the renewal the engine calls mid-run refreshes the same
// connection, writing each rotated refresh token back; and neither a
// refresh token nor the client secret reaches anything a run is given
// (searched raw and base64).
func TestAPackageServerIsSignedInAtItsIssuerAndRenewedMidRun(t *testing.T) {
	v, m, meta, opened := connectionFixture(t)
	v.IssuerOnly = true
	m.Capabilities.MCPServers = []manifest.MCPServer{{
		PyPI: "workspace-mcp", Version: "2.0.1",
		OAuth: &manifest.MCPOAuth{Issuer: v.URL, Scopes: []string{"read"}, Env: map[string]string{"GOOGLE_ACCESS_TOKEN": "{access_token}"}},
	}}
	home, err := runnerHome()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	first, err := ensureConnections(testCmd(&stderr), home, meta, m)
	if err != nil {
		t.Fatalf("first run: %v\n%s", err, stderr.String())
	}
	tok := first.tokens["pypi:workspace-mcp"]
	if *opened != 1 || tok.AccessToken == "" || tok.ExpiresAt.IsZero() {
		t.Fatalf("first run: opened %d, tokens %v", *opened, first.tokens)
	}
	if !strings.Contains(stderr.String(), "needs you to sign in to pypi:workspace-mcp (signing in at "+v.URL+")") {
		t.Fatalf("the consent does not name the package and its issuer:\n%s", stderr.String())
	}
	for _, f := range v.Requests {
		if f.Has("resource") {
			t.Fatalf("a token request carried a resource: %v", f)
		}
	}
	second, err := ensureConnections(testCmd(&stderr), home, meta, m)
	if err != nil || *opened != 1 {
		t.Fatalf("the second run asked again (opened %d): %v", *opened, err)
	}
	if !strings.Contains(second.line, "pypi:workspace-mcp (issuer "+v.URL+", scopes read, client dcr)") {
		t.Fatalf("receipt line = %q", second.line)
	}
	// Two renewals in a row: the second works only if the first wrote the
	// rotated refresh token back.
	var renewed []string
	for range 2 {
		r, err := second.renew(context.Background(), "pypi:workspace-mcp")
		if err != nil || r.AccessToken == "" || r.ExpiresAt.IsZero() {
			t.Fatalf("renew: %+v %v", r, err)
		}
		renewed = append(renewed, r.AccessToken)
	}
	if renewed[0] == renewed[1] || renewed[0] == second.tokens["pypi:workspace-mcp"].AccessToken {
		t.Fatalf("renewals did not bring fresh tokens: %v", renewed)
	}

	probes := append([]string{probeClientSecret}, v.Issued...)
	given := stderr.String() + first.line + second.line + " " + tok.AccessToken + " " + strings.Join(renewed, " ")
	assertNoProbe(t, "what the run is given and the terminal", given, probes)
	err = filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if filepath.Dir(path) == connection.Dir(home) && strings.HasSuffix(path, ".secret") {
			return nil
		}
		b, rerr := os.ReadFile(path) //nolint:gosec // the test's runner home
		if rerr != nil {
			return rerr
		}
		assertNoProbe(t, path, string(b), probes)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertNoProbe(t *testing.T, where, s string, probes []string) {
	t.Helper()
	for _, p := range probes {
		if strings.Contains(s, p) || strings.Contains(s, base64.StdEncoding.EncodeToString([]byte(p))) || strings.Contains(s, base64.RawURLEncoding.EncodeToString([]byte(p))) {
			t.Fatalf("%s holds %q", where, p)
		}
	}
}

func TestNoTerminalRefusesARunWithoutAConnectionByName(t *testing.T) {
	v, m, meta, opened := connectionFixture(t)
	consentTTY = func(io.Reader) bool { return false }
	home, _ := runnerHome()
	_, err := ensureConnections(testCmd(io.Discard), home, meta, m)
	var ce *engine.ClassifiedError
	if !errors.As(err, &ce) || ce.Code != engine.ExitPolicy || !strings.Contains(err.Error(), v.ServerURL()) || !strings.Contains(err.Error(), "agentry connect acme/mail@1.0.0") {
		t.Fatalf("err = %v, want exit 11 naming the server and agentry connect", err)
	}
	if *opened != 0 {
		t.Fatal("a browser was opened with no terminal")
	}
}

func TestARefusedRefreshAsksForConsentAgain(t *testing.T) {
	v, m, meta, opened := connectionFixture(t)
	home, _ := runnerHome()
	if _, err := ensureConnections(testCmd(io.Discard), home, meta, m); err != nil {
		t.Fatal(err)
	}
	v.Refused = true // the person revoked it at the vendor
	var stderr bytes.Buffer
	got, err := ensureConnections(testCmd(&stderr), home, meta, m)
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr.String())
	}
	if *opened != 2 || got.tokens[v.ServerURL()].AccessToken == "" || !strings.Contains(stderr.String(), "no longer works") {
		t.Fatalf("opened %d, stderr:\n%s", *opened, stderr.String())
	}
}

func TestConnectionsListRemoveAndUninstall(t *testing.T) {
	v, m, meta, _ := connectionFixture(t)
	home, _ := runnerHome()
	if _, err := ensureConnections(testCmd(io.Discard), home, meta, m); err != nil {
		t.Fatal(err)
	}
	out, _, err := runCLI(t, nil, "connections", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []connectionOut
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 1 || rows[0].Server != v.ServerURL() || rows[0].State != "connected" || rows[0].KeptIn != "file" {
		t.Fatalf("list = %s (%v)", out, err)
	}
	assertNoProbe(t, "connections list", out, append([]string{probeClientSecret}, v.Issued...))

	// Uninstalling the version removes its connection and revokes it.
	out, _, err = runCLI(t, nil, "uninstall", "acme/mail", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var un uninstallOutput
	if err := json.Unmarshal([]byte(out), &un); err != nil || len(un.Connections) != 1 || un.Connections[0].Revocation != connection.RevokeDone {
		t.Fatalf("uninstall = %s (%v)", out, err)
	}
	if len(v.Revoked) != 1 {
		t.Fatalf("revoked at the vendor: %v", v.Revoked)
	}
	out, _, _ = runCLI(t, nil, "connections", "list")
	if !strings.Contains(out, "no connections kept") {
		t.Fatalf("after uninstall: %s", out)
	}
}

func TestConnectionsClientSetListRemove(t *testing.T) {
	_, _, _, _ = connectionFixture(t)
	consentTTY = func(io.Reader) bool { return false }
	if _, _, err := runCLI(t, strings.NewReader("my-secret\n"), "connections", "client", "set", "https://accounts.google.com", "--client-id", "123.apps.googleusercontent.com"); err != nil {
		t.Fatal(err)
	}
	out, _, err := runCLI(t, nil, "connections", "client", "list")
	if err != nil || !strings.Contains(out, "https://accounts.google.com  client 123.apps.googleusercontent.com  (secret kept in the file)") || strings.Contains(out, "my-secret") {
		t.Fatalf("list = %q (%v)", out, err)
	}
	home, _ := runnerHome()
	got, err := connection.OpenClients(home, nil).Get("https://accounts.google.com")
	if err != nil || got.Secret != "my-secret" {
		t.Fatalf("kept %+v %v", got, err)
	}
	if out, _, err := runCLI(t, nil, "connections", "client", "remove", "https://accounts.google.com"); err != nil || !strings.Contains(out, "forgot") {
		t.Fatalf("remove = %q (%v)", out, err)
	}
}

// agentry connect takes nobody through consent for a package its runs
// would refuse.
func TestConnectRefusesAnUnsupportedPackage(t *testing.T) {
	gsuite := manifest.MCPServer{PyPI: "mcp-gsuite", Version: "0.4.1", OAuth: &manifest.MCPOAuth{Issuer: "https://accounts.google.com"}}
	err := refuseUnsupported([]manifest.MCPServer{{PyPI: "workspace-mcp", Version: "2.0.1", OAuth: gsuite.OAuth}, gsuite})
	var ce *engine.ClassifiedError
	if !errors.As(err, &ce) || ce.Code != engine.ExitPolicy || !strings.Contains(err.Error(), "pypi:mcp-gsuite cannot run on an access token alone") || !strings.Contains(err.Error(), "pypi:workspace-mcp") {
		t.Fatalf("err = %v", err)
	}
	if err := refuseUnsupported([]manifest.MCPServer{{PyPI: "workspace-mcp", Version: "2.0.1", OAuth: gsuite.OAuth}}); err != nil {
		t.Fatal(err)
	}
}

func TestConnectNeedsAnInstalledRelease(t *testing.T) {
	_, _, _, _ = connectionFixture(t)
	_, _, err := runCLI(t, nil, "connect", "acme/mail")
	if err == nil || !strings.Contains(err.Error(), "is not installed on this machine") {
		t.Fatalf("err = %v", err)
	}
}

func TestGoogleGetsAgentrysOwnSteps(t *testing.T) {
	s := ownClientSteps("https://accounts.google.com", "https://gmailmcp.googleapis.com/mcp/v1")
	for _, want := range []string{"Desktop app", "gmailmcp.googleapis.com", "7 days", "In production", "Test users"} {
		if !strings.Contains(s, want) {
			t.Fatalf("Google steps miss %q:\n%s", want, s)
		}
	}
	if strings.Contains(ownClientSteps("https://auth.example", "https://mcp.example/mcp"), "Google") {
		t.Fatal("another vendor got Google's steps")
	}
}

// TestAHostedRunTakesItsTokensFromAFile: --connections-file hands the run
// the access tokens a host was given, keyed as the engine keys them, renews
// nothing and opens no store; the receipt's line says so.
func TestAHostedRunTakesItsTokensFromAFile(t *testing.T) {
	isolateEnv(t)
	t.Setenv("AGENTRY_RUNNER_HOME", t.TempDir())
	prev := vendorHTTP
	vendorHTTP = func() *http.Client { t.Fatal("a vendor was reached"); return nil }
	t.Cleanup(func() { vendorHTTP = prev })
	m := &manifest.Manifest{}
	m.Capabilities.MCPServers = []manifest.MCPServer{
		{URL: "https://mcp.linear.app/mcp", Auth: manifest.AuthOAuth, Scopes: []string{"read"}},
		{PyPI: "workspace-mcp", Version: "2.0.1", OAuth: &manifest.MCPOAuth{Issuer: "https://accounts.google.com", Scopes: []string{"gmail.readonly"}}},
		{URL: "https://api.example.com/mcp", Auth: manifest.AuthNone},
	}
	exp := time.Date(2026, 10, 7, 22, 0, 0, 0, time.UTC)
	path := writeConnectionsFile(t, 0o600, `{
		"https://mcp.linear.app/mcp": {"accessToken": "lin-access", "expiresAt": "2026-10-07T22:00:00Z", "issuer": "https://mcp.linear.app", "scopes": ["read"]},
		"pypi:workspace-mcp": {"accessToken": "ya29.google-access"}
	}`)
	conns, err := connectionsFromFile(path, m)
	if err != nil {
		t.Fatal(err)
	}
	if conns.renew != nil {
		t.Fatal("a hosted run's connections must renew nothing")
	}
	if got := conns.tokens["https://mcp.linear.app/mcp"]; got.AccessToken != "lin-access" || !got.ExpiresAt.Equal(exp) {
		t.Fatalf("linear: %+v", got)
	}
	if got := conns.tokens["pypi:workspace-mcp"]; got.AccessToken != "ya29.google-access" || !got.ExpiresAt.IsZero() {
		t.Fatalf("workspace-mcp: %+v", got)
	}
	if len(conns.tokens) != 2 {
		t.Fatalf("tokens for %d servers, want the two that sign in", len(conns.tokens))
	}
	for _, want := range []string{"renews nothing", "https://mcp.linear.app/mcp (issuer https://mcp.linear.app, scopes read)", "pypi:workspace-mcp (issuer not named"} {
		if !strings.Contains(conns.line, want) {
			t.Fatalf("receipt line %q lacks %q", conns.line, want)
		}
	}
	if strings.Contains(conns.line, "lin-access") || strings.Contains(conns.line, "ya29") {
		t.Fatal("a token reached the receipt's line")
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("AGENTRY_RUNNER_HOME"), "connections")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the connection store was opened: %v", err)
	}
}

// TestAConnectionsFileIsRefusedWhenItIsNotExact: a file others can read, a
// symlink, an unknown field (there is no place for a refresh token), a
// server the release does not declare, or a declared server it does not
// name each refuse the run with exit 11.
func TestAConnectionsFileIsRefusedWhenItIsNotExact(t *testing.T) {
	isolateEnv(t)
	m := &manifest.Manifest{}
	m.Capabilities.MCPServers = []manifest.MCPServer{{URL: "https://mcp.linear.app/mcp", Auth: manifest.AuthOAuth}}
	good := `{"https://mcp.linear.app/mcp": {"accessToken": "a"}}`
	link := filepath.Join(t.TempDir(), "link.json")
	if err := os.Symlink(writeConnectionsFile(t, 0o600, good), link); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		path string
		want string
	}{
		"readable by others": {writeConnectionsFile(t, 0o644, good), "it must be 0600"},
		"a symlink":          {link, "not a regular file"},
		"a refresh token":    {writeConnectionsFile(t, 0o600, `{"https://mcp.linear.app/mcp": {"accessToken": "a", "refreshToken": "r"}}`), "unknown field"},
		"an undeclared server": {writeConnectionsFile(t, 0o600, `{"https://mcp.linear.app/mcp": {"accessToken": "a"}, "https://evil.example/mcp": {"accessToken": "b"}}`),
			"does not declare"},
		"a missing server": {writeConnectionsFile(t, 0o600, `{}`), "handed no connection for it"},
		"an empty token":   {writeConnectionsFile(t, 0o600, `{"https://mcp.linear.app/mcp": {"accessToken": ""}}`), "handed no connection for it"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := connectionsFromFile(c.path, m)
			var ce *engine.ClassifiedError
			if !errors.As(err, &ce) || ce.Code != engine.ExitPolicy || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want exit 11 saying %q", err, c.want)
			}
		})
	}
}

func writeConnectionsFile(t *testing.T, mode os.FileMode, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "connections.json")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// sharedClientFixture is connectionFixture at a vendor that revokes an
// account's whole grant to a client, as Google does, with the person's own
// client kept for its issuer: every connection there is made with it.
func sharedClientFixture(t *testing.T) (*mcpauthtest.Vendor, *manifest.Manifest, *installdir.Meta, string) {
	t.Helper()
	v, m, meta, _ := connectionFixture(t)
	v.DCR, v.WholeGrant = false, true
	v.AddClient("own-client-702", "own-secret-702")
	home, err := runnerHome()
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.OpenClients(home, nil).Set(v.URL, "own-client-702", "own-secret-702"); err != nil {
		t.Fatal(err)
	}
	return v, m, meta, home
}

// connectVersion makes ref's connection to the fixture's server.
func connectVersion(t *testing.T, home string, meta *installdir.Meta, m *manifest.Manifest, ref string) *installdir.Meta {
	t.Helper()
	other := *meta
	other.Ref = ref
	if _, err := ensureConnections(testCmd(io.Discard), home, &other, m); err != nil {
		t.Fatalf("connect %s: %v", ref, err)
	}
	return &other
}

// Connecting again with the same own client overwrites the connection and
// revokes nothing, so at a vendor that revokes the whole grant neither
// the new connection nor another made with the client ends.
func TestConnectingAgainWithTheSameClientRevokesNothing(t *testing.T) {
	v, m, meta, home := sharedClientFixture(t)
	connectVersion(t, home, meta, m, meta.Ref)
	connectVersion(t, home, meta, m, "acme/cal@1.0.0")

	store, clients := connection.Open(home, nil), connection.OpenClients(home, nil)
	k := connection.Key{Ref: meta.Ref, Digest: meta.Digest, Server: v.ServerURL()}
	var stderr bytes.Buffer
	if _, err := consentFor(testCmd(&stderr), store, clients, vendorHTTP(), k, m.Capabilities.MCPServers[0]); err != nil {
		t.Fatalf("connect again: %v\n%s", err, stderr.String())
	}
	if len(v.Revoked) != 0 {
		t.Fatalf("connecting again revoked %d token(s) at the vendor", len(v.Revoked))
	}
	if !strings.Contains(stderr.String(), "made with the same client, so it is not revoked") {
		t.Errorf("stderr does not say why nothing was revoked:\n%s", stderr.String())
	}
	audit, _ := os.ReadFile(filepath.Join(connection.Dir(home), "audit.ndjson"))
	if !strings.Contains(string(audit), `"action":"replaced"`) || !strings.Contains(string(audit), connection.RevokeSameClient) {
		t.Errorf("the replacement is not in the audit log:\n%s", audit)
	}
	assertNoProbe(t, "the replacement's terminal and audit", stderr.String()+string(audit), append([]string{"own-secret-702"}, v.Issued...))
	for _, ref := range []string{meta.Ref, "acme/cal@1.0.0"} {
		key := connection.Key{Ref: ref, Digest: meta.Digest, Server: v.ServerURL()}
		if _, _, err := store.Use(context.Background(), key, vendorHTTP(), clients); err != nil {
			t.Errorf("%s no longer refreshes: %v", ref, err)
		}
	}
}

// A different client is still a removal of the old connection, revoked.
func TestConnectingAgainWithAnotherClientRevokesTheOld(t *testing.T) {
	v, m, meta, home := sharedClientFixture(t)
	connectVersion(t, home, meta, m, meta.Ref)
	v.AddClient("own-client-702b", "own-secret-702b")
	clients := connection.OpenClients(home, nil)
	if err := clients.Set(v.URL, "own-client-702b", "own-secret-702b"); err != nil {
		t.Fatal(err)
	}
	k := connection.Key{Ref: meta.Ref, Digest: meta.Digest, Server: v.ServerURL()}
	var stderr bytes.Buffer
	if _, err := consentFor(testCmd(&stderr), connection.Open(home, nil), clients, vendorHTTP(), k, m.Capabilities.MCPServers[0]); err != nil {
		t.Fatalf("%v\n%s", err, stderr.String())
	}
	if len(v.Revoked) != 1 || !strings.Contains(stderr.String(), "the previous connection was removed (revocation: revoked)") {
		t.Fatalf("revoked %d, stderr:\n%s", len(v.Revoked), stderr.String())
	}
}

// list names the connections that share a client's grant; remove and
// uninstall still revoke and name the kept ones the vendor ended
// with it, which do stop refreshing there.
func TestRemovalNamesTheConnectionsThatShareTheGrant(t *testing.T) {
	v, m, meta, home := sharedClientFixture(t)
	connectVersion(t, home, meta, m, meta.Ref)
	connectVersion(t, home, meta, m, "acme/mail@1.0.1")
	connectVersion(t, home, meta, m, "acme/cal@1.0.0")

	out, _, err := runCLI(t, nil, "connections", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []connectionOut
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 3 {
		t.Fatalf("list = %s (%v)", out, err)
	}
	for _, r := range rows {
		if len(r.SharesGrantWith) != 2 {
			t.Errorf("%s shares its grant with %v, want the other two", r.Ref, r.SharesGrantWith)
		}
	}
	if out, _, _ := runCLI(t, nil, "connections", "list"); !strings.Contains(out, "made with the same client as: acme/cal@1.0.0 → "+v.ServerURL()) {
		t.Errorf("list does not name the shared grant:\n%s", out)
	}

	// Uninstalling acme/mail removes both its versions' connections and
	// names acme/cal's, which it does not remove, as ended; --plan says so
	// first.
	cal := "acme/cal@1.0.0 → " + v.ServerURL()
	out, _, err = runCLI(t, nil, "uninstall", "acme/mail", "--plan")
	if err != nil || strings.Count(out, "stop working too: "+cal) != 2 {
		t.Fatalf("uninstall --plan does not warn (%v):\n%s", err, out)
	}
	out, _, err = runCLI(t, nil, "uninstall", "acme/mail")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "stop working too: "+cal) != 2 || strings.Contains(out, "too: acme/mail") {
		t.Fatalf("uninstall did not name the one kept connection it ended:\n%s", out)
	}
	assertNoProbe(t, "uninstall", out, append([]string{"own-secret-702"}, v.Issued...))
	_, _, err = connection.Open(home, nil).Use(context.Background(), connection.Key{Ref: "acme/cal@1.0.0", Digest: meta.Digest, Server: v.ServerURL()}, vendorHTTP(), connection.OpenClients(home, nil))
	var broken *connection.BrokenError
	if !errors.As(err, &broken) {
		t.Fatalf("the named connection still refreshes at a whole-grant vendor (%v): the warning would be wrong", err)
	}

	// Removing the last one names nothing.
	out, _, err = runCLI(t, nil, "connections", "remove", "--all", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var removed []removedConnection
	if err := json.Unmarshal([]byte(out), &removed); err != nil || len(removed) != 1 || len(removed[0].SharesGrantWith) != 0 {
		t.Fatalf("remove --all = %s (%v)", out, err)
	}
}
