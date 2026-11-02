package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runhost/job"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "releases"), filepath.Join(dir, KeyFile))
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestAStoredReleaseComesBackAndIsNotPlaintextOnDisk(t *testing.T) {
	s, dir := open(t)
	archive := bytes.Repeat([]byte("the archive's own bytes "), 100)
	rel := job.Release{Namespace: "ns", Name: "n", Version: "1.0.0", Digest: digestA, State: job.StateVerified}
	if s.Has(digestA) {
		t.Fatal("an empty store holds a release")
	}
	if err := s.Put(rel, archive); err != nil {
		t.Fatal(err)
	}
	if !s.Has(digestA) || s.Has(digestB) {
		t.Fatal("Has")
	}
	got, b, err := s.Get(digestA)
	if err != nil || got.Name != "n" || !bytes.Equal(b, archive) {
		t.Fatalf("Get = %+v, %d bytes, %v", got, len(b), err)
	}
	entry, _ := os.ReadFile(filepath.Join(dir, "releases", strings.TrimPrefix(digestA, "sha256:")+".release"))
	if bytes.Contains(entry, []byte("the archive's own bytes")) || bytes.Contains(entry, []byte(`"namespace"`)) {
		t.Error("the entry holds plaintext")
	}
	if fi, err := os.Stat(filepath.Join(dir, KeyFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the key file: %v %v", fi.Mode(), err)
	}
	// The same key opens it again.
	s2, err := Open(filepath.Join(dir, "releases"), filepath.Join(dir, KeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s2.Get(digestA); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(digestB); !errors.Is(err, ErrNotStored) {
		t.Errorf("a release not stored: %v", err)
	}
}

// A changed entry, and an entry moved under another digest's name, do not
// open.
func TestATamperedStoredReleaseIsRefused(t *testing.T) {
	s, dir := open(t)
	if err := s.Put(job.Release{Digest: digestA}, []byte("archive")); err != nil {
		t.Fatal(err)
	}
	pa := filepath.Join(dir, "releases", strings.TrimPrefix(digestA, "sha256:")+".release")
	pb := filepath.Join(dir, "releases", strings.TrimPrefix(digestB, "sha256:")+".release")
	b, _ := os.ReadFile(pa)
	if err := os.WriteFile(pb, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(digestB); !errors.Is(err, ErrDamaged) {
		t.Errorf("a moved entry: %v", err)
	}
	b[len(b)-3] ^= 1
	if err := os.WriteFile(pa, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(digestA); !errors.Is(err, ErrDamaged) {
		t.Errorf("a changed entry: %v", err)
	}
	if _, _, err := s.Get("sha256:../../etc"); err == nil {
		t.Error("a digest that is not one was read")
	}
}

// A named entry comes back, is ciphertext on disk, does not open under
// another name or once changed, and is gone once removed.
func TestAnEntryIsSealedToItsName(t *testing.T) {
	s, dir := open(t)
	if err := s.PutEntry("credits", []byte("agy_the-token")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Entry("credits")
	if err != nil || string(got) != "agy_the-token" {
		t.Fatalf("entry = %q, %v", got, err)
	}
	p := filepath.Join(dir, "releases", "credits.entry")
	raw, err := os.ReadFile(p)
	if err != nil || bytes.Contains(raw, []byte("agy_the-token")) {
		t.Fatalf("the entry is plaintext on disk, or missing: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "releases", "other.entry"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Entry("other"); !errors.Is(err, ErrDamaged) {
		t.Fatalf("an entry renamed opened: %v", err)
	}
	raw[len(raw)-1] ^= 1
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Entry("credits"); !errors.Is(err, ErrDamaged) {
		t.Fatalf("a changed entry opened: %v", err)
	}
	if err := s.RemoveEntry("credits"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Entry("credits"); !errors.Is(err, ErrNoEntry) {
		t.Fatalf("a removed entry: %v", err)
	}
	for _, bad := range []string{"", "Credits", "../x", "a/b", strings.Repeat("a", 33)} {
		if err := s.PutEntry(bad, nil); err == nil {
			t.Errorf("entry name %q was taken", bad)
		}
	}
}
