package hostlink

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The shared files folder is also the tools server's, where a declared
// command may run beside the host link and turn a folder the host link has
// just used into a link out of the folder. These tests swap a folder for such
// a link in exactly that gap (server.afterCheck) and hold that the copy is
// refused and that nothing outside is read or written. They run on whatever
// container runtime the test binary runs under.

const outsideSecret = "OUTSIDE-SECRET"

// swapFixture is a host link server over a shared folder holding notes/, and
// a folder outside it that notes/ is swapped for a link to.
type swapFixture struct {
	s       *server
	files   string
	outside string
	local   string
}

func newSwapFixture(t *testing.T) *swapFixture {
	t.Helper()
	fx := newFixture(t, keyTarget())
	s, err := newServer(fx.cfg, io.Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.close)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "today.md"), []byte(outsideSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(fx.files, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx.files, "notes", "today.md"), []byte("inside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(fx.cfg.TmpDir, "local")
	if err := os.WriteFile(local, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return &swapFixture{s: s, files: fx.files, outside: outside, local: local}
}

// swapOnce makes dir (a folder of the shared folder) a link out of it the
// first time the gap is reached.
func (fx *swapFixture) swapOnce(t *testing.T, dir string) {
	t.Helper()
	swapped := false
	fx.s.afterCheck = func() {
		if swapped {
			return
		}
		swapped = true
		if err := os.RemoveAll(filepath.Join(fx.files, dir)); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(fx.outside, filepath.Join(fx.files, dir)); err != nil {
			t.Skipf("no symlinks here: %v", err)
		}
	}
}

func (fx *swapFixture) outsideHolds(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(fx.outside)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if !slices.Equal(got, []string{"today.md"}) {
		t.Fatalf("the folder outside holds %v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(fx.outside, "today.md")); string(b) != outsideSecret+"\n" {
		t.Fatalf("a file outside was changed: %q", b)
	}
}

func TestCopyOutRefusesAFolderSwappedForALinkAfterTheCheck(t *testing.T) {
	fx := newSwapFixture(t)
	fx.swapOnce(t, "notes")
	n, err := fx.s.copyOut("notes/today.md", fx.local)
	if err == nil {
		t.Fatalf("copyOut through a swapped link copied %d bytes", n)
	}
	if b, _ := os.ReadFile(fx.local); len(b) != 0 {
		t.Fatalf("the copy holds %q", b)
	}
	fx.outsideHolds(t)
}

func TestCopyInRefusesAFolderSwappedForALinkAfterTheCheck(t *testing.T) {
	src := func(t *testing.T, fx *swapFixture) {
		if err := os.WriteFile(fx.local, []byte("payload\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("a folder that was there", func(t *testing.T) {
		fx := newSwapFixture(t)
		src(t, fx)
		fx.swapOnce(t, "notes")
		if n, err := fx.s.copyIn(fx.local, "notes/new.md"); err == nil {
			t.Fatalf("copyIn through a swapped link copied %d bytes", n)
		}
		fx.outsideHolds(t)
	})
	t.Run("a folder it made", func(t *testing.T) {
		fx := newSwapFixture(t)
		src(t, fx)
		fx.swapOnce(t, "deep")
		if n, err := fx.s.copyIn(fx.local, "deep/new.md"); err == nil {
			t.Fatalf("copyIn through a swapped link copied %d bytes", n)
		}
		fx.outsideHolds(t)
	})
	t.Run("a folder below one it made", func(t *testing.T) {
		fx := newSwapFixture(t)
		src(t, fx)
		fx.swapOnce(t, "deep")
		if n, err := fx.s.copyIn(fx.local, "deep/more/new.md"); err == nil {
			t.Fatalf("copyIn through a swapped link copied %d bytes", n)
		}
		fx.outsideHolds(t)
	})
}

func TestCopyOutSaysWhatIsNotAFile(t *testing.T) {
	fx := newSwapFixture(t)
	if err := os.Symlink(filepath.Join(fx.files, "notes", "today.md"), filepath.Join(fx.files, "alias.md")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	for _, rel := range []string{"alias.md", "notes", "missing.md", "notes/today.md/below"} {
		_, err := fx.s.copyOut(rel, fx.local)
		if err == nil || !strings.Contains(err.Error(), "is not a file") {
			t.Errorf("copyOut(%q) = %v, want %q", rel, err, "is not a file")
		}
	}
}
