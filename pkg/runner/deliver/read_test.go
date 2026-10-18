package deliver

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

func readReason(t *testing.T, err error) string {
	t.Helper()
	var re *ReadError
	if !errors.As(err, &re) {
		t.Fatalf("want a *ReadError, got %v", err)
	}
	return re.Reason
}

func TestReadGivesTheBytesTheEnvelopeNames(t *testing.T) {
	dir, files := runFolder(t, map[string]string{"reports/a.md": "# hello\n"}, "ok")
	rp, b, err := Read(dir, files[0], 1<<20)
	if err != nil || string(b) != "# hello\n" || rp != filepath.Join(dir, "reports", "a.md") {
		t.Fatalf("Read = %q, %q, %v", rp, b, err)
	}
	abs, err := Locate(dir, files[0])
	if err != nil || abs != filepath.Join(dir, "reports", "a.md") {
		t.Fatalf("Locate = %q, %v", abs, err)
	}
}

func TestReadRefusesWhatIsNotTheListedFile(t *testing.T) {
	dir, files := runFolder(t, map[string]string{"reports/a.md": "# hello\n"}, "ok")
	f := files[0]
	write(t, dir, "reports/a.md", "# changed\n")
	if r := readReason(t, mustFail(t, dir, f)); r != receipt.DeliveryErrChanged {
		t.Fatalf("a changed file: %s", r)
	}
	gone := f
	gone.Path = "reports/gone.md"
	if r := readReason(t, mustFail(t, dir, gone)); r != receipt.DeliveryErrMissing {
		t.Fatalf("a missing file: %s", r)
	}
}

func mustFail(t *testing.T, dir string, f receipt.Deliverable) error {
	t.Helper()
	_, _, err := Read(dir, f, 1<<20)
	if err == nil {
		t.Fatal("Read accepted it")
	}
	if _, lerr := Locate(dir, f); lerr == nil {
		t.Fatal("Locate accepted it")
	}
	return err
}

func TestReadRefusesAPathOutsideReports(t *testing.T) {
	dir, files := runFolder(t, map[string]string{"reports/a.md": "x"}, "ok")
	outside := filepath.Join(filepath.Dir(dir), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"result.json", "../secret.txt", "reports/../result.json", "reports/../../secret.txt", "/etc/passwd", outside, "reports", "", `reports\a.md`} {
		f := files[0]
		f.Path = p
		if r := readReason(t, mustFail(t, dir, f)); r != receipt.DeliveryErrOutside && r != receipt.DeliveryErrMissing {
			t.Fatalf("%q: %s", p, r)
		}
	}
}

func TestReadRefusesALinkAnywhereInThePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links need privileges on Windows")
	}
	dir, files := runFolder(t, map[string]string{"reports/a.md": "x", "reports/sub/b.md": "y"}, "ok")
	outside := t.TempDir()
	write(t, outside, "c.md", "z")
	if err := os.Symlink(filepath.Join(outside, "c.md"), filepath.Join(dir, "reports", "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "reports", "dir")); err != nil {
		t.Fatal(err)
	}
	link := listed("reports/link.md", "", "z", true)
	if r := readReason(t, mustFail(t, dir, link)); r != receipt.DeliveryErrLink {
		t.Fatalf("a linked file: %s", r)
	}
	viaDir := listed("reports/dir/c.md", "", "z", true)
	if r := readReason(t, mustFail(t, dir, viaDir)); r != receipt.DeliveryErrLink {
		t.Fatalf("a file through a linked folder: %s", r)
	}
	_ = files
}

func TestReadRefusesATypeAFolderViewActsOn(t *testing.T) {
	dir, _ := runFolder(t, map[string]string{"reports/a.md": "x", "reports/x.lnk": "L"}, "ok")
	f := listed("reports/x.lnk", "", "L", true)
	if r := readReason(t, mustFail(t, dir, f)); r != receipt.DeliveryErrType {
		t.Fatalf("a shortcut: %s", r)
	}
}

func TestReadRefusesAFileOverTheLimitBeforeReadingIt(t *testing.T) {
	dir, files := runFolder(t, map[string]string{"reports/a.md": "0123456789"}, "ok")
	if _, _, err := Read(dir, files[0], 5); readReason(t, err) != receipt.DeliveryErrTooLarge {
		t.Fatalf("want too-large, got %v", err)
	}
	if _, _, err := Read(dir, files[0], 0); readReason(t, err) != receipt.DeliveryErrTooLarge {
		t.Fatalf("a zero limit reads nothing: %v", err)
	}
	if _, err := Locate(dir, files[0]); err != nil {
		t.Fatalf("Locate has no size limit: %v", err)
	}
}
