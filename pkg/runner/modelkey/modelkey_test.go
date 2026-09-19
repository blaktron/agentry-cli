package modelkey

import (
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

// fakeKeychain swaps the seams for an in-memory map for one test.
func fakeKeychain(t *testing.T) map[string]string {
	t.Helper()
	store := map[string]string{}
	oldSet, oldGet, oldDel := keyringSet, keyringGet, keyringDelete
	keyringSet = func(service, account, val string) error {
		if service != Service {
			t.Fatalf("service %q", service)
		}
		store[account] = val
		return nil
	}
	keyringGet = func(_, account string) (string, error) {
		v, ok := store[account]
		if !ok {
			return "", keyring.ErrNotFound
		}
		return v, nil
	}
	keyringDelete = func(_, account string) error {
		if _, ok := store[account]; !ok {
			return keyring.ErrNotFound
		}
		delete(store, account)
		return nil
	}
	t.Cleanup(func() { keyringSet, keyringGet, keyringDelete = oldSet, oldGet, oldDel })
	return store
}

func TestStoreLookupDelete(t *testing.T) {
	store := fakeKeychain(t)
	if err := Store("Anthropic", "sk-test-1"); err != nil {
		t.Fatal(err)
	}
	if store["provider:anthropic"] != "sk-test-1" {
		t.Fatalf("stored under %v", store)
	}
	v, err := Lookup("anthropic")
	if err != nil || v != "sk-test-1" {
		t.Fatalf("lookup = %q %v", v, err)
	}
	if ok, err := Present("anthropic"); !ok || err != nil {
		t.Fatalf("present = %v %v", ok, err)
	}
	if err := Delete("anthropic"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if ok, err := Present("anthropic"); ok || err != nil {
		t.Fatalf("after delete present = %v %v", ok, err)
	}
	if ProviderOf(Account("Anthropic")) != "anthropic" || ProviderOf("dev.example.test") != "" {
		t.Fatal("ProviderOf")
	}
	if err := Delete("anthropic"); err != nil {
		t.Fatalf("deleting a missing entry must not fail: %v", err)
	}
}

func TestLookupMissingNamesTheFix(t *testing.T) {
	fakeKeychain(t)
	_, err := Lookup("openai")
	if err == nil || !strings.Contains(err.Error(), "provider:openai") || !strings.Contains(err.Error(), "agentry setup") {
		t.Fatalf("err = %v", err)
	}
}

func TestStoreRefusesAnEmptyKey(t *testing.T) {
	store := fakeKeychain(t)
	if err := Store("anthropic", "  "); err == nil || len(store) != 0 {
		t.Fatalf("err=%v store=%v", err, store)
	}
}

func TestStoreNamesTheFallback(t *testing.T) {
	fakeKeychain(t)
	keyringSet = func(string, string, string) error {
		return errors.New("The name org.freedesktop.secrets was not provided by any .service files")
	}
	err := Store("anthropic", "sk-1")
	if err == nil || !strings.Contains(err.Error(), "org.freedesktop.secrets") || !strings.Contains(err.Error(), "environment variable") {
		t.Fatalf("err = %v", err)
	}
	if runtime.GOOS == "linux" && !strings.Contains(err.Error(), "Secret Service") {
		t.Fatalf("linux hint missing: %v", err)
	}
}

func TestPresentTellsNoEntryFromNoKeychain(t *testing.T) {
	fakeKeychain(t)
	keyringGet = func(string, string) (string, error) { return "", errors.New("dbus: connection refused") }
	if ok, err := Present("anthropic"); ok || err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("present = %v %v", ok, err)
	}
}

func TestDisabledRefusesEveryCallByName(t *testing.T) {
	store := fakeKeychain(t)
	t.Setenv(EnvNoKeyring, "1")
	if err := Store("anthropic", "sk-1"); err == nil || !strings.Contains(err.Error(), EnvNoKeyring) || len(store) != 0 {
		t.Fatalf("store: %v %v", err, store)
	}
	if _, err := Lookup("anthropic"); err == nil || !strings.Contains(err.Error(), EnvNoKeyring) {
		t.Fatalf("lookup: %v", err)
	}
	if _, err := Present("anthropic"); err == nil {
		t.Fatal("present under the switch must be an error, not 'not stored'")
	}
	if !Disabled() {
		t.Fatal("Disabled")
	}
}
