package mcpauth

// Sign in with ChatGPT, plan usage (plan model-sign-ins.md §4.3, D8–D16;
// agentry-cli#739): a person's ChatGPT plan pays for their local runs.
// The protocol is OpenAI's, as its pages under
// developers.openai.com/siwc/token-sharing-open-source state it (read
// 2026-10-09: sign-in, profiles-and-sessions, token-reference,
// errors-and-recovery, models-and-inference):
//
//   - The first authorization registers the app: client_id
//     dynamic_agent_client with agent_name_hint, and a stable per-host
//     ext_agent_host_id. The redirect carries the client id OpenAI issued
//     (oaiapp_…), which every later request uses: the code exchange, each
//     refresh, a revocation, a later sign-in on the same host.
//   - The redirect is http://127.0.0.1:<port>/auth/callback: only the port
//     may vary, and "localhost" may not stand in for the address.
//   - PKCE S256, a fresh state and a fresh nonce on every attempt; resource
//     https://api.openai.com/v1 on the authorization, the exchange and each
//     refresh.
//   - Plan usage is allowed only when the token answer's granted scopes
//     hold chatgpt.tokens.use.direct; an ID token alone authorizes nothing.
//   - Access tokens last an hour; each refresh returns a replacement
//     refresh token, so refreshes are serialized and the replacement kept
//     before the access token is used (the caller's store does both).
//   - Sign-out revokes the refresh token at the discovery document's
//     revocation_endpoint; an empty 200 is success, an invalid token too.
//
// Nothing here reaches a run: the CLI is the only refresher, and a run is
// handed an access token alone (invariant I3). Every request goes through
// the caller's HTTP client, the egress Guard in production.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// OpenAI's endpoints and constants for ChatGPT plan usage.
const (
	ChatGPTIssuer       = "https://auth.openai.com"
	ChatGPTAuthorizeURL = ChatGPTIssuer + "/api/accounts/authorize"
	ChatGPTTokenURL     = ChatGPTIssuer + "/api/accounts/oauth/token"
	ChatGPTDiscoveryURL = ChatGPTIssuer + "/.well-known/openid-configuration"
	// ChatGPTResource is the resource every request names, and the base of
	// the inference API the plan's token is good for.
	ChatGPTResource = "https://api.openai.com/v1"
	// ChatGPTCallbackPath is the loopback redirect's fixed path.
	ChatGPTCallbackPath = "/auth/callback"
	// ChatGPTDynamicClient is the client id of a first authorization, which
	// registers the app.
	ChatGPTDynamicClient = "dynamic_agent_client"
	// ChatGPTAgentName is the app's name at registration. Codex's
	// app-server sends its initialize clientInfo.name as the request
	// originator, which OpenAI asks to match it.
	ChatGPTAgentName = "Agentry"
	// ChatGPTPlanScope is the scope that allows plan usage.
	ChatGPTPlanScope = "chatgpt.tokens.use.direct"
)

// ChatGPTScopes are what a sign-in asks for.
var ChatGPTScopes = []string{"openid", "profile", "email", "offline_access", "resource.invoke", ChatGPTPlanScope}

// ChatGPT signs in, refreshes and revokes against OpenAI's authorization
// server, or a test's (the endpoint fields; zero is OpenAI's).
type ChatGPT struct {
	// HTTP reaches OpenAI; Guard{}.Client() in production.
	HTTP *http.Client
	// Issuer, AuthorizeURL, TokenURL, DiscoveryURL and APIBase are OpenAI's
	// when empty.
	Issuer, AuthorizeURL, TokenURL, DiscoveryURL, APIBase string
	// Present shows the consent address and, where it can, opens it.
	Present func(authURL string)
	// Pasted carries redirect addresses the person pasted back; nil where
	// none can be.
	Pasted <-chan string
	// Say tells the person something mid-sign-in.
	Say func(string)
	// Timeout bounds the wait on the browser; ConsentTimeout when zero.
	Timeout time.Duration
	// Now is the clock for the ID token's expiry; time.Now when nil.
	Now func() time.Time
}

func (g ChatGPT) issuer() string       { return orDefault(g.Issuer, ChatGPTIssuer) }
func (g ChatGPT) authorizeURL() string { return orDefault(g.AuthorizeURL, ChatGPTAuthorizeURL) }
func (g ChatGPT) tokenURL() string     { return orDefault(g.TokenURL, ChatGPTTokenURL) }
func (g ChatGPT) discoveryURL() string { return orDefault(g.DiscoveryURL, ChatGPTDiscoveryURL) }
func (g ChatGPT) apiBase() string {
	return strings.TrimRight(orDefault(g.APIBase, ChatGPTResource), "/")
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// ChatGPTRegistration is what this host keeps of its registration with
// OpenAI between sign-ins, sign-outs included: the host's stable id, the
// client id OpenAI issued, and the account (the ID token's subject) it was
// issued for. A first sign-in has only the host id.
type ChatGPTRegistration struct {
	HostID   string `json:"hostId"`
	ClientID string `json:"clientId,omitempty"`
	Subject  string `json:"subject,omitempty"`
}

// NewChatGPTHostID makes a host id: an opaque UUIDv4 URN, which OpenAI
// takes as ext_agent_host_id. It is not a secret and proves nothing.
func NewChatGPTHostID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// ChatGPTSignIn is a finished sign-in: the registration to keep, the
// account it is, the grant, and the models the account may use.
type ChatGPTSignIn struct {
	ClientID string
	Subject  string
	// Email is the account's address, for "Signed in as …"; "" when the ID
	// token carried none.
	Email  string
	Token  Token
	Models []string
}

// ErrChatGPTNoPlanScope is a sign-in that did not grant plan usage.
var ErrChatGPTNoPlanScope = errors.New("the sign-in did not grant ChatGPT plan usage (" + ChatGPTPlanScope + "): your plan or workspace may not allow it in other apps; use an OpenAI API key instead")

// ChatGPTNotEligibleError is OpenAI's 403 subscription_sharing_user_not_eligible:
// the account, its workspace or a policy does not allow plan usage here.
// OpenAI's advice is to say so and not to retry or sign in again.
type ChatGPTNotEligibleError struct{ Detail string }

func (e *ChatGPTNotEligibleError) Error() string {
	s := "ChatGPT says this account cannot use its plan in other apps (subscription_sharing_user_not_eligible)"
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	return s + ". Use an OpenAI API key instead (agentry setup)"
}

// SignIn runs the authorization in the person's browser and returns the
// grant. reg is this host's registration: a host id always, and the
// client id and account of an earlier sign-in, which a sign-in again
// reuses (OpenAI's reauthorization). A sign-in as another account than
// reg's is refused: that account needs its own registration (reg with the
// host id alone). The grant is checked for plan usage, and the account for
// its eligibility, before it is returned; a caller keeps nothing it got an
// error for.
func (g ChatGPT) SignIn(ctx context.Context, reg ChatGPTRegistration) (*ChatGPTSignIn, error) {
	if reg.HostID == "" {
		return nil, errors.New("sign in with ChatGPT: no host id")
	}
	if g.HTTP == nil {
		return nil, errors.New("sign in with ChatGPT: no HTTP client")
	}
	lb, err := ListenAt(ChatGPTCallbackPath)
	if err != nil {
		return nil, err
	}
	defer lb.Close()
	clientID := reg.ClientID
	if clientID == "" {
		clientID = ChatGPTDynamicClient
	}
	md := ServerMetadata{Issuer: g.issuer(), AuthorizationEndpoint: g.authorizeURL(), TokenEndpoint: g.tokenURL()}
	req, err := NewRequest(md, Client{ID: clientID, AuthMethod: AuthNone}, lb.RedirectURI, ChatGPTResource, ChatGPTScopes)
	if err != nil {
		return nil, err
	}
	nonce, err := randomString(24)
	if err != nil {
		return nil, err
	}
	req.Extra = url.Values{"nonce": {nonce}, "ext_agent_host_id": {reg.HostID}}
	if reg.ClientID == "" {
		req.Extra.Set("agent_name_hint", ChatGPTAgentName)
	}
	lb.Expect(req)
	if g.Present != nil {
		g.Present(req.URL())
	}
	timeout := g.Timeout
	if timeout == 0 {
		timeout = ConsentTimeout
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	code, query, err := awaitRedirect(wctx, lb, req, g.Pasted, g.Say)
	if err != nil {
		var oe *OAuthError
		if errors.As(err, &oe) && oe.Code == "access_denied" {
			return nil, errors.New("the sign-in was declined in the browser")
		}
		return nil, err
	}
	issued := query.Get("client_id")
	switch {
	case reg.ClientID == "" && issued == "":
		return nil, errors.New("ChatGPT registered no client for this sign-in (its redirect carries no client_id)")
	case reg.ClientID != "" && issued != "" && issued != reg.ClientID:
		return nil, errors.New("ChatGPT answered for another registration than the one this sign-in asked for: refused")
	case reg.ClientID != "":
		issued = reg.ClientID
	}
	// The exchange presents the issued client, not dynamic_agent_client.
	ex := *req
	ex.Client = Client{ID: issued, AuthMethod: AuthNone}
	tok, err := Exchange(ctx, g.HTTP, &ex, code)
	if err != nil {
		return nil, fmt.Errorf("sign in with ChatGPT: %w", err)
	}
	// From here a grant exists at OpenAI: anything that refuses it revokes it.
	refuse := func(err error) (*ChatGPTSignIn, error) {
		if tok.RefreshToken != "" {
			_ = g.Revoke(context.WithoutCancel(ctx), issued, tok.RefreshToken)
		}
		return nil, err
	}
	if !slices.Contains(tok.Scopes, ChatGPTPlanScope) {
		return refuse(ErrChatGPTNoPlanScope)
	}
	if tok.RefreshToken == "" {
		return refuse(errors.New("ChatGPT issued no refresh token (offline_access was not granted), so no run could renew it"))
	}
	claims, err := g.idClaims(tok.IDToken, issued, nonce)
	if err != nil {
		return refuse(err)
	}
	if reg.Subject != "" && claims.Subject != reg.Subject {
		return refuse(errors.New("you signed in to another ChatGPT account than the one this computer is registered for; to use that account, remove the sign-in and sign in as a new account (agentry model-signins add chatgpt --new-account)"))
	}
	models, err := g.Models(ctx, tok.AccessToken)
	if err != nil {
		return refuse(err)
	}
	return &ChatGPTSignIn{ClientID: issued, Subject: claims.Subject, Email: claims.Email, Token: *tok, Models: models}, nil
}

// awaitRedirect takes the first good answer from the loopback listener or
// the pasted addresses, with the redirect's whole query; a pasted address
// that is not this sign-in's is said and waited past.
func awaitRedirect(ctx context.Context, lb *Loopback, req *Request, pasted <-chan string, say func(string)) (string, url.Values, error) {
	for {
		select {
		case o := <-lb.result:
			return o.code, o.query, o.err
		case line, ok := <-pasted:
			if !ok {
				pasted = nil
				continue
			}
			u, err := url.Parse(strings.TrimSpace(line))
			if err != nil || u.RawQuery == "" {
				if say != nil {
					say("that is not the address the browser was sent to; paste the whole address from its address bar")
				}
				continue
			}
			code, err := req.Callback(u)
			if errors.Is(err, ErrStateMismatch) {
				if say != nil {
					say(err.Error())
				}
				continue
			}
			return code, u.Query(), err
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return "", nil, errors.New("the sign-in timed out waiting for the browser; start it again")
			}
			return "", nil, ctx.Err()
		}
	}
}

// idClaims are the ID token's claims a sign-in reads.
type idClaims struct {
	Issuer   string          `json:"iss"`
	Audience json.RawMessage `json:"aud"`
	Expiry   int64           `json:"exp"`
	Nonce    string          `json:"nonce"`
	Subject  string          `json:"sub"`
	Email    string          `json:"email"`
}

// idClaims checks the ID token's issuer, audience, expiry and nonce and
// returns its claims. Its signature is not checked: the token came straight
// from the token endpoint over TLS, which OpenID Connect Core (3.1.3.7,
// item 6) lets stand in for it, and it authorizes nothing here (the granted
// scope does); its subject only tells one account from another.
func (g ChatGPT) idClaims(raw, clientID, nonce string) (*idClaims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("ChatGPT sent no usable ID token")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, errors.New("ChatGPT's ID token is not readable")
	}
	var c idClaims
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, errors.New("ChatGPT's ID token is not readable")
	}
	now := time.Now
	if g.Now != nil {
		now = g.Now
	}
	var aud []string
	if json.Unmarshal(c.Audience, &aud) != nil {
		var one string
		_ = json.Unmarshal(c.Audience, &one)
		aud = []string{one}
	}
	switch {
	case strings.TrimRight(c.Issuer, "/") != strings.TrimRight(g.issuer(), "/"):
		return nil, fmt.Errorf("ChatGPT's ID token names another issuer (%q): refused", c.Issuer)
	case !slices.Contains(aud, clientID):
		return nil, errors.New("ChatGPT's ID token is not for this app's registration: refused")
	case c.Expiry == 0:
		return nil, errors.New("ChatGPT's ID token carries no expiry: refused")
	case time.Unix(c.Expiry, 0).Before(now()):
		return nil, errors.New("ChatGPT's ID token has expired: sign in again")
	case c.Nonce != nonce:
		return nil, errors.New("ChatGPT's ID token is not the answer to this sign-in (its nonce does not match): refused")
	case c.Subject == "":
		return nil, errors.New("ChatGPT's ID token names no account")
	}
	return &c, nil
}

// Models lists the models the account's plan may use (OpenAI: the catalog
// at /v1/models with the plan's token, those of visibility "list"). It is
// also the eligibility check: OpenAI's 403 subscription_sharing_user_not_eligible
// is a *ChatGPTNotEligibleError.
func (g ChatGPT) Models(ctx context.Context, accessToken string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.apiBase()+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	res, err := g.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach ChatGPT's model list: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	// A model catalog carries each model's metadata, so it may be larger
	// than the token answers maxBody bounds.
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxCatalog))
	if err != nil {
		return nil, fmt.Errorf("read ChatGPT's model list: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		code, msg := apiError(raw, accessToken)
		if res.StatusCode == http.StatusForbidden && code == "subscription_sharing_user_not_eligible" {
			return nil, &ChatGPTNotEligibleError{Detail: msg}
		}
		detail := ""
		if code != "" {
			detail = " (" + code + ")"
		}
		return nil, fmt.Errorf("ChatGPT's model list answered %d%s", res.StatusCode, detail)
	}
	var list struct {
		Models []catalogModel `json:"models"`
		Data   []catalogModel `json:"data"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, errors.New("ChatGPT's model list is not readable")
	}
	var out []string
	for _, m := range append(list.Models, list.Data...) {
		id := m.Slug
		if id == "" {
			id = m.ID
		}
		if id != "" && (m.Visibility == "" || m.Visibility == "list") && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// maxCatalog bounds the model list read.
const maxCatalog = 4 << 20

type catalogModel struct {
	Slug       string `json:"slug"`
	ID         string `json:"id"`
	Visibility string `json:"visibility"`
}

// apiError reads an OpenAI error body's code and message, the message
// trimmed and with the token masked.
func apiError(raw []byte, token string) (code, msg string) {
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(raw, &body)
	msg = body.Error.Message
	if msg == "" {
		msg = body.Detail
	}
	return safeCode(body.Error.Code), trimDescription(msg, []string{token})
}

// Refresh trades the refresh token for a new access token and the
// replacement refresh token, as the issued client.
func (g ChatGPT) Refresh(ctx context.Context, clientID, refreshToken string) (*Token, error) {
	if clientID == "" {
		return nil, errors.New("the ChatGPT sign-in keeps no client id; sign in again")
	}
	return Refresh(ctx, g.HTTP, g.tokenURL(), Client{ID: clientID, AuthMethod: AuthNone}, refreshToken, ChatGPTResource)
}

// ChatGPTGrantGone reports whether a refresh error means the grant is gone
// and the person must sign in again (OpenAI's errors-and-recovery page:
// these codes, plus the OAuth ones GrantRevoked knows).
func ChatGPTGrantGone(err error) bool {
	if GrantRevoked(err) {
		return true
	}
	var oe *OAuthError
	if !errors.As(err, &oe) {
		return false
	}
	switch oe.Code {
	case "invalid_refresh_token", "token_expired", "refresh_token_expired", "refresh_token_invalidated", "refresh_token_reused":
		return true
	}
	return false
}

// sameVendor reports whether host is the issuer's host or under the
// issuer's registrable domain (its last two labels; an address only as
// itself).
func sameVendor(host, issuer string) bool {
	iu, err := url.Parse(issuer)
	if err != nil {
		return false
	}
	ih := strings.ToLower(iu.Hostname())
	host = strings.ToLower(host)
	if host == ih {
		return true
	}
	labels := strings.Split(ih, ".")
	if len(labels) < 2 || net.ParseIP(ih) != nil {
		return false
	}
	return strings.HasSuffix(host, "."+strings.Join(labels[len(labels)-2:], "."))
}

// revokeTries and revokeBackoff bound Revoke's retries of a network failure
// or a 5xx, which OpenAI asks for while the refresh token is still at hand.
var (
	revokeTries   = 3
	revokeBackoff = time.Second
)

// Revoke revokes the refresh token at the revocation endpoint OpenAI's
// discovery document names, as the issued client. An empty 200 is success,
// for a token already invalid too.
func (g ChatGPT) Revoke(ctx context.Context, clientID, refreshToken string) error {
	var doc struct {
		RevocationEndpoint string `json:"revocation_endpoint"`
	}
	if err := getJSON(ctx, g.HTTP, g.discoveryURL(), &doc); err != nil {
		return fmt.Errorf("find ChatGPT's revocation endpoint: %w", err)
	}
	u, err := url.Parse(doc.RevocationEndpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("ChatGPT's discovery document names no usable revocation endpoint")
	}
	// The refresh token goes to OpenAI and nowhere else (invariant I6): the
	// endpoint must be the issuer's host or under its domain.
	if !sameVendor(u.Hostname(), g.issuer()) {
		return fmt.Errorf("ChatGPT's discovery document names a revocation endpoint off OpenAI's domain (%s): refused", u.Hostname())
	}
	var last error
	for try := 0; try < revokeTries; try++ {
		if try > 0 {
			select {
			case <-time.After(revokeBackoff * time.Duration(1<<(try-1))):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		last = Revoke(ctx, g.HTTP, doc.RevocationEndpoint, Client{ID: clientID, AuthMethod: AuthNone}, refreshToken, "refresh_token")
		var oe *OAuthError
		if last == nil || (errors.As(last, &oe) && oe.Status < 500) {
			return last
		}
	}
	return last
}
