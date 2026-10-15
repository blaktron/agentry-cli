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
	if v, ok := store["stored:provider:anthropic"]; !ok || v != markerValue {
		t.Fatalf("no presence marker beside the key, or it holds the key: %v", store)
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
	if len(store) != 0 {
		t.Fatalf("delete left entries behind: %v", store)
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

// countReads wraps the fake keychain's Get to count reads per account.
func countReads(t *testing.T) map[string]int {
	t.Helper()
	reads := map[string]int{}
	get := keyringGet
	keyringGet = func(service, account string) (string, error) {
		reads[account]++
		return get(service, account)
	}
	return reads
}

// Present answers from the presence marker and never reads the key
// itself: a settings page that asks on every mount does not pull the
// secret into memory, or prompt for it.
func TestPresentNeverReadsTheKey(t *testing.T) {
	fakeKeychain(t)
	if err := Store("anthropic", "sk-test-1"); err != nil {
		t.Fatal(err)
	}
	reads := countReads(t)
	for range 5 {
		if ok, err := Present("anthropic"); !ok || err != nil {
			t.Fatalf("present = %v %v", ok, err)
		}
		if ok, err := Present("openai"); ok || err != nil {
			t.Fatalf("present(openai) = %v %v", ok, err)
		}
	}
	// A stored key is never read. (With no marker and no key, the
	// fallback asks for the key's entry and is told there is none: no
	// value enters memory.)
	if reads["provider:anthropic"] != 0 {
		t.Fatalf("Present read a stored key: %v", reads)
	}
	// Lookup, the run's read, still reads the key and returns it.
	if v, err := Lookup("anthropic"); v != "sk-test-1" || err != nil {
		t.Fatalf("lookup = %q %v", v, err)
	}
	if reads["provider:anthropic"] != 1 {
		t.Fatalf("lookup reads = %v", reads)
	}
}

// A key stored before the marker existed (an older CLI or desktop) is
// read once to learn it is there, the marker is written, and it is never
// read by Present again.
func TestPresentMarksAKeyStoredWithoutAMarkerOnce(t *testing.T) {
	store := fakeKeychain(t)
	store["provider:anthropic"] = "sk-legacy"
	reads := countReads(t)
	for range 3 {
		if ok, err := Present("anthropic"); !ok || err != nil {
			t.Fatalf("present = %v %v", ok, err)
		}
	}
	if reads["provider:anthropic"] != 1 {
		t.Fatalf("the legacy key was read %d times, want once", reads["provider:anthropic"])
	}
	if _, ok := store["stored:provider:anthropic"]; !ok {
		t.Fatalf("no marker written: %v", store)
	}
}

// A marker whose key was removed some other way (an older client's
// Delete, the OS's own keychain app) goes on saying "stored" to Present;
// Confirm reads the entry, says it is gone, and clears the marker. The
// run's read is untouched by either.
func TestConfirmClearsAStaleMarker(t *testing.T) {
	store := fakeKeychain(t)
	store["stored:provider:anthropic"] = markerValue
	if ok, _ := Present("anthropic"); !ok {
		t.Fatal("a marker is what Present answers from")
	}
	if _, err := Lookup("anthropic"); err == nil || !strings.Contains(err.Error(), "agentry setup") {
		t.Fatalf("lookup err = %v", err)
	}
	if _, ok := store["stored:provider:anthropic"]; !ok {
		t.Fatal("Lookup changed the keychain")
	}
	if ok, err := Confirm("anthropic"); ok || err != nil {
		t.Fatalf("confirm = %v %v", ok, err)
	}
	if ok, err := Present("anthropic"); ok || err != nil {
		t.Fatalf("after confirm present = %v %v", ok, err)
	}
}

// A marker that cannot be written leaves the key stored: Present falls
// back to the key's entry and still answers truthfully.
func TestStoreSurvivesAFailedMarkerWrite(t *testing.T) {
	store := fakeKeychain(t)
	set := keyringSet
	keyringSet = func(service, account, val string) error {
		if strings.HasPrefix(account, markerPrefix) {
			return errors.New("item quota exceeded")
		}
		return set(service, account, val)
	}
	if err := Store("anthropic", "sk-1"); err != nil {
		t.Fatalf("store: %v", err)
	}
	if store["provider:anthropic"] != "sk-1" {
		t.Fatalf("key not stored: %v", store)
	}
	if ok, err := Present("anthropic"); !ok || err != nil {
		t.Fatalf("present = %v %v", ok, err)
	}
}

// A marker that will not leave the keychain stops Delete before the key
// is touched, and says it was the marker.
func TestDeleteNamesAStuckMarker(t *testing.T) {
	store := fakeKeychain(t)
	if err := Store("anthropic", "sk-1"); err != nil {
		t.Fatal(err)
	}
	del := keyringDelete
	keyringDelete = func(service, account string) error {
		if strings.HasPrefix(account, markerPrefix) {
			return errors.New("locked")
		}
		return del(service, account)
	}
	err := Delete("anthropic")
	if err == nil || !strings.Contains(err.Error(), "presence marker") {
		t.Fatalf("err = %v", err)
	}
	if store["provider:anthropic"] != "sk-1" {
		t.Fatal("the key went although the marker stayed")
	}
}
