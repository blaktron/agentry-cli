package mcpauth_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/mcpauth/mcpauthtest"
)

func TestChatGPTSignInRegistersThenReusesTheIssuedClient(t *testing.T) {
	v := mcpauthtest.NewChatGPT(t)
	host, err := mcpauth.NewChatGPTHostID()
	if err != nil || !strings.HasPrefix(host, "urn:uuid:") || len(host) != len("urn:uuid:")+36 {
		t.Fatalf("host id %q (%v)", host, err)
	}
	ctx := context.Background()
	got, err := v.Flow().SignIn(ctx, mcpauth.ChatGPTRegistration{HostID: host})
	if err != nil {
		t.Fatal(err)
	}
	if got.ClientID != "oaiapp_fixture" || got.Subject != "user-fixture" || got.Email != "person@example.com" {
		t.Fatalf("sign-in %+v", got)
	}
	if !slices.Equal(got.Models, []string{"gpt-fixture", "gpt-fixture-mini"}) {
		t.Fatalf("models %v: want the listed ones only", got.Models)
	}
	if got.Token.RefreshToken == "" || got.Token.ExpiresAt.IsZero() {
		t.Fatalf("token %+v", got.Token)
	}
	first := v.Authorizations[0]
	for k, want := range map[string]string{
		"client_id": "dynamic_agent_client", "agent_name_hint": "Agentry", "ext_agent_host_id": host,
		"resource": "https://api.openai.com/v1", "code_challenge_method": "S256", "response_type": "code",
		"scope": "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct",
	} {
		if first.Get(k) != want {
			t.Errorf("authorization %s = %q, want %q", k, first.Get(k), want)
		}
	}
	if !strings.HasPrefix(first.Get("redirect_uri"), "http://127.0.0.1:") || !strings.HasSuffix(first.Get("redirect_uri"), "/auth/callback") {
		t.Errorf("redirect %q", first.Get("redirect_uri"))
	}
	ex := v.Requests[0]
	if ex.Get("client_id") != "oaiapp_fixture" || ex.Get("grant_type") != "authorization_code" || ex.Get("resource") != mcpauth.ChatGPTResource {
		t.Errorf("exchange form %v: the issued client and the resource", ex)
	}

	// A sign-in again on this host is OpenAI's reauthorization: the issued
	// client, no registration hint, the same host id, a fresh nonce.
	again, err := v.Flow().SignIn(ctx, mcpauth.ChatGPTRegistration{HostID: host, ClientID: got.ClientID, Subject: got.Subject})
	if err != nil {
		t.Fatal(err)
	}
	second := v.Authorizations[1]
	if second.Get("client_id") != "oaiapp_fixture" || second.Has("agent_name_hint") || second.Get("ext_agent_host_id") != host {
		t.Errorf("reauthorization %v", second)
	}
	if second.Get("nonce") == first.Get("nonce") || second.Get("state") == first.Get("state") {
		t.Error("a sign-in reused its nonce or state")
	}
	if again.ClientID != got.ClientID {
		t.Errorf("reauthorization client %q", again.ClientID)
	}
}

func TestChatGPTSignInRefusesAGrantWithoutPlanUsageAndRevokesIt(t *testing.T) {
	v := mcpauthtest.NewChatGPT(t)
	v.NoPlanScope = true
	_, err := v.Flow().SignIn(context.Background(), mcpauth.ChatGPTRegistration{HostID: "urn:uuid:fixture"})
	if !errors.Is(err, mcpauth.ErrChatGPTNoPlanScope) {
		t.Fatalf("err %v, want ErrChatGPTNoPlanScope", err)
	}
	if !strings.Contains(err.Error(), "API key") {
		t.Errorf("%q: the advice names the API key", err)
	}
	if len(v.Revocations) != 1 || v.Live(v.Issued[0]) {
		t.Fatalf("the refused grant was not revoked: %v", v.Revocations)
	}
	if len(v.ModelCalls) != 0 {
		t.Error("a grant without plan usage was used")
	}
}

func TestChatGPTSignInSaysAnIneligibleAccountShouldUseAKey(t *testing.T) {
	v := mcpauthtest.NewChatGPT(t)
	v.NotEligible = true
	_, err := v.Flow().SignIn(context.Background(), mcpauth.ChatGPTRegistration{HostID: "urn:uuid:fixture"})
	var ne *mcpauth.ChatGPTNotEligibleError
	if !errors.As(err, &ne) {
		t.Fatalf("err %v, want *ChatGPTNotEligibleError", err)
	}
	for _, want := range []string{"subscription_sharing_user_not_eligible", "OpenAI API key", "not eligible for ChatGPT plan usage"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "PROBE-") {
		t.Errorf("%q carries a token", err)
	}
	if v.Live(v.Issued[0]) {
		t.Error("the ineligible account's grant was kept at OpenAI")
	}
}

func TestChatGPTSignInRefusesAnotherAccountThanTheRegistrations(t *testing.T) {
	v := mcpauthtest.NewChatGPT(t)
	ctx := context.Background()
	in, err := v.Flow().SignIn(ctx, mcpauth.ChatGPTRegistration{HostID: "urn:uuid:fixture"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.Flow().SignIn(ctx, mcpauth.ChatGPTRegistration{HostID: "urn:uuid:fixture", ClientID: in.ClientID, Subject: "someone-else"})
	if err == nil || !strings.Contains(err.Error(), "another ChatGPT account") {
		t.Fatalf("err %v", err)
	}
	if v.Live(v.Issued[1]) {
		t.Error("the other account's grant was kept at OpenAI")
	}
}

func TestChatGPTRefreshRotatesAndAGoneGrantSaysSo(t *testing.T) {
	v := mcpauthtest.NewChatGPT(t)
	ctx := context.Background()
	in, err := v.Flow().SignIn(ctx, mcpauth.ChatGPTRegistration{HostID: "urn:uuid:fixture"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := v.Flow().Refresh(ctx, in.ClientID, in.Token.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if tok.RefreshToken == "" || tok.RefreshToken == in.Token.RefreshToken || v.Live(in.Token.RefreshToken) {
		t.Fatal("the refresh did not rotate the grant")
	}
	last := v.Requests[len(v.Requests)-1]
	if last.Get("client_id") != "oaiapp_fixture" || last.Has("scope") || last.Get("resource") != mcpauth.ChatGPTResource {
		t.Errorf("refresh form %v: the issued client, no scope, the resource", last)
	}
	for _, code := range []string{"invalid_grant", "refresh_token_reused", "refresh_token_expired"} {
		v.Refused, v.RefuseCode = true, code
		_, err = v.Flow().Refresh(ctx, in.ClientID, tok.RefreshToken)
		if !mcpauth.ChatGPTGrantGone(err) {
			t.Errorf("%s: %v is not a gone grant", code, err)
		}
	}
}

func TestChatGPTRevokeRevokesTheRefreshTokenAsTheIssuedClient(t *testing.T) {
	v := mcpauthtest.NewChatGPT(t)
	ctx := context.Background()
	in, err := v.Flow().SignIn(ctx, mcpauth.ChatGPTRegistration{HostID: "urn:uuid:fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Flow().Revoke(ctx, in.ClientID, in.Token.RefreshToken); err != nil {
		t.Fatal(err)
	}
	f := v.Revocations[0]
	if f.Get("token") != in.Token.RefreshToken || f.Get("token_type_hint") != "refresh_token" || f.Get("client_id") != "oaiapp_fixture" {
		t.Errorf("revocation form %v", f)
	}
	if v.Live(in.Token.RefreshToken) {
		t.Error("the grant is still live")
	}
	// An already invalid token is a success too (an empty 200).
	if err := v.Flow().Revoke(ctx, in.ClientID, in.Token.RefreshToken); err != nil {
		t.Errorf("revoking again: %v", err)
	}
}

// The refresh token goes to OpenAI alone (I6): a discovery document that
// names a revocation endpoint elsewhere is refused, and nothing is sent.
func TestChatGPTRevokesAtOpenAIsDomainAlone(t *testing.T) {
	v := mcpauthtest.NewChatGPT(t)
	ctx := context.Background()
	in, err := v.Flow().SignIn(ctx, mcpauth.ChatGPTRegistration{HostID: "urn:uuid:fixture"})
	if err != nil {
		t.Fatal(err)
	}
	v.RevocationURL = "https://revoke.example.net/oauth/revoke"
	err = v.Flow().Revoke(ctx, in.ClientID, in.Token.RefreshToken)
	if err == nil || !strings.Contains(err.Error(), "off OpenAI's domain") {
		t.Fatalf("err %v", err)
	}
	if !v.Live(in.Token.RefreshToken) || len(v.Revocations) != 0 {
		t.Fatal("a revocation was sent")
	}
	for host, ok := range map[string]bool{
		"auth.openai.com": true, "revoke.auth.openai.com": true, "api.openai.com": true,
		"openai.com.example.net": false, "example.net": false, "notopenai.com": false,
	} {
		if mcpauth.SameVendorForTest(host, mcpauth.ChatGPTIssuer) != ok {
			t.Errorf("%s: want %v", host, ok)
		}
	}
}

// An ID token must say when it expires (OpenID Connect requires exp).
func TestChatGPTRefusesAnIDTokenWithoutExpiry(t *testing.T) {
	v := mcpauthtest.NewChatGPT(t)
	v.NoExpiry = true
	_, err := v.Flow().SignIn(context.Background(), mcpauth.ChatGPTRegistration{HostID: "urn:uuid:fixture"})
	if err == nil || !strings.Contains(err.Error(), "no expiry") || v.Live(v.Issued[0]) {
		t.Fatalf("err %v", err)
	}
}
