package mcpauth_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/mcpauth/mcpauthtest"
)

func consent(v *mcpauthtest.Vendor) mcpauth.Consent {
	return mcpauth.Consent{
		HTTP:      v.Guard().Client(),
		ServerURL: v.ServerURL(),
		Present:   v.Browser(),
		Timeout:   10 * time.Second,
	}
}

func TestConsentRegistersDynamicallyAndExchanges(t *testing.T) {
	v := mcpauthtest.New(t)
	v.DCR = true
	v.Scope = "read write"
	g, err := consent(v).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if g.Client.Kind != mcpauth.ClientDCR || g.Client.AuthMethod != "none" || g.Client.Secret != "" {
		t.Fatalf("client = %+v, want a public dynamically registered one", g.Client)
	}
	if !strings.HasPrefix(g.Token.RefreshToken, "PROBE-REFRESH-") || g.Token.AccessToken == "" || g.Token.ExpiresAt.IsZero() {
		t.Fatalf("token = %+v", g.Token)
	}
	if g.Discovery.Resource != v.ServerURL() || g.Discovery.Server.Issuer != v.URL {
		t.Fatalf("discovery = %+v", g.Discovery)
	}
	if strings.Join(g.Scopes, " ") != "read write" {
		t.Fatalf("scopes = %v, want the challenge's when the manifest names none", g.Scopes)
	}
	// The exchange carried the verifier and the resource (RFC 8707).
	last := v.Requests[len(v.Requests)-1]
	if last.Get("code_verifier") == "" || last.Get("resource") != v.ServerURL() {
		t.Fatalf("exchange form = %v", last)
	}
}

func TestTheManifestsScopesWin(t *testing.T) {
	v := mcpauthtest.New(t)
	v.DCR, v.Scope = true, "read write"
	c := consent(v)
	c.Scopes = []string{"read"}
	g, err := c.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(g.Scopes, " ") != "read" {
		t.Fatalf("scopes = %v", g.Scopes)
	}
}

func TestOwnClientComesFirstAndAuthenticates(t *testing.T) {
	v := mcpauthtest.New(t)
	v.DCR = true
	v.AddClient("mine", "my-secret")
	c := consent(v)
	c.OwnClient = func(issuer string) (*mcpauth.Client, error) {
		if issuer != v.URL {
			t.Fatalf("own client asked for %q", issuer)
		}
		return &mcpauth.Client{ID: "mine", Secret: "my-secret"}, nil
	}
	g, err := c.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if g.Client.Kind != mcpauth.ClientOwn || g.Client.ID != "mine" || g.Client.AuthMethod != "client_secret_post" {
		t.Fatalf("client = %+v", g.Client)
	}
}

func TestNoClientMeansTheirOwn(t *testing.T) {
	v := mcpauthtest.New(t) // no DCR, no CIMD: Google's shape
	_, err := consent(v).Run(context.Background())
	var own *mcpauth.NeedsOwnClientError
	if !errors.As(err, &own) || own.Issuer != v.URL {
		t.Fatalf("err = %v, want NeedsOwnClientError", err)
	}
}

func TestCIMDIsUsedOnlyWhereItIsServed(t *testing.T) {
	v := mcpauthtest.New(t)
	v.CIMD, v.DCR = true, true
	doc := http.NewServeMux()
	var served bool
	var docURL string
	doc.HandleFunc("/agentry-desktop.json", func(w http.ResponseWriter, _ *http.Request) {
		if !served {
			http.NotFound(w, nil)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"client_id": docURL, "redirect_uris": []string{"http://127.0.0.1/callback"}})
	})
	site := httptest.NewTLSServer(doc)
	t.Cleanup(site.Close)
	docURL = site.URL + "/agentry-desktop.json"
	guard := v.Guard()
	guard.TLS.RootCAs.AddCert(site.Certificate())

	c := consent(v)
	c.HTTP, c.CIMDURL = guard.Client(), docURL
	g, err := c.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if g.Client.Kind != mcpauth.ClientDCR {
		t.Fatalf("an unserved document must fall back to registration: %+v", g.Client)
	}
	served = true
	if g, err = c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.Client.Kind != mcpauth.ClientCIMD || g.Client.ID != docURL {
		t.Fatalf("a served document is the client: %+v", g.Client)
	}
}

func TestPasteBackFinishesTheConsent(t *testing.T) {
	v := mcpauthtest.New(t)
	v.DCR = true
	pasted := make(chan string, 2)
	c := consent(v)
	c.Pasted = pasted
	var said []string
	c.Say = func(s string) { said = append(said, s) }
	c.Present = func(authURL string) {
		// The browser is elsewhere: the person pastes where it ended up,
		// after a wrong paste that is waited past.
		loc, err := v.Redirect(authURL)
		if err != nil {
			t.Error(err)
			return
		}
		u, _ := url.Parse(loc)
		q := u.Query()
		q.Set("state", "someone-elses")
		wrong := *u
		wrong.RawQuery = q.Encode()
		pasted <- wrong.String()
		pasted <- loc
	}
	g, err := c.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(said, "\n")
	if g.Token.AccessToken == "" || strings.Count(all, "state does not match") != 1 || !strings.Contains(said[0], "The sign-in is at "+v.URL) {
		t.Fatalf("grant %+v, said %q", g, said)
	}
}

func TestTheRedirectIsHeldToItsIssuer(t *testing.T) {
	md := mcpauth.ServerMetadata{Issuer: "https://as.example", AuthorizationEndpoint: "https://as.example/a", ISSParameterSupported: true}
	r, err := mcpauth.NewRequest(md, mcpauth.Client{ID: "c"}, "http://127.0.0.1:1/callback", "https://mcp.example/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		q    url.Values
		want string
	}{
		"another issuer": {url.Values{"state": {r.State}, "code": {"x"}, "iss": {"https://evil.example"}}, "another issuer"},
		"no issuer":      {url.Values{"state": {r.State}, "code": {"x"}}, "names no issuer"},
		"wrong state":    {url.Values{"state": {"nope"}, "code": {"x"}, "iss": {md.Issuer}}, "state does not match"},
		"refused":        {url.Values{"state": {r.State}, "error": {"access_denied"}, "iss": {md.Issuer}}, "access_denied"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := r.Callback(&url.URL{RawQuery: tc.q.Encode()})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	code, err := r.Callback(&url.URL{RawQuery: url.Values{"state": {r.State}, "code": {"good"}, "iss": {md.Issuer}}.Encode()})
	if err != nil || code != "good" {
		t.Fatalf("code %q, err %v", code, err)
	}
	u, _ := url.Parse(r.URL())
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("resource") != "https://mcp.example/mcp" {
		t.Fatalf("authorization request = %v", q)
	}
}

func TestDiscoveryRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		prm, asm string
		want     string
	}{
		"another resource": {prm: `{"resource":"https://elsewhere.example/mcp","authorization_servers":["%s"]}`, want: "another resource"},
		"issuer mismatch":  {asm: `{"issuer":"https://impostor.example","authorization_endpoint":"%[1]s/a","token_endpoint":"%[1]s/t","code_challenge_methods_supported":["S256"]}`, want: "calls itself"},
		"no S256":          {asm: `{"issuer":"%[1]s","authorization_endpoint":"%[1]s/a","token_endpoint":"%[1]s/t","code_challenge_methods_supported":["plain"]}`, want: "S256"},
		"http endpoint":    {asm: `{"issuer":"%[1]s","authorization_endpoint":"http://x.example/a","token_endpoint":"%[1]s/t","code_challenge_methods_supported":["S256"]}`, want: "not https"},
	} {
		t.Run(name, func(t *testing.T) {
			var base string
			mux := http.NewServeMux()
			mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/prm"`)
				w.WriteHeader(http.StatusUnauthorized)
			})
			mux.HandleFunc("/prm", func(w http.ResponseWriter, _ *http.Request) {
				body := `{"resource":"` + base + `/mcp","authorization_servers":["` + base + `"]}`
				if tc.prm != "" {
					body = strings.ReplaceAll(tc.prm, "%s", base)
				}
				_, _ = w.Write([]byte(body))
			})
			mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
				body := `{"issuer":"` + base + `","authorization_endpoint":"` + base + `/a","token_endpoint":"` + base + `/t","code_challenge_methods_supported":["S256"]}`
				if tc.asm != "" {
					body = strings.ReplaceAll(tc.asm, "%[1]s", base)
				}
				_, _ = w.Write([]byte(body))
			})
			srv := httptest.NewTLSServer(mux)
			defer srv.Close()
			base = srv.URL
			g := mcpauth.Guard{AllowLoopback: true}
			c := g.Client()
			c.Transport = srv.Client().Transport // trust the test certificate
			_, err := mcpauth.DiscoverRemote(context.Background(), c, base+"/mcp")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRefreshRotatesAndARevokedGrantSaysSo(t *testing.T) {
	v := mcpauthtest.New(t)
	v.DCR = true
	g, err := consent(v).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c := v.Guard().Client()
	tok, err := mcpauth.Refresh(context.Background(), c, g.Discovery.Server.TokenEndpoint, g.Client, g.Token.RefreshToken, g.Discovery.Resource)
	if err != nil {
		t.Fatal(err)
	}
	if tok.RefreshToken == g.Token.RefreshToken || v.Live(g.Token.RefreshToken) {
		t.Fatal("the vendor rotates: the old refresh token is spent")
	}
	_, err = mcpauth.Refresh(context.Background(), c, g.Discovery.Server.TokenEndpoint, g.Client, g.Token.RefreshToken, g.Discovery.Resource)
	if !mcpauth.GrantRevoked(err) {
		t.Fatalf("a spent refresh token: %v", err)
	}
	if strings.Contains(err.Error(), "PROBE-REFRESH-") {
		t.Fatal("an error carries a token")
	}
	if err := mcpauth.Revoke(context.Background(), c, g.Discovery.Server.RevocationEndpoint, g.Client, tok.RefreshToken, "refresh_token"); err != nil {
		t.Fatal(err)
	}
	if v.Live(tok.RefreshToken) {
		t.Fatal("revoked, yet live")
	}
}

func TestTheGuardRefusesNonPublicAddresses(t *testing.T) {
	g := mcpauth.Guard{}
	for _, a := range []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.169.254", "0.1.2.3", "100.64.0.1",
		"::1", "fc00::1", "fe80::1", "::ffff:10.0.0.1", "2002:7f00:1::1", "64:ff9b::a00:1", "64:ff9b:1::1", "224.0.0.1", "0.0.0.0"} {
		if g.IsAllowedIP(netip.MustParseAddr(a)) {
			t.Errorf("%s allowed", a)
		}
	}
	for _, a := range []string{"8.8.8.8", "2606:4700::1111"} {
		if !g.IsAllowedIP(netip.MustParseAddr(a)) {
			t.Errorf("%s refused", a)
		}
	}
	// A name that resolves to a private address is refused at the dial,
	// and plain http before it.
	g.LookupNetIP = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("10.0.0.5")}, nil
	}
	_, err := g.Client().Get("https://vendor.example/.well-known/oauth-authorization-server")
	var blocked *mcpauth.BlockedAddressError
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v, want a blocked address", err)
	}
	if _, err := g.Client().Get("http://vendor.example/"); err == nil || !strings.Contains(err.Error(), "https only") {
		t.Fatalf("plain http: %v", err)
	}
}

func TestTheGuardFollowsNoRedirectAndTakesNoProxy(t *testing.T) {
	var reachedTarget bool
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reachedTarget = true }))
	defer target.Close()
	redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/elsewhere", http.StatusFound)
	}))
	defer redirector.Close()
	pool := redirector.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	g := mcpauth.Guard{AllowLoopback: true, TLS: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	// A proxy in the environment that would refuse everything: the guard
	// never asks it.
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("https_proxy", "http://127.0.0.1:1")
	res, err := g.Client().Get(redirector.URL + "/start")
	if err != nil {
		t.Fatalf("the guard took the environment's proxy, or failed: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusFound || reachedTarget {
		t.Fatalf("status %d, target reached %v: a redirect must end the request", res.StatusCode, reachedTarget)
	}
}

func TestAnEchoedTokenIsMaskedInTheVendorsError(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "token " + r.PostForm.Get("refresh_token") + " is revoked; secret " + r.PostForm.Get("client_secret")})
	}))
	defer srv.Close()
	pool := srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	c := mcpauth.Guard{AllowLoopback: true, TLS: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}.Client()
	_, err := mcpauth.Refresh(context.Background(), c, srv.URL+"/token", mcpauth.Client{ID: "id", Secret: "PROBE-SECRET", AuthMethod: mcpauth.AuthSecretPost}, "PROBE-REFRESH-1", "")
	if err == nil || strings.Contains(err.Error(), "PROBE-") || !strings.Contains(err.Error(), "[redacted]") || !mcpauth.GrantRevoked(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestNoScopesDeclaredAsksForNoneBeyondTheChallenge(t *testing.T) {
	v := mcpauthtest.New(t) // its metadata lists read and write; its 401 names none
	v.DCR = true
	c := consent(v)
	var authURL string
	browser := v.Browser()
	c.Present = func(u string) { authURL = u; browser(u) }
	g, err := c.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(authURL)
	if len(g.Scopes) != 0 || u.Query().Has("scope") {
		t.Fatalf("asked for %v (%q): scopes_supported is not the contract", g.Scopes, u.Query().Get("scope"))
	}
}

// TestAKeptVerifierFinishesTheSignIn: a client whose redirect returns to
// another request (the registry's HTTPS callback) registers as a web
// client under its own name, keeps the state and the verifier, rebuilds
// the request from them when the vendor's answer arrives, and exchanges
// the code; the consent address carries its extra parameters, which
// never replace the request's own.
func TestAKeptVerifierFinishesTheSignIn(t *testing.T) {
	v := mcpauthtest.New(t)
	v.DCR = true
	ctx := context.Background()
	c := v.Guard().Client()
	d, err := mcpauth.DiscoverRemote(ctx, c, v.ServerURL())
	if err != nil {
		t.Fatal(err)
	}
	const redirect = "https://registry.example/api/v1/x/vault/me/connections/callback"
	cl, err := mcpauth.RegisterAs(ctx, c, d.Server, redirect, mcpauth.Registration{Name: "Agentry", ApplicationType: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if reg := v.Registrations[len(v.Registrations)-1]; reg["client_name"] != "Agentry" || reg["application_type"] != "web" {
		t.Fatalf("registered as %v", reg)
	}
	start, err := mcpauth.NewRequest(d.Server, cl, redirect, d.Resource, []string{"read"})
	if err != nil {
		t.Fatal(err)
	}
	start.Extra = url.Values{"access_type": {"offline"}, "prompt": {"consent"}, "state": {"not-this-one"}}
	addr := start.URL()
	q := mustQuery(t, addr)
	if q.Get("access_type") != "offline" || q.Get("prompt") != "consent" || q.Get("state") != start.State || len(q["state"]) != 1 {
		t.Fatalf("the consent address: %v", q)
	}
	state, verifier := start.State, start.Verifier()
	if verifier == "" {
		t.Fatal("no verifier to keep")
	}

	back, err := v.Redirect(addr)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(back)
	if err != nil {
		t.Fatal(err)
	}
	resumed := mcpauth.ResumeRequest(d.Server, cl, redirect, d.Resource, []string{"read"}, state, verifier)
	code, err := resumed.Callback(u)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := mcpauth.Exchange(ctx, c, resumed, code)
	if err != nil || tok.RefreshToken == "" || tok.ExpiresAt.IsZero() {
		t.Fatalf("exchange: %+v %v", tok, err)
	}
	if got := v.Requests[len(v.Requests)-1].Get("code_verifier"); got != verifier {
		t.Fatal("the exchange did not carry the kept verifier")
	}

	other := mcpauth.ResumeRequest(d.Server, cl, redirect, d.Resource, nil, "another-state", verifier)
	if _, err := other.Callback(u); !errors.Is(err, mcpauth.ErrStateMismatch) {
		t.Fatalf("a resumed request with another state: %v", err)
	}
}

// TestNativeRegistrationIsUnchanged: the CLI's own Register still names
// Agentry Desktop, a native client.
func TestNativeRegistrationIsUnchanged(t *testing.T) {
	v := mcpauthtest.New(t)
	v.DCR = true
	ctx := context.Background()
	c := v.Guard().Client()
	d, err := mcpauth.DiscoverRemote(ctx, c, v.ServerURL())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mcpauth.Register(ctx, c, d.Server, "http://127.0.0.1:1/callback"); err != nil {
		t.Fatal(err)
	}
	if reg := v.Registrations[0]; reg["client_name"] != mcpauth.NativeClientName || reg["application_type"] != "native" || reg["token_endpoint_auth_method"] != "none" {
		t.Fatalf("registered as %v", reg)
	}
}

// TestARegisteredClientIssuedASecretSendsIt: a vendor that issues a
// secret to a client that asked to be public, and names no method, gets
// the secret sent the way its metadata allows, never none; the exchange
// then authenticates.
func TestARegisteredClientIssuedASecretSendsIt(t *testing.T) {
	v := mcpauthtest.New(t)
	v.DCR, v.SecretAnyway, v.ClientSecret = true, true, "PROBE-SECRET-DCR"
	ctx := context.Background()
	c := v.Guard().Client()
	d, err := mcpauth.DiscoverRemote(ctx, c, v.ServerURL())
	if err != nil {
		t.Fatal(err)
	}
	const redirect = "https://registry.example/callback"
	cl, err := mcpauth.RegisterAs(ctx, c, d.Server, redirect, mcpauth.Registration{Name: "Agentry", ApplicationType: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if cl.Secret != "PROBE-SECRET-DCR" || cl.AuthMethod != mcpauth.AuthSecretPost {
		t.Fatalf("client = %+v, want the issued secret sent by post", cl)
	}
	r, err := mcpauth.NewRequest(d.Server, cl, redirect, d.Resource, nil)
	if err != nil {
		t.Fatal(err)
	}
	back, err := v.Redirect(r.URL())
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(back)
	code, err := r.Callback(u)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mcpauth.Exchange(ctx, c, r, code); err != nil {
		t.Fatalf("the confidential client's exchange: %v", err)
	}
}

// TestASecretSentByBasicAuthIsMasked: a client secret travels in the
// Authorization header under client_secret_basic, not in the form; a
// vendor that echoes it has it masked all the same.
func TestASecretSentByBasicAuthIsMasked(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, secret, _ := r.BasicAuth()
		secret, _ = url.QueryUnescape(secret)
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client", "error_description": "client secret " + secret + " is wrong"})
	}))
	defer srv.Close()
	pool := srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	c := mcpauth.Guard{AllowLoopback: true, TLS: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}.Client()
	cl := mcpauth.Client{ID: "id", Secret: "PROBE-SECRET-BASIC", AuthMethod: mcpauth.AuthSecretBasic}
	_, err := mcpauth.Refresh(context.Background(), c, srv.URL+"/token", cl, "rt", "")
	if err == nil || strings.Contains(err.Error(), "PROBE-") || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("refresh: %v", err)
	}
	err = mcpauth.Revoke(context.Background(), c, srv.URL+"/revoke", cl, "rt", "refresh_token")
	if err == nil || strings.Contains(err.Error(), "PROBE-") {
		t.Fatalf("revoke: %v", err)
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}
