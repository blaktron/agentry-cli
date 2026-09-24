//go:build !windows

package engine

import (
	"errors"
	"os"
	"syscall"
)

// tryLockFile takes an exclusive flock on f without blocking.
func tryLockFile(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) //nolint:gosec // a file descriptor fits an int
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errRelayLockHeld
	}
	return err
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:gosec // a file descriptor fits an int
}
