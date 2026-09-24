package compile

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// TestIntegrityKeyIsCreatedOnceAndShared: concurrent first runs of one
// runner home agree on one key, it is private to its owner, and later
// calls read it back.
func TestIntegrityKeyIsCreatedOnceAndShared(t *testing.T) {
	home := filepath.Join(t.TempDir(), "runner")
	const n = 8
	keys := make([][]byte, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			keys[i], errs[i] = IntegrityKey(home)
		}()
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("IntegrityKey: %v", errs[i])
		}
		if len(keys[i]) != integrityKeyLen || !bytes.Equal(keys[i], keys[0]) {
			t.Fatalf("concurrent first runs disagree on the key")
		}
	}
	again, err := IntegrityKey(home)
	if err != nil || !bytes.Equal(again, keys[0]) {
		t.Fatalf("the key was not read back: %v", err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(home, IntegrityKeyFile))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("key file mode %v (err %v), want 0600", fi.Mode().Perm(), err)
		}
	}
	entries, _ := os.ReadDir(home)
	if len(entries) != 1 {
		t.Fatalf("temporary key files were left behind: %v", entries)
	}
}

// TestReadInstalledPlanTellsTheCasesApart pins the four outcomes a reader
// acts on: no plan, no usable record (recompile), a record that vouches
// against the bytes (refuse), and a record that vouches for them.
func TestReadInstalledPlanTellsTheCasesApart(t *testing.T) {
	key := bytes.Repeat([]byte{1}, integrityKeyLen)
	other := bytes.Repeat([]byte{2}, integrityKeyLen)
	dir := t.TempDir()
	if _, err := ReadInstalledPlan(key, dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("no plan: %v", err)
	}
	plan := []byte(`{"planVersion":"2"}` + "\n")
	if err := os.WriteFile(filepath.Join(dir, "plan.json"), plan, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInstalledPlan(key, dir); !errors.Is(err, ErrNoIntegrityRecord) {
		t.Fatalf("no record: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, IntegrityFile), []byte("garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInstalledPlan(key, dir); !errors.Is(err, ErrNoIntegrityRecord) {
		t.Fatalf("malformed record: %v", err)
	}
	if err := Seal(other, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInstalledPlan(key, dir); !errors.Is(err, ErrNoIntegrityRecord) {
		t.Fatalf("record under another key: %v", err)
	}
	if err := Seal(key, dir); err != nil {
		t.Fatal(err)
	}
	if b, err := ReadInstalledPlan(key, dir); err != nil || !bytes.Equal(b, plan) {
		t.Fatalf("intact plan: %v", err)
	}
	// One byte of whitespace is an edit: the record covers the exact bytes.
	if err := os.WriteFile(filepath.Join(dir, "plan.json"), append(plan, ' '), 0o644); err != nil {
		t.Fatal(err)
	}
	var tampered *TamperedPlanError
	if _, err := ReadInstalledPlan(key, dir); !errors.As(err, &tampered) {
		t.Fatalf("edited plan: %v", err)
	}
}

// TestCompileWritesTheRecordOnlyWithAKey: the record is part of a
// compile run from the runner home, and absent from a keyless compile
// (goldens, other callers), which a reader then recompiles.
func TestCompileWritesTheRecordOnlyWithAKey(t *testing.T) {
	key := bytes.Repeat([]byte{3}, integrityKeyLen)
	m := parseManifest(t, fullCapsYAML)
	ws := workspaceFor(t, m, nil)
	for _, withKey := range []bool{false, true} {
		in := Inputs{
			Manifest: m, Digest: "sha256:0123456789abcdef", Source: "local", WorkspaceDir: ws,
			Policy: testPolicy(), PolicySHA: "fixedpolicysha", RunnerImage: "agentry-runner:test",
		}
		if withKey {
			in.IntegrityKey = key
		}
		dir := t.TempDir()
		if _, err := Compile(in, dir); err != nil {
			t.Fatal(err)
		}
		_, err := ReadInstalledPlan(key, dir)
		if withKey && err != nil {
			t.Fatalf("keyed compile: %v", err)
		}
		if !withKey && !errors.Is(err, ErrNoIntegrityRecord) {
			t.Fatalf("keyless compile: %v", err)
		}
	}
}
