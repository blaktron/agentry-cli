//go:build !windows

package engine

import (
	"io/fs"
	"os"
	"syscall"
)

// outboxOpenFlags opens a file of the share outbox for reading without
// waiting on it, should a name turn into a pipe after it was listed.
const outboxOpenFlags = os.O_RDONLY | syscall.O_NONBLOCK

// outboxLinks is the number of links to a file of the share outbox; one
// for a file of its own.
func outboxLinks(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink) //nolint:unconvert // Nlink's width differs by platform
	}
	return 1
}

// outboxNeedsHandOver says the share outbox holds something the person
// may not read or remove as it is: an entry owned by another user that is a
// folder or is not readable by everyone, or one the walk could not read.
func outboxNeedsHandOver(dir string) bool {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return false
	}
	defer func() { _ = root.Close() }()
	own := uint32(os.Getuid()) //nolint:gosec // a uid is never negative here
	needs := false
	_ = fs.WalkDir(root.FS(), ".", func(name string, de fs.DirEntry, err error) error {
		if err != nil {
			needs = true
			return fs.SkipAll
		}
		if name == "." {
			return nil
		}
		fi, err := de.Info()
		if err != nil {
			return nil
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if ok && st.Uid != own && (fi.IsDir() || fi.Mode().Perm()&0o004 == 0) {
			needs = true
			return fs.SkipAll
		}
		return nil
	})
	return needs
}
