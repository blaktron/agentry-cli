//go:build !windows

package managed

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A file someone other than the administrator could have written is
// refused, as is one that is not a regular file; none at all is none.
func TestTheMachineSettingsFileMustBeTheAdministrators(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "managed.json")
	if _, _, found, err := readAdminFile(path); found || err != nil {
		t.Fatalf("no file: %v %v", found, err)
	}
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := adminOwned
	t.Cleanup(func() { adminOwned = prev })
	// The test's files are the test's user's: stand that user in for root,
	// and hold the test's own folder and file to the real mode rule. The
	// folders above them are the system's temporary space, which another
	// test run may share.
	ours := map[string]bool{filepath.Base(dir): true, "managed.json": true, "link.json": true}
	adminOwned = func(fi fs.FileInfo) bool { return !ours[fi.Name()] || fi.Mode().Perm()&0o022 == 0 }
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, found, err := readAdminFile(path); !found || err != nil {
		t.Fatalf("an administrator's file: %v %v", found, err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := readAdminFile(path); err == nil || !strings.Contains(err.Error(), "someone other than this computer's administrator") {
		t.Fatalf("a world-writable file: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := readAdminFile(path); err == nil || !strings.Contains(err.Error(), dir) {
		t.Fatalf("a world-writable folder: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := readAdminFile(link); err == nil {
		t.Fatal("a symbolic link was read")
	}
}
