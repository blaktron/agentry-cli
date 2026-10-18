//go:build linux

package beneath

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// eachMode runs f with paths resolved by openat2, when this machine answers
// it, and again with the os.Root fallback.
func eachMode(t *testing.T, f func(t *testing.T, wantBeneath bool)) {
	t.Run("openat2", func(t *testing.T) {
		if !usable() {
			t.Skip("this machine does not answer openat2 with RESOLVE_BENEATH")
		}
		f(t, true)
	})
	t.Run("fallback", func(t *testing.T) {
		if followsDirectoryLinks(t) {
			if !usable() {
				t.Fatal("this kernel follows a link to a directory opened with O_NOFOLLOW and has no openat2, so no root here holds")
			}
			// gVisor answers openat with O_DIRECTORY|O_NOFOLLOW by following a
			// link to a directory, so the os.Root fallback cannot hold there.
			// It is never taken there either: Open uses openat2 whenever the
			// kernel answers it, which the openat2 subtest proves.
			t.Skip("this kernel follows a link to a directory opened with O_NOFOLLOW; the fallback is not used where openat2 answers")
		}
		disabled = true
		defer func() { disabled = false }()
		f(t, false)
	})
}

// followsDirectoryLinks reports whether openat with O_DIRECTORY|O_NOFOLLOW
// opens through a link to a directory, which Linux refuses with ENOTDIR and
// os.Root relies on.
func followsDirectoryLinks(t *testing.T) bool {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(dir, "link")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	dfd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Close(dfd) }()
	fd, err := syscall.Openat(dfd, "link", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	_ = syscall.Close(fd)
	return true
}
