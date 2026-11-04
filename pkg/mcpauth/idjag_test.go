package mcpauth_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/mcpauth/mcpauthtest"
)

// idjagSetup is an identity provider, an MCP authorization server that
// trusts it, and both servers' metadata as a client discovers it.
type idjagSetup struct {
	idp    *mcpauthtest.IdentityProvider
	as     *mcpauthtest.AuthorizationServer
	idpMD  *mcpauth.ServerMetadata
	asMD   *mcpauth.ServerMetadata
	client *http.Client
}

func newIDJAGSetup(t *testing.T, configure func(*mcpauthtest.IdentityProvider, *mcpauthtest.AuthorizationServer)) idjagSetup {
	t.Helper()
	idp := mcpauthtest.NewIdentityProvider(t, "requesting-app")
	idp.ClientSecret = "idp-secret"
	as := mcpauthtest.NewAuthorizationServer(t, idp, "https://registry.example/oauth-client.json")
	if configure != nil {
		configure(idp, as)
	}
	c := idp.Guard().Client()
	idpMD, err := mcpauth.DiscoverIssuer(context.Background(), c, idp.URL)
	if err != nil {
		t.Fatalf("discover the identity provider: %v", err)
	}
	asMD, err := mcpauth.DiscoverTokenIssuer(context.Background(), c, as.URL)
	if err != nil {
		t.Fatalf("discover the authorization server: %v", err)
	}
	return idjagSetup{idp: idp, as: as, idpMD: idpMD, asMD: asMD, client: c}
}

func (s idjagSetup) idpClient(method string) mcpauth.Client {
	return mcpauth.Client{ID: "requesting-app", Secret: "idp-secret", AuthMethod: method}
}

func (s idjagSetup) asClient() mcpauth.Client {
	return mcpauth.Client{ID: "https://registry.example/oauth-client.json", AuthMethod: mcpauth.AuthNone}
}

func (s idjagSetup) request(subject string) mcpauth.IDJAGRequest {
	return mcpauth.IDJAGRequest{
		Client:       s.idpClient(mcpauth.AuthSecretBasic),
		SubjectToken: subject,
		Audience:     s.as.URL,
		Resource:     s.as.ResourceURL(),
		Scopes:       []string{"read", "write"},
	}
}

func TestAnIDJAGIsRequestedByTokenExchangeAndRedeemedForAnAccessToken(t *testing.T) {
	s := newIDJAGSetup(t, nil)
	ctx := context.Background()
	jag, err := mcpauth.RequestIDJAG(ctx, s.client, *s.idpMD, s.request(s.idp.SignIn("alice")))
	if err != nil {
		t.Fatal(err)
	}
	if !s.idp.Handed(jag.Assertion) || jag.ExpiresAt.IsZero() || strings.Join(jag.Scopes, " ") != "read write" {
		t.Fatalf("ID-JAG = %+v", jag)
	}
	form := s.idp.Requests[len(s.idp.Requests)-1]
	want := map[string]string{
		"grant_type":           "urn:ietf:params:oauth:grant-type:token-exchange",
		"requested_token_type": "urn:ietf:params:oauth:token-type:id-jag",
		"subject_token_type":   "urn:ietf:params:oauth:token-type:id_token",
		"audience":             s.as.URL,
		"resource":             s.as.ResourceURL(),
		"scope":                "read write",
	}
	for k, v := range want {
		if form.Get(k) != v {
			t.Errorf("exchange %s = %q, want %q", k, form.Get(k), v)
		}
	}
	if form.Get("subject_token") == "" || form.Get("client_secret") != "" {
		t.Fatalf("exchange form = %v: a subject token, and the secret in the basic header only", form)
	}

	tok, err := mcpauth.RedeemIDJAG(ctx, s.client, *s.asMD, mcpauth.RedeemRequest{Client: s.asClient(), Assertion: jag.Assertion, Resource: s.as.ResourceURL()})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.as.Issued) != 1 || tok.AccessToken != s.as.Issued[0] || tok.ExpiresAt.IsZero() {
		t.Fatalf("token = %+v", tok)
	}
	redeem := s.as.Requests[len(s.as.Requests)-1]
	if redeem.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || redeem.Get("assertion") != jag.Assertion ||
		redeem.Get("resource") != s.as.ResourceURL() || redeem.Get("client_id") != s.asClient().ID || redeem.Has("scope") {
		t.Fatalf("redeem form = %v", redeem)
	}
}

func TestTheExchangeAuthenticatesWithASecretInTheForm(t *testing.T) {
	s := newIDJAGSetup(t, nil)
	r := s.request(s.idp.SignIn("alice"))
	r.Client = s.idpClient(mcpauth.AuthSecretPost)
	if _, err := mcpauth.RequestIDJAG(context.Background(), s.client, *s.idpMD, r); err != nil {
		t.Fatal(err)
	}
	form := s.idp.Requests[len(s.idp.Requests)-1]
	if form.Get("client_id") != "requesting-app" || form.Get("client_secret") != "idp-secret" {
		t.Fatalf("exchange form = %v", form)
	}
}

func TestTheExchangeAuthenticatesWithAPrivateKeyJWT(t *testing.T) {
	signer := mcpauthtest.NewES256Signer(t)
	s := newIDJAGSetup(t, func(idp *mcpauthtest.IdentityProvider, _ *mcpauthtest.AuthorizationServer) {
		idp.ClientSecret = ""
		idp.PublicKey = &signer.Key.PublicKey
	})
	r := s.request(s.idp.SignIn("alice"))
	r.Client = mcpauth.Client{ID: "requesting-app", AuthMethod: mcpauth.AuthPrivateKeyJWT, Signer: signer}
	if _, err := mcpauth.RequestIDJAG(context.Background(), s.client, *s.idpMD, r); err != nil {
		t.Fatal(err)
	}
	form := s.idp.Requests[len(s.idp.Requests)-1]
	if form.Get("client_id") != "requesting-app" || form.Has("client_secret") ||
		form.Get("client_assertion_type") != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" {
		t.Fatalf("exchange form = %v", form)
	}
	head := jwtPart(t, form.Get("client_assertion"), 0)
	if head["alg"] != "ES256" || head["kid"] != signer.Kid || head["typ"] != "JWT" {
		t.Fatalf("assertion header = %v", head)
	}
	if len(signer.Signed) != 1 {
		t.Fatalf("signed %d inputs, want 1", len(signer.Signed))
	}

	// A second assertion is a new one: its jti is never reused.
	if _, err := mcpauth.RequestIDJAG(context.Background(), s.client, *s.idpMD, r); err != nil {
		t.Fatal(err)
	}
	first := jwtPart(t, s.idp.Requests[0].Get("client_assertion"), 1)
	second := jwtPart(t, s.idp.Requests[1].Get("client_assertion"), 1)
	if first["jti"] == second["jti"] {
		t.Fatal("two client assertions share a jti")
	}
}

func TestTheAssertionAudienceCanBeTheIssuer(t *testing.T) {
	signer := mcpauthtest.NewES256Signer(t)
	s := newIDJAGSetup(t, func(idp *mcpauthtest.IdentityProvider, _ *mcpauthtest.AuthorizationServer) {
		idp.ClientSecret = ""
		idp.PublicKey = &signer.Key.PublicKey
	})
	r := s.request(s.idp.SignIn("alice"))
	r.Client = mcpauth.Client{ID: "requesting-app", AuthMethod: mcpauth.AuthPrivateKeyJWT, Signer: signer, AssertionAudience: s.idp.URL}
	// The fake holds the audience to its token endpoint, so an assertion
	// addressed to the issuer is refused there: the override was sent.
	_, err := mcpauth.RequestIDJAG(context.Background(), s.client, *s.idpMD, r)
	var oe *mcpauth.OAuthError
	if !errors.As(err, &oe) || oe.Code != "invalid_client" {
		t.Fatalf("err = %v, want invalid_client", err)
	}
	if aud := jwtPart(t, s.idp.Requests[0].Get("client_assertion"), 1)["aud"]; aud != s.idp.URL {
		t.Fatalf("aud = %v, want the issuer", aud)
	}
}

func TestAPrivateKeyJWTClientWithoutASignerIsRefusedBeforeAnyRequest(t *testing.T) {
	s := newIDJAGSetup(t, nil)
	r := s.request(s.idp.SignIn("alice"))
	r.Client = mcpauth.Client{ID: "requesting-app", AuthMethod: mcpauth.AuthPrivateKeyJWT}
	if _, err := mcpauth.RequestIDJAG(context.Background(), s.client, *s.idpMD, r); err == nil {
		t.Fatal("a private_key_jwt client with no signer was accepted")
	}
	if len(s.idp.Requests) != 0 {
		t.Fatalf("%d requests reached the provider", len(s.idp.Requests))
	}
}

func TestTheProvidersPolicyRefusalKeepsItsCodeAndDescription(t *testing.T) {
	s := newIDJAGSetup(t, func(idp *mcpauthtest.IdentityProvider, _ *mcpauthtest.AuthorizationServer) {
		idp.Allow = func(string, string) bool { return false }
	})
	_, err := mcpauth.RequestIDJAG(context.Background(), s.client, *s.idpMD, s.request(s.idp.SignIn("bob")))
	var oe *mcpauth.OAuthError
	if !errors.As(err, &oe) || oe.Code != "access_denied" || oe.Description != "policy does not allow this app" {
		t.Fatalf("err = %#v", err)
	}
}

func TestARefusalNeverEchoesTheSubjectToken(t *testing.T) {
	s := newIDJAGSetup(t, nil)
	_, err := mcpauth.RequestIDJAG(context.Background(), s.client, *s.idpMD, s.request("not-an-id-token-it-issued"))
	var oe *mcpauth.OAuthError
	if !errors.As(err, &oe) || oe.Code != "invalid_grant" {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "not-an-id-token-it-issued") || !strings.Contains(oe.Description, "[redacted]") {
		t.Fatalf("the refusal kept the subject token: %q", err)
	}
	if !mcpauth.GrantRevoked(err) {
		t.Fatal("invalid_grant at the provider must read as a sign-in to make again")
	}
}

func TestAnAnswerThatIsNotAnIDJAGIsRefused(t *testing.T) {
	cases := map[string]func(*mcpauthtest.IdentityProvider){
		"another issued token type": func(p *mcpauthtest.IdentityProvider) {
			p.IssuedTokenType = "urn:ietf:params:oauth:token-type:access_token"
		},
		"a bearer token type": func(p *mcpauthtest.IdentityProvider) { p.TokenType = "Bearer" },
		"not a JWT":           func(p *mcpauthtest.IdentityProvider) { p.Assertion = "opaque-value" },
		"not JSON":            func(p *mcpauthtest.IdentityProvider) { p.Raw = []byte("<html>signed in</html>") },
		"no token": func(p *mcpauthtest.IdentityProvider) {
			p.Raw = []byte(`{"issued_token_type":"urn:ietf:params:oauth:token-type:id-jag","token_type":"N_A"}`)
		},
		"oversized": func(p *mcpauthtest.IdentityProvider) {
			p.Raw = []byte(`{"issued_token_type":"urn:ietf:params:oauth:token-type:id-jag","token_type":"N_A","access_token":"` + strings.Repeat("a", 1<<17) + `"}`)
		},
	}
	for name, configure := range cases {
		t.Run(name, func(t *testing.T) {
			s := newIDJAGSetup(t, func(idp *mcpauthtest.IdentityProvider, _ *mcpauthtest.AuthorizationServer) { configure(idp) })
			jag, err := mcpauth.RequestIDJAG(context.Background(), s.client, *s.idpMD, s.request(s.idp.SignIn("alice")))
			if err == nil {
				t.Fatalf("accepted %+v", jag)
			}
		})
	}
}

func TestAnIDJAGRequestNeedsAnHTTPSAudience(t *testing.T) {
	s := newIDJAGSetup(t, nil)
	for _, aud := range []string{"", "http://as.example", "as.example"} {
		r := s.request(s.idp.SignIn("alice"))
		r.Audience = aud
		if _, err := mcpauth.RequestIDJAG(context.Background(), s.client, *s.idpMD, r); err == nil {
			t.Fatalf("audience %q accepted", aud)
		}
	}
	if len(s.idp.Requests) != 0 {
		t.Fatalf("%d requests reached the provider", len(s.idp.Requests))
	}
}

func TestAServerWithoutTheIDJAGProfileIsNotRedeemedAt(t *testing.T) {
	for name, configure := range map[string]func(*mcpauthtest.AuthorizationServer){
		"no grant profile":    func(a *mcpauthtest.AuthorizationServer) { a.NoProfile = true },
		"no JWT bearer grant": func(a *mcpauthtest.AuthorizationServer) { a.NoJWTBearer = true },
	} {
		t.Run(name, func(t *testing.T) {
			s := newIDJAGSetup(t, func(_ *mcpauthtest.IdentityProvider, as *mcpauthtest.AuthorizationServer) { configure(as) })
			if s.asMD.AcceptsIDJAG() {
				t.Fatal("metadata read as accepting ID-JAGs")
			}
			jag, err := mcpauth.RequestIDJAG(context.Background(), s.client, *s.idpMD, s.request(s.idp.SignIn("alice")))
			if err != nil {
				t.Fatal(err)
			}
			_, err = mcpauth.RedeemIDJAG(context.Background(), s.client, *s.asMD, mcpauth.RedeemRequest{Client: s.asClient(), Assertion: jag.Assertion, Resource: s.as.ResourceURL()})
			if !errors.Is(err, mcpauth.ErrNoIDJAGProfile) {
				t.Fatalf("err = %v, want ErrNoIDJAGProfile", err)
			}
			if len(s.as.Requests) != 0 {
				t.Fatalf("%d requests reached the server", len(s.as.Requests))
			}
		})
	}
}

func TestTheServersMetadataReadsAsAcceptingIDJAGs(t *testing.T) {
	s := newIDJAGSetup(t, nil)
	if !s.asMD.AcceptsIDJAG() {
		t.Fatalf("metadata = %+v", s.asMD)
	}
	// The identity provider takes token exchange but redeems no ID-JAG.
	if s.idpMD.AcceptsIDJAG() {
		t.Fatal("the identity provider read as an ID-JAG authorization server")
	}
	// A sign-in's discovery still asks for an authorization endpoint and
	// PKCE, which a server that only redeems ID-JAGs need not have.
	if _, err := mcpauth.DiscoverIssuer(context.Background(), s.client, s.as.URL); err == nil {
		t.Fatal("a sign-in's discovery accepted a server with no authorization endpoint")
	}
}

func TestARedeemRefusalNeverEchoesTheAssertion(t *testing.T) {
	s := newIDJAGSetup(t, nil)
	_, err := mcpauth.RedeemIDJAG(context.Background(), s.client, *s.asMD, mcpauth.RedeemRequest{Client: s.asClient(), Assertion: "e30.e30.forged", Resource: s.as.ResourceURL(), Scopes: []string{"read"}})
	var oe *mcpauth.OAuthError
	if !errors.As(err, &oe) || oe.Code != "invalid_grant" {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "e30.e30.forged") {
		t.Fatalf("the refusal kept the assertion: %q", err)
	}
	if got := s.as.Requests[0].Get("scope"); got != "read" {
		t.Fatalf("scope = %q", got)
	}
}

func TestTheGrantsGoThroughTheGuard(t *testing.T) {
	s := newIDJAGSetup(t, nil)
	strict := mcpauth.Guard{}.Client()
	var blocked *mcpauth.BlockedAddressError
	if _, err := mcpauth.RequestIDJAG(context.Background(), strict, *s.idpMD, s.request(s.idp.SignIn("alice"))); !errors.As(err, &blocked) {
		t.Fatalf("exchange err = %v, want a blocked address", err)
	}
	if _, err := mcpauth.RedeemIDJAG(context.Background(), strict, *s.asMD, mcpauth.RedeemRequest{Client: s.asClient(), Assertion: "e30.e30.sig", Resource: s.as.ResourceURL()}); !errors.As(err, &blocked) {
		t.Fatalf("redeem err = %v, want a blocked address", err)
	}
	if _, err := mcpauth.DiscoverTokenIssuer(context.Background(), strict, s.as.URL); !errors.As(err, &blocked) {
		t.Fatalf("discovery err = %v, want a blocked address", err)
	}
}

func TestTokenIssuerDiscoveryHoldsTheIssuerAndHTTPS(t *testing.T) {
	cases := map[string]func(base string) map[string]any{
		"another issuer": func(base string) map[string]any {
			return map[string]any{"issuer": "https://elsewhere.example", "token_endpoint": base + "/token"}
		},
		"an http token endpoint": func(base string) map[string]any {
			return map[string]any{"issuer": base, "token_endpoint": "http://plain.example/token"}
		},
		"no token endpoint": func(base string) map[string]any {
			return map[string]any{"issuer": base}
		},
	}
	for name, md := range cases {
		t.Run(name, func(t *testing.T) {
			var srv *httptest.Server
			srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(md(srv.URL))
			}))
			defer srv.Close()
			c := (&mcpauthtest.IdentityProvider{Server: srv}).Guard().Client()
			if _, err := mcpauth.DiscoverTokenIssuer(context.Background(), c, srv.URL); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestARedeemAnswerThatIsNotATokenIsRefused(t *testing.T) {
	cases := map[string][]byte{
		"not JSON":     []byte("<html>signed in</html>"),
		"no token":     []byte(`{"token_type":"Bearer"}`),
		"not a bearer": []byte(`{"access_token":"at","token_type":"DPoP"}`),
		"oversized":    []byte(`{"token_type":"Bearer","access_token":"` + strings.Repeat("a", 1<<17) + `"}`),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			s := newIDJAGSetup(t, func(_ *mcpauthtest.IdentityProvider, as *mcpauthtest.AuthorizationServer) { as.Raw = raw })
			jag, err := mcpauth.RequestIDJAG(context.Background(), s.client, *s.idpMD, s.request(s.idp.SignIn("alice")))
			if err != nil {
				t.Fatal(err)
			}
			if tok, err := mcpauth.RedeemIDJAG(context.Background(), s.client, *s.asMD, mcpauth.RedeemRequest{Client: s.asClient(), Assertion: jag.Assertion}); err == nil {
				t.Fatalf("accepted %+v", tok)
			}
		})
	}
}

func TestTheGrantsRefuseAnHTTPTokenEndpoint(t *testing.T) {
	s := newIDJAGSetup(t, nil)
	idp, as := *s.idpMD, *s.asMD
	idp.TokenEndpoint = strings.Replace(idp.TokenEndpoint, "https://", "http://", 1)
	as.TokenEndpoint = strings.Replace(as.TokenEndpoint, "https://", "http://", 1)
	if _, err := mcpauth.RequestIDJAG(context.Background(), s.client, idp, s.request(s.idp.SignIn("alice"))); err == nil {
		t.Fatal("the exchange went to an http endpoint")
	}
	if _, err := mcpauth.RedeemIDJAG(context.Background(), s.client, as, mcpauth.RedeemRequest{Client: s.asClient(), Assertion: "e30.e30.sig"}); err == nil {
		t.Fatal("the redemption went to an http endpoint")
	}
	if len(s.idp.Requests)+len(s.as.Requests) != 0 {
		t.Fatal("a request was sent")
	}
}

func TestARefusalNeverEchoesTheClientAssertion(t *testing.T) {
	signer := mcpauthtest.NewES256Signer(t)
	s := newIDJAGSetup(t, func(idp *mcpauthtest.IdentityProvider, _ *mcpauthtest.AuthorizationServer) {
		idp.ClientSecret = ""
		idp.PublicKey = &signer.Key.PublicKey
		idp.EchoClientAssertion = true
	})
	r := s.request(s.idp.SignIn("alice"))
	r.Client = mcpauth.Client{ID: "requesting-app", AuthMethod: mcpauth.AuthPrivateKeyJWT, Signer: signer, AssertionAudience: "https://elsewhere.example"}
	_, err := mcpauth.RequestIDJAG(context.Background(), s.client, *s.idpMD, r)
	var oe *mcpauth.OAuthError
	if !errors.As(err, &oe) || oe.Code != "invalid_client" {
		t.Fatalf("err = %v", err)
	}
	if sent := s.idp.Requests[0].Get("client_assertion"); sent == "" || strings.Contains(err.Error(), sent) || !strings.Contains(oe.Description, "[redacted]") {
		t.Fatalf("the refusal kept the client assertion: %q", err)
	}
}

type failingSigner struct {
	alg string
	err error
}

func (f failingSigner) Algorithm() string { return f.alg }
func (f failingSigner) KeyID() string     { return "" }
func (f failingSigner) Sign(context.Context, []byte) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return []byte("sig"), nil
}

func TestASignersErrorNeverReachesTheMessage(t *testing.T) {
	s := newIDJAGSetup(t, nil)
	cause := errors.New("key service answered: secret-key-reference-xyz")
	r := s.request(s.idp.SignIn("alice"))
	r.Client = mcpauth.Client{ID: "requesting-app", AuthMethod: mcpauth.AuthPrivateKeyJWT, Signer: failingSigner{alg: "ES256", err: cause}}
	_, err := mcpauth.RequestIDJAG(context.Background(), s.client, *s.idpMD, r)
	var se *mcpauth.SignError
	if !errors.As(err, &se) || !errors.Is(err, cause) {
		t.Fatalf("err = %v, want a SignError wrapping the cause", err)
	}
	if strings.Contains(err.Error(), "secret-key-reference-xyz") {
		t.Fatalf("the message carries the key service's answer: %q", err)
	}
	if len(s.idp.Requests) != 0 {
		t.Fatal("a request was sent")
	}
}

func TestAnAssertionIsNeverUnsignedOrSignedWithASharedKey(t *testing.T) {
	s := newIDJAGSetup(t, nil)
	for _, alg := range []string{"none", "HS256", "", "ES256K-unknown"} {
		r := s.request(s.idp.SignIn("alice"))
		r.Client = mcpauth.Client{ID: "requesting-app", AuthMethod: mcpauth.AuthPrivateKeyJWT, Signer: failingSigner{alg: alg}}
		if _, err := mcpauth.RequestIDJAG(context.Background(), s.client, *s.idpMD, r); err == nil {
			t.Fatalf("algorithm %q accepted", alg)
		}
	}
	if len(s.idp.Requests) != 0 {
		t.Fatalf("%d requests were sent", len(s.idp.Requests))
	}
}

func TestCredentialsAreNeverSerialised(t *testing.T) {
	signer := mcpauthtest.NewES256Signer(t)
	for name, v := range map[string]any{
		"client":         mcpauth.Client{ID: "id", Secret: "CRED-secret", AuthMethod: mcpauth.AuthPrivateKeyJWT, Signer: signer, AssertionAudience: "CRED-aud"},
		"ID-JAG request": mcpauth.IDJAGRequest{SubjectToken: "CRED-subject", Audience: "https://as.example"},
		"ID-JAG":         mcpauth.IDJAG{Assertion: "CRED-assertion"},
		"redeem request": mcpauth.RedeemRequest{Assertion: "CRED-assertion"},
	} {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(string(raw), "CRED-") || strings.Contains(string(raw), signer.Kid) {
			t.Fatalf("%s serialised a credential: %s", name, raw)
		}
	}
}

func TestASignInsDiscoveryStillNeedsAnAuthorizationEndpoint(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": srv.URL, "token_endpoint": srv.URL + "/token", "code_challenge_methods_supported": []string{"S256"}})
	}))
	defer srv.Close()
	c := (&mcpauthtest.IdentityProvider{Server: srv}).Guard().Client()
	if _, err := mcpauth.DiscoverIssuer(context.Background(), c, srv.URL); err == nil {
		t.Fatal("a sign-in's discovery accepted a server with no authorization endpoint")
	}
	if _, err := mcpauth.DiscoverTokenIssuer(context.Background(), c, srv.URL); err != nil {
		t.Fatalf("token issuer discovery: %v", err)
	}
}

func jwtPart(t *testing.T, jwt string, i int) map[string]any {
	t.Helper()
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("%q is not a compact JWS", jwt)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[i])
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
