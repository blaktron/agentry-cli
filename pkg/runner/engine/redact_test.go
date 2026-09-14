package engine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestRedactorErrKeepsClassAndIdentity: an error that carries a value is
// re-issued masked under the same exit class; one that carries none is
// returned as is, so errors.Is still sees it.
func TestRedactorErrKeepsClassAndIdentity(t *testing.T) {
	// B is seven characters and stays (a switch, not a credential); C is
	// eight and is masked: the boundary of redactMinLen.
	r := newRedactor("tok-0123456789", map[string]string{"A": "sekret-value-1234", "B": "seven77", "C": "eight888"}, []string{"sk-host-key-9876"})
	base := errors.New("gateway said: token=tok-0123456789 A=sekret-value-1234 B=seven77 C=eight888 K=sk-host-key-9876")
	got := r.Err(Classify(ExitGateway, base))
	var ce *ClassifiedError
	if !errors.As(got, &ce) || ce.Code != ExitGateway {
		t.Fatalf("class lost: %v", got)
	}
	if want := "gateway said: token=[redacted] A=[redacted] B=seven77 C=[redacted] K=[redacted]"; got.Error() != want {
		t.Errorf("Err = %q, want %q", got.Error(), want)
	}
	clean := Classify(ExitHarness, errors.New("nothing here"))
	if r.Err(clean) != clean {
		t.Error("an error with nothing to mask must be returned as is")
	}
	if r.Err(nil) != nil || (*redactor)(nil).Err(clean) != clean {
		t.Error("nil redactor or nil error must pass through")
	}
}

// TestRedactorFile: a receipt file with a value is rewritten masked; one
// without is left byte-identical (its mtime included: no write).
func TestRedactorFile(t *testing.T) {
	r := newRedactor("tok-0123456789", map[string]string{"A": "sekret-value-1234"}, nil)
	dir := t.TempDir()
	leaky := filepath.Join(dir, "agent-stderr.log")
	if err := os.WriteFile(leaky, []byte("boot\nAuthorization: Bearer sekret-value-1234\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.File(leaky)
	if b, _ := os.ReadFile(leaky); string(b) != "boot\nAuthorization: Bearer [redacted]\n" {
		t.Errorf("leaky file = %q", b)
	}
	clean := filepath.Join(dir, "clean.log")
	if err := os.WriteFile(clean, []byte("boot\nall fine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(clean)
	r.File(clean)
	after, _ := os.Stat(clean)
	if b, _ := os.ReadFile(clean); string(b) != "boot\nall fine\n" || !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("clean file was rewritten: %q", b)
	}
	r.File(filepath.Join(dir, "absent.log")) // no receipt, no panic
}
