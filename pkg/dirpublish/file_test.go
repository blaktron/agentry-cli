package dirpublish

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// CreateFile puts the file in place when nothing is there, never replaces
// a file that is, and leaves no staging file behind either way.
func TestCreateFileNeverReplaces(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, "policy.yaml")
	created, err := CreateFile(dest, []byte("first"), 0o644)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	created, err = CreateFile(dest, []byte("second"), 0o644)
	if err != nil || created {
		t.Fatalf("second write: created=%v err=%v", created, err)
	}
	if b, _ := os.ReadFile(dest); string(b) != "first" {
		t.Fatalf("dest = %q, want the first writer's file", b)
	}
	if got := entryNames(t, parent); len(got) != 1 {
		t.Fatalf("left behind: %v", got)
	}
}

// Concurrent creators of one path: exactly one creates it, every reader
// that finds the file finds it whole, and nothing is left beside it.
func TestCreateFileConcurrent(t *testing.T) {
	data := bytes.Repeat([]byte("policy line\n"), 4096)
	for range 50 {
		parent := t.TempDir()
		dest := filepath.Join(parent, "policy.yaml")
		var wg sync.WaitGroup
		var mu sync.Mutex
		creators := 0
		start := make(chan struct{})
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, err := os.Stat(dest); err == nil {
					if b, _ := os.ReadFile(dest); !bytes.Equal(b, data) {
						t.Errorf("a reader saw %d of %d bytes", len(b), len(data))
					}
					return
				}
				created, err := CreateFile(dest, data, 0o644)
				if err != nil {
					t.Error(err)
					return
				}
				if created {
					mu.Lock()
					creators++
					mu.Unlock()
				}
				if b, _ := os.ReadFile(dest); !bytes.Equal(b, data) {
					t.Errorf("after CreateFile a reader saw %d of %d bytes", len(b), len(data))
				}
			}()
		}
		close(start)
		wg.Wait()
		if creators != 1 {
			t.Fatalf("%d writers created the file, want 1", creators)
		}
		if got := entryNames(t, parent); len(got) != 1 {
			t.Fatalf("left behind: %v", got)
		}
	}
}

// ReplaceFile replaces the file whole: a reader during the replacement
// reads the old file or the new one, never a truncated one.
func TestReplaceFileReadersSeeWholeFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows refuses a rename over a file a reader holds open; ReplaceFile retries that, but a reader in a tight loop outlasts the retries")
	}
	parent := t.TempDir()
	dest := filepath.Join(parent, "policy.yaml")
	a, b := bytes.Repeat([]byte("a"), 64<<10), bytes.Repeat([]byte("b"), 64<<10)
	if err := ReplaceFile(dest, a, 0o644); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			got, err := os.ReadFile(dest)
			if err != nil || (!bytes.Equal(got, a) && !bytes.Equal(got, b)) {
				t.Errorf("reader saw %d bytes, err %v", len(got), err)
				return
			}
		}
	}()
	for i := range 200 {
		next := a
		if i%2 == 0 {
			next = b
		}
		if err := ReplaceFile(dest, next, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	if got := entryNames(t, parent); len(got) != 1 {
		t.Fatalf("left behind: %v", got)
	}
}

// Staging a file clears an abandoned staging file beside dest and leaves
// a fresh one (another writer's, in progress) alone; InFlight names the
// fresh one.
func TestStageFileSweepsAbandonedFiles(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, "policy.yaml")
	abandoned := filepath.Join(parent, ".policy.yaml"+stagingInfix+"dead")
	live := filepath.Join(parent, ".policy.yaml"+stagingInfix+"live")
	for _, f := range []string{abandoned, live} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * staleSibling)
	if err := os.Chtimes(abandoned, old, old); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFile(dest, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Error("an abandoned staging file survived")
	}
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		if e.Name() == filepath.Base(live) && !InFlight(e) {
			t.Error("a fresh staging file is not in flight")
		}
	}
	if got := entryNames(t, parent); len(got) != 2 { // dest and the live staging file
		t.Fatalf("left: %v", got)
	}
}

// Without hard links CreateFile still never replaces a file that is
// there: it creates dest exclusively in place.
func TestCreateFileWithoutHardLinks(t *testing.T) {
	saved := link
	link = func(string, string) error { return &os.LinkError{Op: "link", Err: errors.ErrUnsupported} }
	t.Cleanup(func() { link = saved })
	parent := t.TempDir()
	dest := filepath.Join(parent, "policy.yaml")
	created, err := CreateFile(dest, []byte("first"), 0o644)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	created, err = CreateFile(dest, []byte("second"), 0o644)
	if err != nil || created {
		t.Fatalf("second write: created=%v err=%v", created, err)
	}
	if b, _ := os.ReadFile(dest); string(b) != "first" {
		t.Fatalf("dest = %q, want the first writer's file", b)
	}
	if got := entryNames(t, parent); len(got) != 1 {
		t.Fatalf("left behind: %v", got)
	}
}

// ReplaceFile writes through a symbolic link to the file it names and
// keeps an existing file's permission bits, as a write in place did.
func TestReplaceFileKeepsLinkAndMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links and permission bits are POSIX here")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "policy.yaml")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "policy.yaml")
	if err := os.Symlink(target, dest); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFile(dest, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(dest); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced by a file: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(target); string(b) != "new" {
		t.Fatalf("target = %q", b)
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600 kept", fi.Mode().Perm())
	}
}

// refuseReplace makes a rename over an existing file fail, as Windows
// refuses one while a scanner still maps a file that was just written,
// for the rest of the test.
func refuseReplace(t *testing.T) {
	t.Helper()
	old := replace
	replace = func(from, to string) error {
		if _, err := os.Lstat(to); err == nil {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: errors.New("Access is denied.")}
		}
		return os.Rename(from, to)
	}
	t.Cleanup(func() { replace = old })
}

// A file a rename may not replace is retired aside and the new file put
// in the name it leaves free; neither the staging file nor the retired
// one is left beside it.
func TestReplaceFileRetiresAFileItCannotReplace(t *testing.T) {
	refuseReplace(t)
	dir := t.TempDir()
	dest := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(dest, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFile(dest, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dest); string(b) != "new" {
		t.Fatalf("dest = %q", b)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(dest); fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v, want 0600 kept", fi.Mode().Perm())
		}
	}
	if names := dirNames(t, dir); len(names) != 1 || names[0] != "policy.yaml" {
		t.Fatalf("left beside it: %v", names)
	}
}

// Only a regular file is retired: a directory at dest is refused and left
// whole, and the staging file is removed.
func TestReplaceFileRetiresOnlyAFile(t *testing.T) {
	refuseReplace(t)
	dir := t.TempDir()
	dest := filepath.Join(dir, "policy.yaml")
	if err := os.MkdirAll(filepath.Join(dest, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFile(dest, []byte("new"), 0o644); err == nil {
		t.Fatal("a directory was replaced by a file")
	}
	if fi, err := os.Stat(filepath.Join(dest, "keep")); err != nil || !fi.IsDir() {
		t.Fatalf("the directory was not left whole: %v", err)
	}
	if names := dirNames(t, dir); len(names) != 1 || names[0] != "policy.yaml" {
		t.Fatalf("left beside it: %v", names)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
