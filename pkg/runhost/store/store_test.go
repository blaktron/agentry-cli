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
