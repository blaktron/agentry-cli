// `agentry login` (§11): PAT from stdin, verified against GET /user, stored
// — here always via the config-file fallback (AGENTRY_NO_KEYRING), because
// tests must never touch the developer's real OS keychain — and the signing
// keys pinned at login (§8.8 TOFU). The keychain half of the storage split
// is keyringSet's error contract, covered below without a keychain.
package main

import (
	"strings"
	"testing"
)

func TestLoginStoresTokenInConfigFallback(t *testing.T) {
	isolateEnv(t) // sets AGENTRY_NO_KEYRING=1: the keychain path is off
	fr := newFakeRegistry(t)
	fr.enableSigning(t)
	fr.userHandle = "chris"

	stdout, stderr, err := runCLI(t, strings.NewReader("agy_newtoken\n"), "--registry", fr.url(), "login")
	if err != nil {
		t.Fatalf("login: %v (stderr %q)", err, stderr)
	}
	if !strings.Contains(stdout, "logged in to "+fr.url()+" as chris (token stored in config file)") {
		t.Errorf("login banner = %q", stdout)
	}
	// Keychain unavailable → the fallback is announced, not silent.
	if !strings.Contains(stderr, "storing the token in the config file instead") {
		t.Errorf("fallback warning = %q", stderr)
	}
	// Login is the TOFU moment: keys pinned now, not at first download.
	if !strings.Contains(stdout, "pinned 1 signing key(s) on first use (TOFU)") {
		t.Errorf("pin note = %q", stdout)
	}

	cfg, err := loadCLIConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultRegistry != fr.url() {
		t.Errorf("defaultRegistry = %q", cfg.DefaultRegistry)
	}
	entry := cfg.findRegistry(fr.url())
	if entry == nil || entry.TokenRef != tokenRefFile || entry.Token != "agy_newtoken" {
		t.Errorf("stored entry = %+v", entry)
	}
	// The stored credential now authenticates subsequent commands.
	if got := credentialFor(fr.url()); got != "agy_newtoken" {
		t.Errorf("credentialFor after login = %q", got)
	}
}

func TestLoginUnsignedRegistryNote(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t) // no signing keys published

	stdout, _, err := runCLI(t, strings.NewReader("agy_tok\n"), "--registry", fr.url(), "login")
	if err != nil {
		t.Fatal(err)
	}
	// An empty key set is a legitimate self-host configuration (§8.8), so
	// login succeeds and says what that means.
	if !strings.Contains(stdout, "publishes no signing keys") {
		t.Errorf("unsigned-registry note = %q", stdout)
	}
}

func TestLoginRejections(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)

	// Whitespace-only input is no token.
	_, _, err := runCLI(t, strings.NewReader("\n"), "--registry", fr.url(), "login")
	if err == nil || !strings.Contains(err.Error(), "no token provided") {
		t.Errorf("empty token: %v", err)
	}

	// A token the registry rejects renders its problem document, and
	// nothing is stored.
	_, _, err = runCLI(t, strings.NewReader("wrong_prefix\n"), "--registry", fr.url(), "login")
	if err == nil || !strings.Contains(err.Error(), "invalid token") {
		t.Errorf("rejected token: %v", err)
	}
	cfg, _ := loadCLIConfig()
	if len(cfg.Registries) != 0 {
		t.Errorf("a rejected login stored a registry entry: %+v", cfg.Registries)
	}
}

// TestKeyringSetDisabled: with AGENTRY_NO_KEYRING the keychain is refused
// up front — the seam login's fallback relies on. The success path needs a
// real OS keychain and stays untested by design.
func TestKeyringSetDisabled(t *testing.T) {
	t.Setenv(envNoKeyring, "1")
	err := keyringSet("reg.example", "agy_x")
	if err == nil || !strings.Contains(err.Error(), envNoKeyring) {
		t.Errorf("disabled keyring error = %v", err)
	}
	if keyringDisabled() != true {
		t.Error("keyringDisabled must reflect the env var")
	}
}
