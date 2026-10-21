//go:build !windows

package storefile

import (
	"errors"
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

// tryLockFile takes an exclusive flock on f without waiting: errHeld when
// another open file holds it.
func tryLockFile(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) //nolint:gosec // a file descriptor fits an int
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errHeld
	}
	return err
}
