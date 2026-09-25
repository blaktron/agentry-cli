package trust

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/attest"
)

// withStatus is s's key as a registry advertising a status records it.
func (s *signer) withStatus(status, retiredAt string) SigningKey {
	k := s.keySet().Keys[0]
	k.Status, k.RetiredAt = status, retiredAt
	return k
}

func setOfKeys(keys ...SigningKey) KeySet { return KeySet{Keys: keys} }

// countersigned is env with a second signature by other over the same
// payload, the way a rotation countersigns an existing attestation.
func countersigned(t *testing.T, env attest.DSSEEnvelope, other *signer) attest.DSSEEnvelope {
	t.Helper()
	payload, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(attest.PAE(env.PayloadType, payload))
	sig, err := ecdsa.SignASN1(rand.Reader, other.priv, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	env.Signatures = append(env.Signatures, attest.DSSESignature{KeyID: other.keyID, Sig: base64.StdEncoding.EncodeToString(sig)})
	return env
}

func TestDiffKeys(t *testing.T) {
	a, b, c := newSigner(t), newSigner(t), newSigner(t)
	swapped := a.keySet().Keys[0]
	swapped.PublicKey = b.pubPEM
	tests := []struct {
		name               string
		pinned, advertised KeySet
		want               KeyChange
		rotation           bool
	}{
		{"same", setOfKeys(a.keySet().Keys[0]), setOfKeys(a.withStatus(KeyStatusRetired, "2026-09-20")), KeyChange{}, false},
		{"superset is a rotation", setOfKeys(a.keySet().Keys[0]), setOfKeys(a.withStatus(KeyStatusRetired, "2026-09-20"), b.withStatus(KeyStatusActive, "")), KeyChange{Added: []string{b.keyID}}, true},
		{"removed", setOfKeys(a.keySet().Keys[0], b.keySet().Keys[0]), setOfKeys(b.keySet().Keys[0]), KeyChange{Removed: []string{a.keyID}}, false},
		{"replaced", setOfKeys(a.keySet().Keys[0]), setOfKeys(swapped), KeyChange{Replaced: []string{a.keyID}}, false},
		{"an id listed twice with another key", setOfKeys(a.keySet().Keys[0]), setOfKeys(a.keySet().Keys[0], swapped, b.keySet().Keys[0]), KeyChange{Added: []string{b.keyID}, Replaced: []string{a.keyID}}, false},
		{"swapped", setOfKeys(a.keySet().Keys[0]), setOfKeys(c.keySet().Keys[0]), KeyChange{Added: []string{c.keyID}, Removed: []string{a.keyID}}, false},
	}
	for _, tc := range tests {
		got := DiffKeys(tc.pinned, tc.advertised)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: DiffKeys = %+v, want %+v", tc.name, got, tc.want)
		}
		if got.Rotation() != tc.rotation {
			t.Errorf("%s: Rotation = %v", tc.name, got.Rotation())
		}
	}
}

// The pin file carries the registry's status record through a round trip.
func TestPinCarriesStatus(t *testing.T) {
	isolatePins(t)
	a, b := newSigner(t), newSigner(t)
	set := setOfKeys(a.withStatus(KeyStatusRetired, "2026-09-20T10:00:00Z"), b.withStatus(KeyStatusActive, ""))
	if err := SavePinned("https://registry.test", set); err != nil {
		t.Fatal(err)
	}
	got, err := LoadPinned("https://registry.test")
	if err != nil || !reflect.DeepEqual(got, set) {
		t.Fatalf("round trip = (%+v, %v), want %+v", got, err, set)
	}
}

func TestPinAtLoginClassifiesTheChange(t *testing.T) {
	old, next := newSigner(t), newSigner(t)
	ctx := context.Background()

	t.Run("a superset is named a rotation and not adopted", func(t *testing.T) {
		isolatePins(t)
		reg := newRegistry(t, old.keySet(), nil)
		_ = PinAtLogin(ctx, http.DefaultClient, reg.srv.URL)
		reg.set(func(f *registryFixture) {
			f.keys = setOfKeys(old.withStatus(KeyStatusRetired, "2026-09-20"), next.withStatus(KeyStatusActive, ""))
		})
		note := PinAtLogin(ctx, http.DefaultClient, reg.srv.URL)
		for _, want := range []string{"possible key rotation", "added key id(s) " + next.keyID, "key " + old.keyID + " retired 2026-09-20", "consistent with a key rotation", "Confirm the rotation with the registry's operator", "log in again to re-pin deliberately"} {
			if !strings.Contains(note, want) {
				t.Errorf("note %q lacks %q", note, want)
			}
		}
		if strings.Contains(note, "compromise") || strings.Contains(note, "WARNING") {
			t.Errorf("a rotation is not reported as a compromise: %q", note)
		}
		pinned, _ := LoadPinned(reg.srv.URL)
		if len(pinned.Keys) != 1 || pinned.Keys[0].KeyID != old.keyID {
			t.Fatalf("the new key was adopted without a re-pin: %+v", pinned)
		}
		if !pinned.Keys[0].Retired() || pinned.Keys[0].RetiredAt != "2026-09-20" {
			t.Fatalf("the pinned key's status was not recorded: %+v", pinned.Keys[0])
		}
	})
	t.Run("a removed key is a warning that names it", func(t *testing.T) {
		isolatePins(t)
		reg := newRegistry(t, setOfKeys(old.keySet().Keys[0], next.keySet().Keys[0]), nil)
		_ = PinAtLogin(ctx, http.DefaultClient, reg.srv.URL)
		reg.set(func(f *registryFixture) { f.keys = next.keySet() })
		note := PinAtLogin(ctx, http.DefaultClient, reg.srv.URL)
		if !strings.Contains(note, "WARNING") || !strings.Contains(note, "removed key id(s) "+old.keyID) || !strings.Contains(note, "possible compromise") || !strings.Contains(note, "log in again to re-pin deliberately") {
			t.Fatalf("note = %q", note)
		}
		if pinned, _ := LoadPinned(reg.srv.URL); len(pinned.Keys) != 2 {
			t.Fatalf("the pin changed: %+v", pinned)
		}
	})
	t.Run("a different public key under a pinned id is a warning", func(t *testing.T) {
		isolatePins(t)
		reg := newRegistry(t, old.keySet(), nil)
		_ = PinAtLogin(ctx, http.DefaultClient, reg.srv.URL)
		swapped := old.keySet()
		swapped.Keys[0].PublicKey = next.pubPEM
		swapped.Keys[0].Status = KeyStatusRetired
		reg.set(func(f *registryFixture) { f.keys = swapped })
		note := PinAtLogin(ctx, http.DefaultClient, reg.srv.URL)
		if !strings.Contains(note, "WARNING") || !strings.Contains(note, "different public key under key id(s) "+old.keyID) || strings.Contains(note, "retired") {
			t.Fatalf("note = %q", note)
		}
		pinned, _ := LoadPinned(reg.srv.URL)
		if pinned.Keys[0].PublicKey != old.pubPEM || pinned.Keys[0].Retired() {
			t.Fatalf("a replaced key's PEM or status reached the pin: %+v", pinned.Keys[0])
		}
	})
	t.Run("an unchanged set records a retirement", func(t *testing.T) {
		isolatePins(t)
		reg := newRegistry(t, old.keySet(), nil)
		_ = PinAtLogin(ctx, http.DefaultClient, reg.srv.URL)
		reg.set(func(f *registryFixture) { f.keys = setOfKeys(old.withStatus(KeyStatusRetired, "2026-09-21")) })
		note := PinAtLogin(ctx, http.DefaultClient, reg.srv.URL)
		if !strings.Contains(note, "signing keys unchanged (1 pinned); key "+old.keyID+" retired 2026-09-21") {
			t.Fatalf("note = %q", note)
		}
	})
}

func TestVerifyAttestationAcrossARotation(t *testing.T) {
	old, next := newSigner(t), newSigner(t)
	ctx := context.Background()
	rotated := setOfKeys(old.withStatus(KeyStatusRetired, "2026-09-20"), next.withStatus(KeyStatusActive, ""))

	t.Run("a release the retired key signed verifies and says so", func(t *testing.T) {
		isolatePins(t)
		env := old.envelope(t, testDigestHex)
		reg := newRegistry(t, rotated, &env)
		if err := SavePinned(reg.srv.URL, old.keySet()); err != nil { // pinned while it was active
			t.Fatal(err)
		}
		res := VerifyAttestation(ctx, http.DefaultClient, reg.srv.URL, "", "/att", "sha256:"+testDigestHex)
		if res.State != StateVerified || res.KeyID != old.keyID || !res.KeyRetired || res.KeyRetiredAt != "2026-09-20" {
			t.Fatalf("res = %+v, want verified by the retired key", res)
		}
		if res.RetiredNote() != "signed by a retired key (retired 2026-09-20)" {
			t.Fatalf("note = %q", res.RetiredNote())
		}
	})
	t.Run("a key the pin records as retired verifies when the registry cannot be read", func(t *testing.T) {
		isolatePins(t)
		env := old.envelope(t, testDigestHex)
		reg := newRegistry(t, rotated, &env)
		if err := SavePinned(reg.srv.URL, setOfKeys(old.withStatus(KeyStatusRetired, "2026-09-20T10:00:00Z"))); err != nil {
			t.Fatal(err)
		}
		reg.set(func(f *registryFixture) { f.keyStatus = http.StatusBadGateway })
		res := VerifyAttestation(ctx, http.DefaultClient, reg.srv.URL, "", "/att", "sha256:"+testDigestHex)
		if res.State != StateVerified || !res.KeyRetired || res.RetiredNote() != "signed by a retired key (retired 2026-09-20)" {
			t.Fatalf("res = %+v, note %q", res, res.RetiredNote())
		}
	})
	t.Run("under AGENTRY_PIN_REGISTRY the pin's record answers", func(t *testing.T) {
		isolatePins(t)
		env := old.envelope(t, testDigestHex)
		reg := newRegistry(t, rotated, &env) // the registry says retired
		t.Setenv(EnvPinRegistry, reg.srv.URL)
		if err := SavePinned(reg.srv.URL, old.keySet()); err != nil { // the pin says active
			t.Fatal(err)
		}
		res := VerifyAttestation(ctx, http.DefaultClient, reg.srv.URL, "", "/att", "sha256:"+testDigestHex)
		if res.State != StateVerified || res.KeyRetired {
			t.Fatalf("res = %+v, want verified with the pin's (active) record", res)
		}
	})
	t.Run("a countersigned release is reported under the active key", func(t *testing.T) {
		isolatePins(t)
		env := countersigned(t, old.envelope(t, testDigestHex), next)
		reg := newRegistry(t, rotated, &env)
		if err := SavePinned(reg.srv.URL, rotated); err != nil {
			t.Fatal(err)
		}
		res := VerifyAttestation(ctx, http.DefaultClient, reg.srv.URL, "", "/att", "sha256:"+testDigestHex)
		if res.State != StateVerified || res.KeyID != next.keyID || res.KeyRetired || res.RetiredNote() != "" {
			t.Fatalf("res = %+v, want verified by the active key", res)
		}
	})
	t.Run("a release only the new key signed is refused as before, named a rotation", func(t *testing.T) {
		isolatePins(t)
		env := next.envelope(t, testDigestHex)
		reg := newRegistry(t, rotated, &env)
		if err := SavePinned(reg.srv.URL, old.keySet()); err != nil {
			t.Fatal(err)
		}
		res := VerifyAttestation(ctx, http.DefaultClient, reg.srv.URL, "", "/att", "sha256:"+testDigestHex)
		if res.State != StateUnverifiable || !res.KeyRotated || res.KeyChange == nil || !res.KeyChange.Rotation() {
			t.Fatalf("res = %+v, want unverifiable, a rotation", res)
		}
		if !strings.Contains(res.Detail, "consistent with a key rotation") || !strings.Contains(res.Detail, "confirm the rotation") || strings.Contains(res.Detail, "compromise") {
			t.Fatalf("detail = %q", res.Detail)
		}
		if pinned, _ := LoadPinned(reg.srv.URL); len(pinned.Keys) != 1 {
			t.Fatalf("the new key was adopted: %+v", pinned)
		}
	})
	t.Run("a replaced key under a pinned id stays a mismatch", func(t *testing.T) {
		isolatePins(t)
		env := next.envelope(t, testDigestHex)
		swapped := old.keySet()
		swapped.Keys[0].PublicKey = next.pubPEM
		reg := newRegistry(t, swapped, &env)
		if err := SavePinned(reg.srv.URL, old.keySet()); err != nil {
			t.Fatal(err)
		}
		res := VerifyAttestation(ctx, http.DefaultClient, reg.srv.URL, "", "/att", "sha256:"+testDigestHex)
		if res.State != StateMismatch || res.KeyRotated || res.KeyChange == nil || len(res.KeyChange.Replaced) != 1 {
			t.Fatalf("res = %+v, want a mismatch naming the replaced key", res)
		}
	})
}
