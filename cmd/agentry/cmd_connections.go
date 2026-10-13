// Connections (plan mcp-oauth-connections.md §6, agentry-cli#664, #665): a
// person's OAuth grant to a remote MCP server, or at a package server's
// issuer, for one QuickAgent release.
// A run asks for each one it lacks at Start, on a terminal, and refreshes
// each before the sandbox starts; `agentry connect` makes them ahead of a
// run; `agentry connections` lists and removes them and keeps the person's
// own OAuth clients. The run is given an access token and nothing else.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/runner/connection"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/profile"
	"github.com/blaktron/agentry-cli/pkg/runner/setup"
)

// Seams for tests: the vendor client (the egress guard in production), the
// keychain (the OS's), whether consent can be asked, and the CIMD address.
var (
	vendorHTTP     = func() *http.Client { return mcpauth.Guard{}.Client() }
	connectionKeys = func() connection.Keychain { return connection.OSKeychain{} }
	consentTTY     = stdinIsTTY
	nativeCIMD     = mcpauth.NativeClientMetadataURL
)

// oauthServers are the release's servers that sign in by OAuth: remote
// servers declared auth: oauth, and package servers with an oauth block.
func oauthServers(m *manifest.Manifest) []manifest.MCPServer {
	var out []manifest.MCPServer
	for _, s := range m.Capabilities.MCPServers {
		if s.NeedsConnection() {
			out = append(out, s)
		}
	}
	return out
}

// refuseUnsupported refuses a package that cannot run on an access token
// alone (plan D25) before anyone is taken through consent for it: its runs
// are refused at plan time anyway.
func refuseUnsupported(servers []manifest.MCPServer) error {
	for _, srv := range servers {
		if alt, bad := manifest.UnsupportedOAuthPackage(srv.SourceID()); bad && !srv.IsRemote() {
			return engine.Classify(engine.ExitPolicy, fmt.Errorf("declared MCP server %s cannot run on an access token alone, so it cannot take a connection: run a version of the QuickAgent that declares %s instead", srv.SourceID(), alt))
		}
	}
	return nil
}

// connectionServer is how a connection names its server, the third part of
// its key and engine.Params.Connections' key: a remote server's address, a
// package server's ecosystem-qualified id ("pypi:workspace-mcp"), matching
// spec.MCPServerSpec.ConnectionKey.
func connectionServer(srv manifest.MCPServer) string {
	if srv.IsRemote() {
		return srv.URL
	}
	return srv.SourceID()
}

// connectionScopes are the scopes a server's consent asks for.
func connectionScopes(srv manifest.MCPServer) []string {
	if srv.OAuth != nil {
		return srv.OAuth.Scopes
	}
	return srv.Scopes
}

// connectionWhat names a server in the person's words: a remote server's
// address, or a package server and where it signs in.
func connectionWhat(srv manifest.MCPServer) string {
	if srv.IsRemote() {
		return srv.URL
	}
	return srv.SourceID() + " (signing in at " + srv.OAuth.Issuer + ")"
}

// runConnections is what a run of a release is given of its connections:
// each server's access token, the receipt's line naming each one used
// (server, issuer, scopes, client kind; never a token), and the renewal
// the engine asks for before a token expires (plan D24).
type runConnections struct {
	tokens map[string]engine.ConnectionToken
	line   string
	renew  func(ctx context.Context, server string) (engine.ConnectionToken, error)
}

// ensureConnections opens, or on a terminal makes, the connection of every
// server of the release that signs in by OAuth, and refreshes each under
// its lock (connection.Store.Use) before the sandbox starts. With no
// terminal a server with no working connection refuses the run with exit
// 11, naming it. The renewal it returns refreshes the same way mid-run.
func ensureConnections(cmd *cobra.Command, home string, meta *installdir.Meta, m *manifest.Manifest) (*runConnections, error) {
	servers := oauthServers(m)
	if len(servers) == 0 {
		return &runConnections{}, nil
	}
	store := connection.Open(home, connectionKeys())
	clients := connection.OpenClients(home, connectionKeys())
	httpc := vendorHTTP()
	out := &runConnections{
		tokens: map[string]engine.ConnectionToken{},
		renew: func(ctx context.Context, server string) (engine.ConnectionToken, error) {
			a, _, err := store.Use(ctx, connection.Key{Ref: meta.Ref, Digest: meta.Digest, Server: server}, httpc, clients)
			return engine.ConnectionToken{AccessToken: a.Token, ExpiresAt: a.ExpiresAt}, err
		},
	}
	var used []string
	for _, srv := range servers {
		k := connection.Key{Ref: meta.Ref, Digest: meta.Digest, Server: connectionServer(srv)}
		access, c, err := store.Use(cmd.Context(), k, httpc, clients)
		var broken *connection.BrokenError
		switch {
		case err == nil:
		case errors.Is(err, connection.ErrNone) || errors.As(err, &broken):
			if !consentTTY(cmd.InOrStdin()) {
				why := "has no connection for this version"
				if broken != nil {
					why = "has a connection the vendor no longer honours (" + broken.Reason + ")"
				}
				return nil, engine.Classify(engine.ExitPolicy, fmt.Errorf(
					"declared MCP server %s signs in by OAuth and %s, and there is no terminal to ask for consent: run `agentry connect %s` on a terminal first",
					connectionServer(srv), why, meta.Ref))
			}
			if broken != nil {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%v\n", broken)
			}
			g, cerr := consentFor(cmd, store, clients, httpc, k, srv)
			if cerr != nil {
				return nil, engine.Classify(engine.ExitPolicy, cerr)
			}
			access = connection.Access{Token: g.Token.AccessToken, ExpiresAt: g.Token.ExpiresAt}
			if c, err = store.Get(k); err != nil {
				return nil, engine.Classify(engine.ExitCLI, err)
			}
		default:
			// The vendor could not be reached or answered something
			// unexpected: not a policy refusal, and the connection is kept.
			return nil, engine.Classify(engine.ExitCLI, err)
		}
		out.tokens[k.Server] = engine.ConnectionToken{AccessToken: access.Token, ExpiresAt: access.ExpiresAt}
		used = append(used, fmt.Sprintf("%s (issuer %s, scopes %s, client %s)", k.Server, c.Issuer, scopeList(c.Scopes), c.Client.Kind))
	}
	out.line = "servers signed in to by the person's connection for this release, refreshed by the runner before the run, and renewed before a token expires during the run where the server reads it from a file (a renewal that fails is said on the run's stderr); the run was given an access token alone: " + strings.Join(used, "; ")
	return out, nil
}

// fileConnection is one server's entry in --connections-file
// (agentry-cli#722): an access token a host fetched for the run, its
// expiry, and the issuer and scopes for the receipt's line. Never a
// refresh token or a client secret: the file has no field for one.
type fileConnection struct {
	AccessToken string    `json:"accessToken"`
	ExpiresAt   time.Time `json:"expiresAt"`
	Issuer      string    `json:"issuer,omitempty"`
	Scopes      []string  `json:"scopes,omitempty"`
}

// maxConnectionsFile bounds --connections-file.
const maxConnectionsFile = 256 << 10

// connectionsFromFile is a hosted run's connections (agentry-cli#722, plan
// mcp-oauth-connections.md D8): the access tokens the deployment's worker
// refreshed at the hand-off, keyed as engine.Params.Connections is. Nothing
// is consented to, refreshed or renewed here, and no store is opened: a
// token that expires during the run ends that server's calls (D13). The
// file must be a regular file only its owner can read; a key the release
// does not declare as signing in by OAuth, an unknown field, or a declared
// server it does not name refuses the run.
func connectionsFromFile(path string, m *manifest.Manifest) (*runConnections, error) {
	refuse := func(format string, a ...any) error {
		return engine.Classify(engine.ExitPolicy, fmt.Errorf("--connections-file: "+format, a...))
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, engine.Classify(engine.ExitCLI, fmt.Errorf("--connections-file: %w", err))
	}
	switch {
	case !fi.Mode().IsRegular():
		return nil, refuse("%s is not a regular file", path)
	case runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0:
		return nil, refuse("%s can be read by others (mode %04o); it must be 0600", path, fi.Mode().Perm())
	case fi.Size() > maxConnectionsFile:
		return nil, refuse("%s is larger than %d bytes", path, maxConnectionsFile)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, engine.Classify(engine.ExitCLI, fmt.Errorf("--connections-file: %w", err))
	}
	var file map[string]fileConnection
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return nil, refuse("%s is not a connections file: %v", path, err)
	}
	servers := oauthServers(m)
	declared := map[string]bool{}
	for _, srv := range servers {
		declared[connectionServer(srv)] = true
	}
	for k := range file {
		if !declared[k] {
			return nil, refuse("it names %s, which this release does not declare as an MCP server that signs in by OAuth", k)
		}
	}
	out := &runConnections{tokens: map[string]engine.ConnectionToken{}}
	var used []string
	for _, srv := range servers {
		k := connectionServer(srv)
		c, ok := file[k]
		if !ok || c.AccessToken == "" {
			return nil, refuse("declared MCP server %s signs in by OAuth and this run was handed no connection for it", k)
		}
		out.tokens[k] = engine.ConnectionToken{AccessToken: c.AccessToken, ExpiresAt: c.ExpiresAt}
		issuer := c.Issuer
		if issuer == "" {
			issuer = "not named"
		}
		used = append(used, fmt.Sprintf("%s (issuer %s, scopes %s)", k, issuer, scopeList(c.Scopes)))
	}
	if len(used) > 0 {
		out.line = "servers signed in to by connections the deployment keeps, refreshed before the run was handed to this host; the run was given an access token alone and renews nothing during the run, so a token that expires ends that server's calls: " + strings.Join(used, "; ")
	}
	return out, nil
}

func scopeList(s []string) string {
	if len(s) == 0 {
		return "the vendor's default"
	}
	return strings.Join(s, " ")
}

// consentFor asks the person, on the terminal, to connect srv for the
// release k names: the vendor's consent screen in the browser, or its
// address printed and the redirect pasted back (D21). A vendor that needs
// the person's own client (Google) asks for one first, with Agentry's
// steps where it has them (D22). The grant is kept, replacing a broken one
// (whose refresh token is revoked where the vendor offers it).
func consentFor(cmd *cobra.Command, store *connection.Store, clients *connection.Clients, httpc *http.Client, k connection.Key, srv manifest.MCPServer) (*mcpauth.Grant, error) {
	errOut := cmd.ErrOrStderr()
	_, _ = fmt.Fprintf(errOut, "\n%s needs you to sign in to %s.\n", k.Ref, connectionWhat(srv))
	_, _ = fmt.Fprintln(errOut, "The connection is kept on this machine for this version alone and reused by its later runs; each run is given an access token, never the refresh token or a client secret.")
	if scopes := connectionScopes(srv); len(scopes) > 0 {
		_, _ = fmt.Fprintf(errOut, "It asks for: %s\n", strings.Join(scopes, " "))
	}
	in := bufio.NewReader(cmd.InOrStdin())
	for attempt := 0; ; attempt++ {
		g, err := runConsent(cmd, in, httpc, clients, srv)
		var own *mcpauth.NeedsOwnClientError
		if errors.As(err, &own) && attempt == 0 {
			if aerr := askOwnClient(cmd, in, clients, own.Issuer, connectionServer(srv)); aerr != nil {
				return nil, aerr
			}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("connect %s: %w", connectionServer(srv), err)
		}
		switch old, gerr := store.Get(k); {
		case gerr != nil || old == nil:
		case old.MadeWith(g):
			// Revoking the old refresh token would end the grant just made
			// at a vendor that revokes a client's whole grant (Google), and
			// every other connection made with the client: Put overwrites
			// the record and its secret, and nothing is revoked.
			store.RecordReplaced(old, "replaced by a new consent")
			_, _ = fmt.Fprintln(errOut, "the previous connection is replaced; it was made with the same client, so it is not revoked at the vendor (at one that revokes a client's whole grant, such as Google, that would end the new one too); its refresh token is dropped from this machine")
		default:
			if outcome, rerr := store.Remove(cmd.Context(), k, httpc, clients, "replaced by a new consent"); rerr == nil {
				_, _ = fmt.Fprintf(errOut, "the previous connection was removed (revocation: %s)\n", outcome)
			}
		}
		if _, err := store.Put(k, g, ""); err != nil {
			return nil, err
		}
		_, _ = fmt.Fprintf(errOut, "connected %s for %s\n", connectionServer(srv), k.Ref)
		return g, nil
	}
}

// runConsent runs one consent. The redirect comes back to the loopback
// listener, or, where the browser is elsewhere, its address is pasted at
// the prompt; stdin is read only once the address is shown, and the line
// being read is finished before returning, so nothing typed later is lost.
func runConsent(cmd *cobra.Command, in *bufio.Reader, httpc *http.Client, clients *connection.Clients, srv manifest.MCPServer) (*mcpauth.Grant, error) {
	bc := newBrowserConsent(cmd, in, "Approve it in your browser:")
	c := mcpauth.Consent{
		HTTP: httpc, ServerURL: srv.URL, Scopes: connectionScopes(srv),
		OwnClient: clients.Get, CIMDURL: nativeCIMD,
		Pasted: bc.pasted, Say: bc.say, Present: bc.present,
	}
	if !srv.IsRemote() {
		c.Issuer = srv.OAuth.Issuer
	}
	g, err := c.Run(cmd.Context())
	bc.finish()
	return g, err
}

// browserConsent is a consent shown in the person's browser whose redirect
// may be pasted back from another machine (D21), for every flow the CLI
// runs there (a connection's, Sign in with ChatGPT, a connect): present
// prints the address under heading, opens it where it can and starts
// reading pasted addresses; finish, once the flow has returned, waits for
// that reader, so the next question reads its own answer.
type browserConsent struct {
	errOut   io.Writer
	in       *bufio.Reader
	heading  string
	pasted   chan string
	done     chan struct{}
	finished chan struct{}
}

func newBrowserConsent(cmd *cobra.Command, in *bufio.Reader, heading string) *browserConsent {
	return &browserConsent{errOut: cmd.ErrOrStderr(), in: in, heading: heading, pasted: make(chan string), done: make(chan struct{})}
}

func (b *browserConsent) say(s string) { _, _ = fmt.Fprintln(b.errOut, s) }

func (b *browserConsent) present(authURL string) {
	_, _ = fmt.Fprintf(b.errOut, "%s\n  %s\n", b.heading, authURL)
	if os.Getenv("AGENTRY_NO_BROWSER") == "" {
		if err := openBrowser(authURL); err != nil {
			_, _ = fmt.Fprintf(b.errOut, "(could not open a browser: %v; open the address above yourself)\n", err)
		}
	}
	_, _ = fmt.Fprintln(b.errOut, "Waiting for the browser (up to 10 minutes). If it is on another machine and ends on a page that does not load, paste that page's address here and press Enter:")
	b.finished = make(chan struct{})
	go readPasted(b.in, b.pasted, b.done, b.finished)
}

func (b *browserConsent) finish() {
	close(b.done)
	if b.finished != nil {
		select {
		case <-b.finished:
		default:
			_, _ = fmt.Fprintln(b.errOut, "Press Enter to continue.")
			<-b.finished
		}
	}
}

// readPasted sends each non-empty line until done; it returns once a line
// read after done, or the input's end, so the caller can wait for it.
func readPasted(in *bufio.Reader, out chan<- string, done <-chan struct{}, finished chan<- struct{}) {
	defer close(finished)
	for {
		line, err := in.ReadString('\n')
		select {
		case <-done:
			return
		default:
		}
		if line = strings.TrimSpace(line); line != "" {
			select {
			case out <- line:
			case <-done:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// askOwnClient asks for the person's own OAuth client for issuer and keeps
// it for every later consent there (D23), after Agentry's steps for making
// one where it has written them (D22).
func askOwnClient(cmd *cobra.Command, in *bufio.Reader, clients *connection.Clients, issuer, server string) error {
	errOut := cmd.ErrOrStderr()
	_, _ = fmt.Fprintf(errOut, "\n%s lets no app register itself, so signing in there needs an OAuth client of your own.\n", issuer)
	_, _ = fmt.Fprintln(errOut, ownClientSteps(issuer, server))
	p := &setup.TTYPrompter{In: in, Err: errOut}
	id, err := p.Ask("Client ID", "")
	if err != nil {
		return err
	}
	secret, err := p.AskSecret("Client secret")
	if err != nil {
		return err
	}
	if strings.TrimSpace(id) == "" {
		return errors.New("no client id was given, so the connection cannot be made")
	}
	if err := clients.Set(issuer, id, strings.TrimSpace(secret)); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(errOut, "kept your OAuth client for %s; every QuickAgent you connect there will use it (agentry connections client list)\n", issuer)
	return nil
}

// ownClientSteps are Agentry's steps for making one's own client (D22):
// Google's, which Agentry writes; any other vendor's general shape.
func ownClientSteps(issuer, server string) string {
	if strings.TrimRight(issuer, "/") == "https://accounts.google.com" {
		api := "the API of the product this server reaches"
		if u, err := url.Parse(server); err == nil && strings.HasSuffix(u.Hostname(), ".googleapis.com") {
			h := u.Hostname()
			api = "the product's API and its MCP API (" + h + ")"
		}
		return strings.Join([]string{
			"Make one in Google Cloud (once; every QuickAgent you connect to Google uses it):",
			"  1. In the Google Cloud console (console.cloud.google.com), create a project or pick one.",
			"  2. APIs & Services → Library: enable " + api + ".",
			"  3. Google Auth Platform → Branding and Audience: set the app up; under Audience choose Internal",
			"     (a Google Workspace account) or External and then Publish app, so it is In production. A client",
			"     left in Testing gets sign-ins that stop working after 7 days.",
			"  4. If the app stays in Testing: Audience → Test users → add the Google account you will sign in",
			"     with. Google refuses the sign-in of any account a Testing app does not list.",
			"  5. Data Access: add the scopes this QuickAgent asks for.",
			"  6. Clients → Create client → Application type: Desktop app. Copy its client ID and client secret.",
			"Google may show an \"unverified app\" screen for a client of your own; it is your own app.",
		}, "\n")
	}
	return strings.Join([]string{
		"Make one in the vendor's developer settings (once; every QuickAgent you connect there uses it):",
		"  register an OAuth app or client of the native or desktop kind, with the redirect address",
		"  http://127.0.0.1/callback (any port), the authorization code grant and refresh tokens, and the",
		"  scopes this QuickAgent asks for. Copy its client ID and, if it has one, its client secret.",
	}, "\n")
}

// installedRelease is the release installed on this machine that sel
// names (ns/name or ns/name@version), with its manifest. Several installed
// versions and no version named is a refusal that lists them.
func installedRelease(home, sel string) (*installdir.Meta, *manifest.Manifest, error) {
	ref, err := manifest.ParseRef(sel)
	if err != nil {
		return nil, nil, err
	}
	agentDir := installdir.AgentDir(home, ref.Namespace, ref.Name)
	entries, err := os.ReadDir(agentDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	type found struct {
		meta *installdir.Meta
		dir  string
	}
	var all []found
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(agentDir, e.Name())
		digest := recordedDigest(dir)
		if digest == "" {
			continue
		}
		meta, ierr := installdir.Inspect(dir, digest)
		if ierr != nil && !errors.Is(ierr, installdir.ErrNoRecord) {
			continue
		}
		if ref.Range != "" && meta.Version != ref.Range {
			continue
		}
		all = append(all, found{meta, dir})
	}
	switch {
	case len(all) == 0:
		return nil, nil, fmt.Errorf("%s is not installed on this machine: install it first (agentry install %s, or agentry run %s --prepare)", sel, sel, sel)
	case len(all) > 1:
		var vs []string
		for _, f := range all {
			vs = append(vs, f.meta.Ref)
		}
		return nil, nil, fmt.Errorf("several versions of %s are installed (%s): name one, ns/name@version", sel, strings.Join(vs, ", "))
	}
	m, err := localrun.ManifestFrom(filepath.Join(all[0].dir, "workspace"))
	if err != nil {
		return nil, nil, err
	}
	return all[0].meta, m, nil
}

// recordedDigest is the digest an install's install.json records, "" for
// none; Inspect then holds the install to it.
func recordedDigest(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "install.json")) //nolint:gosec // our install layout
	if err != nil {
		return ""
	}
	var meta installdir.Meta
	if json.Unmarshal(raw, &meta) != nil {
		return ""
	}
	return meta.Digest
}

func newConnectCmd() *cobra.Command {
	var again bool
	cmd := &cobra.Command{
		Use:   "connect <ns/name[@version]>",
		Short: "Sign in to the MCP servers an installed QuickAgent version needs, ahead of a run",
		Long: "Make the connection each MCP server of an installed QuickAgent version needs\n" +
			"(a remote server's auth: oauth, a package server's oauth block): the vendor's\n" +
			"consent screen in your browser, or its address\n" +
			"printed and the redirect pasted back. A connection is kept for that version\n" +
			"alone and reused by its runs; --again replaces one that works.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := runnerHome()
			if err != nil {
				return err
			}
			meta, m, err := installedRelease(home, args[0])
			if err != nil {
				return err
			}
			servers := oauthServers(m)
			if len(servers) == 0 {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s declares no MCP server that signs in by OAuth: nothing to connect\n", meta.Ref)
				return nil
			}
			if err := refuseUnsupported(servers); err != nil {
				return err
			}
			if !consentTTY(cmd.InOrStdin()) {
				return engine.Classify(engine.ExitPolicy, errors.New("connecting needs a terminal to ask for your consent"))
			}
			store := connection.Open(home, connectionKeys())
			clients := connection.OpenClients(home, connectionKeys())
			httpc := vendorHTTP()
			for _, srv := range servers {
				k := connection.Key{Ref: meta.Ref, Digest: meta.Digest, Server: connectionServer(srv)}
				if c, err := store.Get(k); err == nil && c.Broken == "" && !again {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: connected (issuer %s, since %s); --again replaces it\n", k.Server, c.Issuer, c.CreatedAt)
					continue
				}
				if _, err := consentFor(cmd, store, clients, httpc, k, srv); err != nil {
					return engine.Classify(engine.ExitPolicy, err)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&again, "again", false, "make the connection again even where one works")
	return cmd
}

// connectionOut is one connection in `connections list --json`.
type connectionOut struct {
	Ref       string   `json:"ref"`
	Digest    string   `json:"digest"`
	Server    string   `json:"server"`
	Issuer    string   `json:"issuer"`
	Client    string   `json:"client"`
	Scopes    []string `json:"scopes,omitempty"`
	State     string   `json:"state"`
	CreatedAt string   `json:"createdAt"`
	LastUsed  string   `json:"lastUsedAt,omitempty"`
	KeptIn    string   `json:"keptIn"`
	// SharesGrantWith names the other connections made with the same
	// client at the same issuer, "ns/name@version → server": a vendor that
	// revokes the whole grant ends them when this one is removed.
	SharesGrantWith []string `json:"sharesGrantWith,omitempty"`
}

// grantNames names connections as "ns/name@version → server".
func grantNames(cs []*connection.Connection) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Ref+" → "+c.Server)
	}
	return out
}

func newConnectionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "connections",
		Short: "List and remove the OAuth connections kept for QuickAgent versions, and your own OAuth clients",
	}
	cmd.AddCommand(newConnectionsListCmd(), newConnectionsRemoveCmd(), newConnectionsClientCmd())
	return cmd
}

func newConnectionsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List kept connections (never a token)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			home, err := runnerHome()
			if err != nil {
				return err
			}
			list, err := connection.Open(home, nil).List()
			if err != nil {
				return err
			}
			rows := make([]connectionOut, 0, len(list))
			for _, c := range list {
				state := "connected"
				if c.Broken != "" {
					state = "broken: " + c.Broken
				}
				rows = append(rows, connectionOut{
					Ref: c.Ref, Digest: c.Digest, Server: c.Server, Issuer: c.Issuer, Client: c.Client.Kind,
					Scopes: c.Scopes, State: state, CreatedAt: c.CreatedAt, LastUsed: c.LastUsedAt, KeptIn: c.SecretIn,
					SharesGrantWith: grantNames(connection.SharesGrant(list, c)),
				})
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), rows)
			}
			out := cmd.OutOrStdout()
			if len(rows) == 0 {
				_, _ = fmt.Fprintln(out, "no connections kept (a run asks for one the first time a QuickAgent version needs it)")
				return nil
			}
			for _, r := range rows {
				last := r.LastUsed
				if last == "" {
					last = "never used"
				}
				_, _ = fmt.Fprintf(out, "%s → %s\n  digest %s · issuer %s · %s client · scopes %s · %s · last used: %s · kept in the %s\n",
					r.Ref, r.Server, installdir.DigestSuffix(r.Digest), r.Issuer, r.Client, scopeList(r.Scopes), r.State, last, r.KeptIn)
				if len(r.SharesGrantWith) > 0 {
					_, _ = fmt.Fprintf(out, "  made with the same client as: %s (removing one may end the others at a vendor that revokes the whole grant)\n", strings.Join(r.SharesGrantWith, ", "))
				}
			}
			return nil
		},
	}
}

// removedConnection is one removal in `connections remove --json`.
type removedConnection struct {
	Ref        string `json:"ref"`
	Server     string `json:"server"`
	Revocation string `json:"revocation"`
	Error      string `json:"error,omitempty"`
	// SharesGrantWith names the connections this removal leaves, made with
	// the same client at the same issuer, which a vendor that revokes the
	// whole grant ends with this one (the operator, 2026-10-06: removal
	// still revokes, and says so). Empty unless the revocation was done;
	// with --plan, those it would end.
	SharesGrantWith []string `json:"sharesGrantWith,omitempty"`
}

// line is the removal as the terminal says it.
func (r removedConnection) line() string {
	l := fmt.Sprintf("removed the connection of %s to %s (revocation: %s)", r.Ref, r.Server, r.Revocation)
	return l + endsToo(r.SharesGrantWith)
}

// endsToo is a removal's warning about the connections it ends with it.
func endsToo(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return "; at a vendor that revokes the whole grant, these, made with the same client, stop working too: " + strings.Join(names, ", ")
}

// sharedBeyond names the connections in all made with c's client that
// going, the removal's own set, does not hold.
func sharedBeyond(all []*connection.Connection, c *connection.Connection, going map[connection.Key]bool) []string {
	var left []*connection.Connection
	for _, o := range connection.SharesGrant(all, c) {
		if !going[o.Key()] {
			left = append(left, o)
		}
	}
	return grantNames(left)
}

func newConnectionsRemoveCmd() *cobra.Command {
	var all bool
	var server string
	cmd := &cobra.Command{
		Use:   "remove [ns/name[@version]]",
		Short: "Remove kept connections (revoked at the vendor where it offers that)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if all == (len(args) == 1) {
				return errors.New("pass exactly one of ns/name[@version] or --all")
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			sel := ""
			if len(args) == 1 {
				sel = args[0]
			}
			removed, err := removeConnections(cmd.Context(), home, sel, server, "removed by the person")
			if flags.jsonOut {
				if jerr := emitJSON(cmd.OutOrStdout(), removed); jerr != nil {
					return jerr
				}
				return err
			}
			for _, r := range removed {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), r.line())
			}
			if len(removed) == 0 && err == nil {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "nothing matched")
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "remove every kept connection")
	cmd.Flags().StringVar(&server, "server", "", "remove only the connection to this server address")
	return cmd
}

// removeConnections removes the connections of the releases sel matches
// ("" for all), to server where one is named, revoking each at the vendor
// where it offers that (D17), and names, for each one revoked, the kept
// connections it does not remove that were made with the same client.
func removeConnections(ctx context.Context, home, sel, server, why string) ([]removedConnection, error) {
	store := connection.Open(home, connectionKeys())
	clients := connection.OpenClients(home, connectionKeys())
	list, err := store.List()
	if err != nil {
		return nil, err
	}
	var chosen []*connection.Connection
	going := map[connection.Key]bool{}
	for _, c := range list {
		if sel != "" && !profile.RefMatches(c.Ref, sel) {
			continue
		}
		if server != "" && c.Server != server {
			continue
		}
		chosen = append(chosen, c)
		going[c.Key()] = true
	}
	out := []removedConnection{}
	var errs []error
	httpc := vendorHTTP()
	for _, c := range chosen {
		rctx, cancel := context.WithTimeout(ctx, revokeTimeout)
		outcome, rerr := store.Remove(rctx, c.Key(), httpc, clients, why)
		cancel()
		r := removedConnection{Ref: c.Ref, Server: c.Server, Revocation: outcome}
		if outcome == connection.RevokeDone {
			r.SharesGrantWith = sharedBeyond(list, c, going)
		}
		if rerr != nil {
			r.Error = rerr.Error()
			errs = append(errs, rerr)
		}
		out = append(out, r)
	}
	return out, errors.Join(errs...)
}

// revokeTimeout bounds one removal's revocation at the vendor; a vendor
// that does not answer in time is recorded as a failed revocation and the
// connection is removed all the same.
const revokeTimeout = 30 * time.Second

func newConnectionsClientCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "client",
		Short: "Keep, list and remove your own OAuth clients (one per vendor's issuer)",
	}
	var clientID string
	set := &cobra.Command{
		Use:   "set <issuer>",
		Short: "Keep your own OAuth client for an issuer (the secret is read from stdin)",
		Long: "Keep your own OAuth client for a vendor that lets no app register itself\n" +
			"(Google: https://accounts.google.com). Every QuickAgent you connect there\n" +
			"uses it. The client secret is read from stdin: typed without echo on a\n" +
			"terminal, or piped; an empty one keeps a client without a secret.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := runnerHome()
			if err != nil {
				return err
			}
			secret, err := readClientSecret(cmd)
			if err != nil {
				return err
			}
			if err := connection.OpenClients(home, connectionKeys()).Set(args[0], clientID, secret); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "kept your OAuth client for %s\n", args[0])
			return nil
		},
	}
	set.Flags().StringVar(&clientID, "client-id", "", "the client's id (required)")
	_ = set.MarkFlagRequired("client-id")
	list := &cobra.Command{
		Use:   "list",
		Short: "List your kept OAuth clients (never a secret)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			home, err := runnerHome()
			if err != nil {
				return err
			}
			cs, err := connection.OpenClients(home, nil).List()
			if err != nil {
				return err
			}
			if flags.jsonOut {
				if cs == nil {
					cs = []connection.OwnClient{}
				}
				return emitJSON(cmd.OutOrStdout(), cs)
			}
			if len(cs) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no OAuth clients of your own kept")
			}
			for _, c := range cs {
				secret := "no secret"
				if c.SecretIn != "" {
					secret = "secret kept in the " + c.SecretIn
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s  client %s  (%s)\n", c.Issuer, c.ClientID, secret)
			}
			return nil
		},
	}
	remove := &cobra.Command{
		Use:   "remove <issuer>",
		Short: "Forget your OAuth client for an issuer; connections made with it then ask again",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := runnerHome()
			if err != nil {
				return err
			}
			ok, err := connection.OpenClients(home, connectionKeys()).Remove(args[0])
			if err != nil {
				return err
			}
			if !ok {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "no OAuth client kept for %s\n", args[0])
				return nil
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "forgot your OAuth client for %s\n", args[0])
			return nil
		},
	}
	cmd.AddCommand(set, list, remove)
	return cmd
}

// readClientSecret reads the client secret: without echo on a terminal,
// else the first line of stdin.
func readClientSecret(cmd *cobra.Command) (string, error) {
	if consentTTY(cmd.InOrStdin()) {
		p := &setup.TTYPrompter{In: cmd.InOrStdin(), Err: cmd.ErrOrStderr()}
		s, err := p.AskSecret("Client secret (Enter for none)")
		return strings.TrimSpace(s), err
	}
	raw, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 4096))
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(raw), "\n")
	return strings.TrimSpace(line), nil
}
