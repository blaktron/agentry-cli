package beneath

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
}

// room is a directory to hold names beneath, and a folder outside it that a
// link swapped into the room points at.
type room struct {
	dir, outside string
	d            *Dir
}

func newRoom(t *testing.T, wantBeneath bool) room {
	t.Helper()
	r := room{dir: t.TempDir(), outside: t.TempDir()}
	write(t, filepath.Join(r.outside, "secret.txt"), "outside")
	d, err := Open(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if d.Beneath() != wantBeneath {
		t.Fatalf("Beneath() = %v, want %v", d.Beneath(), wantBeneath)
	}
	r.d = d
	return r
}

// swap makes notes a link out of the room, as a process that can write the
// room does between a name's check and its use.
func (r room) swap(t *testing.T) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(r.dir, "notes")); err != nil {
		t.Fatal(err)
	}
	symlink(t, r.outside, filepath.Join(r.dir, "notes"))
}

func (r room) outsideHolds(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(r.outside)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestANameIsHeldBeneathTheDirectory(t *testing.T) {
	eachMode(t, func(t *testing.T, wantBeneath bool) {
		r := newRoom(t, wantBeneath)
		write(t, filepath.Join(r.dir, "a", "b.txt"), "hello")

		f, err := r.d.Open("a/b.txt")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(f)
		_ = f.Close()
		if string(b) != "hello" {
			t.Fatalf("read %q", b)
		}
		for _, rel := range []string{"a/../a/b.txt", "./a/b.txt"} {
			f, err := r.d.Open(rel)
			if err != nil {
				t.Fatalf("Open(%q): %v", rel, err)
			}
			_ = f.Close()
		}
		for _, rel := range []string{"../x", "a/../../x", "/etc/passwd"} {
			if f, err := r.d.Open(rel); err == nil {
				_ = f.Close()
				t.Fatalf("Open(%q) left the directory", rel)
			}
		}
		if _, err := r.d.Open("missing"); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("Open(missing) = %v, want not exist", err)
		}
	})
}

func TestADirectoryIsMadeChangedRenamedAndRemovedBeneath(t *testing.T) {
	eachMode(t, func(t *testing.T, wantBeneath bool) {
		r := newRoom(t, wantBeneath)
		if err := r.d.Mkdir("d", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := r.d.Mkdir("d", 0o755); !errors.Is(err, fs.ErrExist) {
			t.Fatalf("Mkdir of a name taken = %v, want exist", err)
		}
		if err := r.d.Mkdir("d/e", 0o700); err != nil {
			t.Fatal(err)
		}
		if err := r.d.Chmod("d/e", 0o750); err != nil {
			t.Fatal(err)
		}
		if fi, err := os.Stat(filepath.Join(r.dir, "d", "e")); err != nil || fi.Mode().Perm() != 0o750 {
			t.Fatalf("mode = %v, %v", fi, err)
		}
		f, err := r.d.OpenFile("d/e/f.txt", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString("x")
		_ = f.Close()
		if _, err := r.d.OpenFile("d/e/f.txt", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640); !errors.Is(err, fs.ErrExist) {
			t.Fatalf("O_EXCL over a name taken = %v, want exist", err)
		}
		if err := r.d.Rename("d/e/f.txt", "d/g.txt"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(r.dir, "d", "g.txt")); err != nil {
			t.Fatal(err)
		}
		if err := r.d.Remove("d"); err == nil {
			t.Fatal("Remove took a directory that is not empty")
		}
		for _, rel := range []string{"d/g.txt", "d/e", "d"} {
			if err := r.d.Remove(rel); err != nil {
				t.Fatalf("Remove(%q): %v", rel, err)
			}
		}
		if err := r.d.Remove("d"); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("Remove of a name gone = %v, want not exist", err)
		}
		if _, err := os.Lstat(filepath.Join(r.dir, "d")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatal("d is still there")
		}
	})
}

func TestStatDescribesAPipeWithoutWaitingForIt(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		t.Skip("no named pipes here")
	}
	eachMode(t, func(t *testing.T, wantBeneath bool) {
		r := newRoom(t, wantBeneath)
		if err := mkfifo(filepath.Join(r.dir, "pipe")); err != nil {
			t.Skipf("no named pipes here: %v", err)
		}
		done := make(chan fs.FileInfo, 1)
		go func() {
			fi, _ := r.d.Stat("pipe")
			done <- fi
		}()
		select {
		case fi := <-done:
			if fi == nil || fi.Mode()&fs.ModeNamedPipe == 0 {
				t.Fatalf("Stat(pipe) = %v", fi)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Stat waited for a pipe")
		}
	})
}

// A link inside the directory to another place inside it is followed; one
// out of it is not, whether the link is the last element or a directory on
// the way, and absolute or relative.
func TestALinkIsFollowedOnlyWhileItStaysBeneath(t *testing.T) {
	eachMode(t, func(t *testing.T, wantBeneath bool) {
		r := newRoom(t, wantBeneath)
		write(t, filepath.Join(r.dir, "real", "x.txt"), "inside")
		symlink(t, "real", filepath.Join(r.dir, "near"))
		symlink(t, filepath.Join(r.outside, "secret.txt"), filepath.Join(r.dir, "abs-file"))
		symlink(t, r.outside, filepath.Join(r.dir, "abs-dir"))
		rel, err := filepath.Rel(r.dir, filepath.Join(r.outside, "secret.txt"))
		if err != nil {
			t.Fatal(err)
		}
		symlink(t, rel, filepath.Join(r.dir, "rel-file"))

		f, err := r.d.Open("near/x.txt")
		if err != nil {
			t.Fatalf("a link to a place beneath was refused: %v", err)
		}
		_ = f.Close()
		for _, name := range []string{"abs-file", "abs-dir/secret.txt", "rel-file"} {
			if f, err := r.d.Open(name); err == nil {
				_ = f.Close()
				t.Errorf("Open(%q) followed a link out of the directory", name)
			}
		}
		if f, err := r.d.OpenFile("abs-file", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644); err == nil {
			_ = f.Close()
			t.Error("a write followed a link out of the directory")
		}
		if b, _ := os.ReadFile(filepath.Join(r.outside, "secret.txt")); string(b) != "outside" {
			t.Errorf("the file outside holds %q", b)
		}
	})
}

// A directory swapped for a link out of the room is refused by every way of
// naming something beneath it, and nothing outside changes.
func TestADirectorySwappedForALinkOutIsRefusedByEveryCall(t *testing.T) {
	calls := map[string]func(d *Dir) error{
		"Open":     func(d *Dir) error { return closeFile(d.Open("notes/secret.txt")) },
		"OpenFile": func(d *Dir) error { return closeFile(d.OpenFile("notes/new.txt", os.O_CREATE|os.O_WRONLY, 0o644)) },
		"OpenFile on a name that is there": func(d *Dir) error {
			return closeFile(d.OpenFile("notes/secret.txt", os.O_WRONLY|os.O_TRUNC, 0o644))
		},
		"Stat":        func(d *Dir) error { _, err := d.Stat("notes/secret.txt"); return err },
		"Mkdir":       func(d *Dir) error { return d.Mkdir("notes/sub", 0o755) },
		"Chmod":       func(d *Dir) error { return d.Chmod("notes/secret.txt", 0o777) },
		"Remove":      func(d *Dir) error { return d.Remove("notes/secret.txt") },
		"Rename from": func(d *Dir) error { return d.Rename("notes/secret.txt", "taken.txt") },
		"Rename into": func(d *Dir) error { return d.Rename("mine.txt", "notes/taken.txt") },
		"Lchown":      func(d *Dir) error { return d.Lchown("notes/secret.txt", os.Getuid(), os.Getgid()) },
		"OpenNoLinks": func(d *Dir) error { return closeFile(d.OpenNoLinks("notes/secret.txt")) },
		"FS":          func(d *Dir) error { return closeFile(d.FS().Open("notes/secret.txt")) },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			eachMode(t, func(t *testing.T, wantBeneath bool) {
				r := newRoom(t, wantBeneath)
				write(t, filepath.Join(r.dir, "mine.txt"), "mine")
				write(t, filepath.Join(r.dir, "notes", "secret.txt"), "inside")
				r.swap(t)
				if err := call(r.d); err == nil {
					t.Fatal("the call went through a link swapped in for a directory")
				}
				if got := r.outsideHolds(t); !slices.Equal(got, []string{"secret.txt"}) {
					t.Fatalf("the folder outside holds %v", got)
				}
				if b, _ := os.ReadFile(filepath.Join(r.outside, "secret.txt")); string(b) != "outside" {
					t.Fatalf("the file outside holds %q", b)
				}
				if fi, err := os.Stat(filepath.Join(r.outside, "secret.txt")); err != nil || fi.Mode().Perm() != 0o644 {
					t.Fatalf("the file outside is %v, %v", fi, err)
				}
				if _, err := os.Stat(filepath.Join(r.dir, "mine.txt")); err != nil {
					t.Fatalf("a file beneath was taken: %v", err)
				}
			})
		})
	}
}

func closeFile(f interface{ Close() error }, err error) error {
	if err != nil {
		return err
	}
	return f.Close()
}

func TestRemoveTakesALinkAndNotWhatItPointsTo(t *testing.T) {
	eachMode(t, func(t *testing.T, wantBeneath bool) {
		r := newRoom(t, wantBeneath)
		symlink(t, r.outside, filepath.Join(r.dir, "out"))
		if err := r.d.Remove("out"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(r.dir, "out")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatal("the link is still there")
		}
		if got := r.outsideHolds(t); !slices.Equal(got, []string{"secret.txt"}) {
			t.Fatalf("the folder outside holds %v", got)
		}
	})
}

func TestOpenNoLinksRefusesALinkAnywhereInThePath(t *testing.T) {
	eachMode(t, func(t *testing.T, wantBeneath bool) {
		r := newRoom(t, wantBeneath)
		write(t, filepath.Join(r.dir, "real", "x.txt"), "inside")
		symlink(t, "real", filepath.Join(r.dir, "near"))
		symlink(t, "x.txt", filepath.Join(r.dir, "real", "last"))
		symlink(t, "nowhere", filepath.Join(r.dir, "dangling"))

		f, err := r.d.OpenNoLinks("real/x.txt")
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		for _, rel := range []string{"near/x.txt", "real/last", "dangling"} {
			if f, err := r.d.OpenNoLinks(rel); !errors.Is(err, ErrLink) {
				if f != nil {
					_ = f.Close()
				}
				t.Errorf("OpenNoLinks(%q) = %v, want ErrLink", rel, err)
			}
		}
		if _, err := r.d.OpenNoLinks("missing/x.txt"); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("OpenNoLinks(missing) = %v, want not exist", err)
		}
	})
}

func TestFSWalksOnlyWhatIsBeneath(t *testing.T) {
	eachMode(t, func(t *testing.T, wantBeneath bool) {
		r := newRoom(t, wantBeneath)
		write(t, filepath.Join(r.dir, "a", "b.txt"), "b")
		write(t, filepath.Join(r.dir, "a", "c", "d.txt"), "d")
		symlink(t, r.outside, filepath.Join(r.dir, "a", "out"))
		var seen []string
		err := fs.WalkDir(r.d.FS(), "a", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				seen = append(seen, p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"a/b.txt", "a/c/d.txt", "a/out"}; !slices.Equal(seen, want) {
			t.Fatalf("walked %v, want %v", seen, want)
		}
		if _, err := r.d.FS().Open("../x"); err == nil {
			t.Fatal("the FS opened a name that is not valid")
		}
		if _, err := r.d.FS().Open("a/out/secret.txt"); err == nil {
			t.Fatal("the FS opened through a link out")
		}
	})
}

// The directory and its link are swapped as fast as a hostile process can,
// while the room is written through: nothing lands outside.
func TestNothingLandsOutsideWhileALinkIsSwappedInAndOut(t *testing.T) {
	eachMode(t, func(t *testing.T, wantBeneath bool) {
		r := newRoom(t, wantBeneath)
		symlink(t, r.outside, filepath.Join(r.dir, "probe"))
		_ = os.Remove(filepath.Join(r.dir, "probe"))
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			notes := filepath.Join(r.dir, "notes")
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = os.RemoveAll(notes)
				_ = os.Symlink(r.outside, notes)
				_ = os.Remove(notes)
				_ = os.Mkdir(notes, 0o755)
			}
		}()
		deadline := time.Now().Add(1500 * time.Millisecond)
		for i := 0; time.Now().Before(deadline); i++ {
			if f, err := r.d.OpenFile("notes/w.txt", os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				_ = f.Close()
			}
			_ = r.d.Mkdir("notes/sub", 0o755)
			_ = r.d.Chmod("notes", 0o755)
			_ = r.d.Remove("notes/w.txt")
			_ = r.d.Rename("notes/a", "notes/b")
		}
		close(stop)
		wg.Wait()
		if got := r.outsideHolds(t); !slices.Equal(got, []string{"secret.txt"}) {
			t.Fatalf("the folder outside holds %v", got)
		}
	})
}

func TestACallAfterCloseFails(t *testing.T) {
	eachMode(t, func(t *testing.T, wantBeneath bool) {
		r := newRoom(t, wantBeneath)
		d, err := Open(r.dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
		if f, err := d.Open("secret.txt"); err == nil {
			_ = f.Close()
			t.Fatal("Open after Close succeeded")
		}
		if err := d.Mkdir("x", 0o755); err == nil {
			t.Fatal("Mkdir after Close succeeded")
		}
		if err := d.Close(); err != nil {
			t.Fatalf("a second Close = %v", err)
		}
	})
}

func TestOpenOfAFolderThatIsNotThereFails(t *testing.T) {
	eachMode(t, func(t *testing.T, _ bool) {
		if d, err := Open(filepath.Join(t.TempDir(), "nope")); err == nil {
			_ = d.Close()
			t.Fatal("Open of a folder that is not there succeeded")
		} else if !strings.Contains(err.Error(), "nope") {
			t.Fatalf("the error does not name the folder: %v", err)
		}
	})
}

// A listing of a directory opened beneath the Dir asks for each entry's
// size, which needs the file to carry the path it was opened by.
func TestAListingGivesTheSizesOfItsEntries(t *testing.T) {
	eachMode(t, func(t *testing.T, wantBeneath bool) {
		r := newRoom(t, wantBeneath)
		write(t, filepath.Join(r.dir, "a", "b.txt"), "12345")
		d, err := r.d.Open("a")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = d.Close() }()
		entries, err := d.ReadDir(-1)
		if err != nil || len(entries) != 1 {
			t.Fatalf("ReadDir = %v, %v", entries, err)
		}
		fi, err := entries[0].Info()
		if err != nil || fi.Size() != 5 {
			t.Fatalf("Info = %v, %v", fi, err)
		}
	})
}

func TestCreateRegularMakesAndEmptiesAFileAndRefusesAnythingElse(t *testing.T) {
	eachMode(t, func(t *testing.T, wantBeneath bool) {
		r := newRoom(t, wantBeneath)
		write(t, filepath.Join(r.dir, "old.pdf"), "old contents")
		write(t, filepath.Join(r.outside, "target"), "outside")
		symlink(t, filepath.Join(r.outside, "target"), filepath.Join(r.dir, "link.pdf"))
		symlink(t, "old.pdf", filepath.Join(r.dir, "near.pdf"))
		if err := os.Mkdir(filepath.Join(r.dir, "dir.pdf"), 0o755); err != nil {
			t.Fatal(err)
		}

		for _, name := range []string{"new.pdf", "old.pdf"} {
			f, err := r.d.CreateRegular(name, 0o644)
			if err != nil {
				t.Fatalf("CreateRegular(%q): %v", name, err)
			}
			_, _ = f.WriteString("fresh")
			_ = f.Close()
			if b, _ := os.ReadFile(filepath.Join(r.dir, name)); string(b) != "fresh" {
				t.Fatalf("%s holds %q", name, b)
			}
		}
		for _, name := range []string{"link.pdf", "near.pdf", "dir.pdf"} {
			f, err := r.d.CreateRegular(name, 0o644)
			if f != nil {
				_ = f.Close()
			}
			if !errors.Is(err, ErrNotRegular) {
				t.Errorf("CreateRegular(%q) = %v, want ErrNotRegular", name, err)
			}
		}
		if b, _ := os.ReadFile(filepath.Join(r.outside, "target")); string(b) != "outside" {
			t.Errorf("the file a link led to holds %q", b)
		}
		if b, _ := os.ReadFile(filepath.Join(r.dir, "old.pdf")); string(b) != "fresh" {
			t.Errorf("a link to a file beneath emptied it: %q", b)
		}
		if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
			return
		}
		if err := mkfifo(filepath.Join(r.dir, "pipe.pdf")); err != nil {
			t.Skipf("no named pipes here: %v", err)
		}
		done := make(chan error, 1)
		go func() {
			f, err := r.d.CreateRegular("pipe.pdf", 0o644)
			if f != nil {
				_ = f.Close()
			}
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, ErrNotRegular) {
				t.Errorf("CreateRegular(pipe) = %v, want ErrNotRegular", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("CreateRegular waited for a pipe")
		}
	})
}
