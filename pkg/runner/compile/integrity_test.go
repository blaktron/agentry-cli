package compile

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
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

// recompileInto stands in for ensurePlan in another process: it clears the
// plan directory and writes a new plan.json and then its record, in that
// order, as Compile does.
func recompileInto(t *testing.T, key []byte, dir string, plan []byte) {
	t.Helper()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plan.json"), plan, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeIntegrityRecord(key, dir, plan); err != nil {
		t.Fatal(err)
	}
}

// TestReadInstalledPlanSurvivesAConcurrentRecompile: a recompile of the
// same plan directory landing between the reader's reads of plan.json and
// of its record paired one compile's bytes with another compile's record,
// and a run was refused as tampered although nothing had been edited. The
// recompile is interleaved after each read in turn; the reader must return
// one compile's plan, never a TamperedPlanError. A real edit, which stays
// put, is still refused.
func TestReadInstalledPlanSurvivesAConcurrentRecompile(t *testing.T) {
	key := bytes.Repeat([]byte{4}, integrityKeyLen)
	planA := []byte(`{"planVersion":"2","compiledBy":"cli"}` + "\n")
	planB := []byte(`{"planVersion":"2","compiledBy":"desktop"}` + "\n")
	for _, after := range []string{"record", "plan"} {
		t.Run("recompile after the "+after+" read", func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "plan", "autonomous")
			recompileInto(t, key, dir, planA)
			fired := false
			readHook = func(read string) {
				if read == after && !fired {
					fired = true
					recompileInto(t, key, dir, planB)
				}
			}
			t.Cleanup(func() { readHook = nil })
			b, err := ReadInstalledPlan(key, dir)
			if !fired {
				t.Fatalf("the recompile was never interleaved after the %s read", after)
			}
			var tampered *TamperedPlanError
			if errors.As(err, &tampered) {
				t.Fatalf("a concurrent recompile was reported as tampering: %v", err)
			}
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if !bytes.Equal(b, planA) && !bytes.Equal(b, planB) {
				t.Fatalf("read returned neither compile's plan: %q", b)
			}
		})
	}
	t.Run("a real edit is still refused", func(t *testing.T) {
		dir := t.TempDir()
		recompileInto(t, key, dir, planA)
		if err := os.WriteFile(filepath.Join(dir, "plan.json"), append(planA, ' '), 0o644); err != nil {
			t.Fatal(err)
		}
		var tampered *TamperedPlanError
		if _, err := ReadInstalledPlan(key, dir); !errors.As(err, &tampered) {
			t.Fatalf("edited plan: %v", err)
		}
	})
}

// TestConcurrentCompilesLeaveOneWholePlan: compiles of the same plan
// directory running at once, as the CLI and the desktop do when started
// together, could write into one directory and leave one compile's
// plan.json beside another's record, a pair no reader can tell from an
// edit. Each compile publishes a whole directory, so readers running
// alongside never see a tampered plan, and the directory left at the end is
// one compile's, record, plan and files alike.
func TestConcurrentCompilesLeaveOneWholePlan(t *testing.T) {
	key := bytes.Repeat([]byte{5}, integrityKeyLen)
	m := parseManifest(t, fullCapsYAML)
	ws := workspaceFor(t, m, nil)
	dir := filepath.Join(t.TempDir(), "plan", "autonomous")
	compileAs := func(by string) error {
		_, err := Compile(Inputs{
			Manifest: m, Digest: "sha256:0123456789abcdef", Source: "local", WorkspaceDir: ws,
			Policy: testPolicy(), PolicySHA: "fixedpolicysha", RunnerImage: "agentry-runner:test",
			CompiledBy: by, IntegrityKey: key,
		}, dir)
		return err
	}
	if err := compileAs("first"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	stop := make(chan struct{})
	for i := range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				var tampered *TamperedPlanError
				if _, err := ReadInstalledPlan(key, dir); errors.As(err, &tampered) {
					errs <- fmt.Errorf("reader %d: %w", i, err)
					return
				}
			}
		})
	}
	var writers sync.WaitGroup
	for i := range 8 {
		writers.Go(func() {
			for j := range 5 {
				if err := compileAs(fmt.Sprintf("writer-%d-%d", i, j)); err != nil {
					errs <- err
				}
			}
		})
	}
	writers.Wait()
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	b, err := ReadInstalledPlan(key, dir)
	if err != nil {
		t.Fatalf("the plan left behind: %v", err)
	}
	p, err := spec.ParsePlan(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyFiles(p, dir); err != nil {
		t.Fatalf("the plan left behind is not one compile's: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "autonomous" {
			t.Errorf("a compile left %s beside the plan", e.Name())
		}
	}
}
