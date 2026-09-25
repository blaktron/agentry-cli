package dirpublish

import (
	"bytes"
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
