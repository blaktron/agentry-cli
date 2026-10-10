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

func TestOpenRouterConnectMintsAKeyThroughPKCE(t *testing.T) {
	v := mcpauthtest.NewOpenRouter(t)
	got, err := v.Flow().Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.Key, "sk-or-v1-PROBE-") || !v.Handed(got.Key) {
		t.Fatalf("key %q: not the one the fake minted", got.Key)
	}
	if got.RefreshToken != "" || !got.ExpiresAt.IsZero() || got.ClientID != "" {
		t.Fatalf("an OpenRouter key has no refresh, expiry or client: %+v", got)
	}
	q := v.Authorizations[0]
	if q.Get("code_challenge_method") != "S256" || q.Get("key_label") != "Agentry" || !strings.HasPrefix(q.Get("callback_url"), "http://127.0.0.1:") {
		t.Fatalf("authorization %v", q)
	}
	if ex := v.Exchanges[0]; ex["code_challenge_method"] != "S256" || ex["code_verifier"] == "" {
		t.Fatalf("exchange %v: the verifier must ride the key request", ex)
	}
}

func TestOpenRouterConnectRefusalMasksWhatItSent(t *testing.T) {
	v := mcpauthtest.NewOpenRouter(t)
	f := v.Flow()
	// The browser carries another challenge than the verifier's, so the
	// exchange is refused, and the refusal echoes the verifier back.
	present := f.Present
	f.Present = func(u string) { present(strings.Replace(u, "code_challenge=", "code_challenge=x", 1)) }
	_, err := f.Connect(context.Background())
	if err == nil {
		t.Fatal("a refused exchange connected")
	}
	ex := v.Exchanges[0]
	if strings.Contains(err.Error(), ex["code_verifier"]) || strings.Contains(err.Error(), ex["code"]) {
		t.Fatalf("the error quotes what the request sent: %v", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("error %v: want the vendor's words, masked", err)
	}
}

func TestOpenRouterConnectRefusesAKeyThatIsNotOpenRouters(t *testing.T) {
	v := mcpauthtest.NewOpenRouter(t)
	v.KeyPrefix = "sk-ant-PROBE-"
	if _, err := v.Flow().Connect(context.Background()); err == nil || !strings.Contains(err.Error(), "not one of its own") {
		t.Fatalf("err %v: a key of another shape must be refused", err)
	}
}

func TestOpenRouterConnectDeclinedInTheBrowser(t *testing.T) {
	v := mcpauthtest.NewOpenRouter(t)
	v.Deny = true
	if _, err := v.Flow().Connect(context.Background()); err == nil || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("err %v", err)
	}
	if len(v.Exchanges) != 0 {
		t.Fatal("a declined connect asked for a key")
	}
}

func TestPoeConnectMintsAKeyWithItsExpiry(t *testing.T) {
	v := mcpauthtest.NewPoe(t)
	v.ExpiresIn = 30 * 24 * 3600
	got, err := v.Flow().Connect(context.Background(), v.ClientID)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Handed(got.Key) || got.Label != "person@example.com" || got.ClientID != v.ClientID || got.RefreshToken != "" {
		t.Fatalf("connect %+v", got)
	}
	if d := time.Until(got.ExpiresAt); d < 29*24*time.Hour || d > 31*24*time.Hour {
		t.Fatalf("expiry %v: want the chosen 30 days", got.ExpiresAt)
	}
	f := v.Requests[0]
	if f.Get("client_id") != v.ClientID || f.Get("code_verifier") == "" || f.Get("client_secret") != "" {
		t.Fatalf("token request %v: a public client with PKCE, no secret", f)
	}
}

func TestPoeConnectKeyWithoutExpiry(t *testing.T) {
	v := mcpauthtest.NewPoe(t)
	got, err := v.Flow().Connect(context.Background(), v.ClientID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ExpiresAt.IsZero() {
		t.Fatalf("expiry %v: null is a key that does not expire", got.ExpiresAt)
	}
}

func TestPoeConnectWithAnUnknownClientFailsInTheBrowser(t *testing.T) {
	v := mcpauthtest.NewPoe(t)
	f := v.Flow()
	f.Timeout = 2 * time.Second
	if _, err := f.Connect(context.Background(), "someone-else"); err == nil {
		t.Fatal("connected as a client Poe does not know")
	}
}

func TestPoeClientIDComesFromTheVariableUntilM8(t *testing.T) {
	t.Setenv(mcpauth.PoeClientIDEnv, "")
	if _, err := mcpauth.PoeClientID(); err == nil || !strings.Contains(err.Error(), mcpauth.PoeClientIDEnv) {
		t.Fatalf("err %v: a build with no client must name the variable", err)
	}
	t.Setenv(mcpauth.PoeClientIDEnv, " poe-client ")
	if id, err := mcpauth.PoeClientID(); err != nil || id != "poe-client" {
		t.Fatalf("id %q (%v)", id, err)
	}
}

func TestHuggingFaceConnectAsAgentrysClientDocument(t *testing.T) {
	v := mcpauthtest.NewHuggingFace(t)
	got, err := v.Flow().Connect(context.Background(), v.CIMDURL())
	if err != nil {
		t.Fatal(err)
	}
	if !v.Handed(got.Key) || !strings.HasPrefix(got.Key, "hf_oauth_") || !v.Live(got.RefreshToken) {
		t.Fatalf("connect %+v", got)
	}
	if got.Label != "person" || got.ClientID != v.CIMDURL() {
		t.Fatalf("connect %+v", got)
	}
	if got.ExpiresAt.IsZero() {
		t.Fatal("Hugging Face said when the token expires")
	}
	if s := v.Authorizations[0].Get("scope"); s != "openid profile inference-api" {
		t.Fatalf("scope %q", s)
	}
}

func TestHuggingFaceConnectRefusesAnUnservedClientDocument(t *testing.T) {
	v := mcpauthtest.NewHuggingFace(t)
	v.NoCIMD = true
	_, err := v.Flow().Connect(context.Background(), v.CIMDURL())
	if err == nil || !strings.Contains(err.Error(), "agentry-web#1331") {
		t.Fatalf("err %v", err)
	}
	if len(v.Authorizations) != 0 {
		t.Fatal("the browser was sent to a consent that would fail")
	}
}

func TestHuggingFaceConnectRefusesAGrantWithoutInference(t *testing.T) {
	v := mcpauthtest.NewHuggingFace(t)
	v.NoInference = true
	if _, err := v.Flow().Connect(context.Background(), v.AppID); err == nil || !strings.Contains(err.Error(), "inference-api") {
		t.Fatalf("err %v", err)
	}
}

func TestHuggingFaceRefreshKeepsOrRotatesTheRefreshToken(t *testing.T) {
	v := mcpauthtest.NewHuggingFace(t)
	f := v.Flow()
	got, err := f.Connect(context.Background(), v.AppID)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := f.Refresh(context.Background(), v.AppID, got.RefreshToken)
	if err != nil || !v.Handed(tok.AccessToken) || tok.AccessToken == got.Key {
		t.Fatalf("refresh %+v (%v)", tok, err)
	}
	v.Rotate = true
	tok2, err := f.Refresh(context.Background(), v.AppID, got.RefreshToken)
	if err != nil || tok2.RefreshToken == "" || tok2.RefreshToken == got.RefreshToken || v.Live(got.RefreshToken) {
		t.Fatalf("rotated refresh %+v (%v)", tok2, err)
	}
	v.Refused = true
	if _, err := f.Refresh(context.Background(), v.AppID, tok2.RefreshToken); !mcpauth.GrantRevoked(err) {
		t.Fatalf("err %v: a refused refresh is a grant gone", err)
	}
}

func TestHuggingFaceRevokeOnlyWhereItsDiscoveryNamesAnEndpoint(t *testing.T) {
	v := mcpauthtest.NewHuggingFace(t)
	f := v.Flow()
	got, err := f.Connect(context.Background(), v.AppID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Revoke(context.Background(), v.AppID, got.RefreshToken); !errors.Is(err, mcpauth.ErrRevokeNotOffered) {
		t.Fatalf("err %v: Hugging Face names no revocation endpoint", err)
	}
	v.Revocation = true
	if err := f.Revoke(context.Background(), v.AppID, got.RefreshToken); err != nil || v.Live(got.RefreshToken) {
		t.Fatalf("revoke (%v): the named endpoint must be used", err)
	}
}

// Hugging Face's discovery is held to its own host for the endpoints the
// code, the verifier and the refresh token go to (I6).
func TestHuggingFaceRefusesATokenEndpointOnAnotherHost(t *testing.T) {
	v := mcpauthtest.NewHuggingFace(t)
	v.ForeignToken = true
	if _, err := v.Flow().Connect(context.Background(), v.AppID); err == nil || !strings.Contains(err.Error(), "another host") {
		t.Fatalf("err %v", err)
	}
	if _, err := v.Flow().Refresh(context.Background(), v.AppID, "rt"); err == nil || !strings.Contains(err.Error(), "another host") {
		t.Fatalf("refresh err %v", err)
	}
	if len(v.Authorizations) != 0 || len(v.Requests) != 0 {
		t.Fatal("a request went out before the discovery was refused")
	}
}
