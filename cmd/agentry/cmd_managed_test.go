package main

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
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
		"acme, version 4, from " + fr.url() + " (issued 2026-11-08T12:00:00Z)",
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
	var view managedView
	if err := json.Unmarshal([]byte(stdout), &view); err != nil {
		t.Fatalf("json: %v\n%s", err, stdout)
	}
	if len(view.Documents) != 1 || len(view.Problems) != 0 {
		t.Fatalf("json = %+v", view)
	}
	doc := view.Documents[0]
	if doc.Registry != fr.url() || doc.Organization != "acme" || doc.Version != 4 || len(doc.Settings) != 2 ||
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
	if !strings.Contains(stdout, "No managed settings.") {
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
	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "managed", "show")
	wantClassified(t, err, 11)
	if !strings.Contains(stdout, "cannot verify the managed settings held for "+fr.url()) {
		t.Errorf("show of an edited copy printed %q", stdout)
	}
	stdout, _, err = runCLI(t, nil, "--registry", fr.url(), "--json", "managed", "show")
	wantClassified(t, err, 11)
	var view managedView
	if err := json.Unmarshal([]byte(stdout), &view); err != nil || len(view.Documents) != 0 || len(view.Problems) != 1 || view.Problems[0].Registry != fr.url() {
		t.Errorf("json of an edited copy = %+v, %v", view, err)
	}
	stdout, _, _ = runCLI(t, nil, "--registry", fr.url(), "whoami")
	if !strings.Contains(stdout, "settings: the managed settings held for "+fr.url()+" cannot be verified") {
		t.Errorf("whoami does not say the copy cannot be verified:\n%s", stdout)
	}
}

// A sign-in of the same account keeps its organization's lock while the
// registry cannot answer; it is not a sign-out.
func TestReSigningInKeepsTheLockWhenTheRegistryCannotAnswer(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fr.enableSigning(t)
	fr.orgs = []map[string]any{{"handle": "acme", "role": "member", "state": "active"}}
	fr.serveManaged(3, map[string]managedsettings.Setting{managedsettings.UnsafeMode: managedValue(managedsettings.UnsafeModeForbidden, true)})
	if _, stderr, err := runCLI(t, strings.NewReader("agy_tok\n"), "--registry", fr.url(), "login", "--paste"); err != nil {
		t.Fatalf("login: %v (%s)", err, stderr)
	}
	fr.mu.Lock()
	fr.managedStatus = http.StatusServiceUnavailable
	fr.mu.Unlock()
	if _, stderr, err := runCLI(t, strings.NewReader("agy_tok\n"), "--registry", fr.url(), "login", "--paste"); err != nil {
		t.Fatalf("login again: %v (%s)", err, stderr)
	}
	if got := managed.Current().UnsafeModeRefusal(); !strings.Contains(got, "forbidden by acme") {
		t.Errorf("after signing in again UnsafeModeRefusal = %q", got)
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

// holdManagedFrom makes this device hold fr's managed settings document,
// signed in to fr with an environment token, its key pinned.
func holdManagedFrom(t *testing.T, fr *fakeRegistry, settings map[string]managedsettings.Setting) {
	t.Helper()
	fr.enableSigning(t)
	fr.orgs = []map[string]any{{"handle": "acme", "role": "member", "state": "active"}}
	t.Setenv(envToken, "agy_test")
	if err := trust.SavePinned(fr.url(), trust.KeySet{Keys: []trust.SigningKey{{KeyID: fr.keyID, Algorithm: "ecdsa-p256-sha256", PublicKey: fr.pubPEM}}}); err != nil {
		t.Fatal(err)
	}
	fr.serveManaged(5, settings)
	if _, stderr, err := runCLI(t, nil, "--registry", fr.url(), "managed", "refresh"); err != nil {
		t.Fatalf("managed refresh: %v (%s)", err, stderr)
	}
}

// A run sent to a runhost carries this device's managed settings: the
// runhost has its own sign-in, and the organization's lock on this device
// travels with the job for the runhost's run to apply.
func TestARunSentToARunhostCarriesTheDevicesManagedSettings(t *testing.T) {
	srv, _ := newRunOnLab(t)
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))
	holdManagedFrom(t, fr, map[string]managedsettings.Setting{managedsettings.UnsafeMode: managedValue(managedsettings.UnsafeModeForbidden, true)})

	_, stderr, err := runCLI(t, strings.NewReader("the task"), "--registry", fr.url(), "run", "acme/demo@1.0.0",
		"--on", "lab", "--allow-unsigned", "--yes", "--out", filepath.Join(t.TempDir(), "out"), "--output-format", "json")
	if err != nil {
		t.Fatalf("run --on: %v\n%s", err, stderr)
	}
	argv, _ := os.ReadFile(filepath.Join(srv.dir, "argv"))
	if !strings.Contains(string(argv), "--runhost-managed\n") {
		t.Errorf("the runhost's run was not handed the device's managed settings:\n%s", argv)
	}
	raw, err := os.ReadFile(filepath.Join(srv.dir, "managed.seen"))
	if err != nil {
		t.Fatalf("the job staged no managed settings: %v", err)
	}
	var n managed.Narrowing
	if err := json.Unmarshal(raw, &n); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(n.UnsafeModeRefusal, "forbidden by acme") || len(n.Documents) != 1 || n.Documents[0].Organization != "acme" || n.Documents[0].Version != 5 {
		t.Errorf("the staged narrowing = %+v", n)
	}
}

// A lock this device's CLI does not know refuses a run on a runhost before
// anything is sent, as it refuses one here.
func TestAnUnknownLockRefusesARunOnARunhostBeforeSending(t *testing.T) {
	srv, _ := newRunOnLab(t)
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))
	holdManagedFrom(t, fr, map[string]managedsettings.Setting{"run.somethingNewer": managedValue("strict", true)})

	_, _, err := runCLI(t, strings.NewReader("the task"), "--registry", fr.url(), "run", "acme/demo@1.0.0",
		"--on", "lab", "--allow-unsigned", "--yes", "--out", filepath.Join(t.TempDir(), "out"))
	wantClassified(t, err, 11)
	if !strings.Contains(err.Error(), "run.somethingNewer") {
		t.Errorf("refusal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(srv.dir, "argv")); err == nil {
		t.Error("the run started on the runhost")
	}
}

// The runhost's run applies the narrowing it was handed: an Unsafe Mode run
// is refused with the device's reason, even where this machine holds no
// managed settings of its own.
func TestARunhostsRunAppliesTheSendingDevicesNarrowing(t *testing.T) {
	isolateEnv(t)
	if err := refuseUnsafeMode(nil); err != nil {
		t.Fatalf("no managed settings anywhere refused Unsafe Mode: %v", err)
	}
	sender := &managed.Narrowing{UnsafeModeRefusal: "Unsafe Mode is forbidden by acme's managed settings (run.unsafeMode)"}
	err := refuseUnsafeMode(sender)
	wantClassified(t, err, 11)
	if !strings.Contains(err.Error(), "forbidden by acme") || !strings.Contains(err.Error(), "the device that sent this run") {
		t.Errorf("refusal: %v", err)
	}

	file := filepath.Join(t.TempDir(), "managed.json")
	raw, _ := json.Marshal(sender)
	mustWriteFile(t, file, raw)
	got, err := senderManaged(runOpts{installed: true, runhostManaged: file})
	if err != nil || got.UnsafeModeRefusal != sender.UnsafeModeRefusal {
		t.Fatalf("senderManaged = %+v, %v", got, err)
	}
	mustWriteFile(t, file, []byte(`{"unsafeModeRefusal":"two\nlines"}`))
	if _, err := senderManaged(runOpts{installed: true, runhostManaged: file}); err == nil {
		t.Error("a narrowing that is not one was taken")
	}
	if _, err := senderManaged(runOpts{runhostManaged: file}); err == nil {
		t.Error("--runhost-managed was taken without --installed")
	}
}
