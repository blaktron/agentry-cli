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

func TestDirShape(t *testing.T) {
	got := Dir("/home/op/.agentry/runner", "demo", "triage", "1.2.0", "abcdef123456")
	want := filepath.Join("/home/op/.agentry/runner", "profiles", "demo", "triage", "1.2.0-abcdef123456")
	if got != want {
		t.Errorf("Dir = %q, want %q", got, want)
	}
}

func TestListSortedAndCorruptTolerated(t *testing.T) {
	home := t.TempDir()
	// Two healthy profiles across namespaces, deliberately created in
	// reverse of their ref order.
	zDir := Dir(home, "zeta", "z", "1.0.0", "zzzzzzzzzzzz")
	if err := New("zeta/z@1.0.0", "sha256:z", true, nil).Save(zDir); err != nil {
		t.Fatal(err)
	}
	aDir := Dir(home, "alpha", "a", "2.0.0", "aaaaaaaaaaaa")
	if err := New("alpha/a@2.0.0", "sha256:a", false, nil).Save(aDir); err != nil {
		t.Fatal(err)
	}
	// One corrupt profile: listing must show it (nil Profile), not skip it
	// or abort the walk.
	badDir := Dir(home, "broken", "b", "1.0.0", "bbbbbbbbbbbb")
	if err := os.MkdirAll(badDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "profile.json"), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}

	entries, err := List(home)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("List returned %d entries, want 3 (corrupt one included)", len(entries))
	}
	// Broken entries sort first (empty ref), then refs ascending.
	if entries[0].Profile != nil || entries[0].Dir != badDir {
		t.Errorf("entries[0] = %+v, want the broken profile with nil Profile", entries[0])
	}
	if entries[1].Profile == nil || entries[1].Profile.Ref != "alpha/a@2.0.0" {
		t.Errorf("entries[1] = %+v", entries[1].Profile)
	}
	if entries[2].Profile == nil || entries[2].Profile.Ref != "zeta/z@1.0.0" {
		t.Errorf("entries[2] = %+v", entries[2].Profile)
	}
}

func TestListEmptyHome(t *testing.T) {
	entries, err := List(t.TempDir()) // no profiles/ dir at all
	if err != nil || len(entries) != 0 {
		t.Fatalf("List of a fresh home = (%v, %v), want empty", entries, err)
	}
}

func TestSaveErrors(t *testing.T) {
	t.Run("profile dir blocked by a file", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := New("a/b@1.0.0", "sha256:x", true, nil).Save(filepath.Join(blocker, "sub"))
		if err == nil || !strings.Contains(err.Error(), "create profile dir") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("profile.json occupied by a directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "profile.json"), 0o700); err != nil {
			t.Fatal(err)
		}
		err := New("a/b@1.0.0", "sha256:x", true, nil).Save(dir)
		if err == nil || !strings.Contains(err.Error(), "write profile") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestRemoveError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions do not refuse removal")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "victim")
	if err := New("a/b@1.0.0", "sha256:x", true, nil).Save(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	if err := Remove(dir); err == nil || !strings.Contains(err.Error(), "remove profile") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadErrors(t *testing.T) {
	t.Run("unreadable profile.json", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "profile.json"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "read profile") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("corrupt json", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "profile.json"), []byte("{nope"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "parse") {
			t.Fatalf("err = %v", err)
		}
	})
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
