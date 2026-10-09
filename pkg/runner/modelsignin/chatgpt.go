package modelsignin

// The ChatGPT sign-in (plan model-sign-ins.md §4.3, D8–D16;
// agentry-cli#739): OpenAI's flow is mcpauth.ChatGPT; this keeps what it
// hands back, and refreshes and revokes through it. The CLI and the
// desktop both sign in through SignInChatGPT.

import (
	"context"
	"errors"
	"fmt"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
)

// ChatGPTVendor refreshes and revokes ChatGPT sign-ins through flow, as
// the client each sign-in's record names.
type ChatGPTVendor struct{ Flow mcpauth.ChatGPT }

// Refresh implements Vendor. A refusal that means the grant is gone (OpenAI's
// codes for it) is ErrRevoked, so the sign-in is marked broken.
func (v ChatGPTVendor) Refresh(ctx context.Context, rec Record, refreshToken string) (Refreshed, error) {
	tok, err := v.Flow.Refresh(ctx, rec.ClientID, refreshToken)
	if mcpauth.ChatGPTGrantGone(err) {
		return Refreshed{}, fmt.Errorf("%w (%v)", ErrRevoked, err)
	}
	if err != nil {
		return Refreshed{}, err
	}
	return Refreshed{AccessToken: tok.AccessToken, ExpiresAt: tok.ExpiresAt, RefreshToken: tok.RefreshToken}, nil
}

// Revoke implements Vendor.
func (v ChatGPTVendor) Revoke(ctx context.Context, rec Record, refreshToken string) error {
	return v.Flow.Revoke(ctx, rec.ClientID, refreshToken)
}

// ChatGPTSignedIn is a kept ChatGPT sign-in and the models its plan offers.
type ChatGPTSignedIn struct {
	Record *Record
	Models []string
}

// SignInChatGPT signs in with ChatGPT through flow and keeps the grant,
// replacing a ChatGPT sign-in kept before (its grant is revoked once the
// new one is kept). The host's registration is reused, so OpenAI sees the
// same app on this computer; newAccount drops it first, so the sign-in
// registers again for another account. Nothing is kept from a sign-in that
// failed: an ineligible account (*mcpauth.ChatGPTNotEligibleError) or a
// grant without plan usage (mcpauth.ErrChatGPTNoPlanScope) is revoked at
// OpenAI by the flow and reported.
func (s *Store) SignInChatGPT(ctx context.Context, flow mcpauth.ChatGPT, newAccount bool) (*ChatGPTSignedIn, error) {
	host, err := s.HostID(mcpauth.NewChatGPTHostID)
	if err != nil {
		return nil, err
	}
	if newAccount {
		if err := s.DropRegistration(ChatGPT); err != nil {
			return nil, err
		}
	}
	reg, err := s.Registration(ChatGPT)
	if err != nil {
		return nil, err
	}
	want := mcpauth.ChatGPTRegistration{HostID: host}
	if reg != nil {
		want.ClientID, want.Subject = reg.ClientID, reg.Subject
	}
	got, err := flow.SignIn(ctx, want)
	if err != nil {
		return nil, err
	}
	// The registration is kept first: it outlives the sign-in, and a later
	// one on this host presents it.
	if err := s.PutRegistration(ChatGPT, Registration{ClientID: got.ClientID, Subject: got.Subject}); err != nil {
		_ = flow.Revoke(context.WithoutCancel(ctx), got.ClientID, got.Token.RefreshToken)
		return nil, err
	}
	// A sign-in kept before is replaced; its grant is revoked once the new
	// one is safely kept, never before.
	var prevToken string
	var prevRec *Record
	if old, gerr := s.Get(ChatGPT); gerr == nil && old.SecretIn != "" {
		if rt, rerr := s.readSecret(old, ChatGPT); rerr == nil {
			prevToken, prevRec = rt, old
		}
	} else if gerr != nil && !errors.Is(gerr, ErrNone) {
		return nil, gerr
	}
	rec, err := s.Put(ChatGPT, Record{Label: got.Email, Scopes: got.Token.Scopes, ClientID: got.ClientID}, Grant{RefreshToken: got.Token.RefreshToken})
	if err != nil {
		_ = flow.Revoke(context.WithoutCancel(ctx), got.ClientID, got.Token.RefreshToken)
		return nil, err
	}
	if prevToken != "" && prevToken != got.Token.RefreshToken {
		_ = flow.Revoke(context.WithoutCancel(ctx), prevRec.ClientID, prevToken)
	}
	return &ChatGPTSignedIn{Record: rec, Models: got.Models}, nil
}
