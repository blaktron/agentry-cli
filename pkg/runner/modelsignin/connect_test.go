package modelsignin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/mcpauth/mcpauthtest"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// modelKeys is the model key's keychain entries, in memory: no test may
// touch a person's keychain.
type modelKeys struct{ m map[string]string }

func connectStore(t *testing.T) (*Store, *memKeys, *modelKeys) {
	t.Helper()
	keys := &memKeys{m: map[string]string{}}
	mk := &modelKeys{m: map[string]string{}}
	s := Open(t.TempDir(), keys)
	s.StoreKey = func(p, k string) error { mk.m[p] = k; return nil }
	s.DeleteKey = func(p string) error { delete(mk.m, p); return nil }
	return s, keys, mk
}

// grep reports whether secret appears in any file under dir.
func grep(t *testing.T, dir, secret string) bool {
	t.Helper()
	found := false
	_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), secret) { //nolint:gosec // the test's store
				found = true
			}
		}
		return nil
	})
	return found
}

// An OpenRouter connect keeps its key in the model key's keychain entry,
// where runs read a pasted key, and only marks the record connected; the
// key is in no file of the store (I6). Disconnect forgets it and names
// where to delete it at OpenRouter, which no app can revoke.
func TestAnOpenRouterConnectIsAKeyMarkedConnected(t *testing.T) {
	v := mcpauthtest.NewOpenRouter(t)
	s, keys, mk := connectStore(t)
	k, err := v.Flow().Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.KeepConnected(OpenRouter, k)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Connected || r.Kind != policy.KeyKindAPI || r.Provider != "openrouter" || r.SecretIn != "" {
		t.Fatalf("record %+v", r)
	}
	if mk.m["openrouter"] != k.Key || len(keys.m) != 0 {
		t.Fatalf("the key must be the model key's entry alone: model %v sign-in %v", mk.m, keys.m)
	}
	if grep(t, s.dir, k.Key) {
		t.Fatal("the key is in the store's files")
	}
	if err := s.RefreshConnected(context.Background(), "openrouter"); err != nil || mk.m["openrouter"] != k.Key {
		t.Fatalf("a key with no refresh is left as it is: %v", err)
	}
	out, err := s.Remove(context.Background(), OpenRouter, "test")
	if err != nil || !strings.Contains(out, "stays valid at OpenRouter") || !strings.Contains(out, mcpauth.OpenRouterKeysPage) {
		t.Fatalf("remove: %q %v", out, err)
	}
	if _, ok := mk.m["openrouter"]; ok {
		t.Fatal("Disconnect left the key in the keychain")
	}
	if _, err := s.Get(OpenRouter); !errors.Is(err, ErrNone) {
		t.Fatalf("after remove: %v", err)
	}
}

// A Poe key carries the expiry the person chose; past it a run is refused
// with the fix, before any container starts.
func TestAPoeKeyPastItsExpiryIsRefusedAtRunStart(t *testing.T) {
	v := mcpauthtest.NewPoe(t)
	v.ExpiresIn = 3600
	s, _, mk := connectStore(t)
	k, err := v.Flow().Connect(context.Background(), v.ClientID)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.KeepConnected(Poe, k)
	if err != nil || r.KeyExpiresAt == "" || r.Label != "person@example.com" || mk.m["poe"] != k.Key {
		t.Fatalf("record %+v %v", r, err)
	}
	if err := s.RefreshConnected(context.Background(), "poe"); err != nil {
		t.Fatalf("a live key: %v", err)
	}
	s.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	var ee *ExpiredError
	if err := s.RefreshConnected(context.Background(), "poe"); !errors.As(err, &ee) || !strings.Contains(err.Error(), "agentry model-signins add poe") {
		t.Fatalf("err %v", err)
	}
	if out, err := s.Remove(context.Background(), Poe, "test"); err != nil || !strings.Contains(out, mcpauth.PoeKeysPage) {
		t.Fatalf("remove: %q %v", out, err)
	}
}

// A Hugging Face connect keeps the token in the model key's entry and the
// refresh token as a sign-in's; each run start refreshes it and writes the
// new token into the entry (D32). The refresh token never reaches the
// model key's entry (I3).
func TestAHuggingFaceTokenIsRefreshedAtRunStart(t *testing.T) {
	v := mcpauthtest.NewHuggingFace(t)
	v.Rotate = true
	flow := v.Flow()
	t.Cleanup(RegisterVendor(HuggingFace, HuggingFaceVendor{Flow: flow}))
	s, keys, mk := connectStore(t)
	k, err := flow.Connect(context.Background(), v.AppID)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.KeepConnected(HuggingFace, k)
	if err != nil || r.SecretIn != secretInKeychain || r.KeyExpiresAt != "" || r.ClientID != v.AppID || r.Label != "person" {
		t.Fatalf("record %+v %v", r, err)
	}
	if err := s.RefreshConnected(context.Background(), "huggingface"); err != nil {
		t.Fatal(err)
	}
	now := mk.m["huggingface"]
	if now == k.Key || !v.Handed(now) || !strings.HasPrefix(now, "hf_oauth_") {
		t.Fatalf("the entry holds %q: want a renewed token", now)
	}
	if v.Live(k.RefreshToken) {
		t.Fatal("the rotated refresh token is still live")
	}
	for _, kept := range keys.m {
		if !strings.Contains(kept, "PROBE-HF-REFRESH-") || strings.Contains(kept, k.RefreshToken) {
			t.Fatalf("the sign-in's secret %q: want the rotated refresh token", kept)
		}
	}
	if strings.Contains(now, "REFRESH") {
		t.Fatal("a refresh token reached the model key's entry")
	}
	// Hugging Face names no revocation endpoint: forgotten, with the page.
	out, err := s.Remove(context.Background(), HuggingFace, "test")
	if err != nil || !strings.Contains(out, "not offered by Hugging Face") || !strings.Contains(out, mcpauth.HuggingFaceAppsPage) {
		t.Fatalf("remove: %q %v", out, err)
	}
	if len(keys.m) != 0 || len(mk.m) != 0 {
		t.Fatalf("after remove: sign-in %v model %v", keys.m, mk.m)
	}
}

// Where Hugging Face's discovery names a revocation endpoint, Disconnect
// revokes, then forgets.
func TestAHuggingFaceDisconnectRevokesWhereItCan(t *testing.T) {
	v := mcpauthtest.NewHuggingFace(t)
	v.Revocation = true
	flow := v.Flow()
	t.Cleanup(RegisterVendor(HuggingFace, HuggingFaceVendor{Flow: flow}))
	s, _, _ := connectStore(t)
	k, err := flow.Connect(context.Background(), v.AppID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.KeepConnected(HuggingFace, k); err != nil {
		t.Fatal(err)
	}
	if out, err := s.Remove(context.Background(), HuggingFace, "test"); err != nil || out != RevokeDone || v.Live(k.RefreshToken) {
		t.Fatalf("remove: %q %v", out, err)
	}
}

// A refused refresh marks the connect broken, and the run is refused with
// the fix.
func TestARefusedHuggingFaceRefreshMarksTheConnectBroken(t *testing.T) {
	v := mcpauthtest.NewHuggingFace(t)
	flow := v.Flow()
	t.Cleanup(RegisterVendor(HuggingFace, HuggingFaceVendor{Flow: flow}))
	s, _, _ := connectStore(t)
	k, err := flow.Connect(context.Background(), v.AppID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.KeepConnected(HuggingFace, k); err != nil {
		t.Fatal(err)
	}
	v.Refused = true
	var be *BrokenError
	if err := s.RefreshConnected(context.Background(), "huggingface"); !errors.As(err, &be) {
		t.Fatalf("err %v", err)
	}
}

// A keychain that will not take the key leaves nothing marked connected;
// one that will not give it up keeps the record.
func TestAKeyTheKeychainRefusesKeepsNothing(t *testing.T) {
	v := mcpauthtest.NewOpenRouter(t)
	s, _, mk := connectStore(t)
	k, err := v.Flow().Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.StoreKey = func(string, string) error { return errors.New("no keychain") }
	if _, err := s.KeepConnected(OpenRouter, k); err == nil || strings.Contains(err.Error(), k.Key) {
		t.Fatalf("err %v", err)
	}
	if _, err := s.Get(OpenRouter); !errors.Is(err, ErrNone) {
		t.Fatalf("a record was left: %v", err)
	}
	s.StoreKey = func(p, key string) error { mk.m[p] = key; return nil }
	if _, err := s.KeepConnected(OpenRouter, k); err != nil {
		t.Fatal(err)
	}
	s.DeleteKey = func(string) error { return errors.New("locked") }
	if _, err := s.Remove(context.Background(), OpenRouter, "test"); err == nil {
		t.Fatal("removed while the key stayed in the keychain")
	}
	if _, err := s.Get(OpenRouter); err != nil {
		t.Fatalf("the record must stay: %v", err)
	}
}

// Only a connect vendor's provider names one, and a pasted key's provider
// is left alone.
func TestConnectedVendorIsTheConnectVendorsOnly(t *testing.T) {
	for p, want := range map[string]string{"openrouter": OpenRouter, "poe": Poe, "huggingface": HuggingFace, "openai": "", "anthropic": "", "copilot": ""} {
		if got := ConnectedVendor(p); got != want {
			t.Errorf("ConnectedVendor(%s) = %q, want %q", p, got, want)
		}
	}
	s, _, _ := connectStore(t)
	if err := s.RefreshConnected(context.Background(), "huggingface"); err != nil {
		t.Fatalf("no connect kept: %v", err)
	}
}
