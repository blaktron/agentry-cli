// Package storefile holds the file primitives the stores under the runner
// home share (the credential stores of pkg/runner/connection and
// pkg/runner/modelsignin, the MCP server store of pkg/runner/mcpinstall): an
// exclusive lock file a writer holds, waited for or tried, a 0600 write by
// rename, and a removal that takes a missing file as done.
package storefile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// WriteAtomic writes data 0600 beside path and renames it over path.
func WriteAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// RemoveIfThere removes path; a missing one is not an error.
func RemoveIfThere(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Lock holds an exclusive lock on the file at path, made 0600 (its
// directory 0700) when missing, waiting for another process holding it,
// until the returned func is called. what names it in an error.
func Lock(path, what string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // a store's own lock file
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", what, err)
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock %s: %w", what, err)
	}
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
	}, nil
}

// errHeld is a lock another open file holds.
var errHeld = errors.New("lock held")

// TryLock is Lock without waiting: held says another process, or another
// open file of this one, holds the lock, and nothing is held then.
func TryLock(path, what string) (unlock func(), held bool, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // a store's own lock file
	if err != nil {
		return nil, false, fmt.Errorf("lock %s: %w", what, err)
	}
	if err := tryLockFile(f); err != nil {
		_ = f.Close()
		if errors.Is(err, errHeld) {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("lock %s: %w", what, err)
	}
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
	}, false, nil
}
