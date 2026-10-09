// Package storefile holds the file primitives the person's credential
// stores under the runner home share (pkg/runner/connection,
// pkg/runner/modelsignin): an exclusive lock file a refresh writes back
// under, a 0600 write by rename, and a removal that takes a missing file
// as done.
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
