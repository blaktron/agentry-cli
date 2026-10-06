package main

// Connections at Start (plan mcp-oauth-connections.md §6, agentry-cli#664),
// against a fake vendor: consent once, then later runs without it; no
// terminal is exit 11 naming the server; a refused refresh asks again; and
// D8's condition, that a probe refresh token and client secret reach
// nothing a run is given or leaves behind but the store's own secret file.

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
	if *opened != 1 || first.tokens[v.ServerURL()] == "" {
		t.Fatalf("first run: opened %d, tokens %v", *opened, first.tokens)
	}
	second, err := ensureConnections(testCmd(&stderr), home, meta, m)
	if err != nil {
		t.Fatal(err)
	}
	if *opened != 1 {
		t.Fatal("the second run of the same version asked for consent again")
	}
	if tok := second.tokens[v.ServerURL()]; tok == "" || tok == first.tokens[v.ServerURL()] {
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

	// D8: every refresh token the vendor issued, and the client secret,
	// reach none of what a run is given (the tokens map, the receipt line),
	// nothing said on the terminal, and no file under the runner home but
	// the store's own secret files: searched raw and base64.
	probes := append([]string{probeClientSecret}, v.Issued...)
	given := stderr.String() + first.line + second.line
	for _, tok := range []map[string]string{first.tokens, second.tokens} {
		for _, t := range tok {
			given += " " + t
		}
	}
	assertNoProbe(t, "what the run is given and the terminal", given, probes)
	err = filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if filepath.Dir(path) == connection.Dir(home) && strings.HasSuffix(path, ".secret") {
			return nil // the store's 0600 secret half, where no keychain answers (D6)
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
	if *opened != 2 || got.tokens[v.ServerURL()] == "" || !strings.Contains(stderr.String(), "no longer works") {
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

func TestConnectNeedsAnInstalledRelease(t *testing.T) {
	_, _, _, _ = connectionFixture(t)
	_, _, err := runCLI(t, nil, "connect", "acme/mail")
	if err == nil || !strings.Contains(err.Error(), "is not installed on this machine") {
		t.Fatalf("err = %v", err)
	}
}

func TestGoogleGetsAgentrysOwnSteps(t *testing.T) {
	s := ownClientSteps("https://accounts.google.com", "https://gmailmcp.googleapis.com/mcp/v1")
	for _, want := range []string{"Desktop app", "gmailmcp.googleapis.com", "7 days", "In production"} {
		if !strings.Contains(s, want) {
			t.Fatalf("Google steps miss %q:\n%s", want, s)
		}
	}
	if strings.Contains(ownClientSteps("https://auth.example", "https://mcp.example/mcp"), "Google") {
		t.Fatal("another vendor got Google's steps")
	}
}
