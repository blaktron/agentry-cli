package trust

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/attest"
	"github.com/blaktron/agentry/pkg/keyhandover"
)

// handOver is from's signed statement handing the registry at reg over to to.
func handOver(t *testing.T, reg string, from, to *signer) keyhandover.Envelope {
	t.Helper()
	payload, err := keyhandover.Seal(keyhandover.Handover{
		Schema: keyhandover.Schema, Registry: reg, From: from.keyID,
		To: keyhandover.Key{KeyID: to.keyID, Algorithm: keyhandover.Algorithm, PublicKeyPEM: to.pubPEM},
		At: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(attest.PAE(keyhandover.PayloadType, payload))
	sig, err := ecdsa.SignASN1(rand.Reader, from.priv, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return keyhandover.Wrap(payload, attest.DSSESignature{KeyID: from.keyID, Sig: base64.StdEncoding.EncodeToString(sig)})
}

func advertised(active *signer, retired ...*signer) KeySet {
	set := KeySet{Keys: []SigningKey{{KeyID: active.keyID, Algorithm: "ecdsa-p256-sha256", PublicKey: active.pubPEM, Status: KeyStatusActive}}}
	for _, r := range retired {
		set.Keys = append(set.Keys, SigningKey{KeyID: r.keyID, Algorithm: "ecdsa-p256-sha256", PublicKey: r.pubPEM, Status: KeyStatusRetired, RetiredAt: "2026-11-09T12:00:00Z"})
	}
	return set
}

func pinnedIDs(t *testing.T, reg string) map[string]string {
	t.Helper()
	set, err := LoadPinned(reg)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, k := range set.Keys {
		out[k.KeyID] = k.Status
	}
	return out
}

// A key the registry rotated to is pinned at login when the pinned key
// handed over to it, through one link or two, and the outgoing keys are
// recorded as retired; the note says so once.
func TestLoginAdoptsAKeyHandedOverByAPinnedOne(t *testing.T) {
	for _, links := range []int{1, 2} {
		isolatePins(t)
		a, b, c := newSigner(t), newSigner(t), newSigner(t)
		reg := newRegistry(t, advertised(a), nil)
		if r := PinAtLoginResult(context.Background(), nil, reg.srv.URL); r.Status != "pinned" {
			t.Fatalf("first login: %+v", r)
		}
		newest := b
		chain := keyhandover.Chain{Registry: reg.srv.URL, Handovers: []keyhandover.Envelope{handOver(t, reg.srv.URL, a, b)}}
		keys := advertised(b, a)
		if links == 2 {
			newest = c
			chain.Handovers = append(chain.Handovers, handOver(t, reg.srv.URL, b, c))
			keys = advertised(c, b, a)
		}
		reg.set(func(f *registryFixture) { f.keys, f.handovers = keys, &chain })

		r := PinAtLoginResult(context.Background(), nil, reg.srv.URL)
		if r.Status != "handed-over" || !strings.Contains(r.Note, "handed over to key "+newest.keyID) {
			t.Fatalf("%d link(s): login after the rotation: %+v", links, r)
		}
		pins := pinnedIDs(t, reg.srv.URL)
		if pins[newest.keyID] != KeyStatusActive || pins[a.keyID] != KeyStatusRetired {
			t.Fatalf("%d link(s): pins %v", links, pins)
		}
		if again := PinAtLoginResult(context.Background(), nil, reg.srv.URL); again.Status != "unchanged" {
			t.Fatalf("%d link(s): the next login said %+v; the handover is reported once", links, again)
		}
	}
}

// A new key with no handover from a pinned key is reported as before and
// never pinned: a link from a key that was never pinned, a broken link, a
// handover for another registry, and no handover at all.
func TestLoginNeverAdoptsAKeyNoPinnedKeyHandedOver(t *testing.T) {
	a, b, c, x := newSigner(t), newSigner(t), newSigner(t), newSigner(t)
	cases := map[string]func(reg string) *keyhandover.Chain{
		"no handover": func(string) *keyhandover.Chain { return nil },
		"from a key not pinned": func(reg string) *keyhandover.Chain {
			return &keyhandover.Chain{Registry: reg, Handovers: []keyhandover.Envelope{handOver(t, reg, x, c)}}
		},
		"another registry": func(reg string) *keyhandover.Chain {
			return &keyhandover.Chain{Registry: reg, Handovers: []keyhandover.Envelope{handOver(t, "https://other.example", a, c)}}
		},
		"a broken link": func(reg string) *keyhandover.Chain {
			return &keyhandover.Chain{Registry: reg, Handovers: []keyhandover.Envelope{handOver(t, reg, x, b), handOver(t, reg, b, c)}}
		},
	}
	for name, chain := range cases {
		isolatePins(t)
		reg := newRegistry(t, advertised(a), nil)
		PinAtLoginResult(context.Background(), nil, reg.srv.URL)
		reg.set(func(f *registryFixture) { f.keys, f.handovers = advertised(c, a), chain(reg.srv.URL) })
		r := PinAtLoginResult(context.Background(), nil, reg.srv.URL)
		if r.Status != "rotation" {
			t.Errorf("%s: login said %+v, want the rotation report", name, r)
		}
		if _, pinned := pinnedIDs(t, reg.srv.URL)[c.keyID]; pinned {
			t.Errorf("%s: the new key was pinned", name)
		}
	}
}

// A release signed only by the new key verifies once the pinned key's
// handover to it is adopted, and the result says so.
func TestVerifyAdoptsAHandedOverKeyAndVerifies(t *testing.T) {
	isolatePins(t)
	a, b := newSigner(t), newSigner(t)
	digest := strings.Repeat("ab", 32)
	env := b.envelope(t, digest)
	reg := newRegistry(t, advertised(a), &env)
	PinAtLoginResult(context.Background(), nil, reg.srv.URL)
	reg.set(func(f *registryFixture) {
		f.keys = advertised(b, a)
		f.handovers = &keyhandover.Chain{Registry: f.srv.URL, Handovers: []keyhandover.Envelope{handOver(t, f.srv.URL, a, b)}}
	})
	res := VerifyAttestation(context.Background(), nil, reg.srv.URL, "", "/att", "sha256:"+digest)
	if res.State != StateVerified || res.KeyID != b.keyID || res.HandedOver == "" {
		t.Fatalf("verify after a handed-over rotation: %+v", res)
	}
}
