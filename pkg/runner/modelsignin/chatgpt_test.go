package modelsignin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/mcpauth/mcpauthtest"
)

// chatgptStore is a store on a fake OpenAI, with its vendor registered.
func chatgptStore(t *testing.T) (*Store, *mcpauthtest.ChatGPT, string) {
	t.Helper()
	v := mcpauthtest.NewChatGPT(t)
	home := t.TempDir()
	t.Cleanup(RegisterVendor(ChatGPT, ChatGPTVendor{Flow: v.Flow()}))
	return Open(home, &memKeys{m: map[string]string{}}), v, home
}

// storeFiles is every file under the store, read whole.
func storeFiles(t *testing.T, home string) string {
	t.Helper()
	var all strings.Builder
	_ = filepath.WalkDir(Dir(home), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p) //nolint:gosec // the test's temp dir
			all.Write(b)
		}
		return nil
	})
	return all.String()
}

func TestSignInChatGPTKeepsTheGrantAndTheRegistration(t *testing.T) {
	s, v, home := chatgptStore(t)
	ctx := context.Background()
	in, err := s.SignInChatGPT(ctx, v.Flow(), false)
	if err != nil {
		t.Fatal(err)
	}
	r := in.Record
	if r.Kind != "chatgpt-plan" || r.Provider != "openai" || r.Label != "person@example.com" || r.ClientID != "oaiapp_fixture" || r.SecretIn != "keychain" {
		t.Fatalf("record %+v", r)
	}
	if len(in.Models) != 2 {
		t.Errorf("models %v", in.Models)
	}
	reg, err := s.Registration(ChatGPT)
	if err != nil || reg == nil || reg.ClientID != "oaiapp_fixture" || reg.Subject != "user-fixture" {
		t.Fatalf("registration %+v (%v)", reg, err)
	}
	host, _ := s.HostID(func() (string, error) { return "", errors.New("must not make another") })
	if !strings.HasPrefix(host, "urn:uuid:") || v.Authorizations[0].Get("ext_agent_host_id") != host {
		t.Fatalf("host id %q, sent %q", host, v.Authorizations[0].Get("ext_agent_host_id"))
	}
	// No token is on disk: the refresh token is in the keychain, and no
	// access token is kept at all (I3).
	if files := storeFiles(t, home); strings.Contains(files, "PROBE-") {
		t.Fatal("a token was written into the store's files")
	}

	// Signing in again reuses the registration and revokes the old grant
	// once the new one is kept.
	first := v.Issued[0]
	if _, err := s.SignInChatGPT(ctx, v.Flow(), false); err != nil {
		t.Fatal(err)
	}
	if v.Authorizations[1].Get("client_id") != "oaiapp_fixture" || v.Authorizations[1].Get("ext_agent_host_id") != host {
		t.Errorf("the second sign-in did not reuse the registration: %v", v.Authorizations[1])
	}
	if v.Live(first) {
		t.Error("the replaced grant is still live at OpenAI")
	}
}

func TestAChatGPTRunRefreshesRotatesAndBreaksOnAGoneGrant(t *testing.T) {
	s, v, _ := chatgptStore(t)
	ctx := context.Background()
	if _, err := s.SignInChatGPT(ctx, v.Flow(), false); err != nil {
		t.Fatal(err)
	}
	a, err := s.Use(ctx, ChatGPT)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a.Token, "PROBE-ACCESS-") || a.ExpiresAt.IsZero() {
		t.Fatalf("access %+v", a)
	}
	b, err := s.Use(ctx, ChatGPT)
	if err != nil {
		t.Fatalf("the rotated refresh token was not kept: %v", err)
	}
	if b.Token == a.Token {
		t.Error("the second refresh handed out the same token")
	}
	v.Refused, v.RefuseCode = true, "refresh_token_reused"
	_, err = s.Use(ctx, ChatGPT)
	var broken *BrokenError
	if !errors.As(err, &broken) {
		t.Fatalf("err %v, want *BrokenError", err)
	}
	r, _ := s.Get(ChatGPT)
	if r.Broken == "" {
		t.Error("the sign-in is not marked broken")
	}
}

func TestRemoveRevokesAChatGPTSignInAtOpenAIAndKeepsTheRegistration(t *testing.T) {
	s, v, _ := chatgptStore(t)
	ctx := context.Background()
	if _, err := s.SignInChatGPT(ctx, v.Flow(), false); err != nil {
		t.Fatal(err)
	}
	live := v.Issued[len(v.Issued)-1]
	outcome, err := s.Remove(ctx, ChatGPT, "removed by the person")
	if err != nil || outcome != RevokeDone {
		t.Fatalf("remove: %q, %v", outcome, err)
	}
	if v.Live(live) || len(v.Revocations) != 1 || v.Revocations[0].Get("client_id") != "oaiapp_fixture" {
		t.Fatalf("not revoked at OpenAI: %v", v.Revocations)
	}
	if _, err := s.Get(ChatGPT); !errors.Is(err, ErrNone) {
		t.Fatalf("the sign-in is still kept: %v", err)
	}
	// OpenAI asks that a sign-out keep the account-to-client mapping and the
	// host id for a later sign-in.
	if reg, _ := s.Registration(ChatGPT); reg == nil || reg.ClientID != "oaiapp_fixture" {
		t.Errorf("the registration was forgotten: %+v", reg)
	}
}

func TestANewAccountRegistersAgain(t *testing.T) {
	s, v, _ := chatgptStore(t)
	ctx := context.Background()
	if _, err := s.SignInChatGPT(ctx, v.Flow(), false); err != nil {
		t.Fatal(err)
	}
	v.Subject, v.IssuedClient = "user-other", "oaiapp_other"
	if _, err := s.SignInChatGPT(ctx, v.Flow(), false); err == nil || !strings.Contains(err.Error(), "another ChatGPT account") {
		t.Fatalf("a sign-in as another account through this host's registration: %v", err)
	}
	in, err := s.SignInChatGPT(ctx, v.Flow(), true)
	if err != nil {
		t.Fatal(err)
	}
	last := v.Authorizations[len(v.Authorizations)-1]
	if last.Get("client_id") != mcpauth.ChatGPTDynamicClient || in.Record.ClientID != "oaiapp_other" {
		t.Errorf("a new account did not register: %v, %+v", last, in.Record)
	}
}

func TestAnIneligibleChatGPTAccountKeepsNothing(t *testing.T) {
	s, v, home := chatgptStore(t)
	v.NotEligible = true
	_, err := s.SignInChatGPT(context.Background(), v.Flow(), false)
	var ne *mcpauth.ChatGPTNotEligibleError
	if !errors.As(err, &ne) {
		t.Fatalf("err %v", err)
	}
	if _, err := s.Get(ChatGPT); !errors.Is(err, ErrNone) {
		t.Error("an ineligible account's sign-in was kept")
	}
	if strings.Contains(storeFiles(t, home), "PROBE-") {
		t.Error("a token was written into the store's files")
	}
}
