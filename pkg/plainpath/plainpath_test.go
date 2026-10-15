package plainpath

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The links here stay inside the root, so os.Root would follow them: only
// the check refuses them, on any host.
func TestFileAndDir(t *testing.T) {
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "wiki", "real"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "wiki", "real", "page.md"), []byte("# Page\n"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "file.txt"), []byte("x"), 0o644))
	// Relative targets: os.Root follows those when they stay inside it
	// (an absolute one it refuses by itself), so only the check is tested.
	linked := os.Symlink("real", filepath.Join(dir, "wiki", "linked")) == nil &&
		os.Symlink("file.txt", filepath.Join(dir, "link.txt")) == nil
	root, err := os.OpenRoot(dir)
	must(err)
	t.Cleanup(func() { _ = root.Close() })

	if fi, err := File(root, "wiki/real/page.md"); err != nil || fi.Size() != 7 {
		t.Errorf("a plain file: %v, %v", fi, err)
	}
	if err := Dir(root, "wiki/real"); err != nil {
		t.Errorf("a plain directory: %v", err)
	}
	if _, err := File(root, "wiki/real"); err == nil || err.Error() != "not a regular file" {
		t.Errorf("a directory as a file: %v", err)
	}
	if _, err := File(root, "file.txt/x"); err == nil || !strings.Contains(err.Error(), "file.txt is not a directory") {
		t.Errorf("a file on the way: %v", err)
	}
	if _, err := File(root, "wiki/missing.md"); err == nil {
		t.Error("a missing file passed")
	}
	if !linked {
		t.Skip("symlinks are not available")
	}
	// The fixture proves itself: os.Root reads through these links.
	if _, err := root.Stat(filepath.Join("wiki", "linked", "page.md")); err != nil {
		t.Fatalf("os.Root does not follow the fixture's link, so the check is not what is tested: %v", err)
	}
	if _, err := File(root, "wiki/linked/page.md"); err == nil || !strings.Contains(err.Error(), "wiki/linked is not a directory") {
		t.Errorf("a linked directory on the way, inside the root: %v", err)
	}
	if err := Dir(root, "wiki/linked"); err == nil {
		t.Error("a linked directory passed as a directory")
	}
	if _, err := File(root, "link.txt"); err == nil || err.Error() != "not a regular file" {
		t.Errorf("a linked file: %v", err)
	}
}
