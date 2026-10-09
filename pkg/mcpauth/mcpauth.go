// Package mcpauth signs a person in to a remote MCP server by OAuth, as the
// MCP authorization specification (2026-07-28) asks of a client (plan
// mcp-oauth-connections.md §6, agentry-cli#664): Protected Resource
// Metadata (RFC 9728) from the server's 401, the authorization server's
// metadata (RFC 8414, OpenID discovery), the client the vendor will take
// (the person's own, Agentry's Client ID Metadata Document, or dynamic
// registration, in that order here), PKCE with S256, the resource
// parameter (RFC 8707), the iss check on the redirect (RFC 9207), the code
// exchange, refresh and revocation (RFC 7009).
//
// Every request to a vendor goes through Guard: https, public addresses
// only, no redirect, no proxy from the environment. Nothing here logs or
// returns a vendor's answer: an error carries the OAuth error code and
// never a body, which can hold a token.
//
// What a run receives is an access token and nothing else (D8's
// condition): the refresh token and any client secret stay with the
// caller's store (pkg/runner/connection) and this package's callers.
package mcpauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// maxBody bounds every vendor answer read.
const maxBody = 1 << 16

// ResourceMetadata is a protected resource's metadata (RFC 9728).
type ResourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported,omitempty"`
}

// ServerMetadata is an authorization server's metadata (RFC 8414), the
// fields a client uses.
type ServerMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint,omitempty"`
	RevocationEndpoint                string   `json:"revocation_endpoint,omitempty"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported,omitempty"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported,omitempty"`
	ISSParameterSupported             bool     `json:"authorization_response_iss_parameter_supported,omitempty"`
	CIMDSupported                     bool     `json:"client_id_metadata_document_supported,omitempty"`
}

// Discovery is what a remote server says about how it is signed in to.
type Discovery struct {
	// Resource is the server's canonical address, sent as the resource
	// parameter.
	Resource string
	// ChallengeScopes are the scopes its 401 named; ResourceScopes the ones
	// its metadata lists.
	ChallengeScopes []string
	ResourceScopes  []string
	Server          ServerMetadata
}

// OAuthError is an authorization server's refusal: the RFC 6749 error code
// and, trimmed, its description. Never the body.
type OAuthError struct {
	Status      int
	Code        string
	Description string
}

func (e *OAuthError) Error() string {
	s := "the vendor refused (" + strconv.Itoa(e.Status)
	if e.Code != "" {
		s += ", " + e.Code
	}
	s += ")"
	if e.Description != "" {
		s += ": " + e.Description
	}
	return s
}

// GrantRevoked reports whether err is the vendor saying the grant is no
// longer good: a new consent is the only way on.
func GrantRevoked(err error) bool {
	var oe *OAuthError
	if !errors.As(err, &oe) {
		return false
	}
	switch oe.Code {
	case "invalid_grant", "invalid_client", "unauthorized_client":
		return true
	}
	return false
}

// DiscoverRemote reads how serverURL is signed in to: an unauthenticated
// initialize, whose 401 names the resource metadata (else the well-known
// addresses on the server's origin), then the first authorization server's
// metadata.
func DiscoverRemote(ctx context.Context, c *http.Client, serverURL string) (*Discovery, error) {
	su, err := url.Parse(serverURL)
	if err != nil || su.Scheme != "https" || su.Host == "" {
		return nil, fmt.Errorf("%s is not an https address", serverURL)
	}
	metaURL, scopes, err := challenge(ctx, c, serverURL)
	if err != nil {
		return nil, err
	}
	var candidates []string
	if metaURL != "" {
		candidates = []string{metaURL}
	} else {
		origin := su.Scheme + "://" + su.Host
		if p := strings.TrimRight(su.Path, "/"); p != "" {
			candidates = append(candidates, origin+"/.well-known/oauth-protected-resource"+p)
		}
		candidates = append(candidates, origin+"/.well-known/oauth-protected-resource")
	}
	var rm ResourceMetadata
	var lastErr error
	found := false
	for _, u := range candidates {
		if lastErr = getJSON(ctx, c, u, &rm); lastErr == nil {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("%s publishes no OAuth resource metadata: %w", serverURL, lastErr)
	}
	if err := sameResource(rm.Resource, su); err != nil {
		return nil, err
	}
	if len(rm.AuthorizationServers) == 0 {
		return nil, fmt.Errorf("%s names no authorization server", serverURL)
	}
	md, err := DiscoverIssuer(ctx, c, rm.AuthorizationServers[0])
	if err != nil {
		return nil, err
	}
	return &Discovery{Resource: rm.Resource, ChallengeScopes: scopes, ResourceScopes: rm.ScopesSupported, Server: *md}, nil
}

// challenge makes the unauthenticated request and reads its
// WWW-Authenticate: the resource metadata address and the scopes, either
// of which may be absent.
func challenge(ctx context.Context, c *http.Client, serverURL string) (string, []string, error) {
	// Any initialize will do: the request exists to be refused with the
	// server's challenge, and a server that answers it takes no sign-in.
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"agentry","version":"1"}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL, strings.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	res, err := c.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("reach %s: %w", serverURL, err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxBody))
	_ = res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		return "", nil, nil
	}
	params := bearerParams(res.Header.Values("WWW-Authenticate"))
	var scopes []string
	if s := params["scope"]; s != "" {
		scopes = strings.Fields(s)
	}
	return params["resource_metadata"], scopes, nil
}

// bearerParams parses the auth-params of a Bearer challenge.
func bearerParams(headers []string) map[string]string {
	out := map[string]string{}
	for _, h := range headers {
		h = strings.TrimSpace(h)
		if len(h) < 6 || !strings.EqualFold(h[:6], "bearer") {
			continue
		}
		rest := h[6:]
		for rest != "" {
			rest = strings.TrimLeft(rest, " ,")
			eq := strings.IndexByte(rest, '=')
			if eq <= 0 {
				break
			}
			name := strings.ToLower(strings.TrimSpace(rest[:eq]))
			rest = rest[eq+1:]
			var val string
			if strings.HasPrefix(rest, `"`) {
				end := strings.IndexByte(rest[1:], '"')
				if end < 0 {
					break
				}
				val, rest = rest[1:1+end], rest[2+end:]
			} else {
				end := strings.IndexByte(rest, ',')
				if end < 0 {
					end = len(rest)
				}
				val, rest = strings.TrimSpace(rest[:end]), rest[end:]
			}
			out[name] = val
		}
	}
	return out
}

// sameResource holds the metadata's resource to the server it was read
// for (RFC 9728 §3.3): the same origin, and a path the server's sits under.
func sameResource(resource string, server *url.URL) error {
	ru, err := url.Parse(resource)
	if err != nil || ru.Scheme != "https" || !strings.EqualFold(ru.Host, server.Host) {
		return fmt.Errorf("the resource metadata of %s names another resource (%q)", server.Redacted(), resource)
	}
	rp, sp := strings.TrimRight(ru.Path, "/"), strings.TrimRight(server.Path, "/")
	if rp != "" && sp != rp && !strings.HasPrefix(sp, rp+"/") {
		return fmt.Errorf("the resource metadata of %s names another resource (%q)", server.Redacted(), resource)
	}
	return nil
}

// DiscoverIssuer reads an authorization server's metadata from the
// well-known addresses the specification orders, and holds it to its own
// issuer and to PKCE with S256.
func DiscoverIssuer(ctx context.Context, c *http.Client, issuer string) (*ServerMetadata, error) {
	iu, err := url.Parse(issuer)
	if err != nil || iu.Scheme != "https" || iu.Host == "" || iu.RawQuery != "" || iu.Fragment != "" {
		return nil, fmt.Errorf("authorization server %q is not an https issuer", issuer)
	}
	origin := iu.Scheme + "://" + iu.Host
	p := strings.TrimRight(iu.Path, "/")
	var candidates []string
	if p != "" {
		candidates = []string{
			origin + "/.well-known/oauth-authorization-server" + p,
			origin + "/.well-known/openid-configuration" + p,
			origin + p + "/.well-known/openid-configuration",
		}
	} else {
		candidates = []string{
			origin + "/.well-known/oauth-authorization-server",
			origin + "/.well-known/openid-configuration",
		}
	}
	var md ServerMetadata
	var lastErr error
	for _, u := range candidates {
		md = ServerMetadata{}
		if lastErr = getJSON(ctx, c, u, &md); lastErr == nil {
			break
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("authorization server %s publishes no metadata: %w", issuer, lastErr)
	}
	if strings.TrimRight(md.Issuer, "/") != strings.TrimRight(issuer, "/") {
		return nil, fmt.Errorf("authorization server %s calls itself %q: refused", issuer, md.Issuer)
	}
	for _, e := range []string{md.AuthorizationEndpoint, md.TokenEndpoint} {
		if !strings.HasPrefix(e, "https://") {
			return nil, fmt.Errorf("authorization server %s names an endpoint that is not https", issuer)
		}
	}
	for _, e := range []string{md.RegistrationEndpoint, md.RevocationEndpoint} {
		if e != "" && !strings.HasPrefix(e, "https://") {
			return nil, fmt.Errorf("authorization server %s names an endpoint that is not https", issuer)
		}
	}
	if !slices.Contains(md.CodeChallengeMethodsSupported, "S256") {
		return nil, fmt.Errorf("authorization server %s does not offer PKCE with S256, which a sign-in needs", issuer)
	}
	return &md, nil
}

func getJSON(ctx context.Context, c *http.Client, u string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	res, err := c.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", u, res.StatusCode)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("%s: not JSON metadata", u)
	}
	return nil
}

// Client is the identity a sign-in presents. Secret is empty for a public
// client; it is kept by the caller's store and never sent into a run.
type Client struct {
	ID     string `json:"clientId"`
	Secret string `json:"-"`
	// Kind is how it was obtained: ClientOwn, ClientCIMD or ClientDCR.
	Kind string `json:"kind"`
	// AuthMethod is how it authenticates at the token endpoint: AuthNone,
	// AuthSecretBasic or AuthSecretPost.
	AuthMethod string `json:"authMethod"`
}

// Token endpoint authentication methods (RFC 7591 §2).
const (
	AuthNone        = "none"
	AuthSecretBasic = "client_secret_basic"
	AuthSecretPost  = "client_secret_post"
)

// The kinds of Client.
const (
	ClientOwn  = "own"  // the person's own, made at the vendor (D23)
	ClientCIMD = "cimd" // Agentry's Client ID Metadata Document
	ClientDCR  = "dcr"  // dynamically registered for this connection
)

// Token is a token endpoint's answer.
type Token struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time // zero when the vendor did not say
	Scopes       []string  // as granted, when the vendor said
	// IDToken is the OpenID Connect ID token, when the vendor sent one
	// (a sign-in that asked for openid); "" otherwise.
	IDToken string
}

// Request is one authorization request, kept until its redirect returns.
// A client whose redirect returns to another process (the registry's HTTPS
// callback, answered after the start's request has ended) keeps the state
// and the verifier itself and rebuilds the request with ResumeRequest.
type Request struct {
	Server      ServerMetadata
	Client      Client
	RedirectURI string
	Resource    string
	Scopes      []string
	State       string
	// Extra are further authorization parameters the consent address
	// carries, such as Google's access_type=offline and prompt=consent,
	// without which a web client is given no refresh token. They never
	// replace one the request sets itself.
	Extra    url.Values
	verifier string
}

// NewRequest makes a request with a fresh state and PKCE verifier.
func NewRequest(md ServerMetadata, client Client, redirectURI, resource string, scopes []string) (*Request, error) {
	state, err := randomString(24)
	if err != nil {
		return nil, err
	}
	verifier, err := randomString(48)
	if err != nil {
		return nil, err
	}
	return &Request{Server: md, Client: client, RedirectURI: redirectURI, Resource: resource, Scopes: scopes, State: state, verifier: verifier}, nil
}

// ResumeRequest rebuilds a request from the state and the PKCE verifier its
// client kept (agentry-cli#715): the registry seals the verifier between
// the consent's start and its callback, which arrive as two requests.
func ResumeRequest(md ServerMetadata, client Client, redirectURI, resource string, scopes []string, state, verifier string) *Request {
	return &Request{Server: md, Client: client, RedirectURI: redirectURI, Resource: resource, Scopes: scopes, State: state, verifier: verifier}
}

// Verifier is the request's PKCE verifier, for a client that must keep it
// (sealed) until the redirect returns. It is a secret of the sign-in:
// never logged, never sent but to the token endpoint.
func (r *Request) Verifier() string { return r.verifier }

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// URL is the consent address to open in the person's browser.
func (r *Request) URL() string {
	sum := sha256.Sum256([]byte(r.verifier))
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", r.Client.ID)
	q.Set("redirect_uri", r.RedirectURI)
	q.Set("state", r.State)
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
	q.Set("code_challenge_method", "S256")
	if r.Resource != "" {
		q.Set("resource", r.Resource)
	}
	if len(r.Scopes) > 0 {
		q.Set("scope", strings.Join(r.Scopes, " "))
	}
	for k, vs := range r.Extra {
		if _, set := q[k]; set {
			continue
		}
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	sep := "?"
	if strings.Contains(r.Server.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	return r.Server.AuthorizationEndpoint + sep + q.Encode()
}

// ErrStateMismatch is a redirect that is not the answer to this request.
var ErrStateMismatch = errors.New("the redirect is not the answer to this sign-in (its state does not match)")

// Callback reads the redirect the vendor sent the browser to: the code, if
// the state is this request's and the issuer is the one asked (RFC 9207).
func (r *Request) Callback(u *url.URL) (string, error) {
	q := u.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(r.State)) != 1 {
		return "", ErrStateMismatch
	}
	iss := q.Get("iss")
	if iss == "" && r.Server.ISSParameterSupported {
		return "", errors.New("the redirect names no issuer, which this authorization server promises to: refused")
	}
	if iss != "" && strings.TrimRight(iss, "/") != strings.TrimRight(r.Server.Issuer, "/") {
		return "", fmt.Errorf("the redirect came from another issuer (%q, not %s): refused", iss, r.Server.Issuer)
	}
	if e := q.Get("error"); e != "" {
		return "", &OAuthError{Status: http.StatusFound, Code: safeCode(e), Description: trimDescription(q.Get("error_description"), nil)}
	}
	code := q.Get("code")
	if code == "" {
		return "", errors.New("the redirect carries no authorization code")
	}
	return code, nil
}

// Exchange trades the code for tokens.
func Exchange(ctx context.Context, c *http.Client, r *Request, code string) (*Token, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", r.RedirectURI)
	form.Set("code_verifier", r.verifier)
	if r.Resource != "" {
		form.Set("resource", r.Resource)
	}
	return tokenRequest(ctx, c, r.Server.TokenEndpoint, r.Client, form)
}

// Refresh trades a refresh token for a new access token (and, from a
// vendor that rotates them, a new refresh token).
func Refresh(ctx context.Context, c *http.Client, tokenEndpoint string, client Client, refreshToken, resource string) (*Token, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	if resource != "" {
		form.Set("resource", resource)
	}
	return tokenRequest(ctx, c, tokenEndpoint, client, form)
}

func tokenRequest(ctx context.Context, c *http.Client, endpoint string, client Client, form url.Values) (*Token, error) {
	res, err := postForm(ctx, c, endpoint, client, form)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("read the vendor's token answer: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		return nil, oauthError(res.StatusCode, raw, form, client.Secret)
	}
	var out struct {
		AccessToken  string          `json:"access_token"`
		TokenType    string          `json:"token_type"`
		ExpiresIn    json.RawMessage `json:"expires_in"`
		RefreshToken string          `json:"refresh_token"`
		Scope        string          `json:"scope"`
		IDToken      string          `json:"id_token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.AccessToken == "" {
		return nil, errors.New("the vendor's token answer carries no access token")
	}
	if !strings.EqualFold(out.TokenType, "bearer") {
		return nil, fmt.Errorf("the vendor issued a %q token; only a bearer token can be sent", out.TokenType)
	}
	t := &Token{AccessToken: out.AccessToken, RefreshToken: out.RefreshToken, Scopes: strings.Fields(out.Scope), IDToken: out.IDToken}
	if secs, ok := expiresIn(out.ExpiresIn); ok && secs > 0 {
		t.ExpiresAt = time.Now().Add(time.Duration(secs) * time.Second)
	}
	return t, nil
}

// expiresIn reads expires_in as a number or, as some vendors send it, a
// string of one.
func expiresIn(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		v, err := n.Int64()
		return v, err == nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		v, err := strconv.ParseInt(s, 10, 64)
		return v, err == nil
	}
	return 0, false
}

// Revoke asks the vendor to revoke a token (RFC 7009). An endpoint of ""
// is a vendor that offers none, which the caller records as such.
func Revoke(ctx context.Context, c *http.Client, endpoint string, client Client, token, hint string) error {
	form := url.Values{}
	form.Set("token", token)
	if hint != "" {
		form.Set("token_type_hint", hint)
	}
	res, err := postForm(ctx, c, endpoint, client, form)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if res.StatusCode != http.StatusOK {
		return oauthError(res.StatusCode, raw, form, client.Secret)
	}
	return nil
}

func postForm(ctx context.Context, c *http.Client, endpoint string, client Client, form url.Values) (*http.Response, error) {
	switch client.AuthMethod {
	case AuthSecretBasic:
	case AuthSecretPost:
		form.Set("client_id", client.ID)
		form.Set("client_secret", client.Secret)
	default:
		form.Set("client_id", client.ID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if client.AuthMethod == AuthSecretBasic {
		req.SetBasicAuth(url.QueryEscape(client.ID), url.QueryEscape(client.Secret))
	}
	res, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach the vendor's token endpoint: %w", err)
	}
	return res, nil
}

// oauthError builds the error from a refusal, keeping its code and a
// trimmed description and dropping everything else in the body. A value
// the request carried (a refresh token, a client secret, a code, a
// verifier) that the vendor echoes in its description is masked: the
// description reaches the terminal and a broken connection's record. also
// names values sent outside the form, such as a client secret in a basic
// Authorization header.
func oauthError(status int, raw []byte, sent url.Values, also ...string) error {
	var body struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &body)
	var mask []string
	for _, k := range []string{"refresh_token", "client_secret", "code", "code_verifier", "token"} {
		if v := sent.Get(k); v != "" {
			mask = append(mask, v)
		}
	}
	for _, v := range also {
		if v != "" {
			mask = append(mask, v)
		}
	}
	return &OAuthError{Status: status, Code: safeCode(body.Error), Description: trimDescription(body.Description, mask)}
}

// safeCode keeps an error code to the RFC 6749 character set.
func safeCode(s string) string {
	if len(s) > 64 {
		return ""
	}
	for _, r := range s {
		if !codeChar(r) {
			return ""
		}
	}
	return s
}

func codeChar(r rune) bool {
	switch {
	case r == '_', r == '-', r == '.':
		return true
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return false
}

// trimDescription keeps a description short and printable, with every
// value in mask replaced.
func trimDescription(s string, mask []string) string {
	for _, m := range mask {
		s = strings.ReplaceAll(s, m, "[redacted]")
	}
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "…"
	}
	return strings.TrimSpace(s)
}
