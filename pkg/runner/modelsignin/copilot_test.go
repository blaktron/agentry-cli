package modelsignin

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/mcpauth/mcpauthtest"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// copilotFixture is a fake GitHub, its flow registered as the Copilot
// vendor, and Agentry's GitHub App's client id set to the fake's.
func copilotFixture(t *testing.T) (*mcpauthtest.GitHub, mcpauth.GitHub) {
	t.Helper()
	v := mcpauthtest.NewGitHub(t)
	flow := v.Flow()
	t.Setenv("AGENTRY_GITHUB_APP_CLIENT_ID", mcpauthtest.GitHubClientID)
	t.Setenv("AGENTRY_GITHUB_OAUTH_APP_CLIENT_ID", mcpauthtest.GitHubClientID)
	t.Cleanup(RegisterVendor(Copilot, CopilotVendor{Flow: flow}))
	return v, flow
}

// A Copilot sign-in keeps the refresh token in the keychain and the account
// and the app on the record; a run is handed a fresh access token through
// the session callback's delivery, with a renewal, and never the refresh
// token (I3).
func TestACopilotSignInIsKeptAndARunIsHandedItsAccessTokenAlone(t *testing.T) {
	v, flow := copilotFixture(t)
	keys := &memKeys{m: map[string]string{}}
	s := Open(t.TempDir(), keys)
	got, err := s.SignInCopilot(context.Background(), flow, mcpauth.GitHubApp)
	if err != nil {
		t.Fatal(err)
	}
	r := got.Record
	if r.Label != "octo-fixture" || r.App != mcpauth.GitHubApp || r.ClientID != mcpauthtest.GitHubClientID || r.SecretIn != secretInKeychain || r.Kind != policy.KeyKindCopilotSignIn {
		t.Fatalf("record %+v", r)
	}
	g, err := s.ForRun(context.Background(), policy.KeyKindCopilotSignIn, spec.ModelSignInCallback, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(g.Access.Token, "ghu_") || !v.LiveAccess(g.Access.Token) || g.Renew == nil {
		t.Fatalf("grant %+v", g)
	}
	a2, err := g.Renew(context.Background())
	if err != nil || a2.Token == g.Access.Token {
		t.Fatalf("renew %+v %v", a2, err)
	}
	for _, kept := range keys.m {
		if strings.Contains(kept, g.Access.Token) || strings.Contains(kept, a2.Token) {
			t.Fatal("an access token was kept")
		}
	}
}

// Signing in again replaces the kept sign-in and revokes its grant once the
// new one is kept; through the OAuth App, the record says so.
func TestASecondCopilotSignInRevokesTheFirst(t *testing.T) {
	v, flow := copilotFixture(t)
	s := Open(t.TempDir(), &memKeys{m: map[string]string{}})
	if _, err := s.SignInCopilot(context.Background(), flow, mcpauth.GitHubApp); err != nil {
		t.Fatal(err)
	}
	first, _ := s.readSecret(mustGet(t, s), Copilot)
	got, err := s.SignInCopilot(context.Background(), flow, mcpauth.GitHubOAuthApp)
	if err != nil {
		t.Fatal(err)
	}
	if got.Record.App != mcpauth.GitHubOAuthApp || v.Live(first) || !slices.Contains(v.Revoked, first) {
		t.Fatalf("record %+v; first grant live %v, revoked %v", got.Record, v.Live(first), v.Revoked)
	}
}

func mustGet(t *testing.T, s *Store) *Record {
	t.Helper()
	r, err := s.Get(Copilot)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Removing a Copilot sign-in revokes its grant at GitHub without a secret:
// the pair now live and the kept refresh token go to GitHub's credential
// revocation, and none is kept here after.
func TestRemovingACopilotSignInRevokesItsGrant(t *testing.T) {
	v, flow := copilotFixture(t)
	keys := &memKeys{m: map[string]string{}}
	s := Open(t.TempDir(), keys)
	if _, err := s.SignInCopilot(context.Background(), flow, mcpauth.GitHubApp); err != nil {
		t.Fatal(err)
	}
	kept, _ := s.readSecret(mustGet(t, s), Copilot)
	out, err := s.Remove(context.Background(), Copilot, "test")
	if err != nil || out != RevokeSubmitted {
		t.Fatalf("remove: %q %v", out, err)
	}
	if len(v.Revoked) != 3 || !slices.Contains(v.Revoked, kept) || len(v.RevokeAuth) != 0 {
		t.Fatalf("revoked %v (auth %v)", v.Revoked, v.RevokeAuth)
	}
	for _, c := range v.Revoked {
		if v.Live(c) || v.LiveAccess(c) {
			t.Fatalf("%s still live", c)
		}
	}
	if len(keys.m) != 0 {
		t.Fatalf("kept %v", keys.m)
	}
	if _, err := s.Get(Copilot); !errors.Is(err, ErrNone) {
		t.Fatalf("after remove: %v", err)
	}
}

// GitHub refusing the refresh marks the sign-in broken.
func TestARefusedCopilotRefreshMarksTheSignInBroken(t *testing.T) {
	_, flow := copilotFixture(t)
	s := Open(t.TempDir(), &memKeys{m: map[string]string{}})
	if _, err := s.Put(Copilot, Record{ClientID: mcpauthtest.GitHubClientID}, Grant{RefreshToken: "ghr_neverissued"}); err != nil {
		t.Fatal(err)
	}
	var be *BrokenError
	if _, err := s.Use(context.Background(), Copilot); !errors.As(err, &be) {
		t.Fatalf("use: %v", err)
	}
	_ = flow
	if r := mustGet(t, s); r.Broken == "" {
		t.Fatalf("record %+v", r)
	}
}
