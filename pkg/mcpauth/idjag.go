package mcpauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// The client half of MCP's Enterprise-Managed Authorization extension
// (MCP 2026-07-28), built on draft-ietf-oauth-identity-assertion-authz-grant:
// a person signed in at their organization's identity provider is given an
// MCP server's access token with no consent screen. The client asks the
// identity provider, by token exchange (RFC 8693), for an Identity Assertion
// JWT Authorization Grant (ID-JAG) addressed to the MCP server's
// authorization server; the identity provider applies the organization's
// policy; the client redeems the ID-JAG there by the JWT bearer grant
// (RFC 7523) for an access token. The client never sends the person to the
// MCP server's own authorization endpoint.
//
// Neither grant is a sign-in in the browser: both are token requests, sent
// through the caller's guarded client like every other request here, and a
// refusal is an *OAuthError whose description has every value the request
// carried replaced. The metadata both take must come from DiscoverIssuer or
// DiscoverTokenIssuer, which hold it to its issuer: the person's ID token goes
// to the token endpoint it names.

// ExtensionEnterpriseManagedAuthorization is the extension's identifier, as
// a client declares it in a request's client capabilities.
const ExtensionEnterpriseManagedAuthorization = "io.modelcontextprotocol/enterprise-managed-authorization"

// The grant types, token types and grant profile the two requests use.
const (
	GrantTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	GrantJWTBearer     = "urn:ietf:params:oauth:grant-type:jwt-bearer"
	// GrantProfileIDJAG is what an authorization server that redeems
	// ID-JAGs lists in authorization_grant_profiles_supported.
	GrantProfileIDJAG = "urn:ietf:params:oauth:grant-profile:id-jag"
	TokenTypeIDJAG    = "urn:ietf:params:oauth:token-type:id-jag"
	TokenTypeIDToken  = "urn:ietf:params:oauth:token-type:id_token"

	clientAssertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
	// assertionLifetime is how long a client assertion is good for: long
	// enough for one request, short enough that a copy is soon worthless.
	assertionLifetime = 60 * time.Second
)

// ErrNoIDJAGProfile is an authorization server whose metadata does not say
// it redeems ID-JAGs; RedeemIDJAG refuses it before any request.
var ErrNoIDJAGProfile = errors.New("the authorization server does not say it takes ID-JAGs")

// assertionAlgorithms are the JWS algorithms a Signer may sign a client
// assertion with: asymmetric ones only, so "none" and a shared-key HMAC are
// never sent.
var assertionAlgorithms = []string{"ES256", "ES384", "ES512", "RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "EdDSA"}

// SignError is a Signer that could not sign a client assertion. Its message
// is fixed, because a key service's error can carry its own response; the
// cause is kept for errors.Is and errors.As.
type SignError struct{ cause error }

func (e *SignError) Error() string { return "the client assertion could not be signed" }
func (e *SignError) Unwrap() error { return e.cause }

// AcceptsIDJAG reports whether the metadata says the server redeems ID-JAGs:
// the ID-JAG grant profile among its authorization grant profiles and the
// JWT bearer grant among its grant types.
func (md ServerMetadata) AcceptsIDJAG() bool {
	return slices.Contains(md.AuthorizationGrantProfilesSupported, GrantProfileIDJAG) &&
		slices.Contains(md.GrantTypesSupported, GrantJWTBearer)
}

// Signer signs a private_key_jwt client's assertions, so the key can stay in
// a key service. Algorithm is the JWS algorithm ("ES256", "RS256",
// "EdDSA"); KeyID the key's id in the client's published key set, or "".
// Sign returns the JWS signature over input, in the form the algorithm's JWS
// encoding takes (for ES256, R and S of 32 bytes each, not ASN.1).
type Signer interface {
	Algorithm() string
	KeyID() string
	Sign(ctx context.Context, input []byte) ([]byte, error)
}

// clientAssertion makes a private_key_jwt client's assertion (RFC 7523
// §2.2): issued by and about the client, for aud, good for one minute, with
// a fresh jti.
func clientAssertion(ctx context.Context, client Client, aud string) (string, error) {
	if client.Signer == nil {
		return "", errors.New("a private_key_jwt client needs a signer")
	}
	if client.ID == "" {
		return "", errors.New("a private_key_jwt client needs an id")
	}
	alg := client.Signer.Algorithm()
	if !slices.Contains(assertionAlgorithms, alg) {
		return "", fmt.Errorf("a client assertion cannot be signed with %q", safeTokenType(alg))
	}
	jti, err := randomString(24)
	if err != nil {
		return "", err
	}
	head := map[string]string{"alg": alg, "typ": "JWT"}
	if kid := client.Signer.KeyID(); kid != "" {
		head["kid"] = kid
	}
	now := time.Now()
	claims := map[string]any{
		"iss": client.ID,
		"sub": client.ID,
		"aud": aud,
		"jti": jti,
		"iat": now.Unix(),
		"exp": now.Add(assertionLifetime).Unix(),
	}
	h, err := json.Marshal(head)
	if err != nil {
		return "", err
	}
	c, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	input := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	sig, err := client.Signer.Sign(ctx, []byte(input))
	if err != nil {
		return "", &SignError{cause: err}
	}
	if len(sig) == 0 {
		return "", &SignError{cause: errors.New("no signature")}
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// IDJAGRequest is a request to an identity provider for an ID-JAG.
type IDJAGRequest struct {
	// Client is the requesting app as the identity provider knows it.
	Client Client
	// SubjectToken is the person's OpenID Connect ID token from that
	// identity provider. It is never serialised.
	SubjectToken string `json:"-"`
	// Audience is the issuer of the MCP server's authorization server.
	Audience string
	// Resource is the MCP server's address, where there is one.
	Resource string
	// Scopes are the scopes asked of the MCP server, if any.
	Scopes []string
}

// IDJAG is an identity provider's grant: Assertion is the signed JWT to
// redeem, a credential of the person's that the caller keeps out of logs and
// sends only to RedeemIDJAG, and that is never serialised; ExpiresAt is when
// the provider said it lapses, zero when it did not; Scopes the scopes it
// granted, when it said.
type IDJAG struct {
	Assertion string `json:"-"`
	ExpiresAt time.Time
	Scopes    []string
}

// RequestIDJAG asks the identity provider whose metadata is idp for an
// ID-JAG addressed to r.Audience, by token exchange. The answer must say it
// issued an ID-JAG (issued_token_type) of token type N_A, and the grant must
// be a compact JWS; anything else is refused.
func RequestIDJAG(ctx context.Context, c *http.Client, idp ServerMetadata, r IDJAGRequest) (*IDJAG, error) {
	if r.SubjectToken == "" {
		return nil, errors.New("an ID-JAG request needs the person's ID token")
	}
	if !httpsAddress(r.Audience) {
		return nil, fmt.Errorf("an ID-JAG's audience must be an https issuer, not %q", r.Audience)
	}
	if r.Resource != "" && !httpsAddress(r.Resource) {
		return nil, fmt.Errorf("an ID-JAG's resource must be an https address, not %q", r.Resource)
	}
	if !strings.HasPrefix(idp.TokenEndpoint, "https://") {
		return nil, errors.New("the identity provider's token endpoint is not https")
	}
	form := url.Values{}
	form.Set("grant_type", GrantTokenExchange)
	form.Set("requested_token_type", TokenTypeIDJAG)
	form.Set("subject_token", r.SubjectToken)
	form.Set("subject_token_type", TokenTypeIDToken)
	form.Set("audience", r.Audience)
	if r.Resource != "" {
		form.Set("resource", r.Resource)
	}
	if len(r.Scopes) > 0 {
		form.Set("scope", strings.Join(r.Scopes, " "))
	}
	res, err := postForm(ctx, c, idp.TokenEndpoint, r.Client, form)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("read the identity provider's answer: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		return nil, oauthError(res.StatusCode, raw, form, r.Client.Secret)
	}
	if len(raw) > maxBody {
		return nil, errors.New("the identity provider's answer is too large")
	}
	var out struct {
		IssuedTokenType string          `json:"issued_token_type"`
		AccessToken     string          `json:"access_token"`
		TokenType       string          `json:"token_type"`
		ExpiresIn       json.RawMessage `json:"expires_in"`
		Scope           string          `json:"scope"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, errors.New("the identity provider's answer is not JSON")
	}
	if out.IssuedTokenType != TokenTypeIDJAG {
		return nil, fmt.Errorf("the identity provider issued %q, not an ID-JAG", safeTokenType(out.IssuedTokenType))
	}
	if !strings.EqualFold(out.TokenType, "N_A") {
		return nil, fmt.Errorf("the identity provider issued an ID-JAG of token type %q, not N_A", safeTokenType(out.TokenType))
	}
	if !compactJWS(out.AccessToken) {
		return nil, errors.New("the identity provider's ID-JAG is not a signed JWT")
	}
	g := &IDJAG{Assertion: out.AccessToken, Scopes: strings.Fields(out.Scope)}
	if secs, ok := expiresIn(out.ExpiresIn); ok && secs > 0 {
		g.ExpiresAt = time.Now().Add(time.Duration(secs) * time.Second)
	}
	return g, nil
}

// RedeemRequest is an ID-JAG presented at an MCP server's authorization
// server.
type RedeemRequest struct {
	// Client is the identity the ID-JAG names as its client_id, as the
	// authorization server knows it.
	Client Client
	// Assertion is the ID-JAG. It is never serialised.
	Assertion string `json:"-"`
	// Resource is the MCP server's address, sent where there is one
	// (RFC 8707).
	Resource string
	// Scopes narrow what the ID-JAG granted, if any.
	Scopes []string
}

// RedeemIDJAG presents an ID-JAG at the MCP server's authorization server,
// whose metadata is as, by the JWT bearer grant, for a bearer access token.
// A server whose metadata does not say it redeems ID-JAGs is refused before
// any request, with ErrNoIDJAGProfile.
func RedeemIDJAG(ctx context.Context, c *http.Client, as ServerMetadata, r RedeemRequest) (*Token, error) {
	if !as.AcceptsIDJAG() {
		return nil, fmt.Errorf("authorization server %s: %w", as.Issuer, ErrNoIDJAGProfile)
	}
	if !strings.HasPrefix(as.TokenEndpoint, "https://") {
		return nil, fmt.Errorf("authorization server %s names an endpoint that is not https", as.Issuer)
	}
	if r.Assertion == "" {
		return nil, errors.New("no ID-JAG to redeem")
	}
	form := url.Values{}
	form.Set("grant_type", GrantJWTBearer)
	form.Set("assertion", r.Assertion)
	if r.Resource != "" {
		form.Set("resource", r.Resource)
	}
	if len(r.Scopes) > 0 {
		form.Set("scope", strings.Join(r.Scopes, " "))
	}
	return tokenRequest(ctx, c, as.TokenEndpoint, r.Client, form)
}

// httpsAddress reports whether s is an absolute https address with a host
// and no fragment.
func httpsAddress(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.Fragment == ""
}

// compactJWS reports whether s has the shape of a signed JWT: three
// base64url segments, the first two non-empty.
func compactJWS(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return false
	}
	for _, p := range parts {
		if _, err := base64.RawURLEncoding.DecodeString(p); err != nil {
			return false
		}
	}
	return true
}

// safeTokenType keeps a token type short and to the characters a URN or a
// token type name uses, for an error message.
func safeTokenType(s string) string {
	if len(s) > 100 {
		return s[:100] + "…"
	}
	for _, r := range s {
		if !codeChar(r) && r != ':' {
			return "?"
		}
	}
	return s
}
