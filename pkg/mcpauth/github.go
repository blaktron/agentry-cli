package mcpauth

// Sign in with GitHub for a Copilot plan: a person's Copilot plan pays for
// their local runs, through Copilot CLI, which GitHub documents for this use
// ("Copilot requests are made on behalf of each authenticated user, using
// their Copilot subscription", copilot-sdk docs/setup/github-oauth.md). The
// protocol is GitHub's device flow, as docs.github.com states it ("Generating
// a user access token for a GitHub App", "Refreshing user access tokens",
// "Authorizing OAuth apps", "REST API endpoints for revocation"):
//
//   - POST github.com/login/device/code with the client id (and, for an
//     OAuth App, its scope) gives a device code, a user code, the page to
//     enter it at and the polling interval; the CLI shows the code and the
//     page, opens the page, and polls github.com/login/oauth/access_token
//     until the person approves (authorization_pending, slow_down adds
//     five seconds, expired_token and access_denied end it).
//   - The client secret is never needed: not for the device flow, and not
//     for a refresh of a token the device flow issued. So no client of
//     Agentry's carries one.
//   - A GitHub App's user token (ghu_) lasts eight hours with a six-month
//     refresh token (ghr_), as an OAuth App's (gho_) does when its tokens
//     expire; a refresh returns a new pair and spends the old refresh
//     token. A grant without a refresh token (an OAuth App whose tokens do
//     not expire) is refused: a long-lived token is not kept.
//   - GitHub's grant revocation needs the client secret, which no client of
//     Agentry's holds; its credential revocation does not: POST
//     api.github.com/credentials/revoke, unauthenticated, takes ghu_, gho_
//     and ghr_ tokens, revokes each for good and tells the account's owner.
//
// The default is Agentry's GitHub App; its OAuth App is the fallback a
// sign-in may choose, because whether Copilot accepts a GitHub App's user
// token that carries no permission is not established. Nothing here reaches a
// run: the CLI is the only refresher, and a run is handed an access token
// alone. Every request goes through the caller's HTTP client, the egress
// Guard in production, to github.com and api.github.com alone.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// GitHub's endpoints for the device flow, the account and revocation.
const (
	GitHubDeviceCodeURL = "https://github.com/login/device/code"
	GitHubTokenURL      = "https://github.com/login/oauth/access_token"
	GitHubDevicePage    = "https://github.com/login/device"
	GitHubAPI           = "https://api.github.com"
)

// The apps a Copilot sign-in may go through.
const (
	// GitHubApp is Agentry's GitHub App: user tokens it is granted no
	// permission for, outside organizations' OAuth App restrictions.
	GitHubApp = "github-app"
	// GitHubOAuthApp is Agentry's OAuth App, with expiring tokens.
	GitHubOAuthApp = "oauth-app"
)

// gitHubClientIDs are Agentry's public client ids for each app; empty when
// the build carries none, in which case the variable beside each supplies one
// (a test against an app registered before the build carries it). A client id
// is not a secret.
var gitHubClientIDs = map[string]struct{ id, env string }{
	GitHubApp:      {"", "AGENTRY_GITHUB_APP_CLIENT_ID"},
	GitHubOAuthApp: {"", "AGENTRY_GITHUB_OAUTH_APP_CLIENT_ID"},
}

// GitHubAppName is how app is named to a person.
func GitHubAppName(app string) string {
	if app == GitHubOAuthApp {
		return "GitHub OAuth App"
	}
	return "GitHub App"
}

// GitHubOAuthScope is what a sign-in through the OAuth App asks for: the
// account's profile, which names it. Which scope, if any, Copilot itself
// needs of an OAuth App's token is not documented. A GitHub App's device
// flow takes no scope: its token holds the permissions the app is granted,
// none.
const GitHubOAuthScope = "read:user"

// GitHubClientID is app's public client id, and an error naming what is
// missing when this build has none.
func GitHubClientID(app string) (string, error) {
	c, ok := gitHubClientIDs[app]
	if !ok {
		return "", fmt.Errorf("no GitHub app %q (%s or %s)", app, GitHubApp, GitHubOAuthApp)
	}
	if id := strings.TrimSpace(os.Getenv(c.env)); id != "" {
		return id, nil
	}
	if c.id == "" {
		return "", fmt.Errorf("this build of agentry carries no client id for Agentry's %s; set %s to one", GitHubAppName(app), c.env)
	}
	return c.id, nil
}

// GitHub signs in, refreshes and revokes against GitHub, or a test's (the
// endpoint fields; zero is GitHub's).
type GitHub struct {
	// HTTP reaches GitHub; Guard{}.Client() in production.
	HTTP *http.Client
	// DeviceCodeURL, TokenURL, DevicePage and API are GitHub's when empty.
	DeviceCodeURL, TokenURL, DevicePage, API string
	// Present shows the person the page and the code to enter there, and
	// opens the page where it can.
	Present func(page, code string)
	// Sleep waits between polls; a context-aware time.Sleep when nil.
	Sleep func(context.Context, time.Duration) error
	// Now is the clock for expiries; time.Now when nil.
	Now func() time.Time
}

func (g GitHub) deviceCodeURL() string { return orDefault(g.DeviceCodeURL, GitHubDeviceCodeURL) }
func (g GitHub) tokenURL() string      { return orDefault(g.TokenURL, GitHubTokenURL) }
func (g GitHub) devicePage() string    { return orDefault(g.DevicePage, GitHubDevicePage) }
func (g GitHub) api() string           { return strings.TrimRight(orDefault(g.API, GitHubAPI), "/") }

func (g GitHub) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g GitHub) sleep(ctx context.Context, d time.Duration) error {
	if g.Sleep != nil {
		return g.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// GitHubTokens is a grant as GitHub issues it: an access token, its
// refresh token, and when each expires.
type GitHubTokens struct {
	AccessToken      string
	ExpiresAt        time.Time
	RefreshToken     string
	RefreshExpiresAt time.Time
	Scopes           []string
}

// GitHubSignedIn is a completed sign-in: the grant and the account's login.
type GitHubSignedIn struct {
	Tokens GitHubTokens
	Login  string
}

// ErrGitHubDeclined is a person who declined the authorization.
var ErrGitHubDeclined = errors.New("the authorization was declined on GitHub; nothing was kept")

// ErrGitHubExpired is a device code that expired before it was entered.
var ErrGitHubExpired = errors.New("the code expired before it was entered on GitHub; nothing was kept: sign in again")

// GitHubError is an error GitHub's OAuth endpoints answered.
type GitHubError struct {
	Code, Description string
}

func (e *GitHubError) Error() string {
	if e.Description != "" {
		return "GitHub: " + e.Code + ": " + e.Description
	}
	return "GitHub: " + e.Code
}

// GitHubGrantGone reports whether err is GitHub refusing a refresh because
// the grant is gone (revoked, expired, or the app's authorization removed).
func GitHubGrantGone(err error) bool {
	var ge *GitHubError
	return errors.As(err, &ge) && (ge.Code == "bad_refresh_token" || ge.Code == "unauthorized_client")
}

// gitHubTokenAnswer is the token endpoint's JSON answer.
type gitHubTokenAnswer struct {
	AccessToken           string `json:"access_token"`
	ExpiresIn             int64  `json:"expires_in"`
	RefreshToken          string `json:"refresh_token"`
	RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
	Scope                 string `json:"scope"`
	TokenType             string `json:"token_type"`
	Error                 string `json:"error"`
	ErrorDescription      string `json:"error_description"`
	Interval              int64  `json:"interval"`
}

// SignIn runs the device flow for clientID (scope "" for a GitHub App),
// and names the account. A grant without a refresh token is revoked and
// refused. The tokens are the caller's to keep: the access token is the
// one a first run may use, the refresh token is the grant.
func (g GitHub) SignIn(ctx context.Context, clientID, scope string) (*GitHubSignedIn, error) {
	if clientID == "" {
		return nil, errors.New("no GitHub client id")
	}
	form := url.Values{"client_id": {clientID}}
	if scope != "" {
		form.Set("scope", scope)
	}
	var dc struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int64  `json:"expires_in"`
		Interval        int64  `json:"interval"`
		Error           string `json:"error"`
		Description     string `json:"error_description"`
	}
	if err := g.postForm(ctx, g.deviceCodeURL(), form, &dc); err != nil {
		var se *gitHubStatusError
		if errors.As(err, &se) && se.status == http.StatusNotFound {
			return nil, fmt.Errorf("GitHub knows no app with the client id %s, or its device flow is off; nothing was kept", clientID)
		}
		return nil, err
	}
	if dc.Error != "" {
		return nil, &GitHubError{Code: dc.Error, Description: dc.Description}
	}
	if dc.DeviceCode == "" || dc.UserCode == "" {
		return nil, errors.New("GitHub answered the device-code request without a code")
	}
	// The page shown and opened is GitHub's own, whatever the answer says.
	if strings.TrimRight(dc.VerificationURI, "/") != g.devicePage() {
		return nil, fmt.Errorf("GitHub named %q as the page to enter the code at, not %s; nothing was opened", dc.VerificationURI, g.devicePage())
	}
	if g.Present != nil {
		g.Present(g.devicePage(), dc.UserCode)
	}
	interval := time.Duration(max(dc.Interval, 5)) * time.Second
	life := time.Duration(dc.ExpiresIn) * time.Second
	if life <= 0 {
		life = 15 * time.Minute
	}
	deadline := g.now().Add(life)
	poll := url.Values{"client_id": {clientID}, "device_code": {dc.DeviceCode}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}}
	for {
		if err := g.sleep(ctx, interval); err != nil {
			return nil, err
		}
		if g.now().After(deadline) {
			return nil, ErrGitHubExpired
		}
		var a gitHubTokenAnswer
		if err := g.postForm(ctx, g.tokenURL(), poll, &a); err != nil {
			return nil, err
		}
		switch a.Error {
		case "":
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			if a.Interval > 0 {
				interval = time.Duration(a.Interval) * time.Second
			}
			continue
		case "expired_token":
			return nil, ErrGitHubExpired
		case "access_denied":
			return nil, ErrGitHubDeclined
		default:
			return nil, &GitHubError{Code: a.Error, Description: a.ErrorDescription}
		}
		t, err := g.tokens(a)
		if err != nil {
			return nil, err
		}
		if t.RefreshToken == "" {
			// A token that never expires is not kept: revoke it.
			_ = g.Revoke(context.WithoutCancel(ctx), t.AccessToken)
			return nil, errors.New("GitHub issued a token that does not expire, and a model sign-in keeps only an expiring grant: the app must have expiring user tokens on; nothing was kept and the token was revoked")
		}
		login, err := g.Login(ctx, t.AccessToken)
		if err != nil {
			_ = g.Revoke(context.WithoutCancel(ctx), t.AccessToken, t.RefreshToken)
			return nil, fmt.Errorf("name the GitHub account: %w (nothing was kept; the grant was revoked)", err)
		}
		return &GitHubSignedIn{Tokens: t, Login: login}, nil
	}
}

// Refresh trades refreshToken for a new pair, through clientID with no
// secret (the device flow issued it).
func (g GitHub) Refresh(ctx context.Context, clientID, refreshToken string) (GitHubTokens, error) {
	var a gitHubTokenAnswer
	if err := g.postForm(ctx, g.tokenURL(), url.Values{
		"client_id": {clientID}, "grant_type": {"refresh_token"}, "refresh_token": {refreshToken},
	}, &a); err != nil {
		return GitHubTokens{}, err
	}
	if a.Error != "" {
		return GitHubTokens{}, &GitHubError{Code: a.Error, Description: a.ErrorDescription}
	}
	return g.tokens(a)
}

func (g GitHub) tokens(a gitHubTokenAnswer) (GitHubTokens, error) {
	if a.AccessToken == "" {
		return GitHubTokens{}, errors.New("GitHub answered without an access token")
	}
	if !strings.EqualFold(a.TokenType, "bearer") && a.TokenType != "" {
		return GitHubTokens{}, fmt.Errorf("GitHub issued a %q token, not a bearer token", a.TokenType)
	}
	now := g.now()
	t := GitHubTokens{AccessToken: a.AccessToken, RefreshToken: a.RefreshToken}
	if a.ExpiresIn > 0 {
		t.ExpiresAt = now.Add(time.Duration(a.ExpiresIn) * time.Second)
	}
	if a.RefreshTokenExpiresIn > 0 {
		t.RefreshExpiresAt = now.Add(time.Duration(a.RefreshTokenExpiresIn) * time.Second)
	}
	t.Scopes = strings.FieldsFunc(a.Scope, func(r rune) bool { return r == ',' || r == ' ' })
	return t, nil
}

// Login is the account accessToken is for: GitHub's /user, its login.
func (g GitHub) Login(ctx context.Context, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.api()+"/user", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	var u struct {
		Login string `json:"login"`
	}
	if err := g.do(req, http.StatusOK, &u); err != nil {
		return "", err
	}
	if u.Login == "" {
		return "", errors.New("GitHub named no account")
	}
	return u.Login, nil
}

// Revoke revokes each token for good through GitHub's credential
// revocation, which takes no authentication and no client secret (an
// authenticated call is refused there). GitHub tells the account's owner.
// Empty tokens are skipped; none at all is nothing to do.
func (g GitHub) Revoke(ctx context.Context, tokens ...string) error {
	var creds []string
	for _, t := range tokens {
		if t != "" {
			creds = append(creds, t)
		}
	}
	if len(creds) == 0 {
		return nil
	}
	body, err := json.Marshal(map[string]any{"credentials": creds})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.api()+"/credentials/revoke", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return g.do(req, http.StatusAccepted, nil)
}

// postForm posts form to u and decodes GitHub's JSON answer into v: the
// OAuth endpoints answer 200 with an error field, as the device flow
// documents.
func (g GitHub) postForm(ctx context.Context, u string, form url.Values, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	return g.do(req, http.StatusOK, v)
}

// do sends req and decodes a want-status answer into v (nil: no body).
// GitHub's words on any other status are kept short; a token is never in
// them, since GitHub never echoes one.
func (g GitHub) do(req *http.Request, want int, v any) error {
	hc := g.HTTP
	if hc == nil {
		hc = Guard{}.Client()
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("reach %s: %w", req.URL.Host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read %s's answer: %w", req.URL.Host, err)
	}
	if resp.StatusCode != want {
		var e struct {
			Message string `json:"message"`
			Error   string `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		msg := e.Message
		if msg == "" {
			msg = e.Error
		}
		if msg != "" && strings.Contains(resp.Status, msg) {
			msg = ""
		}
		return &gitHubStatusError{status: resp.StatusCode,
			msg: strings.TrimSpace(fmt.Sprintf("%s %s answered %s %s", req.Method, req.URL.Host+req.URL.Path, resp.Status, firstLine(msg)))}
	}
	if v == nil {
		return nil
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s's answer: %w", req.URL.Host, err)
	}
	return nil
}

// gitHubStatusError is an answer of another status than the one wanted.
type gitHubStatusError struct {
	status int
	msg    string
}

func (e *gitHubStatusError) Error() string { return e.msg }

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "…"
	}
	return s
}
