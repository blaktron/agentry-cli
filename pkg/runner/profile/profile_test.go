// Profile store tests: lifecycle, permissions, the version gate, and the
// remove selector.
package profile

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLifecycle(t *testing.T) {
	home := t.TempDir()
	dir := Dir(home, "demo", "x", "1.0.0", "abcdef123456")

	p, err := Load(dir)
	if err != nil || p != nil {
		t.Fatalf("absent profile: got (%v, %v), want (nil, nil)", p, err)
	}

	created := New("demo/x@1.0.0", "sha256:abc", true, map[string]string{"TOKEN_B": "b", "TOKEN_A": "a"})
	if err := created.Save(dir); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dir, "profile.json"))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("profile.json mode = %v, want 0600", fi.Mode().Perm())
		}
	}

	loaded, err := Load(dir)
	if err != nil || loaded == nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Ref != "demo/x@1.0.0" || !loaded.Keep || loaded.Secrets["TOKEN_A"] != "a" {
		t.Fatalf("roundtrip mismatch: %+v", loaded)
	}
	if got := strings.Join(loaded.SecretNames(), ","); got != "TOKEN_A,TOKEN_B" {
		t.Errorf("secret names = %q", got)
	}

	if err := loaded.Touch(dir); err != nil {
		t.Fatal(err)
	}
	again, _ := Load(dir)
	if again.LastUsedAt == "" {
		t.Error("Touch did not stamp lastUsedAt")
	}

	entries, err := List(home)
	if err != nil || len(entries) != 1 {
		t.Fatalf("list: %v, %d entries", err, len(entries))
	}

	if err := Remove(dir); err != nil {
		t.Fatal(err)
	}
	if p, _ := Load(dir); p != nil {
		t.Fatal("profile survived Remove")
	}
	if err := Remove(dir); err != nil {
		t.Fatalf("removing an absent profile must be fine: %v", err)
	}
}

func TestVersionGate(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "profile.json"),
		[]byte(`{"profileVersion":"0","ref":"a/b@1.0.0","digest":"sha256:x","keep":true,"createdAt":"2026-08-31T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("foreign version accepted: %v", err)
	}
}

func TestMatchesRef(t *testing.T) {
	p := New("demo/x@1.2.0", "sha256:x", true, nil)
	tests := []struct {
		sel  string
		want bool
	}{
		{"demo/x@1.2.0", true},
		{"demo/x", true},
		{"demo/x@1.3.0", false},
		{"demo/y", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := p.MatchesRef(tc.sel); got != tc.want {
			t.Errorf("MatchesRef(%q) = %v, want %v", tc.sel, got, tc.want)
		}
	}
	var nilP *Profile
	if nilP.MatchesRef("demo/x") {
		t.Error("nil profile matched")
	}
}
