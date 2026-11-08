package managed

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/attest"
	"github.com/blaktron/agentry/pkg/managedsettings"

	"github.com/blaktron/agentry-cli/pkg/configdir"
	"github.com/blaktron/agentry-cli/pkg/trust"
)

type signer struct {
	priv *ecdsa.PrivateKey
	pem  string
}

func newSigner(t *testing.T) signer {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return signer{priv: priv, pem: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}
}

// envelope signs doc as the registry does, with payloadType in the
// signature and on the envelope. The payload is marshalled directly, so a
// test can sign rows the catalogue does not know.
func (s signer) envelope(t *testing.T, payloadType string, doc managedsettings.Document) managedsettings.Envelope {
	t.Helper()
	payload, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(attest.PAE(payloadType, payload))
	sig, err := ecdsa.SignASN1(rand.Reader, s.priv, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return managedsettings.Envelope{
		PayloadType: payloadType,
		Payload:     base64.StdEncoding.EncodeToString(payload),
		Signatures:  []attest.DSSESignature{{KeyID: "any", Sig: base64.StdEncoding.EncodeToString(sig)}},
	}
}

func str(v string) json.RawMessage { b, _ := json.Marshal(v); return b }

// stubRegistry is a registry that answers GET /api/v1/user, with the
// account's membership, and the managed settings route for one token.
type stubRegistry struct {
	*httptest.Server
	mu     sync.Mutex
	status int
	env    *managedsettings.Envelope
	// org is the organization the account is a member of, in state.
	org, state string
	hits       int
}

func newStub(t *testing.T) *stubRegistry {
	t.Helper()
	s := &stubRegistry{status: http.StatusNoContent, org: "acme", state: "active"}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/user":
			doc := map[string]any{"id": "6f1c2b9e-0000-4000-8000-000000000001", "handle": "member"}
			if s.org != "" {
				doc["orgs"] = []map[string]string{{"handle": s.org, "role": "member", "state": s.state}}
			}
			_ = json.NewEncoder(w).Encode(doc)
		case Route:
			s.hits++
			if s.env == nil || s.status != http.StatusOK {
				w.WriteHeader(s.status)
				return
			}
			_ = json.NewEncoder(w).Encode(s.env)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *stubRegistry) serve(env managedsettings.Envelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status, s.env = http.StatusOK, &env
}

func (s *stubRegistry) answer(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status, s.env = status, nil
}

// setup isolates the config directory and pins k for a fresh stub registry.
func setup(t *testing.T, k signer, retired bool) *stubRegistry {
	t.Helper()
	t.Setenv(configdir.EnvVar, t.TempDir())
	t.Setenv(trust.EnvPinRegistry, "")
	s := newStub(t)
	key := trust.SigningKey{KeyID: "k1", Algorithm: "ecdsa-p256-sha256", PublicKey: k.pem}
	if retired {
		key.Status = trust.KeyStatusRetired
	}
	if err := trust.SavePinned(s.URL, trust.KeySet{Keys: []trust.SigningKey{key}}); err != nil {
		t.Fatal(err)
	}
	return s
}

func docFor(reg string, version int64, settings map[string]managedsettings.Setting) managedsettings.Document {
	return managedsettings.Document{
		Schema: managedsettings.Schema, Registry: reg, Org: "acme",
		Version: version, IssuedAt: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC), Settings: settings,
	}
}

func forbidden() map[string]managedsettings.Setting {
	return map[string]managedsettings.Setting{managedsettings.UnsafeMode: {Value: str(managedsettings.UnsafeModeForbidden), Locked: true}}
}

func refresh(t *testing.T, s *stubRegistry) (*State, error) {
	t.Helper()
	return Refresh(context.Background(), s.Client(), s.URL, "tok")
}

func TestRefreshCachesAVerifiedDocumentAndLoadVerifiesItAgain(t *testing.T) {
	k := newSigner(t)
	s := setup(t, k, false)
	s.serve(k.envelope(t, managedsettings.PayloadType, docFor(s.URL+"/", 3, forbidden())))
	st, err := refresh(t, s)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if st == nil || st.Document.Org != "acme" || st.Document.Version != 3 {
		t.Fatalf("Refresh = %+v", st)
	}
	path, _ := CachePath(s.URL)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("cache mode %v, want 0600", fi.Mode().Perm())
	}
	loaded, err := Load(s.URL)
	if err != nil || loaded == nil || loaded.Document.Version != 3 {
		t.Fatalf("Load = %+v, %v", loaded, err)
	}
	cur := Current()
	if len(cur.Documents) != 1 || len(cur.Problems) != 0 {
		t.Fatalf("Current = %+v", cur)
	}
	if got, want := cur.UnsafeModeRefusal(), "Unsafe Mode is forbidden by acme's managed settings (run.unsafeMode)"; got != want {
		t.Errorf("UnsafeModeRefusal = %q, want %q", got, want)
	}
	if got := cur.RunRefusal(); got != "" {
		t.Errorf("RunRefusal = %q, want none", got)
	}
	if got := loaded.Applied(); len(got) != 1 || got[0] != managedsettings.UnsafeMode {
		t.Errorf("Applied = %v", got)
	}
}

func TestADefaultForbiddenDoesNotRefuseUnsafeMode(t *testing.T) {
	k := newSigner(t)
	s := setup(t, k, false)
	s.serve(k.envelope(t, managedsettings.PayloadType, docFor(s.URL, 1, map[string]managedsettings.Setting{
		managedsettings.UnsafeMode: {Value: str(managedsettings.UnsafeModeForbidden), Locked: false},
	})))
	if _, err := refresh(t, s); err != nil {
		t.Fatal(err)
	}
	if got := Current().UnsafeModeRefusal(); got != "" {
		t.Errorf("an organization default refused Unsafe Mode: %q", got)
	}
}

func TestRefreshRefusesDocumentsItMustNotAccept(t *testing.T) {
	k, other := newSigner(t), newSigner(t)
	for name, tc := range map[string]struct {
		env     func(reg string) managedsettings.Envelope
		retired bool
		// state is the account's membership of acme; "" is none.
		state string
		want  string
	}{
		"forged signature": {env: func(reg string) managedsettings.Envelope {
			return other.envelope(t, managedsettings.PayloadType, docFor(reg, 1, forbidden()))
		}, want: "verifies"},
		"other registry": {env: func(string) managedsettings.Envelope {
			return k.envelope(t, managedsettings.PayloadType, docFor("https://other.example", 1, forbidden()))
		}, want: "issued by https://other.example"},
		"another organization": {env: func(reg string) managedsettings.Envelope {
			d := docFor(reg, 1, forbidden())
			d.Org = "globex"
			return k.envelope(t, managedsettings.PayloadType, d)
		}, want: "not those of acme"},
		"a membership not yet approved": {env: func(reg string) managedsettings.Envelope {
			return k.envelope(t, managedsettings.PayloadType, docFor(reg, 1, forbidden()))
		}, state: "pending", want: "member of no organization"},
		"no membership": {env: func(reg string) managedsettings.Envelope {
			return k.envelope(t, managedsettings.PayloadType, docFor(reg, 1, forbidden()))
		}, state: "none", want: "member of no organization"},
		"attestation payload type": {env: func(reg string) managedsettings.Envelope {
			return k.envelope(t, attest.PayloadType, docFor(reg, 1, forbidden()))
		}, want: "payload type"},
		"retired key only": {env: func(reg string) managedsettings.Envelope {
			return k.envelope(t, managedsettings.PayloadType, docFor(reg, 1, forbidden()))
		}, retired: true, want: "no signing key is pinned"},
	} {
		t.Run(name, func(t *testing.T) {
			s := setup(t, k, tc.retired)
			switch tc.state {
			case "none":
				s.org = ""
			case "":
			default:
				s.state = tc.state
			}
			s.serve(tc.env(s.URL))
			st, err := refresh(t, s)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Refresh = %+v, %v; want an error naming %q", st, err, tc.want)
			}
			if got, _ := Load(s.URL); got != nil {
				t.Errorf("a refused document was cached: %+v", got)
			}
		})
	}
}

func TestTamperedCacheIsRefusedAndHoldsUnsafeModeOff(t *testing.T) {
	k := newSigner(t)
	s := setup(t, k, false)
	s.serve(k.envelope(t, managedsettings.PayloadType, docFor(s.URL, 2, forbidden())))
	if _, err := refresh(t, s); err != nil {
		t.Fatal(err)
	}
	path, _ := CachePath(s.URL)
	raw, _ := os.ReadFile(path)
	var f cacheFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	edited, _ := json.Marshal(docFor(s.URL, 2, map[string]managedsettings.Setting{
		managedsettings.UnsafeMode: {Value: str(managedsettings.UnsafeModeAllowed), Locked: true},
	}))
	f.Envelope.Payload = base64.StdEncoding.EncodeToString(edited)
	raw, _ = json.Marshal(f)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if st, err := Load(s.URL); err == nil {
		t.Fatalf("Load of an edited cache = %+v, want a refusal", st)
	}
	cur := Current()
	if len(cur.Documents) != 0 || len(cur.Problems) != 1 {
		t.Fatalf("Current = %+v", cur)
	}
	if got := cur.UnsafeModeRefusal(); !strings.Contains(got, "do not verify") {
		t.Errorf("UnsafeModeRefusal = %q, want a refusal while the cache does not verify", got)
	}
}

func TestNoneAppliesClearsTheCache(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			k := newSigner(t)
			s := setup(t, k, false)
			s.serve(k.envelope(t, managedsettings.PayloadType, docFor(s.URL, 1, forbidden())))
			if _, err := refresh(t, s); err != nil {
				t.Fatal(err)
			}
			s.answer(status)
			st, err := refresh(t, s)
			if err != nil || st != nil {
				t.Fatalf("Refresh = %+v, %v; want none", st, err)
			}
			if got, err := Load(s.URL); got != nil || err != nil {
				t.Fatalf("Load after %d = %+v, %v", status, got, err)
			}
			if len(Current().Documents) != 0 {
				t.Error("Current still holds a document")
			}
		})
	}
}

func TestAnotherAnswerLeavesTheCache(t *testing.T) {
	k := newSigner(t)
	s := setup(t, k, false)
	s.serve(k.envelope(t, managedsettings.PayloadType, docFor(s.URL, 1, forbidden())))
	if _, err := refresh(t, s); err != nil {
		t.Fatal(err)
	}
	s.answer(http.StatusServiceUnavailable)
	if _, err := refresh(t, s); err == nil {
		t.Fatal("a 503 was taken as an answer")
	}
	if got, _ := Load(s.URL); got == nil {
		t.Fatal("a 503 deleted the cache")
	}
}

func TestOlderVersionIsRefusedFromTheRegistryAndTheCache(t *testing.T) {
	k := newSigner(t)
	s := setup(t, k, false)
	older := k.envelope(t, managedsettings.PayloadType, docFor(s.URL, 4, nil))
	s.serve(older)
	if _, err := refresh(t, s); err != nil {
		t.Fatal(err)
	}
	path, _ := CachePath(s.URL)
	olderCache, _ := os.ReadFile(path)

	s.serve(k.envelope(t, managedsettings.PayloadType, docFor(s.URL, 5, forbidden())))
	if _, err := refresh(t, s); err != nil {
		t.Fatal(err)
	}
	// The registry handing back the older copy is refused, and the newer
	// one stays.
	s.serve(older)
	if _, err := refresh(t, s); err == nil || !strings.Contains(err.Error(), "older than version 5") {
		t.Fatalf("Refresh of an older version = %v, want a refusal", err)
	}
	if st, _ := Load(s.URL); st == nil || st.Document.Version != 5 {
		t.Fatalf("the cache is %+v, want version 5", st)
	}
	// The older copy put back in the cache is refused too, and holds Unsafe
	// Mode off.
	if err := os.WriteFile(path, olderCache, 0o600); err != nil {
		t.Fatal(err)
	}
	if st, err := Load(s.URL); err == nil {
		t.Fatalf("Load of a replayed older copy = %+v, want a refusal", st)
	}
	if Current().UnsafeModeRefusal() == "" {
		t.Error("a replayed older copy let Unsafe Mode on")
	}
	// Another organization, after a change of membership, starts over.
	d := docFor(s.URL, 1, nil)
	d.Org = "globex"
	s.mu.Lock()
	s.org = "globex"
	s.mu.Unlock()
	s.serve(k.envelope(t, managedsettings.PayloadType, d))
	if st, err := refresh(t, s); err != nil || st.Document.Org != "globex" {
		t.Fatalf("Refresh from another organization = %+v, %v", st, err)
	}
}

func TestUnknownLockedSettingRefusesEveryRun(t *testing.T) {
	k := newSigner(t)
	s := setup(t, k, false)
	s.serve(k.envelope(t, managedsettings.PayloadType, docFor(s.URL, 1, map[string]managedsettings.Setting{
		"run.somethingNewer": {Value: str("strict"), Locked: true},
	})))
	if _, err := refresh(t, s); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	got := Current().RunRefusal()
	if !strings.Contains(got, "run.somethingNewer") || !strings.Contains(got, "update Agentry") || !strings.Contains(got, "acme") {
		t.Errorf("RunRefusal = %q", got)
	}

	s.serve(k.envelope(t, managedsettings.PayloadType, docFor(s.URL, 2, map[string]managedsettings.Setting{
		"run.somethingNewer": {Value: str("strict"), Locked: false},
	})))
	if _, err := refresh(t, s); err != nil {
		t.Fatal(err)
	}
	if got := Current().RunRefusal(); got != "" {
		t.Errorf("an unknown default refused runs: %q", got)
	}
}

func TestSyncAsksOnlyWhenTheAnswerIsOldAndKeepsTheCacheOffline(t *testing.T) {
	k := newSigner(t)
	s := setup(t, k, false)
	s.serve(k.envelope(t, managedsettings.PayloadType, docFor(s.URL, 1, forbidden())))
	ctx := context.Background()
	if _, err := Sync(ctx, s.Client(), s.URL, "tok", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := Sync(ctx, s.Client(), s.URL, "tok", time.Hour); err != nil {
		t.Fatal(err)
	}
	if s.hits != 1 {
		t.Errorf("the registry was asked %d times within the hour, want 1", s.hits)
	}
	s.Close()
	st, err := Sync(ctx, http.DefaultClient, s.URL, "tok", 0)
	if err == nil || st == nil || st.Document.Version != 1 {
		t.Fatalf("Sync offline = %+v, %v; want the cached copy and the error", st, err)
	}
}

func TestSyncRemembersThatNoneApply(t *testing.T) {
	k := newSigner(t)
	s := setup(t, k, false)
	ctx := context.Background()
	for range 2 {
		if st, err := Sync(ctx, s.Client(), s.URL, "tok", time.Hour); st != nil || err != nil {
			t.Fatalf("Sync = %+v, %v", st, err)
		}
	}
	if s.hits != 1 {
		t.Errorf("the registry was asked %d times within the hour, want 1", s.hits)
	}
}

func TestForgetDeletesTheCache(t *testing.T) {
	k := newSigner(t)
	s := setup(t, k, false)
	s.serve(k.envelope(t, managedsettings.PayloadType, docFor(s.URL, 1, forbidden())))
	if _, err := refresh(t, s); err != nil {
		t.Fatal(err)
	}
	if err := Forget(s.URL); err != nil {
		t.Fatal(err)
	}
	if st, err := Load(s.URL); st != nil || err != nil {
		t.Fatalf("Load after Forget = %+v, %v", st, err)
	}
	if got := Current(); len(got.Documents)+len(got.Problems) != 0 {
		t.Fatalf("Current after Forget = %+v", got)
	}
}

func TestACacheUnderAnotherRegistrysNameIsRefused(t *testing.T) {
	k := newSigner(t)
	s := setup(t, k, false)
	s.serve(k.envelope(t, managedsettings.PayloadType, docFor(s.URL, 1, forbidden())))
	if _, err := refresh(t, s); err != nil {
		t.Fatal(err)
	}
	path, _ := CachePath(s.URL)
	moved, _ := CachePath("https://other.example")
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	cur := Current()
	if len(cur.Documents) != 0 || len(cur.Problems) != 1 {
		t.Fatalf("Current = %+v", cur)
	}
}

func TestRefreshNeedsASignIn(t *testing.T) {
	k := newSigner(t)
	s := setup(t, k, false)
	if _, err := Refresh(context.Background(), s.Client(), s.URL, ""); err == nil {
		t.Fatal("Refresh without a token asked the registry")
	}
	if _, err := Refresh(context.Background(), s.Client(), s.URL, "wrong"); err == nil {
		t.Fatal("Refresh with a refused token succeeded")
	}
}
