package mcpauth_test

import (
	"context"
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
	if g.Token.AccessToken == "" || len(said) != 1 || !strings.Contains(said[0], "state does not match") {
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
	var blocked *mcpauth.ErrBlockedAddress
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v, want a blocked address", err)
	}
	if _, err := g.Client().Get("http://vendor.example/"); err == nil || !strings.Contains(err.Error(), "https only") {
		t.Fatalf("plain http: %v", err)
	}
}
