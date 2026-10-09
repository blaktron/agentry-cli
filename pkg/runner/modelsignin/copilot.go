package modelsignin

// The Copilot sign-in (plan model-sign-ins.md §4.5, D26; agentry-cli#741):
// GitHub's device flow is mcpauth.GitHub; this keeps what it hands back,
// and refreshes and revokes through it. SignInCopilot is the one entry
// point: the CLI's, and the desktop's when it signs in (M9).

import (
	"context"
	"errors"
	"fmt"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
)

// CopilotVendor refreshes and revokes Copilot sign-ins through flow, as
// the client each sign-in's record names.
type CopilotVendor struct{ Flow mcpauth.GitHub }

// The production Copilot vendor, on GitHub through the egress guard.
func init() {
	RegisterVendor(Copilot, CopilotVendor{Flow: mcpauth.GitHub{HTTP: mcpauth.Guard{}.Client()}})
}

// Refresh implements Vendor. GitHub refusing the refresh token is
// ErrRevoked, so the sign-in is marked broken.
func (v CopilotVendor) Refresh(ctx context.Context, rec Record, refreshToken string) (Refreshed, error) {
	t, err := v.Flow.Refresh(ctx, rec.ClientID, refreshToken)
	if mcpauth.GitHubGrantGone(err) {
		return Refreshed{}, fmt.Errorf("%w (%v)", ErrRevoked, err)
	}
	if err != nil {
		return Refreshed{}, err
	}
	return Refreshed{AccessToken: t.AccessToken, ExpiresAt: t.ExpiresAt, RefreshToken: t.RefreshToken}, nil
}

// Revoke implements Vendor. GitHub revokes a grant only for a caller with
// the client secret, which no client of Agentry's holds, so the grant's
// credentials go to its credential revocation instead, which accepts them
// and acts after it answers: the kept refresh token is traded once more
// for the pair now live, and the access token and both refresh tokens are
// submitted. An access token a run was handed before lapses within its
// eight hours if GitHub does not revoke it with them. When the trade
// fails, the kept refresh token is submitted alone.
func (v CopilotVendor) Revoke(ctx context.Context, rec Record, refreshToken string) error {
	live, err := v.Flow.Refresh(ctx, rec.ClientID, refreshToken)
	if err != nil {
		return v.Flow.Revoke(ctx, refreshToken)
	}
	return v.Flow.Revoke(ctx, live.AccessToken, live.RefreshToken, refreshToken)
}

// RevokeOutcome is what Remove reports of a revocation GitHub accepted: its
// answer comes before it acts and says nothing of what it revoked, so
// "revoked" would be more than it said.
func (CopilotVendor) RevokeOutcome() string { return RevokeSubmitted }

// CopilotSignedIn is a kept Copilot sign-in.
type CopilotSignedIn struct {
	Record *Record
}

// SignInCopilot signs in with GitHub through flow, by app (mcpauth.GitHubApp,
// or mcpauth.GitHubOAuthApp, the fallback), and keeps the grant, replacing
// a Copilot sign-in kept before (whose grant is revoked once the new one is
// kept). Nothing is kept from a sign-in that failed.
func (s *Store) SignInCopilot(ctx context.Context, flow mcpauth.GitHub, app string) (*CopilotSignedIn, error) {
	clientID, err := mcpauth.GitHubClientID(app)
	if err != nil {
		return nil, err
	}
	scope := ""
	if app == mcpauth.GitHubOAuthApp {
		scope = mcpauth.GitHubOAuthScope
	}
	got, err := flow.SignIn(ctx, clientID, scope)
	if err != nil {
		return nil, err
	}
	var prevToken string
	var prevRec *Record
	if old, gerr := s.Get(Copilot); gerr == nil && old.SecretIn != "" {
		if rt, rerr := s.readSecret(old, Copilot); rerr == nil {
			prevToken, prevRec = rt, old
		}
	} else if gerr != nil && !errors.Is(gerr, ErrNone) {
		_ = flow.Revoke(context.WithoutCancel(ctx), got.Tokens.AccessToken, got.Tokens.RefreshToken)
		return nil, gerr
	}
	rec, err := s.Put(Copilot, Record{Label: got.Login, Scopes: got.Tokens.Scopes, ClientID: clientID, App: app},
		Grant{RefreshToken: got.Tokens.RefreshToken})
	if err != nil {
		_ = flow.Revoke(context.WithoutCancel(ctx), got.Tokens.AccessToken, got.Tokens.RefreshToken)
		return nil, err
	}
	if prevToken != "" && prevToken != got.Tokens.RefreshToken {
		_ = CopilotVendor{Flow: flow}.Revoke(context.WithoutCancel(ctx), *prevRec, prevToken)
	}
	return &CopilotSignedIn{Record: rec}, nil
}
