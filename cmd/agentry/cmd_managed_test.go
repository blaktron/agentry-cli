package main

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/attest"
	"github.com/blaktron/agentry/pkg/managedsettings"

	"github.com/blaktron/agentry-cli/pkg/managed"
	"github.com/blaktron/agentry-cli/pkg/trust"
)

// managedRegistry is a fake registry that signs, with its key pinned as a
// sign-in pins it, and an environment token for it.
func managedRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fr.enableSigning(t)
	fr.orgs = []map[string]any{{"handle": "acme", "role": "member", "state": "active"}}
	t.Setenv(envToken, "agy_test")
	if err := trust.SavePinned(fr.url(), trust.KeySet{Keys: []trust.SigningKey{{KeyID: fr.keyID, Algorithm: "ecdsa-p256-sha256", PublicKey: fr.pubPEM}}}); err != nil {
		t.Fatal(err)
	}
	return fr
}

// serveManaged signs settings as the registry's managed settings document
// for the fake's account; nil settings serve none.
func (fr *fakeRegistry) serveManaged(version int64, settings map[string]managedsettings.Setting) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if settings == nil {
		fr.managedEnv = nil
		return
	}
	payload, err := json.Marshal(managedsettings.Document{
		Schema: managedsettings.Schema, Registry: fr.url(), Org: "acme", Version: version, IssuedAt: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC), Settings: settings,
	})
	if err != nil {
		fr.t.Fatal(err)
	}
	digest := sha256.Sum256(attest.PAE(managedsettings.PayloadType, payload))
	sig, err := ecdsa.SignASN1(rand.Reader, fr.signer, digest[:])
	if err != nil {
		fr.t.Fatal(err)
	}
	env := managedsettings.Wrap(payload, attest.DSSESignature{KeyID: fr.keyID, Sig: base64.StdEncoding.EncodeToString(sig)})
	fr.managedEnv = &env
}

func managedValue(v string, locked bool) managedsettings.Setting {
	raw, _ := json.Marshal(v)
	return managedsettings.Setting{Value: raw, Locked: locked}
}

func TestManagedRefreshShowAndWhoami(t *testing.T) {
	fr := managedRegistry(t)
	fr.serveManaged(4, map[string]managedsettings.Setting{
		managedsettings.UnsafeMode: managedValue(managedsettings.UnsafeModeForbidden, true),
		"run.somethingNewer":       managedValue("strict", false),
	})

	stdout, stderr, err := runCLI(t, nil, "--registry", fr.url(), "managed", "refresh")
	if err != nil {
		t.Fatalf("managed refresh: %v (stderr %q)", err, stderr)
	}
	if !strings.Contains(stdout, "managed by acme (version 4)") {
		t.Errorf("refresh printed %q", stdout)
	}

	stdout, _, err = runCLI(t, nil, "--registry", fr.url(), "managed", "show")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"organization: acme", "version:      4", "issued:       2026-11-08T12:00:00Z",
		"run.unsafeMode      forbidden (locked)",
		"run.somethingNewer  strict (default) — this version of Agentry does not know it, and passes it over",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("managed show lacks %q:\n%s", want, stdout)
		}
	}

	stdout, _, err = runCLI(t, nil, "--registry", fr.url(), "--json", "managed", "show")
	if err != nil {
		t.Fatal(err)
	}
	var doc managedView
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("json: %v\n%s", err, stdout)
	}
	if !doc.Managed || doc.Organization != "acme" || doc.Version != 4 || len(doc.Settings) != 2 ||
		doc.Settings[1].ID != managedsettings.UnsafeMode || !doc.Settings[1].Locked || !doc.Settings[1].Applied || doc.Settings[0].Known {
		t.Errorf("json = %+v", doc)
	}

	stdout, _, err = runCLI(t, nil, "--registry", fr.url(), "whoami")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "settings: managed by acme (version 4)") {
		t.Errorf("whoami lacks the managed line:\n%s", stdout)
	}

	// The registry answering that none apply clears the copy.
	fr.serveManaged(0, nil)
	stdout, _, err = runCLI(t, nil, "--registry", fr.url(), "managed", "refresh")
	if err != nil || !strings.Contains(stdout, "No managed settings for "+fr.url()) {
		t.Fatalf("refresh after 204 = %q, %v", stdout, err)
	}
	stdout, _, _ = runCLI(t, nil, "--registry", fr.url(), "managed", "show")
	if !strings.Contains(stdout, "No managed settings for "+fr.url()) {
		t.Errorf("show after 204 = %q", stdout)
	}
	stdout, _, _ = runCLI(t, nil, "--registry", fr.url(), "whoami")
	if strings.Contains(stdout, "settings:") {
		t.Errorf("whoami still names managed settings:\n%s", stdout)
	}
}

func TestManagedShowRefusesATamperedCopy(t *testing.T) {
	fr := managedRegistry(t)
	fr.serveManaged(1, map[string]managedsettings.Setting{managedsettings.UnsafeMode: managedValue(managedsettings.UnsafeModeForbidden, true)})
	if _, _, err := runCLI(t, nil, "--registry", fr.url(), "managed", "refresh"); err != nil {
		t.Fatal(err)
	}
	path, _ := managed.CachePath(fr.url())
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), `"format": 1`, `"format": 1, "extra": true`, 1)
	mustWriteFile(t, path, []byte(edited))
	_, _, err = runCLI(t, nil, "--registry", fr.url(), "managed", "show")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 11 {
		t.Fatalf("show of an edited copy = %v, want exit 11", err)
	}
}

func TestManagedRefreshNeedsASignIn(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	_, _, err := runCLI(t, nil, "--registry", fr.url(), "managed", "refresh")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 {
		t.Fatalf("refresh signed out = %v, want exit 2", err)
	}
}

func TestLogoutDeletesTheManagedSettings(t *testing.T) {
	fr := managedRegistry(t)
	fr.serveManaged(1, map[string]managedsettings.Setting{managedsettings.UnsafeMode: managedValue(managedsettings.UnsafeModeForbidden, true)})
	if _, _, err := runCLI(t, nil, "--registry", fr.url(), "managed", "refresh"); err != nil {
		t.Fatal(err)
	}
	if st, _ := managed.Load(fr.url()); st == nil {
		t.Fatal("nothing cached")
	}
	if _, stderr, err := runCLI(t, nil, "--registry", fr.url(), "logout"); err != nil {
		t.Fatalf("logout: %v (%s)", err, stderr)
	}
	if st, err := managed.Load(fr.url()); st != nil || err != nil {
		t.Fatalf("after logout Load = %+v, %v", st, err)
	}
}

// Signing in reads the account's managed settings once the registry's keys
// are pinned, in place of any copy another account left.
func TestLoginReadsTheManagedSettings(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fr.enableSigning(t)
	fr.orgs = []map[string]any{{"handle": "acme", "role": "member", "state": "active"}}
	fr.serveManaged(3, map[string]managedsettings.Setting{managedsettings.UnsafeMode: managedValue(managedsettings.UnsafeModeForbidden, true)})
	stdout, stderr, err := runCLI(t, strings.NewReader("agy_tok\n"), "--registry", fr.url(), "login", "--paste")
	if err != nil {
		t.Fatalf("login: %v (%s)", err, stderr)
	}
	if !strings.Contains(stdout, "settings: managed by acme (version 3)") {
		t.Errorf("login did not name the managed settings:\n%s\n%s", stdout, stderr)
	}
	if got := managed.Current().UnsafeModeRefusal(); !strings.Contains(got, "forbidden by acme") {
		t.Errorf("after login UnsafeModeRefusal = %q", got)
	}
}
