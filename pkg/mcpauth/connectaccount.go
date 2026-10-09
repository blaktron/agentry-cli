package mcpauth

// Connect your account (plan model-sign-ins.md §4.6, D30–D34;
// agentry-cli#742): OpenRouter, Poe and Hugging Face each document an OAuth
// flow by which a third-party app obtains, for the person, an ordinary API
// key (OpenRouter, Poe) or an inference token (Hugging Face) billed to their
// own account. What a connect hands back is kept like any other model key;
// it is not a model sign-in, and nothing here reaches a run but the key or
// token itself, through the model key's own path. The protocols, as each
// vendor's pages state them (read 2026-10-09):
//
//   - OpenRouter (openrouter.ai/docs/guides/overview/auth/oauth): the
//     browser goes to /auth with callback_url, a PKCE S256 challenge and a
//     state it hands back unchanged; "Localhost callbacks are supported on
//     any port"; the code (single use, ten minutes) and the verifier are
//     POSTed as JSON to /api/v1/auth/keys, which answers {"key": "sk-or-…"}.
//     No client is registered and nothing can be revoked by the app.
//   - Poe (creator.poe.com/docs/external-applications/sign-in-with-poe):
//     Agentry's public client (created by hand on poe.com/api/clients, M8),
//     PKCE S256 mandatory, the one scope apikey:create, a 127.0.0.1
//     redirect; the form-encoded exchange at api.poe.com/token answers
//     api_key and api_key_expires_in (seconds, or null for a key that does
//     not expire), no refresh token. The app cannot revoke the key.
//   - Hugging Face (huggingface.co/docs/hub/oauth): a public client whose id
//     is Agentry's Client ID Metadata Document, PKCE S256, loopback
//     redirects on any port, the scopes openid, profile and inference-api;
//     refresh_token is a grant its discovery lists. Its discovery names no
//     revocation endpoint (measured 2026-10-09; /oauth/revoke is a 404), so
//     a token can be revoked only where a later discovery names one.
//
// Every request goes through the caller's HTTP client, the egress Guard in
// production. No answer body is returned or logged: errors carry a status,
// an error code and a trimmed, masked description.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

// OpenRouter's endpoints.
const (
	OpenRouterAuthURL = "https://openrouter.ai/auth"
	OpenRouterKeysURL = "https://openrouter.ai/api/v1/auth/keys"
	// OpenRouterKeyPrefix starts every key OpenRouter issues.
	OpenRouterKeyPrefix = "sk-or-"
	// OpenRouterKeysPage is where a person deletes a key at OpenRouter.
	OpenRouterKeysPage = "https://openrouter.ai/settings/keys"
)

// Poe's endpoints and its one scope.
const (
	PoeIssuer       = "https://poe.com"
	PoeAuthorizeURL = PoeIssuer + "/oauth/authorize"
	PoeTokenURL     = "https://api.poe.com/token"
	PoeBalanceURL   = "https://api.poe.com/usage/current_balance"
	PoeScope        = "apikey:create"
	// PoeKeysPage is where a person deletes a key at Poe.
	PoeKeysPage = "https://poe.com/api/keys"
	// PoeClientIDEnv names a Poe client id for a build that carries none.
	PoeClientIDEnv = "AGENTRY_POE_CLIENT_ID"
)

// poeClientID is Agentry's public Poe client, which the operator creates on
// poe.com/api/clients (M8, agentry-cli#743); empty until then, when
// PoeClientIDEnv supplies one. A client id is not a secret.
var poeClientID = ""

// PoeClientID is Agentry's Poe client id, and an error naming what is
// missing when this build has none.
func PoeClientID() (string, error) {
	if id := strings.TrimSpace(os.Getenv(PoeClientIDEnv)); id != "" {
		return id, nil
	}
	if poeClientID == "" {
		return "", fmt.Errorf("this build of agentry carries no client id for Agentry's Poe app yet (agentry-cli#743); set %s to one", PoeClientIDEnv)
	}
	return poeClientID, nil
}

// Hugging Face's issuer, Agentry's client for it, and what a connect asks.
const (
	HuggingFaceIssuer = "https://huggingface.co"
	// HuggingFaceClientMetadataURL is Agentry's Client ID Metadata Document
	// for Hugging Face, served by the site (agentry-web#1331): a public
	// native client with loopback redirects on 127.0.0.1 and localhost.
	HuggingFaceClientMetadataURL = "https://secureagentry.net/oauth/clients/agentry-huggingface.json"
	// HuggingFaceClientIDEnv names another client id (a public app
	// registered at huggingface.co) in place of the document.
	HuggingFaceClientIDEnv = "AGENTRY_HF_CLIENT_ID"
	// HuggingFaceInferenceScope lets the token make inference requests to
	// Inference Providers for the person.
	HuggingFaceInferenceScope = "inference-api"
	// HuggingFaceAppsPage is where a person removes an app's access.
	HuggingFaceAppsPage = "https://huggingface.co/settings/connected-applications"
)

// HuggingFaceScopes are what a connect asks for: the inference scope, and
// the profile that names the account.
var HuggingFaceScopes = []string{"openid", "profile", HuggingFaceInferenceScope}

// HuggingFaceClientID is the client a Hugging Face connect presents: the
// variable's, else Agentry's metadata document.
func HuggingFaceClientID() string {
	if id := strings.TrimSpace(os.Getenv(HuggingFaceClientIDEnv)); id != "" {
		return id
	}
	return HuggingFaceClientMetadataURL
}

// ErrRevokeNotOffered is a vendor that offers no way for the app to revoke
// what it was given: the person revokes it at the vendor.
var ErrRevokeNotOffered = errors.New("the vendor offers apps no revocation")

// ConnectedKey is a finished connect: the key or token to keep, with what
// the person knows the account by. Key is a secret: it goes to the model
// key's keychain entry and nowhere else.
type ConnectedKey struct {
	// Key is the API key (OpenRouter, Poe) or the access token (Hugging
	// Face).
	Key string
	// ExpiresAt is when Key stops working: Poe's chosen expiry, Hugging
	// Face's token lifetime; zero when it does not expire or nothing said.
	ExpiresAt time.Time
	// RefreshToken renews a Hugging Face token; "" for a key.
	RefreshToken string
	// Label names the account ("Connected as …"); "" when the vendor said
	// nothing of it.
	Label string
	// ClientID is the client the connect presented; "" for OpenRouter,
	// which takes none.
	ClientID string
	Scopes   []string
}

// KeyConnect is how a connect meets the person: the browser, the pasted
// redirect, the wait.
type KeyConnect struct {
	// HTTP reaches the vendor; Guard{}.Client() in production.
	HTTP *http.Client
	// Present shows the consent address and, where it can, opens it.
	Present func(authURL string)
	// Pasted carries redirect addresses the person pasted back; nil where
	// none can be.
	Pasted <-chan string
	// Say tells the person something mid-connect.
	Say func(string)
	// Timeout bounds the wait on the browser; ConsentTimeout when zero.
	Timeout time.Duration
}

// await shows the request's address and waits for its redirect.
func (k KeyConnect) await(ctx context.Context, lb *Loopback, req *Request, authURL string) (string, error) {
	lb.Expect(req)
	if k.Present != nil {
		k.Present(authURL)
	}
	timeout := k.Timeout
	if timeout == 0 {
		timeout = ConsentTimeout
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	code, _, err := awaitRedirect(wctx, lb, req, k.Pasted, k.Say)
	var oe *OAuthError
	if errors.As(err, &oe) && oe.Code == "access_denied" {
		return "", errors.New("the connect was declined in the browser")
	}
	return code, err
}

// checkKey holds a vendor's key to the shape a key takes before it is kept:
// printable, no space, bounded. The words never quote it.
func checkKey(vendor, key string) error {
	if key == "" || len(key) > 1024 {
		return fmt.Errorf("%s sent no usable key", vendor)
	}
	for _, r := range key {
		if r <= 0x20 || r >= 0x7f {
			return fmt.Errorf("%s sent a key that is not in the shape a key takes", vendor)
		}
	}
	return nil
}

// OpenRouter connects an OpenRouter account: its PKCE flow mints an API
// key. AuthURL and KeysURL are OpenRouter's when empty.
type OpenRouter struct {
	KeyConnect
	AuthURL, KeysURL string
}

// Connect runs OpenRouter's flow in the person's browser and returns the
// key it minted.
func (o OpenRouter) Connect(ctx context.Context) (*ConnectedKey, error) {
	if o.HTTP == nil {
		return nil, errors.New("connect OpenRouter: no HTTP client")
	}
	lb, err := Listen()
	if err != nil {
		return nil, err
	}
	defer lb.Close()
	authURL := orDefault(o.AuthURL, OpenRouterAuthURL)
	md := ServerMetadata{Issuer: "https://openrouter.ai", AuthorizationEndpoint: authURL}
	req, err := NewRequest(md, Client{}, lb.RedirectURI, "", nil)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(req.verifier))
	q := url.Values{
		"callback_url":          {lb.RedirectURI},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"state":                 {req.State},
		"key_label":             {"Agentry"},
	}
	code, err := o.await(ctx, lb, req, authURL+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]string{"code": code, "code_verifier": req.verifier, "code_challenge_method": "S256"})
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, orDefault(o.KeysURL, OpenRouterKeysURL), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/json")
	res, err := o.HTTP.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("connect OpenRouter: reach its key endpoint: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("connect OpenRouter: %w", openRouterError(res.StatusCode, raw, code, req.verifier))
	}
	var out struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, errors.New("connect OpenRouter: its answer carries no key")
	}
	if err := checkKey("OpenRouter", out.Key); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(out.Key, OpenRouterKeyPrefix) {
		return nil, errors.New("connect OpenRouter: it sent a key that is not one of its own (sk-or-…): refused")
	}
	return &ConnectedKey{Key: out.Key}, nil
}

// openRouterError is OpenRouter's refusal, {"error":{"code","message"}},
// with what the request carried masked.
func openRouterError(status int, raw []byte, mask ...string) error {
	var body struct {
		Error struct {
			Code    json.RawMessage `json:"code"`
			Message string          `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	return &OAuthError{Status: status, Description: trimDescription(body.Error.Message, mask)}
}

// Poe connects a Poe account: Sign in with Poe mints an API key with the
// expiry the person chooses on Poe's consent screen. The endpoint fields
// are Poe's when empty.
type Poe struct {
	KeyConnect
	AuthorizeURL, TokenURL, BalanceURL string
}

// Connect runs Sign in with Poe as clientID (PoeClientID) and returns the
// key it minted.
func (p Poe) Connect(ctx context.Context, clientID string) (*ConnectedKey, error) {
	if p.HTTP == nil {
		return nil, errors.New("connect Poe: no HTTP client")
	}
	if clientID == "" {
		return nil, errors.New("connect Poe: no client id")
	}
	lb, err := Listen()
	if err != nil {
		return nil, err
	}
	defer lb.Close()
	md := ServerMetadata{Issuer: PoeIssuer, AuthorizationEndpoint: orDefault(p.AuthorizeURL, PoeAuthorizeURL), TokenEndpoint: orDefault(p.TokenURL, PoeTokenURL)}
	client := Client{ID: clientID, AuthMethod: AuthNone}
	req, err := NewRequest(md, client, lb.RedirectURI, "", []string{PoeScope})
	if err != nil {
		return nil, err
	}
	code, err := p.await(ctx, lb, req, req.URL())
	if err != nil {
		return nil, err
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", req.RedirectURI)
	form.Set("code_verifier", req.verifier)
	res, err := postForm(ctx, p.HTTP, md.TokenEndpoint, client, form)
	if err != nil {
		return nil, fmt.Errorf("connect Poe: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("connect Poe: %w", oauthError(res.StatusCode, raw, form))
	}
	var out struct {
		APIKey    string          `json:"api_key"`
		ExpiresIn json.RawMessage `json:"api_key_expires_in"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, errors.New("connect Poe: its answer carries no key")
	}
	if err := checkKey("Poe", out.APIKey); err != nil {
		return nil, err
	}
	k := &ConnectedKey{Key: out.APIKey, ClientID: clientID, Scopes: []string{PoeScope}}
	if secs, ok := expiresIn(out.ExpiresIn); ok && secs > 0 {
		k.ExpiresAt = time.Now().Add(time.Duration(secs) * time.Second)
	}
	k.Label = p.account(ctx, out.APIKey)
	return k, nil
}

// account is the address Poe names for a key, "" when it names none: what
// the person knows the connect by, never a reason to refuse the key.
func (p Poe) account(ctx context.Context, key string) string {
	var out struct {
		Email string `json:"email"`
	}
	if bearerJSON(ctx, p.HTTP, orDefault(p.BalanceURL, PoeBalanceURL), key, &out) != nil {
		return ""
	}
	return label(out.Email)
}

// HuggingFace connects a Hugging Face account: an OAuth token with the
// inference-api scope, renewed with its refresh token. Issuer is Hugging
// Face's when empty; its endpoints come from its discovery document.
type HuggingFace struct {
	KeyConnect
	Issuer string
}

func (h HuggingFace) discover(ctx context.Context) (*ServerMetadata, error) {
	md, err := DiscoverIssuer(ctx, h.HTTP, orDefault(h.Issuer, HuggingFaceIssuer))
	if err != nil {
		return nil, fmt.Errorf("reach Hugging Face: %w", err)
	}
	return md, nil
}

// Connect runs Hugging Face's authorization as clientID
// (HuggingFaceClientID) and returns the token it granted. A client id
// that is an address is a metadata document, which must be served before
// Hugging Face is sent to it: it would refuse the connect in the browser.
func (h HuggingFace) Connect(ctx context.Context, clientID string) (*ConnectedKey, error) {
	if h.HTTP == nil {
		return nil, errors.New("connect Hugging Face: no HTTP client")
	}
	md, err := h.discover(ctx)
	if err != nil {
		return nil, err
	}
	client := Client{ID: clientID, Kind: ClientOwn, AuthMethod: AuthNone}
	if strings.HasPrefix(clientID, "https://") {
		if !md.CIMDSupported {
			return nil, errors.New("connect Hugging Face: its discovery no longer says it takes client metadata documents, which Agentry's connect presents")
		}
		if !CIMDServed(ctx, h.HTTP, clientID) {
			return nil, fmt.Errorf("connect Hugging Face: Agentry's client document for it (%s) is not served yet (agentry-web#1331); set %s to a public app's client id meanwhile", clientID, HuggingFaceClientIDEnv)
		}
		client.Kind = ClientCIMD
	}
	lb, err := Listen()
	if err != nil {
		return nil, err
	}
	defer lb.Close()
	req, err := NewRequest(*md, client, lb.RedirectURI, "", HuggingFaceScopes)
	if err != nil {
		return nil, err
	}
	code, err := h.await(ctx, lb, req, req.URL())
	if err != nil {
		return nil, err
	}
	tok, err := Exchange(ctx, h.HTTP, req, code)
	if err != nil {
		return nil, fmt.Errorf("connect Hugging Face: %w", err)
	}
	if len(tok.Scopes) > 0 && !slices.Contains(tok.Scopes, HuggingFaceInferenceScope) {
		return nil, errors.New("connect Hugging Face: it did not grant inference-api, without which a run cannot call Inference Providers; connect again and allow it")
	}
	k := &ConnectedKey{Key: tok.AccessToken, ExpiresAt: tok.ExpiresAt, RefreshToken: tok.RefreshToken, ClientID: clientID, Scopes: tok.Scopes}
	if sameHost(md.UserinfoEndpoint, md.Issuer) {
		var who struct {
			PreferredUsername string `json:"preferred_username"`
			Name              string `json:"name"`
		}
		if bearerJSON(ctx, h.HTTP, md.UserinfoEndpoint, tok.AccessToken, &who) == nil {
			k.Label = label(orDefault(who.PreferredUsername, who.Name))
		}
	}
	return k, nil
}

// Refresh trades a Hugging Face refresh token for a new access token (and,
// should Hugging Face rotate it, a new refresh token).
func (h HuggingFace) Refresh(ctx context.Context, clientID, refreshToken string) (*Token, error) {
	md, err := h.discover(ctx)
	if err != nil {
		return nil, err
	}
	return Refresh(ctx, h.HTTP, md.TokenEndpoint, Client{ID: clientID, AuthMethod: AuthNone}, refreshToken, "")
}

// Revoke revokes a Hugging Face refresh token where its discovery names a
// revocation endpoint on Hugging Face's own host, ErrRevokeNotOffered
// where it names none (as of 2026-10-09).
func (h HuggingFace) Revoke(ctx context.Context, clientID, refreshToken string) error {
	md, err := h.discover(ctx)
	if err != nil {
		return err
	}
	if md.RevocationEndpoint == "" {
		return ErrRevokeNotOffered
	}
	if !sameHost(md.RevocationEndpoint, md.Issuer) {
		return errors.New("revoke at Hugging Face: its discovery names a revocation endpoint on another host: refused")
	}
	return Revoke(ctx, h.HTTP, md.RevocationEndpoint, Client{ID: clientID, AuthMethod: AuthNone}, refreshToken, "refresh_token")
}

// sameHost reports whether endpoint is an https address on the issuer's own
// host: a token is sent to no other (invariant I6).
func sameHost(endpoint, issuer string) bool {
	eu, err := url.Parse(endpoint)
	iu, ierr := url.Parse(issuer)
	return endpoint != "" && err == nil && ierr == nil && eu.Scheme == "https" && eu.Host != "" && strings.EqualFold(eu.Host, iu.Host)
}

// bearerJSON GETs u with the token as a bearer and decodes the answer.
func bearerJSON(ctx context.Context, c *http.Client, u, token string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
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
	return json.Unmarshal(raw, into)
}

// label keeps an account's name short and printable for a terminal.
func label(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.ToValidUTF8(strings.TrimSpace(s), ""))
	if r := []rune(s); len(r) > 100 {
		s = string(r[:100])
	}
	return s
}
