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
	if err := keyringDelete(Service, Account(provider)); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("remove the model key for %s from the OS keychain: %w", provider, err)
	}
	return nil
}

// Present reports whether a key is stored for provider without returning
// it — what a settings page shows. "No entry" is (false, nil); a keychain
// that cannot be reached, or is disabled, is an error, so a page never
// says "not stored" about a key it could not look for.
func Present(provider string) (bool, error) {
	if Disabled() {
		return false, disabledErr("look for the model key for " + provider)
	}
	_, err := keyringGet(Service, Account(provider))
	switch {
	case errors.Is(err, keyring.ErrNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("look for %s in the OS keychain: %w", Account(provider), err)
	}
	return true, nil
}
