// Trust-layer tests: real DSSE envelopes signed by in-test ECDSA keys, a
// fake registry serving the well-known key set and attestation endpoints,
// and an isolated pin store — so every AttestationResult state (§8.8) is
// produced by the genuine verification path, not by stubbing outcomes.
// The signing side deliberately reuses pkg/attest's own PAE/payload
// encoding so these tests break if the formats ever drift.
package trust

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
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/attest"

	"github.com/blaktron/agentry-cli/pkg/configdir"
)

// isolatePins points the shared pin store at a per-test directory so no
// test touches (or is confused by) the developer's real ~/.config/agentry.
func isolatePins(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(configdir.EnvVar, dir)
	t.Setenv("HOME", t.TempDir()) // belt and braces: nothing should fall through
	return dir
}

// makeReadOnly strips write permission from dir (restored on cleanup so
// TempDir removal works) and skips the test where that means nothing.
func makeReadOnly(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions do not refuse writes")
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
}

// signer is one in-test registry identity: an ECDSA P-256 key pair (the
// algorithm attest.VerifyEnvelope implements) plus its wire forms.
type signer struct {
	priv   *ecdsa.PrivateKey
	keyID  string
	pubPEM string
}

// newSigner generates a fresh signing identity.
func newSigner(t *testing.T) *signer {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return &signer{
		priv:   priv,
		keyID:  attest.KeyIDFor(&priv.PublicKey),
		pubPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})),
	}
}

// keySet renders the signer as a registry-advertised key set.
func (s *signer) keySet() KeySet {
	return KeySet{Keys: []SigningKey{{KeyID: s.keyID, Algorithm: "ecdsa-p256", PublicKey: s.pubPEM}}}
}

// envelope builds and signs a real DSSE envelope whose subject digest is
// digestHex (no "sha256:" prefix), exactly the way the registry does.
func (s *signer) envelope(t *testing.T, digestHex string) attest.DSSEEnvelope {
	t.Helper()
	stmt := attest.BuildStatement(
		"https://registry.test", "demo/agent", "1.2.3",
		"sha256:"+digestHex, "cafe1234", "github.com/demo/agent",
		attest.ScanRunRef{ID: "scan-1", RulesetVer: "rules-7", ReportDigest: "sha256:rep", JudgeModel: "judge-x"},
		4,
	)
	payload, err := json.Marshal(stmt)
	if err != nil {
		t.Fatal(err)
	}
	pae := attest.PAE(attest.PayloadType, payload)
	sum := sha256.Sum256(pae)
	sig, err := ecdsa.SignASN1(rand.Reader, s.priv, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return attest.DSSEEnvelope{
		PayloadType: attest.PayloadType,
		Payload:     base64.StdEncoding.EncodeToString(payload),
		Signatures:  []attest.DSSESignature{{KeyID: s.keyID, Sig: base64.StdEncoding.EncodeToString(sig)}},
	}
}

// registryFixture is a fake registry: the well-known key document plus one
// attestation path, both mutable mid-test so rotation stories can be told.
type registryFixture struct {
	mu        chan struct{} // 1-slot mutex; keeps the fixture stdlib-simple
	keys      KeySet
	keyStatus int    // 0 = 200
	keyBody   string // non-empty overrides the marshaled key set
	envelope  *attest.DSSEEnvelope
	envStatus int // 0 = 200
	envBody   string
	authSeen  []string // Authorization header values on /att requests
	srv       *httptest.Server
}

// newRegistry serves keys and (optionally) an envelope at /att.
func newRegistry(t *testing.T, keys KeySet, env *attest.DSSEEnvelope) *registryFixture {
	t.Helper()
	f := &registryFixture{mu: make(chan struct{}, 1), keys: keys, envelope: env}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu <- struct{}{}
		defer func() { <-f.mu }()
		switch r.URL.Path {
		case "/.well-known/agentry/signing-keys.json":
			if f.keyStatus != 0 {
				w.WriteHeader(f.keyStatus)
				return
			}
			if f.keyBody != "" {
				_, _ = w.Write([]byte(f.keyBody))
				return
			}
			_ = json.NewEncoder(w).Encode(f.keys)
		case "/att":
			f.authSeen = append(f.authSeen, r.Header.Get("Authorization"))
			if f.envStatus != 0 {
				w.WriteHeader(f.envStatus)
				return
			}
			if f.envBody != "" {
				_, _ = w.Write([]byte(f.envBody))
				return
			}
			if f.envelope == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(f.envelope)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// set mutates fixture fields under the lock (the http handler races the
// test goroutine otherwise).
func (f *registryFixture) set(fn func(*registryFixture)) {
	f.mu <- struct{}{}
	defer func() { <-f.mu }()
	fn(f)
}

// auths snapshots the Authorization headers seen so far.
func (f *registryFixture) auths() []string {
	f.mu <- struct{}{}
	defer func() { <-f.mu }()
	return append([]string(nil), f.authSeen...)
}

const testDigestHex = "0011223344556677889900112233445566778899001122334455667788990011"

func TestFetchKeys(t *testing.T) {
	isolatePins(t)
	s := newSigner(t)

	t.Run("ok with nil client", func(t *testing.T) {
		reg := newRegistry(t, s.keySet(), nil)
		got, err := FetchKeys(context.Background(), nil, reg.srv.URL+"/") // trailing slash must not double up
		if err != nil {
			t.Fatalf("FetchKeys: %v", err)
		}
		if len(got.Keys) != 1 || got.Keys[0].KeyID != s.keyID || got.Keys[0].PublicKey != s.pubPEM {
			t.Fatalf("fetched set = %+v", got)
		}
	})
	t.Run("http error status", func(t *testing.T) {
		reg := newRegistry(t, s.keySet(), nil)
		reg.set(func(f *registryFixture) { f.keyStatus = http.StatusNotFound })
		_, err := FetchKeys(context.Background(), http.DefaultClient, reg.srv.URL)
		if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
			t.Fatalf("err = %v, want HTTP 404", err)
		}
	})
	t.Run("bad json", func(t *testing.T) {
		reg := newRegistry(t, s.keySet(), nil)
		reg.set(func(f *registryFixture) { f.keyBody = "{not json" })
		_, err := FetchKeys(context.Background(), http.DefaultClient, reg.srv.URL)
		if err == nil || !strings.Contains(err.Error(), "decode signing keys") {
			t.Fatalf("err = %v, want decode failure", err)
		}
	})
	t.Run("unreachable registry", func(t *testing.T) {
		_, err := FetchKeys(context.Background(), http.DefaultClient, "http://127.0.0.1:1")
		if err == nil || !strings.Contains(err.Error(), "fetch signing keys") {
			t.Fatalf("err = %v, want transport failure", err)
		}
	})
	t.Run("unbuildable request", func(t *testing.T) {
		if _, err := FetchKeys(context.Background(), http.DefaultClient, "http://bad host"); err == nil {
			t.Fatal("URL with a space must refuse")
		}
	})
}

func TestPinnedKeysPath(t *testing.T) {
	cfg := isolatePins(t)
	tests := []struct {
		url  string
		file string
	}{
		{"https://registry.example", "registry.example.yaml"},
		{"http://registry.example:8443/", "registry.example_8443.yaml"},
		{"https://reg.example/sub/path/", "reg.example_sub_path.yaml"},
		{"reg.example", "reg.example.yaml"}, // schemeless stays usable
	}
	for _, tc := range tests {
		got, err := PinnedKeysPath(tc.url)
		if err != nil {
			t.Fatalf("PinnedKeysPath(%q): %v", tc.url, err)
		}
		if want := filepath.Join(cfg, "keys", tc.file); got != want {
			t.Errorf("PinnedKeysPath(%q) = %q, want %q", tc.url, got, want)
		}
	}

	// With no config-dir override and no HOME, the path cannot resolve.
	t.Setenv(configdir.EnvVar, "")
	t.Setenv("HOME", "")
	if _, err := PinnedKeysPath("https://x.test"); err == nil {
		t.Fatal("PinnedKeysPath with no HOME must error")
	}
}

func TestPinRoundTrip(t *testing.T) {
	isolatePins(t)
	const reg = "https://reg.example"

	// Nothing pinned yet: empty set, nil error.
	empty, err := LoadPinned(reg)
	if err != nil || len(empty.Keys) != 0 {
		t.Fatalf("LoadPinned fresh = (%+v, %v), want empty", empty, err)
	}

	s := newSigner(t)
	if err := SavePinned(reg, s.keySet()); err != nil {
		t.Fatalf("SavePinned: %v", err)
	}
	path, _ := PinnedKeysPath(reg)
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("pin file stat = (%v, %v), want mode 0600", fi, err)
	}
	got, err := LoadPinned(reg)
	if err != nil {
		t.Fatalf("LoadPinned: %v", err)
	}
	if len(got.Keys) != 1 || got.Keys[0] != s.keySet().Keys[0] {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// Corrupt pin file is an error, not silently-empty trust.
	if err := os.WriteFile(path, []byte(":\tnot yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPinned(reg); err == nil || !strings.Contains(err.Error(), "pinned keys") {
		t.Fatalf("corrupt pin file: err = %v", err)
	}
}

func TestPinAtLogin(t *testing.T) {
	s := newSigner(t)
	rotated := newSigner(t)

	t.Run("first use pins", func(t *testing.T) {
		isolatePins(t)
		reg := newRegistry(t, s.keySet(), nil)
		note := PinAtLogin(context.Background(), http.DefaultClient, reg.srv.URL)
		if !strings.Contains(note, "pinned 1 signing key(s) on first use") {
			t.Fatalf("note = %q", note)
		}
		pinned, err := LoadPinned(reg.srv.URL)
		if err != nil || len(pinned.Keys) != 1 || pinned.Keys[0].KeyID != s.keyID {
			t.Fatalf("pin store after login = (%+v, %v)", pinned, err)
		}
	})
	t.Run("unchanged keys", func(t *testing.T) {
		isolatePins(t)
		reg := newRegistry(t, s.keySet(), nil)
		_ = PinAtLogin(context.Background(), http.DefaultClient, reg.srv.URL)
		note := PinAtLogin(context.Background(), http.DefaultClient, reg.srv.URL)
		if !strings.Contains(note, "signing keys unchanged (1 pinned)") {
			t.Fatalf("note = %q", note)
		}
	})
	t.Run("rotation warns and keeps the pin", func(t *testing.T) {
		isolatePins(t)
		reg := newRegistry(t, s.keySet(), nil)
		_ = PinAtLogin(context.Background(), http.DefaultClient, reg.srv.URL)
		reg.set(func(f *registryFixture) { f.keys = rotated.keySet() })
		note := PinAtLogin(context.Background(), http.DefaultClient, reg.srv.URL)
		path, _ := PinnedKeysPath(reg.srv.URL)
		if !strings.Contains(note, "WARNING") || !strings.Contains(note, path) {
			t.Fatalf("note = %q, want a warning naming %s", note, path)
		}
		pinned, _ := LoadPinned(reg.srv.URL)
		if len(pinned.Keys) != 1 || pinned.Keys[0].KeyID != s.keyID {
			t.Fatal("rotation at login must not silently re-pin")
		}
	})
	t.Run("empty key set is legitimate", func(t *testing.T) {
		isolatePins(t)
		reg := newRegistry(t, KeySet{}, nil)
		note := PinAtLogin(context.Background(), http.DefaultClient, reg.srv.URL)
		if !strings.Contains(note, "publishes no signing keys") {
			t.Fatalf("note = %q", note)
		}
	})
	t.Run("fetch failure defers pinning", func(t *testing.T) {
		isolatePins(t)
		note := PinAtLogin(context.Background(), http.DefaultClient, "http://127.0.0.1:1")
		if !strings.Contains(note, "could not fetch signing keys") || !strings.Contains(note, "pinned on first verify") {
			t.Fatalf("note = %q", note)
		}
	})
	t.Run("unreadable pin store reported", func(t *testing.T) {
		isolatePins(t)
		reg := newRegistry(t, s.keySet(), nil)
		path, _ := PinnedKeysPath(reg.srv.URL)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(":\tnot yaml"), 0o600); err != nil {
			t.Fatal(err)
		}
		note := PinAtLogin(context.Background(), http.DefaultClient, reg.srv.URL)
		if !strings.Contains(note, "could not read pinned signing keys") {
			t.Fatalf("note = %q", note)
		}
	})
	t.Run("pin write failure reported", func(t *testing.T) {
		cfg := isolatePins(t)
		makeReadOnly(t, cfg) // LoadPinned still sees a clean not-exist; SavePinned cannot mkdir
		reg := newRegistry(t, s.keySet(), nil)
		note := PinAtLogin(context.Background(), http.DefaultClient, reg.srv.URL)
		if !strings.Contains(note, "could not pin signing keys") {
			t.Fatalf("note = %q", note)
		}
	})
}

func TestVerifyAttestationVerifiedAndPinning(t *testing.T) {
	isolatePins(t)
	s := newSigner(t)
	env := s.envelope(t, testDigestHex)
	reg := newRegistry(t, s.keySet(), &env)
	ctx := context.Background()

	// First verify: TOFU-pins the fetched keys and verifies.
	res := VerifyAttestation(ctx, http.DefaultClient, reg.srv.URL, "", "/att", "sha256:"+testDigestHex)
	if res.State != StateVerified {
		t.Fatalf("state = %s (%s), want verified", res.State, res.Detail)
	}
	if !res.FirstUse {
		t.Error("first verification must report FirstUse (TOFU pin)")
	}
	if res.KeyID != s.keyID {
		t.Errorf("keyID = %q, want %q", res.KeyID, s.keyID)
	}
	// Predicate summary decoded from the signed statement.
	if res.Agent != "demo/agent" || res.Version != "1.2.3" || res.Source != "github.com/demo/agent" {
		t.Errorf("predicate identity = %q %q %q", res.Agent, res.Version, res.Source)
	}
	if res.RulesetVer != "rules-7" || res.ReportDigest != "sha256:rep" || res.JudgeModel != "judge-x" {
		t.Errorf("scan-run fields = %q %q %q", res.RulesetVer, res.ReportDigest, res.JudgeModel)
	}
	pinned, err := LoadPinned(reg.srv.URL)
	if err != nil || len(pinned.Keys) != 1 {
		t.Fatalf("keys not pinned by first verify: (%+v, %v)", pinned, err)
	}

	// Second verify: reuses the pin, no FirstUse.
	res2 := VerifyAttestation(ctx, http.DefaultClient, reg.srv.URL, "", "/att", "sha256:"+testDigestHex)
	if res2.State != StateVerified || res2.FirstUse {
		t.Fatalf("second verify = %+v, want verified via existing pin", res2)
	}

	// Digest without the sha256: prefix must verify identically.
	res3 := VerifyAttestation(ctx, http.DefaultClient, reg.srv.URL, "", "/att", testDigestHex)
	if res3.State != StateVerified {
		t.Fatalf("bare-digest verify = %+v", res3)
	}
}

func TestVerifyAttestationUnsigned(t *testing.T) {
	isolatePins(t)
	s := newSigner(t)
	ctx := context.Background()

	t.Run("no attestation URL", func(t *testing.T) {
		res := VerifyAttestation(ctx, http.DefaultClient, "https://reg.example", "", "", "sha256:"+testDigestHex)
		if res.State != StateUnsigned || !strings.Contains(res.Detail, "no attestation") {
			t.Fatalf("res = %+v", res)
		}
	})
	t.Run("404 attestation", func(t *testing.T) {
		reg := newRegistry(t, s.keySet(), nil) // nil envelope: /att answers 404
		res := VerifyAttestation(ctx, http.DefaultClient, reg.srv.URL, "", "/att", "sha256:"+testDigestHex)
		if res.State != StateUnsigned || !strings.Contains(res.Detail, "no attestation recorded") {
			t.Fatalf("res = %+v", res)
		}
	})
}

func TestVerifyAttestationUnverifiable(t *testing.T) {
	s := newSigner(t)
	env := s.envelope(t, testDigestHex)
	ctx := context.Background()

	t.Run("attestation endpoint unreachable", func(t *testing.T) {
		isolatePins(t)
		res := VerifyAttestation(ctx, http.DefaultClient, "http://127.0.0.1:1", "", "/att", "sha256:"+testDigestHex)
		if res.State != StateUnverifiable || !strings.Contains(res.Detail, "fetch attestation") {
			t.Fatalf("res = %+v", res)
		}
	})
	t.Run("attestation http error", func(t *testing.T) {
		isolatePins(t)
		reg := newRegistry(t, s.keySet(), &env)
		reg.set(func(f *registryFixture) { f.envStatus = http.StatusInternalServerError })
		res := VerifyAttestation(ctx, http.DefaultClient, reg.srv.URL, "", "/att", "sha256:"+testDigestHex)
		if res.State != StateUnverifiable || !strings.Contains(res.Detail, "HTTP 500") {
			t.Fatalf("res = %+v", res)
		}
	})
	t.Run("attestation body not an envelope", func(t *testing.T) {
		isolatePins(t)
		reg := newRegistry(t, s.keySet(), &env)
		reg.set(func(f *registryFixture) { f.envBody = "{broken" })
		res := VerifyAttestation(ctx, http.DefaultClient, reg.srv.URL, "", "/att", "sha256:"+testDigestHex)
		if res.State != StateUnverifiable || !strings.Contains(res.Detail, "decode envelope") {
			t.Fatalf("res = %+v", res)
		}
	})
	t.Run("attestation url unbuildable", func(t *testing.T) {
		isolatePins(t)
		res := VerifyAttestation(ctx, http.DefaultClient, "https://reg.example", "", "http://bad host/att", "sha256:"+testDigestHex)
		if res.State != StateUnverifiable {
			t.Fatalf("res = %+v", res)
		}
	})
	t.Run("key endpoint unreachable with nothing pinned", func(t *testing.T) {
		isolatePins(t)
		cdn := newRegistry(t, s.keySet(), &env) // serves the envelope only
		res := VerifyAttestation(ctx, http.DefaultClient, "http://127.0.0.1:1", "", cdn.srv.URL+"/att", "sha256:"+testDigestHex)
		if res.State != StateUnverifiable {
			t.Fatalf("res = %+v", res)
		}
	})
	t.Run("registry publishes no keys", func(t *testing.T) {
		isolatePins(t)
		reg := newRegistry(t, KeySet{}, &env)
		res := VerifyAttestation(ctx, http.DefaultClient, reg.srv.URL, "", "/att", "sha256:"+testDigestHex)
		if res.State != StateUnverifiable || !strings.Contains(res.Detail, "no signing keys") {
			t.Fatalf("res = %+v", res)
		}
	})
	t.Run("pin store unwritable", func(t *testing.T) {
		cfg := isolatePins(t)
		makeReadOnly(t, cfg)
		reg := newRegistry(t, s.keySet(), &env)
		res := VerifyAttestation(ctx, http.DefaultClient, reg.srv.URL, "", "/att", "sha256:"+testDigestHex)
		if res.State != StateUnverifiable || !strings.Contains(res.Detail, "pin keys") {
			t.Fatalf("res = %+v", res)
		}
	})
	t.Run("pin store unreadable", func(t *testing.T) {
		isolatePins(t)
		reg := newRegistry(t, s.keySet(), &env)
		path, _ := PinnedKeysPath(reg.srv.URL)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(":\tnot yaml"), 0o600); err != nil {
			t.Fatal(err)
		}
		res := VerifyAttestation(ctx, http.DefaultClient, reg.srv.URL, "", "/att", "sha256:"+testDigestHex)
		if res.State != StateUnverifiable {
			t.Fatalf("res = %+v", res)
		}
	})
}

func TestVerifyAttestationMismatchAndRotation(t *testing.T) {
	pinnedKey := newSigner(t)
	otherKey := newSigner(t)
	ctx := context.Background()

	t.Run("signature by an unpinned key, key ids unchanged", func(t *testing.T) {
		isolatePins(t)
		env := otherKey.envelope(t, testDigestHex)
		reg := newRegistry(t, pinnedKey.keySet(), &env)
		if err := SavePinned(reg.srv.URL, pinnedKey.keySet()); err != nil {
			t.Fatal(err)
		}
		res := VerifyAttestation(ctx, http.DefaultClient, reg.srv.URL, "", "/att", "sha256:"+testDigestHex)
		if res.State != StateMismatch || res.KeyRotated {
			t.Fatalf("res = %+v, want plain mismatch", res)
		}
		if !strings.Contains(res.Detail, "no pinned key verifies") {
			t.Fatalf("detail = %q", res.Detail)
		}
	})
	t.Run("signature by a rotated key is flagged, never adopted", func(t *testing.T) {
		isolatePins(t)
		env := otherKey.envelope(t, testDigestHex)
		reg := newRegistry(t, otherKey.keySet(), &env) // registry now advertises the NEW key
		if err := SavePinned(reg.srv.URL, pinnedKey.keySet()); err != nil {
			t.Fatal(err)
		}
		res := VerifyAttestation(ctx, http.DefaultClient, reg.srv.URL, "", "/att", "sha256:"+testDigestHex)
		if res.State != StateUnverifiable || !res.KeyRotated {
			t.Fatalf("res = %+v, want unverifiable+KeyRotated", res)
		}
		if !strings.Contains(res.Detail, "re-pin deliberately") {
			t.Fatalf("detail = %q", res.Detail)
		}
		pinned, _ := LoadPinned(reg.srv.URL)
		if len(pinned.Keys) != 1 || pinned.Keys[0].KeyID != pinnedKey.keyID {
			t.Fatal("rotation must not overwrite the pinned set")
		}
	})
	t.Run("valid signature over a different digest", func(t *testing.T) {
		isolatePins(t)
		env := pinnedKey.envelope(t, testDigestHex)
		reg := newRegistry(t, pinnedKey.keySet(), &env)
		res := VerifyAttestation(ctx, http.DefaultClient, reg.srv.URL, "", "/att",
			"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		if res.State != StateMismatch {
			t.Fatalf("res = %+v, want mismatch: a signature must bind to the downloaded bytes", res)
		}
		if !strings.Contains(res.Detail, "subject digest") || res.KeyID != pinnedKey.keyID {
			t.Fatalf("res = %+v", res)
		}
	})
}

func TestVerifyAttestationTokenScoping(t *testing.T) {
	s := newSigner(t)
	env := s.envelope(t, testDigestHex)
	ctx := context.Background()

	t.Run("registry-host attestation gets the bearer token", func(t *testing.T) {
		isolatePins(t)
		reg := newRegistry(t, s.keySet(), &env)
		res := VerifyAttestation(ctx, http.DefaultClient, reg.srv.URL, "tok-123", "/att", "sha256:"+testDigestHex)
		if res.State != StateVerified {
			t.Fatalf("res = %+v", res)
		}
		auths := reg.auths()
		if len(auths) != 1 || auths[0] != "Bearer tok-123" {
			t.Fatalf("registry saw Authorization %v, want the bearer token", auths)
		}
	})
	t.Run("cdn-host attestation never gets the token", func(t *testing.T) {
		isolatePins(t)
		reg := newRegistry(t, s.keySet(), nil) // keys live here
		cdn := newRegistry(t, KeySet{}, &env)  // envelope lives on another host:port
		res := VerifyAttestation(ctx, http.DefaultClient, reg.srv.URL, "tok-123", cdn.srv.URL+"/att", "sha256:"+testDigestHex)
		if res.State != StateVerified {
			t.Fatalf("res = %+v", res)
		}
		for _, a := range cdn.auths() {
			if a != "" {
				t.Fatalf("token leaked to a non-registry host: %q (§9.4)", a)
			}
		}
	})
}

func TestSameKeyIDs(t *testing.T) {
	a := newSigner(t)
	b := newSigner(t)
	setOf := func(sgs ...*signer) KeySet {
		var ks KeySet
		for _, s := range sgs {
			ks.Keys = append(ks.Keys, s.keySet().Keys...)
		}
		return ks
	}
	tests := []struct {
		name string
		x, y KeySet
		want bool
	}{
		{"identical", setOf(a), setOf(a), true},
		{"order-insensitive", setOf(a, b), setOf(b, a), true},
		{"different ids", setOf(a), setOf(b), false},
		{"different sizes", setOf(a, b), setOf(a), false},
		{"both empty", KeySet{}, KeySet{}, true},
	}
	for _, tc := range tests {
		if got := SameKeyIDs(tc.x, tc.y); got != tc.want {
			t.Errorf("%s: SameKeyIDs = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSameHost(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"https://reg.example/att/1", "https://reg.example", true},
		{"https://reg.example:8443/x", "https://reg.example:8443", true},
		{"https://cdn.example/att/1", "https://reg.example", false},
		{"https://reg.example:8443", "https://reg.example", false}, // port is part of the host
		{"://bad", "https://reg.example", false},
	}
	for _, tc := range tests {
		if got := sameHost(tc.a, tc.b); got != tc.want {
			t.Errorf("sameHost(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestDecodePredicate(t *testing.T) {
	t.Run("typed fields recovered from a generic map", func(t *testing.T) {
		stmt := attest.InTotoStatement{Predicate: map[string]any{
			"agent": "demo/agent", "version": "9.9.9",
			"scanRun": map[string]any{"rulesetVersion": "rv"},
		}}
		pred, err := decodePredicate(stmt)
		if err != nil {
			t.Fatal(err)
		}
		if pred.Agent != "demo/agent" || pred.Version != "9.9.9" || pred.ScanRun.RulesetVer != "rv" {
			t.Fatalf("pred = %+v", pred)
		}
	})
	t.Run("unmarshalable predicate", func(t *testing.T) {
		if _, err := decodePredicate(attest.InTotoStatement{Predicate: make(chan int)}); err == nil {
			t.Fatal("channel predicate must fail to marshal")
		}
	})
	t.Run("wrong-shaped predicate", func(t *testing.T) {
		if _, err := decodePredicate(attest.InTotoStatement{Predicate: "just a string"}); err == nil {
			t.Fatal("non-object predicate must fail to decode")
		}
	})
}

// AGENTRY_PIN_REGISTRY names whose pins verify (agentry-cli#232): a
// run-scoped prefix of a registry uses the registry's own pin file, and
// without it every URL keeps its own.
func TestPinRegistryNamesThePinFile(t *testing.T) {
	t.Setenv("AGENTRY_CONFIG_DIR", t.TempDir())
	root, _ := PinnedKeysPath("https://reg.example")
	prefix, _ := PinnedKeysPath("https://reg.example/api/v1/x/runs/rel")
	if root == prefix {
		t.Fatalf("without the override a prefix shares the root's pins: %s", prefix)
	}
	t.Setenv(EnvPinRegistry, "https://reg.example")
	if got, _ := PinnedKeysPath("https://reg.example/api/v1/x/runs/rel"); got != root {
		t.Fatalf("with the override the prefix pins at %s, want %s", got, root)
	}
}
