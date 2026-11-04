package mcpauthtest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
)

// IdentityProvider is a fake enterprise identity provider that issues
// Identity Assertion JWT Authorization Grants (ID-JAGs) by RFC 8693 token
// exchange: its metadata, and a token endpoint that takes the exchange from
// one requesting app, by client secret or by private_key_jwt with ES256.
// Fields set before the first request shape it.
type IdentityProvider struct {
	*httptest.Server

	// ClientID is the requesting app; ClientSecret, when set, is its secret;
	// PublicKey, when set, verifies its private_key_jwt client assertions.
	ClientID     string
	ClientSecret string
	PublicKey    *ecdsa.PublicKey
	// Allow is the admin's policy: whether an ID-JAG is issued for this
	// audience and resource. Nil allows everything.
	Allow func(audience, resource string) bool
	// IssuedTokenType, TokenType and Assertion override the answer's
	// fields, for tests of a provider that answers wrongly.
	IssuedTokenType, TokenType, Assertion string
	// Raw, when set, is the whole answer's body, sent as is.
	Raw []byte
	// EchoClientAssertion refuses a client it cannot authenticate with a
	// description that repeats the client assertion, as a careless provider
	// might.
	EchoClientAssertion bool

	mu sync.Mutex
	// SubjectTokens are the ID tokens it accepts as subject tokens.
	subjects map[string]bool
	// Requests is every token-endpoint form.
	Requests []url.Values
	// Issued is every ID-JAG it issued.
	Issued []string
	seq    int
}

// NewIdentityProvider starts the fake with a requesting app of the given id.
func NewIdentityProvider(t *testing.T, clientID string) *IdentityProvider {
	t.Helper()
	p := &IdentityProvider{ClientID: clientID, subjects: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", p.metadata)
	mux.HandleFunc("/token", p.token)
	p.Server = httptest.NewTLSServer(mux)
	t.Cleanup(p.Close)
	return p
}

// Guard trusts the fake's certificate and its loopback address. Every
// httptest TLS server presents the same certificate, so this guard also
// reaches an AuthorizationServer started beside it.
func (p *IdentityProvider) Guard() mcpauth.Guard { return guardFor(p.Server) }

// SignIn records an ID token the provider issued to a person, as their
// sign-in at the provider would, and returns it.
func (p *IdentityProvider) SignIn(subject string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	tok := unsignedJWT(map[string]any{"typ": "JWT", "alg": "none"}, map[string]any{"iss": p.URL, "sub": subject, "aud": p.ClientID, "n": p.seq, "r": random()})
	p.subjects[tok] = true
	return tok
}

// Handed reports whether s is an ID-JAG this provider issued.
func (p *IdentityProvider) Handed(s string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, v := range p.Issued {
		if v == s {
			return true
		}
	}
	return false
}

func (p *IdentityProvider) metadata(w http.ResponseWriter, _ *http.Request) {
	writeStatusJSON(w, http.StatusOK, map[string]any{
		"issuer":                                p.URL,
		"authorization_endpoint":                p.URL + "/authorize",
		"token_endpoint":                        p.URL + "/token",
		"code_challenge_methods_supported":      []string{"S256"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token", mcpauth.GrantTokenExchange},
		"token_endpoint_auth_methods_supported": []string{mcpauth.AuthSecretBasic, mcpauth.AuthSecretPost, mcpauth.AuthPrivateKeyJWT},
	})
}

func (p *IdentityProvider) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthRefusal(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	p.mu.Lock()
	p.Requests = append(p.Requests, r.PostForm)
	p.mu.Unlock()
	f := r.PostForm
	if !authenticated(r, f, p.ClientID, p.ClientSecret, p.PublicKey, p.URL+"/token") {
		desc := ""
		if p.EchoClientAssertion {
			desc = "bad assertion " + f.Get("client_assertion")
		}
		oauthRefusal(w, http.StatusUnauthorized, "invalid_client", desc)
		return
	}
	if f.Get("grant_type") != mcpauth.GrantTokenExchange {
		oauthRefusal(w, http.StatusBadRequest, "unsupported_grant_type", "")
		return
	}
	if f.Get("requested_token_type") != mcpauth.TokenTypeIDJAG || f.Get("subject_token_type") != mcpauth.TokenTypeIDToken {
		oauthRefusal(w, http.StatusBadRequest, "invalid_request", "this provider issues ID-JAGs for ID tokens only")
		return
	}
	p.mu.Lock()
	known := p.subjects[f.Get("subject_token")]
	p.mu.Unlock()
	if !known {
		oauthRefusal(w, http.StatusBadRequest, "invalid_grant", "subject token "+f.Get("subject_token")+" is not one this provider issued")
		return
	}
	if f.Get("audience") == "" {
		oauthRefusal(w, http.StatusBadRequest, "invalid_target", "")
		return
	}
	if p.Allow != nil && !p.Allow(f.Get("audience"), f.Get("resource")) {
		oauthRefusal(w, http.StatusBadRequest, "access_denied", "policy does not allow this app")
		return
	}
	if p.Raw != nil {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(p.Raw)
		return
	}
	jag := p.Assertion
	if jag == "" {
		claims := map[string]any{"iss": p.URL, "aud": f.Get("audience"), "client_id": p.ClientID, "jti": random(), "exp": time.Now().Add(5 * time.Minute).Unix()}
		if res := f.Get("resource"); res != "" {
			claims["resource"] = res
		}
		if sc := f.Get("scope"); sc != "" {
			claims["scope"] = sc
		}
		jag = unsignedJWT(map[string]any{"typ": "oauth-id-jag+jwt", "alg": "none"}, claims) + random()
	}
	p.mu.Lock()
	p.Issued = append(p.Issued, jag)
	p.mu.Unlock()
	answer := map[string]any{
		"issued_token_type": orDefault(p.IssuedTokenType, mcpauth.TokenTypeIDJAG),
		"access_token":      jag,
		"token_type":        orDefault(p.TokenType, "N_A"),
		"expires_in":        300,
	}
	if sc := f.Get("scope"); sc != "" {
		answer["scope"] = sc
	}
	writeStatusJSON(w, http.StatusOK, answer)
}

// AuthorizationServer is a fake MCP authorization server that takes an
// ID-JAG by the JWT bearer grant (RFC 7523) and answers with an access
// token. It trusts the ID-JAGs of one IdentityProvider and knows one client.
type AuthorizationServer struct {
	*httptest.Server

	// ClientID is the client it knows; ClientSecret, when set, its secret.
	ClientID     string
	ClientSecret string
	// NoProfile leaves the ID-JAG grant profile out of its metadata, and
	// NoJWTBearer the JWT bearer grant type.
	NoProfile, NoJWTBearer bool
	// Trusts is the identity provider whose ID-JAGs it takes.
	Trusts *IdentityProvider
	// RefreshToken issues a refresh token beside the access token.
	RefreshToken bool
	// Raw, when set, is the whole answer's body to a trusted assertion,
	// sent as is.
	Raw []byte

	mu sync.Mutex
	// Requests is every token-endpoint form.
	Requests []url.Values
	// Issued is every access token it issued.
	Issued []string
}

// NewAuthorizationServer starts the fake, trusting idp's ID-JAGs.
func NewAuthorizationServer(t *testing.T, idp *IdentityProvider, clientID string) *AuthorizationServer {
	t.Helper()
	a := &AuthorizationServer{ClientID: clientID, Trusts: idp}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-authorization-server", a.metadata)
	mux.HandleFunc("/token", a.token)
	a.Server = httptest.NewTLSServer(mux)
	t.Cleanup(a.Close)
	return a
}

// ResourceURL is the MCP server this authorization server issues tokens for.
func (a *AuthorizationServer) ResourceURL() string { return a.URL + "/mcp" }

func (a *AuthorizationServer) metadata(w http.ResponseWriter, _ *http.Request) {
	grants := []string{"refresh_token"}
	if !a.NoJWTBearer {
		grants = append(grants, mcpauth.GrantJWTBearer)
	}
	md := map[string]any{
		"issuer":                                a.URL,
		"token_endpoint":                        a.URL + "/token",
		"grant_types_supported":                 grants,
		"token_endpoint_auth_methods_supported": []string{mcpauth.AuthNone, mcpauth.AuthSecretPost},
	}
	if !a.NoProfile {
		md["authorization_grant_profiles_supported"] = []string{mcpauth.GrantProfileIDJAG}
	}
	writeStatusJSON(w, http.StatusOK, md)
}

func (a *AuthorizationServer) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthRefusal(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	a.mu.Lock()
	a.Requests = append(a.Requests, r.PostForm)
	a.mu.Unlock()
	f := r.PostForm
	if !authenticated(r, f, a.ClientID, a.ClientSecret, nil, "") {
		oauthRefusal(w, http.StatusUnauthorized, "invalid_client", "")
		return
	}
	if f.Get("grant_type") != mcpauth.GrantJWTBearer {
		oauthRefusal(w, http.StatusBadRequest, "unsupported_grant_type", "")
		return
	}
	if a.Trusts == nil || !a.Trusts.Handed(f.Get("assertion")) {
		oauthRefusal(w, http.StatusBadRequest, "invalid_grant", "assertion "+f.Get("assertion")+" is not trusted")
		return
	}
	if a.Raw != nil {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(a.Raw)
		return
	}
	at := "PROBE-ACCESS-" + random()
	a.mu.Lock()
	a.Issued = append(a.Issued, at)
	a.mu.Unlock()
	answer := map[string]any{"access_token": at, "token_type": "Bearer", "expires_in": 3600}
	if a.RefreshToken {
		answer["refresh_token"] = "PROBE-REFRESH-" + random()
	}
	if sc := f.Get("scope"); sc != "" {
		answer["scope"] = sc
	}
	writeStatusJSON(w, http.StatusOK, answer)
}

// ES256Signer signs client assertions with an in-memory P-256 key: the test
// stand-in for a caller's key service.
type ES256Signer struct {
	Key *ecdsa.PrivateKey
	Kid string
	// Signed is every signing input it signed.
	Signed [][]byte
	mu     sync.Mutex
}

// NewES256Signer makes a signer with a fresh key.
func NewES256Signer(t *testing.T) *ES256Signer {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &ES256Signer{Key: k, Kid: "test-" + random()}
}

// Algorithm is ES256.
func (s *ES256Signer) Algorithm() string { return "ES256" }

// KeyID is the key's id.
func (s *ES256Signer) KeyID() string { return s.Kid }

// Sign signs input, answering the JWS signature (R and S, 32 bytes each).
func (s *ES256Signer) Sign(_ context.Context, input []byte) ([]byte, error) {
	s.mu.Lock()
	s.Signed = append(s.Signed, append([]byte(nil), input...))
	s.mu.Unlock()
	sum := sha256.Sum256(input)
	r, ss, err := ecdsa.Sign(rand.Reader, s.Key, sum[:])
	if err != nil {
		return nil, err
	}
	out := make([]byte, 64)
	r.FillBytes(out[:32])
	ss.FillBytes(out[32:])
	return out, nil
}

// authenticated checks a token request's client authentication: a client
// secret (basic or post), a private_key_jwt assertion verified with pub
// (iss and sub the client, aud the token endpoint, not expired), or, where
// the client has neither, its id alone.
func authenticated(r *http.Request, f url.Values, clientID, secret string, pub *ecdsa.PublicKey, aud string) bool {
	if f.Get("client_assertion_type") != "" || f.Get("client_assertion") != "" {
		if pub == nil || f.Get("client_assertion_type") != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" {
			return false
		}
		return verifyAssertion(f.Get("client_assertion"), pub, clientID, aud)
	}
	if id, pw, ok := r.BasicAuth(); ok {
		id, _ = url.QueryUnescape(id)
		pw, _ = url.QueryUnescape(pw)
		return secret != "" && id == clientID && pw == secret
	}
	if f.Get("client_id") != clientID {
		return false
	}
	return secret == "" && pub == nil || secret != "" && f.Get("client_secret") == secret
}

func verifyAssertion(jwt string, pub *ecdsa.PublicKey, clientID, aud string) bool {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return false
	}
	var head struct {
		Alg string `json:"alg"`
	}
	var claims struct {
		Iss string `json:"iss"`
		Sub string `json:"sub"`
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Jti string `json:"jti"`
	}
	if decodePart(parts[0], &head) != nil || head.Alg != "ES256" || decodePart(parts[1], &claims) != nil {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return false
	}
	return claims.Iss == clientID && claims.Sub == clientID && claims.Aud == aud && claims.Jti != "" && claims.Exp > time.Now().Unix()
}

func decodePart(s string, into any) error {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, into)
}

func unsignedJWT(head, claims map[string]any) string {
	h, _ := json.Marshal(head)
	c, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c) + "."
}

func guardFor(s *httptest.Server) mcpauth.Guard {
	pool := x509.NewCertPool()
	pool.AddCert(s.Certificate())
	return mcpauth.Guard{AllowLoopback: true, TLS: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
}

func writeStatusJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthRefusal(w http.ResponseWriter, status int, code, desc string) {
	body := map[string]string{"error": code}
	if desc != "" {
		body["error_description"] = desc
	}
	writeStatusJSON(w, status, body)
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
