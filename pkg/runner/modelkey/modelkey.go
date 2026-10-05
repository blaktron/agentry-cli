// Package modelkey keeps a model provider's API key in the OS keychain —
// Keychain on macOS, Credential Manager on Windows, the Secret Service on
// Linux — for the runs a person launches from somewhere with no shell
// environment of its own: a desktop app opened from the dock or the Start
// Menu (plan desktop-installers.md §9, D12; agentry-cli#148). The policy
// records only that the keychain is the source (apiKeySource: keyring);
// the value is never written to disk in clear, and the named environment
// variable still wins whenever it is set.
//
// The service name is the one the CLI's sign-in uses for registry tokens
// (cmd/agentry's keyringService is this constant); the account
// distinguishes a model key from a token by prefix. A keychain entry is
// readable by any process of the same OS user's unlocked session, where a
// process environment is readable by the process tree alone — the wider
// boundary is the price of a key that survives a GUI launch, and DR-24
// says so.
package modelkey

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/zalando/go-keyring"
)

// Service is the keychain service both clients use.
const Service = "agentry"

// EnvNoKeyring, when set to anything, disables every keychain call here
// and in the CLI's sign-in: the headless escape hatch, for machines with
// no keychain to dial. The environment variable path is then the only one.
const EnvNoKeyring = "AGENTRY_NO_KEYRING"

// Source values for policy.Model.APIKeySource / policy.HarnessPolicy.
const (
	SourceEnv     = "env"
	SourceKeyring = "keyring"
)

// accountPrefix keeps a model key's account apart from a registry token's
// (a bare host).
const accountPrefix = "provider:"

// markerPrefix names the presence marker stored beside each model key
// (stored:provider:<name>), so Present can answer "is a key stored"
// without reading the key (agentry-cli#156). The keychain APIs the
// clients use offer no metadata-only lookup. The marker's value is
// markerValue, never anything derived from the key.
const (
	markerPrefix = "stored:"
	markerValue  = "stored"
)

// markerFor is the presence marker's account for provider's key.
func markerFor(provider string) string { return markerPrefix + Account(provider) }

// The keychain calls, as seams for tests (the real keychain is not present
// on a CI runner, and a test must never touch a person's).
var (
	keyringSet    = keyring.Set
	keyringGet    = keyring.Get
	keyringDelete = keyring.Delete
)

// Disabled reports whether AGENTRY_NO_KEYRING is set.
func Disabled() bool { return os.Getenv(EnvNoKeyring) != "" }

// Account is the keychain account for a provider's key.
func Account(provider string) string {
	return accountPrefix + strings.ToLower(strings.TrimSpace(provider))
}

// ProviderOf is the provider an Account names; "" for another account.
func ProviderOf(account string) string {
	if !strings.HasPrefix(account, accountPrefix) {
		return ""
	}
	return strings.TrimPrefix(account, accountPrefix)
}

func disabledErr(what string) error {
	return fmt.Errorf("%s: the OS keychain is disabled (%s is set); the environment variable the policy names is the way", what, EnvNoKeyring)
}

// Store puts key into the keychain for provider. An empty key is refused:
// there is nothing honest to store.
func Store(provider, key string) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("store the model key: the key is empty")
	}
	if Disabled() {
		return disabledErr("store the model key for " + provider)
	}
	if err := keyringSet(Service, Account(provider), key); err != nil {
		hint := ""
		if runtime.GOOS == "linux" {
			hint = " (a Secret Service such as GNOME Keyring or KWallet must be running)"
		}
		return fmt.Errorf("store the model key for %s in the OS keychain: %w%s; the environment variable the policy names still works", provider, err, hint)
	}
	// The marker follows the key. If it cannot be written the key is
	// still stored, and Present's fallback finds it and writes it then.
	_ = keyringSet(Service, markerFor(provider), markerValue)
	return nil
}

// Lookup reads provider's key. A missing entry is a named refusal.
func Lookup(provider string) (string, error) { return LookupAccount(Account(provider)) }

// LookupAccount reads the key behind a keychain account, as a plan records
// it (spec.EnvVar.FromKeyring).
func LookupAccount(account string) (string, error) {
	if Disabled() {
		return "", disabledErr("read " + account)
	}
	val, err := keyringGet(Service, account)
	switch {
	case errors.Is(err, keyring.ErrNotFound):
		return "", fmt.Errorf("the OS keychain has no entry %s: run `agentry setup` to store the key, or set the environment variable the policy names", account)
	case err != nil:
		return "", fmt.Errorf("read %s from the OS keychain: %w", account, err)
	case val == "":
		return "", fmt.Errorf("the OS keychain entry %s is empty", account)
	}
	return val, nil
}

// Delete removes provider's key; a missing entry is not an error.
func Delete(provider string) error {
	if Disabled() {
		return disabledErr("remove the model key for " + provider)
	}
	// The marker goes first: a Delete cut short then leaves a key with no
	// marker, which Present's fallback still finds, never a marker with
	// no key.
	if err := keyringDelete(Service, markerFor(provider)); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("remove the model key for %s: its presence marker %s would not leave the OS keychain (the key is untouched): %w", provider, markerFor(provider), err)
	}
	if err := keyringDelete(Service, Account(provider)); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("remove the model key for %s from the OS keychain: %w", provider, err)
	}
	return nil
}

// Present reports whether a key is stored for provider without reading
// it — what a settings page shows, on every mount. It reads the presence
// marker Store writes beside the key, so the key's value never enters
// this process and a per-application keychain ACL is not asked for it
// (agentry-cli#156). A key stored before the marker existed, by an older
// CLI or desktop, has none: then the key's entry is read once, the marker
// written, and the value dropped; with no key there, the read returns
// nothing. "No entry" is (false, nil); a keychain that cannot be reached,
// or is disabled, is an error, so a page never says "not stored" about a
// key it could not look for.
//
// A marker outlives a key removed some other way (an older client's
// Delete, the OS's own keychain app): Present then says "stored" and the
// run refuses with the named fix, as it does for any missing key, until
// setup stores a key again or Confirm is asked.
func Present(provider string) (bool, error) {
	if Disabled() {
		return false, disabledErr("look for the model key for " + provider)
	}
	_, err := keyringGet(Service, markerFor(provider))
	switch {
	case err == nil:
		return true, nil
	case !errors.Is(err, keyring.ErrNotFound):
		return false, lookForErr(provider, err)
	}
	return Confirm(provider)
}

// Confirm reports whether a key is stored for provider by reading its
// entry, the way a run will, and brings the presence marker into line:
// written when the key is there, removed when it is not. For a one-shot
// check that must not trust a marker (agentry doctor); the value is
// dropped at once, and a settings page asks Present instead.
func Confirm(provider string) (bool, error) {
	if Disabled() {
		return false, disabledErr("look for the model key for " + provider)
	}
	_, err := keyringGet(Service, Account(provider))
	switch {
	case errors.Is(err, keyring.ErrNotFound):
		_ = keyringDelete(Service, markerFor(provider))
		return false, nil
	case err != nil:
		return false, lookForErr(provider, err)
	}
	_ = keyringSet(Service, markerFor(provider), markerValue)
	return true, nil
}

func lookForErr(provider string, err error) error {
	return fmt.Errorf("look for %s in the OS keychain: %w", Account(provider), err)
}

// Personal reads one of the person's own provider keys (AI Review's, a
// search provider's, Hybrid Analysis's) as the runner policy says to: the
// variable env names, else, with source keyring, the OS keychain entry
// provider:<provider>. "" is no key; an error is a keychain the policy
// names that could not be read.
func Personal(env, source, provider string) (string, error) {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return v, nil
	}
	if source != SourceKeyring {
		return "", nil
	}
	present, err := Present(provider)
	if err != nil || !present {
		return "", err
	}
	v, err := Lookup(provider)
	return strings.TrimSpace(v), err
}
