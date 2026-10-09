package modelsignin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// memKeys is a keychain in memory.
type memKeys struct {
	mu sync.Mutex
	m  map[string]string
}

func (k *memKeys) Get(a string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[a]
	if !ok {
		return "", errors.New("not found")
	}
	return v, nil
}

func (k *memKeys) Set(a, v string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[a] = v
	return nil
}

func (k *memKeys) Delete(a string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.m, a)
	return nil
}

// rotating is a vendor whose refresh tokens rotate, as ChatGPT's do: each
// refresh spends the token it was given and returns the next.
type rotating struct {
	mu      sync.Mutex
	current string
	n       int
	revoked []string
	refuse  bool
}

func (v *rotating) Refresh(_ context.Context, rt string) (Refreshed, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.refuse {
		return Refreshed{}, ErrRevoked
	}
	if rt != v.current {
		return Refreshed{}, errors.New("a spent refresh token: " + rt)
	}
	v.n++
	v.current = "rt-" + string(rune('a'+v.n))
	return Refreshed{AccessToken: "at-" + string(rune('a'+v.n)), ExpiresAt: time.Now().Add(time.Hour), RefreshToken: v.current}, nil
}

func (v *rotating) Revoke(_ context.Context, rt string) error {
	v.revoked = append(v.revoked, rt)
	return nil
}

// Each vendor's kind is the one model setup names, and a sign-in kind
// maps back to its vendor.
func TestVendorsAndKinds(t *testing.T) {
	for v, kind := range map[string]string{
		ChatGPT: policy.KeyKindChatGPTPlan, Claude: policy.KeyKindClaudeSignIn, Copilot: policy.KeyKindCopilotSignIn,
		OpenRouter: policy.KeyKindAPI, Poe: policy.KeyKindAPI, HuggingFace: policy.KeyKindAPI,
	} {
		if KindOf(v) != kind {
			t.Errorf("KindOf(%s) = %s", v, KindOf(v))
		}
		if policy.IsModelSignIn(kind) && VendorOf(kind) != v {
			t.Errorf("VendorOf(%s) = %s", kind, VendorOf(kind))
		}
	}
	if VendorOf(policy.KeyKindAPI) != "" || VendorOf(policy.KeyKindSubscription) != "" {
		t.Fatal("a key kind has a vendor")
	}
}

// Use hands a run the access token alone (I3): the refresh token stays in
// the keychain, a rotated one is written back before Use returns, and two
// runs at once never spend the same rotating token. Nothing in the
// record, on disk, holds a token.
func TestUseHandsOutTheAccessTokenAloneAndSerializesARotatingGrant(t *testing.T) {
	home := t.TempDir()
	keys := &memKeys{m: map[string]string{}}
	s := Open(home, keys)
	v := &rotating{current: "rt-a"}
	t.Cleanup(RegisterVendor(ChatGPT, v))
	if _, err := s.Put(ChatGPT, Record{Label: "person@example.com", Scopes: []string{"openid"}}, Grant{RefreshToken: "rt-a"}); err != nil {
		t.Fatal(err)
	}
	if reflect.TypeFor[Access]().NumField() != 2 {
		t.Fatal("Access grew a field: it carries an access token and its expiry, nothing else")
	}
	var wg sync.WaitGroup
	tokens := make(chan string, 8)
	for range 8 {
		wg.Go(func() {
			a, err := s.Use(context.Background(), ChatGPT)
			if err != nil {
				t.Error(err)
				return
			}
			tokens <- a.Token
		})
	}
	wg.Wait()
	close(tokens)
	seen := map[string]bool{}
	for tok := range tokens {
		if !strings.HasPrefix(tok, "at-") || seen[tok] {
			t.Fatalf("access token %q (seen %v)", tok, seen)
		}
		seen[tok] = true
	}
	if v.n != 8 || !strings.Contains(keys.m["model-signin:chatgpt"], v.current) {
		t.Fatalf("%d refreshes; keychain %q, vendor %q", v.n, keys.m["model-signin:chatgpt"], v.current)
	}
	_ = filepath.WalkDir(Dir(home), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(path) //nolint:gosec // the test's store
			if strings.Contains(string(b), "rt-") || strings.Contains(string(b), "at-") {
				t.Errorf("%s holds a token:\n%s", path, b)
			}
			if fi, _ := d.Info(); fi.Mode().Perm() != 0o600 {
				t.Errorf("%s is %v", path, fi.Mode().Perm())
			}
		}
		return nil
	})
	r, err := s.Get(ChatGPT)
	if err != nil || r.Kind != policy.KeyKindChatGPTPlan || r.SecretIn != secretInKeychain || r.LastUsedAt == "" {
		t.Fatalf("record %+v (%v)", r, err)
	}
}

// Without a keychain the refresh token is kept in a 0600 file.
func TestWithoutAKeychainTheSecretIsAFile(t *testing.T) {
	s := Open(t.TempDir(), nil)
	r, err := s.Put(Copilot, Record{}, Grant{RefreshToken: "ghr_secret"})
	if err != nil || r.SecretIn != secretInFile {
		t.Fatalf("%+v %v", r, err)
	}
	fi, err := os.Stat(s.secretPath(Copilot))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("secret file: %v %v", fi, err)
	}
}

// A vendor's refusal marks the sign-in broken; the next Use says so.
func TestARevokedGrantIsMarkedBroken(t *testing.T) {
	s := Open(t.TempDir(), &memKeys{m: map[string]string{}})
	t.Cleanup(RegisterVendor(ChatGPT, &rotating{refuse: true}))
	if _, err := s.Put(ChatGPT, Record{}, Grant{RefreshToken: "rt-a"}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		_, err := s.Use(context.Background(), ChatGPT)
		var be *BrokenError
		if !errors.As(err, &be) || !strings.Contains(err.Error(), "sign in again") {
			t.Fatalf("%v", err)
		}
	}
}

// A vendor this build has no flow for is kept and listed, and Use and
// Remove say what this build cannot do: Remove forgets it and says to
// revoke it at the vendor.
func TestAVendorWithNoFlowIsKeptAndForgotten(t *testing.T) {
	home := t.TempDir()
	keys := &memKeys{m: map[string]string{}}
	s := Open(home, keys)
	if _, err := s.Put(Copilot, Record{Label: "octocat"}, Grant{RefreshToken: "ghr_x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(Claude, Record{ConfigDir: "agentry-claude-config"}, Grant{}); err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil || len(list) != 2 || list[0].Vendor != Claude || list[1].Vendor != Copilot {
		t.Fatalf("list %+v %v", list, err)
	}
	if _, err := s.Use(context.Background(), Copilot); err == nil || !strings.Contains(err.Error(), "cannot refresh a copilot sign-in yet") {
		t.Fatalf("use: %v", err)
	}
	if _, err := s.Use(context.Background(), Claude); err == nil || !strings.Contains(err.Error(), "keeps no grant") {
		t.Fatalf("use claude: %v", err)
	}
	out, err := s.Remove(context.Background(), Copilot, "test")
	if err != nil || out != RevokeNotOffered || len(keys.m) != 0 {
		t.Fatalf("remove: %q %v (keys %v)", out, err, keys.m)
	}
	if out, err = s.Remove(context.Background(), Claude, "test"); err != nil || !strings.Contains(out, "delete agentry-claude-config to end it") {
		t.Fatalf("remove claude: %q %v", out, err)
	}
	if _, err := s.Get(Copilot); !errors.Is(err, ErrNone) {
		t.Fatalf("after remove: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(Dir(home), "audit.ndjson")) //nolint:gosec // the test's store
	if strings.Count(string(b), "\n") != 2 || strings.Contains(string(b), "octocat") || strings.Contains(string(b), "ghr_") {
		t.Fatalf("audit:\n%s", b)
	}
}

// A Claude sign-in names its directory and no other sign-in does.
func TestOnlyAClaudeSignInNamesADirectory(t *testing.T) {
	s := Open(t.TempDir(), nil)
	if _, err := s.Put(Claude, Record{}, Grant{}); err == nil {
		t.Fatal("a Claude sign-in with no directory")
	}
	if _, err := s.Put(ChatGPT, Record{ConfigDir: "agentry-x"}, Grant{RefreshToken: "rt"}); err == nil {
		t.Fatal("a ChatGPT sign-in with a directory")
	}
}

// slowRotating answers only after the caller's context is cancelled.
type slowRotating struct{ rotating }

func (v *slowRotating) Refresh(ctx context.Context, rt string) (Refreshed, error) {
	time.Sleep(50 * time.Millisecond)
	if ctx.Err() != nil {
		return Refreshed{}, ctx.Err()
	}
	return v.rotating.Refresh(ctx, rt)
}

// A refresh is not cut by its caller's cancellation (a run ending
// mid-refresh): the vendor's rotated token is still written back, so the
// next Use spends the new one, not a spent one.
func TestARotatedTokenIsKeptWhenTheCallerIsCancelled(t *testing.T) {
	s := Open(t.TempDir(), &memKeys{m: map[string]string{}})
	v := &slowRotating{rotating{current: "rt-a"}}
	t.Cleanup(RegisterVendor(ChatGPT, v))
	if _, err := s.Put(ChatGPT, Record{}, Grant{RefreshToken: "rt-a"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	if _, err := s.Use(ctx, ChatGPT); err != nil {
		t.Fatalf("a cancelled caller's refresh: %v", err)
	}
	if _, err := s.Use(context.Background(), ChatGPT); err != nil {
		t.Fatalf("the next refresh spent a stale token: %v", err)
	}
}

// Remove revokes a grant at the vendor where its flow can.
func TestRemoveRevokesWhereTheVendorCan(t *testing.T) {
	s := Open(t.TempDir(), &memKeys{m: map[string]string{}})
	v := &rotating{current: "rt-a"}
	t.Cleanup(RegisterVendor(ChatGPT, v))
	if _, err := s.Put(ChatGPT, Record{}, Grant{RefreshToken: "rt-a"}); err != nil {
		t.Fatal(err)
	}
	out, err := s.Remove(context.Background(), ChatGPT, "test")
	if err != nil || out != RevokeDone || len(v.revoked) != 1 || v.revoked[0] != "rt-a" {
		t.Fatalf("%q %v %v", out, err, v.revoked)
	}
	if _, err := s.Get("gemini"); err == nil {
		t.Fatal("an unknown vendor")
	}
}
