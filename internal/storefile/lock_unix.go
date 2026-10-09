//go:build !windows

package storefile

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive flock on f, waiting for it.
func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX) //nolint:gosec // a file descriptor fits an int
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:gosec // a file descriptor fits an int
}
