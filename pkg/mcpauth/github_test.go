package mcpauth_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/mcpauth/mcpauthtest"
)

// The device flow: the code is shown with GitHub's own page, the token
// endpoint is polled at GitHub's interval (five seconds more after
// slow_down) until the person approves, no secret is ever sent, and the
// account is named. A GitHub App's sign-in asks no scope; the OAuth App's
// asks its one.
func TestTheGitHubDeviceFlowSignsInWithoutASecret(t *testing.T) {
	v := mcpauthtest.NewGitHub(t)
	v.Pending, v.SlowDown = 2, true
	flow := v.Flow()
	var page, code string
	flow.Present = func(p, c string) { page, code = p, c }
	got, err := flow.SignIn(context.Background(), mcpauthtest.GitHubClientID, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Login != "octo-fixture" || !strings.HasPrefix(got.Tokens.AccessToken, "ghu_") || !strings.HasPrefix(got.Tokens.RefreshToken, "ghr_") {
		t.Fatalf("signed in %+v", got)
	}
	if d := time.Until(got.Tokens.ExpiresAt); d < 7*time.Hour || d > 8*time.Hour {
		t.Fatalf("access token expires in %s", d)
	}
	if page != v.URL+"/login/device" || code != "WDJB-MJHT" {
		t.Fatalf("shown %q %q", page, code)
	}
	if len(v.Waits) != 3 || v.Waits[0] != 5*time.Second || v.Waits[1] != 10*time.Second || v.Waits[2] != 10*time.Second {
		t.Fatalf("waits %v", v.Waits)
	}
	if v.DeviceRequests[0].Has("scope") {
		t.Fatalf("a GitHub App's device flow asked a scope: %v", v.DeviceRequests[0])
	}
	for _, f := range v.TokenRequests {
		if f.Has("client_secret") {
			t.Fatalf("a secret was sent: %v", f)
		}
	}

	v2 := mcpauthtest.NewGitHub(t)
	if _, err := v2.Flow().SignIn(context.Background(), mcpauthtest.GitHubClientID, mcpauth.GitHubOAuthScope); err != nil {
		t.Fatal(err)
	}
	if v2.DeviceRequests[0].Get("scope") != "read:user" {
		t.Fatalf("the OAuth App's scope: %v", v2.DeviceRequests[0])
	}
}

// A declined or expired code ends the sign-in with nothing issued; a token
// that does not expire is revoked and refused; a page other than GitHub's
// is never shown.
func TestAGitHubSignInThatCannotBeKeptKeepsNothing(t *testing.T) {
	ctx := context.Background()
	v := mcpauthtest.NewGitHub(t)
	v.Declined = true
	if _, err := v.Flow().SignIn(ctx, mcpauthtest.GitHubClientID, ""); !errors.Is(err, mcpauth.ErrGitHubDeclined) {
		t.Fatalf("declined: %v", err)
	}
	v = mcpauthtest.NewGitHub(t)
	v.Expired = true
	if _, err := v.Flow().SignIn(ctx, mcpauthtest.GitHubClientID, ""); !errors.Is(err, mcpauth.ErrGitHubExpired) {
		t.Fatalf("expired: %v", err)
	}
	v = mcpauthtest.NewGitHub(t)
	v.NoExpiry = true
	_, err := v.Flow().SignIn(ctx, mcpauthtest.GitHubClientID, mcpauth.GitHubOAuthScope)
	if err == nil || !strings.Contains(err.Error(), "does not expire") || len(v.Revoked) != 1 || v.LiveAccess(v.Revoked[0]) {
		t.Fatalf("a non-expiring token: %v, revoked %v", err, v.Revoked)
	}
	v = mcpauthtest.NewGitHub(t)
	flow := v.Flow()
	flow.DevicePage = "https://github.com/login/device"
	shown := false
	flow.Present = func(string, string) { shown = true }
	if _, err := flow.SignIn(ctx, mcpauthtest.GitHubClientID, ""); err == nil || shown {
		t.Fatalf("another page: %v, shown %v", err, shown)
	}
}

// A refresh sends no secret, rotates the pair and spends the old refresh
// token; a spent or revoked one is the grant gone. Revocation sends no
// authentication and revokes each credential for good.
func TestAGitHubGrantRefreshesAndRevokesWithoutASecret(t *testing.T) {
	ctx := context.Background()
	v := mcpauthtest.NewGitHub(t)
	flow := v.Flow()
	at, rt := v.Issue(mcpauthtest.GitHubClientID)
	got, err := flow.Refresh(ctx, mcpauthtest.GitHubClientID, rt)
	if err != nil {
		t.Fatal(err)
	}
	if got.RefreshToken == rt || v.Live(rt) || !v.Live(got.RefreshToken) {
		t.Fatalf("the refresh did not rotate: %+v", got)
	}
	if _, err := flow.Refresh(ctx, mcpauthtest.GitHubClientID, rt); !mcpauth.GitHubGrantGone(err) {
		t.Fatalf("a spent refresh token: %v", err)
	}
	if err := flow.Revoke(ctx, at, got.AccessToken, got.RefreshToken, ""); err != nil {
		t.Fatal(err)
	}
	if len(v.RevokeAuth) != 0 || len(v.Revoked) != 3 || v.Live(got.RefreshToken) || v.LiveAccess(got.AccessToken) {
		t.Fatalf("revoked %v (auth %v)", v.Revoked, v.RevokeAuth)
	}
}

// The client ids are Agentry's public ones; until the build carries
// them, a variable names one and nothing else does.
func TestAGitHubClientIDComesFromTheBuildOrItsVariable(t *testing.T) {
	t.Setenv("AGENTRY_GITHUB_APP_CLIENT_ID", "")
	if _, err := mcpauth.GitHubClientID(mcpauth.GitHubApp); err == nil || !strings.Contains(err.Error(), "carries no client id") {
		t.Fatalf("no id: %v", err)
	}
	t.Setenv("AGENTRY_GITHUB_APP_CLIENT_ID", "Iv1.fromtheoperator")
	if id, err := mcpauth.GitHubClientID(mcpauth.GitHubApp); err != nil || id != "Iv1.fromtheoperator" {
		t.Fatalf("%q %v", id, err)
	}
	if _, err := mcpauth.GitHubClientID("gitlab"); err == nil {
		t.Fatal("an unknown app")
	}
}

// GitHub's refusals reach the person without a token in them: a refused
// refresh, an account GitHub will not name, a failed revocation.
func TestGitHubErrorsCarryNoToken(t *testing.T) {
	ctx := context.Background()
	v := mcpauthtest.NewGitHub(t)
	flow := v.Flow()
	at, rt := v.Issue(mcpauthtest.GitHubClientID)
	_ = flow.Revoke(ctx, at, rt)
	var errs []error
	_, err := flow.Refresh(ctx, mcpauthtest.GitHubClientID, rt)
	errs = append(errs, err)
	_, err = flow.Login(ctx, at)
	errs = append(errs, err)
	v.Close()
	errs = append(errs, flow.Revoke(ctx, at, rt))
	for _, err := range errs {
		if err == nil {
			t.Fatal("no error")
		}
		if strings.Contains(err.Error(), at) || strings.Contains(err.Error(), rt) {
			t.Fatalf("an error carries a token: %v", err)
		}
	}
}
