package dirpublish

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func entryNames(t *testing.T, dir string) []string {
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

// Replace puts a whole new tree in place of what is at dest and leaves no
// sibling behind; staging clears a sibling abandoned by a writer that
// died, and leaves a fresh one (another writer's, in progress) alone.
func TestReplaceReplacesAndSweeps(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, "rel")
	if err := os.MkdirAll(filepath.Join(dest, "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	abandoned := filepath.Join(parent, ".rel"+stagingInfix+"dead")
	// Another writer's fresh sibling, mid-publish: not abandoned.
	inProgress := filepath.Join(parent, ".rel"+retiredInfix+"live")
	for _, d := range []string{abandoned, inProgress} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * staleSibling)
	if err := os.Chtimes(abandoned, old, old); err != nil {
		t.Fatal(err)
	}

	err := Replace(dest, func(dir string) error {
		if info, err := os.Stat(dir); err != nil || info.Mode().Perm()&^0o755 != 0 || info.Mode().Perm()&0o700 != 0o700 {
			t.Errorf("staging %s: %v %v", dir, info, err)
		}
		if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
			t.Error("an abandoned staging sibling survived")
		}
		return os.WriteFile(filepath.Join(dir, "new"), []byte("x"), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "new")); err != nil {
		t.Fatal("the new tree is not at dest")
	}
	if _, err := os.Stat(filepath.Join(dest, "old")); !os.IsNotExist(err) {
		t.Fatal("the replaced tree survived")
	}
	if got := entryNames(t, parent); len(got) != 2 { // dest and the other writer's sibling
		t.Fatalf("left behind: %v", got)
	}
}

// A fill that fails leaves dest as it was and no staging behind, and its
// own error comes back.
func TestReplaceFillFailureLeavesDest(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, "rel")
	if err := os.MkdirAll(filepath.Join(dest, "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	if err := Replace(dest, func(string) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "old")); err != nil {
		t.Fatal("a failed fill touched dest")
	}
	if got := entryNames(t, parent); len(got) != 1 {
		t.Fatalf("left behind: %v", got)
	}
}

// ReplaceUnless keeps a tree another writer finished while this one was
// building, with whatever that writer has since put in it, when settled
// says so; one settled does not accept is replaced.
func TestReplaceUnlessKeepsASettledTree(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, "rel")
	settled := func() bool { _, err := os.Stat(filepath.Join(dest, "whole")); return err == nil }
	sibling := func(marker string) func(string) error {
		return func(dir string) error {
			// The sibling publishes first and starts using its tree.
			if err := os.MkdirAll(filepath.Join(dest, "plan"), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(dest, marker), nil, 0o644); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, "mine"), nil, 0o644)
		}
	}
	published, err := ReplaceUnless(dest, sibling("whole"), settled)
	if err != nil || published {
		t.Fatalf("published=%v err=%v", published, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "plan")); err != nil {
		t.Fatal("the sibling's tree was swept away")
	}
	if _, err := os.Stat(filepath.Join(dest, "mine")); !os.IsNotExist(err) {
		t.Fatal("the duplicate was published over the sibling's tree")
	}
	if got := entryNames(t, parent); len(got) != 1 {
		t.Fatalf("left behind: %v", got)
	}

	// A tree settled does not accept (a half-built or different one) is
	// replaced.
	if err := os.RemoveAll(dest); err != nil {
		t.Fatal(err)
	}
	published, err = ReplaceUnless(dest, sibling("partial"), settled)
	if err != nil || !published {
		t.Fatalf("published=%v err=%v", published, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "mine")); err != nil {
		t.Fatal("an unsettled tree was kept")
	}
	if got := entryNames(t, parent); len(got) != 1 {
		t.Fatalf("left behind: %v", got)
	}
}

// InFlight names a fresh staging or retired sibling, and neither an
// abandoned one nor an ordinary entry.
func TestInFlight(t *testing.T) {
	parent := t.TempDir()
	for _, n := range []string{".rel" + stagingInfix + "a", ".rel" + retiredInfix + "b", ".rel" + stagingInfix + "old", "1.0.0-abc", ".hidden"} {
		if err := os.Mkdir(filepath.Join(parent, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * staleSibling)
	if err := os.Chtimes(filepath.Join(parent, ".rel"+stagingInfix+"old"), old, old); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(parent)
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = InFlight(e)
	}
	want := map[string]bool{".rel" + stagingInfix + "a": true, ".rel" + retiredInfix + "b": true, ".rel" + stagingInfix + "old": false, "1.0.0-abc": false, ".hidden": false}
	for n, w := range want {
		if got[n] != w {
			t.Errorf("InFlight(%s) = %v, want %v", n, got[n], w)
		}
	}
}
