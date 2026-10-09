package modelkey

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sealedDir gives this process a credential folder holding values, the
// way systemd hands a service its LoadCredentialEncrypted= plaintext.
func sealedDir(t *testing.T, values map[string]string) {
	t.Helper()
	b, err := SealPlaintext(values)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, CredentialName), b, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvCredentialsDirectory, dir)
}

func TestSealedCredentialAnswersInsteadOfTheKeychain(t *testing.T) {
	store := fakeKeychain(t)
	store["provider:openai"] = "keychain-value"
	store["stored:provider:openai"] = markerValue
	sealedDir(t, map[string]string{"provider:openai": "sealed-value", "registry.example": "tok"})
	// A signed-out fire's unit turns the keychain off; the sealed keys
	// still answer.
	t.Setenv(EnvNoKeyring, "1")

	if v, err := Lookup("openai"); err != nil || v != "sealed-value" {
		t.Fatalf("Lookup = %q %v, want the sealed value", v, err)
	}
	if ok, err := Present("openai"); !ok || err != nil {
		t.Fatalf("Present = %v %v", ok, err)
	}
	if ok, err := Confirm("openai"); !ok || err != nil {
		t.Fatalf("Confirm = %v %v", ok, err)
	}
	if v, err := Personal("NOT_SET_ANYWHERE_749", SourceKeyring, "openai"); err != nil || v != "sealed-value" {
		t.Fatalf("Personal = %q %v", v, err)
	}
	if v, sealed, err := SealedAccount("registry.example"); !sealed || err != nil || v != "tok" {
		t.Fatalf("SealedAccount = %q %v %v", v, sealed, err)
	}

	// An account the credential does not hold is not looked for in the
	// keychain: there is no fallback.
	store["provider:anthropic"] = "keychain-only"
	if ok, err := Present("anthropic"); ok || err != nil {
		t.Fatalf("Present(anthropic) = %v %v, want false from the sealed keys alone", ok, err)
	}
	if _, err := Lookup("anthropic"); err == nil || !strings.Contains(err.Error(), "sealed keys hold no provider:anthropic") {
		t.Fatalf("Lookup(anthropic) = %v, want the named refusal", err)
	}
	if v, err := Personal("NOT_SET_ANYWHERE_749", SourceKeyring, "anthropic"); err != nil || v != "" {
		t.Fatalf("Personal(anthropic) = %q %v, want no key", v, err)
	}
}

func TestNoCredentialFolderMeansTheKeychain(t *testing.T) {
	store := fakeKeychain(t)
	store["provider:openai"] = "keychain-value"
	// A credential folder of another service, with no agentry-keys in it.
	t.Setenv(EnvCredentialsDirectory, t.TempDir())
	if v, err := Lookup("openai"); err != nil || v != "keychain-value" {
		t.Fatalf("Lookup = %q %v", v, err)
	}
	t.Setenv(EnvCredentialsDirectory, "")
	if _, sealed, _ := SealedAccount("provider:openai"); sealed {
		t.Fatal("sealed with no credential folder")
	}
}

func TestSealedCredentialOfAnotherShapeRefuses(t *testing.T) {
	fakeKeychain(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, CredentialName), []byte("sk-plain"), 0o400); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvCredentialsDirectory, dir)
	if _, err := Lookup("openai"); err == nil || !strings.Contains(err.Error(), "not in a shape") {
		t.Fatalf("Lookup = %v", err)
	}
	if _, err := Present("openai"); err == nil {
		t.Fatal("Present said nothing of an unreadable credential")
	}
}

func TestCollect(t *testing.T) {
	store := fakeKeychain(t)
	store["provider:openai"] = "k1"
	store["provider:typesafe"] = "k2"
	got, err := Collect([]string{"provider:openai"}, []string{"provider:typesafe", "provider:tavily", "provider:openai"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["provider:openai"] != "k1" || got["provider:typesafe"] != "k2" {
		t.Fatalf("Collect = %v", got)
	}
	if _, err := Collect([]string{"provider:anthropic"}, nil); err == nil || !strings.Contains(err.Error(), "agentry setup") {
		t.Fatalf("a missing required key = %v", err)
	}
	// Collect reads the keychain, never a sealed credential it runs beside.
	sealedDir(t, map[string]string{"provider:openai": "old-sealed"})
	if got, err := Collect([]string{"provider:openai"}, nil); err != nil || got["provider:openai"] != "k1" {
		t.Fatalf("Collect under a credential = %v %v", got, err)
	}
	t.Setenv(EnvNoKeyring, "1")
	if _, err := Collect([]string{"provider:openai"}, nil); err == nil {
		t.Fatal("Collect read a disabled keychain")
	}
}

func TestSealPlaintextRefusesEmpty(t *testing.T) {
	if _, err := SealPlaintext(map[string]string{"provider:openai": ""}); err == nil {
		t.Fatal("an empty value was sealed")
	}
}
