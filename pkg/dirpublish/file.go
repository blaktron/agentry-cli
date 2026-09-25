package dirpublish

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// The file half of the package: a single file more than one process may
// write at once (the runner home's policy.yaml, which a CLI run and a
// desktop run started together both create on a fresh install,
// agentry-cli#299) is written whole to a staging file beside it and put
// in place by link or rename, so a reader finds no file, the previous
// file or the new one, never a partly written one.

// CreateFile puts data at dest only if nothing is there: it is written to
// a staging file beside dest, private to this writer, and hard-linked into
// place, which fails while dest exists, so a file another writer (or the
// operator) put there meanwhile is never replaced. created is false when
// dest was already there; either way the staging file is removed.
//
// On a filesystem without hard links (FAT, some network shares) the
// staging file is renamed into place instead when dest is still absent:
// a reader still never sees a partial file, but a file written in the
// instant between the check and the rename can be replaced.
func CreateFile(dest string, data []byte, perm fs.FileMode) (created bool, err error) {
	staging, err := stageFile(dest, data, perm)
	if err != nil {
		return false, fmt.Errorf("stage %s: %w", dest, err)
	}
	defer func() { _ = os.Remove(staging) }()
	lerr := os.Link(staging, dest)
	if lerr == nil {
		return true, nil
	}
	if _, serr := os.Lstat(dest); serr == nil {
		return false, nil
	} else if !errors.Is(serr, fs.ErrNotExist) {
		return false, fmt.Errorf("publish %s: %w", dest, serr)
	}
	// dest is absent and the link failed for another reason: no hard
	// links here.
	if err := os.Rename(staging, dest); err != nil {
		return false, fmt.Errorf("publish %s: %w (link: %v)", dest, err, lerr)
	}
	return true, nil
}

// ReplaceFile puts data at dest in place of whatever is there: it is
// written to a staging file beside dest and renamed over it, so a reader
// finds the previous file or the new one whole. The last writer wins. A
// rename refused because a reader briefly holds dest open (Windows) is
// retried for about a second.
func ReplaceFile(dest string, data []byte, perm fs.FileMode) error {
	staging, err := stageFile(dest, data, perm)
	if err != nil {
		return fmt.Errorf("stage %s: %w", dest, err)
	}
	wait := publishBackoff
	for attempt := range publishAttempts {
		if attempt > 0 {
			time.Sleep(wait)
			wait *= 2
		}
		if err = os.Rename(staging, dest); err == nil {
			return nil
		}
		if _, serr := os.Stat(staging); serr != nil {
			break
		}
	}
	_ = os.Remove(staging)
	return fmt.Errorf("publish %s: %w", dest, err)
}

// stageFile writes data to a new staging file beside dest, named
// .<base>.staging-<rand> and created exclusively with perm (under the
// process umask), synced so a crash never publishes an empty file, after
// clearing any abandoned siblings of dest.
func stageFile(dest string, data []byte, perm fs.FileMode) (string, error) {
	parent, base := filepath.Dir(dest), filepath.Base(dest)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	sweepAbandonedSiblings(parent, base)
	var f *os.File
	var err error
	for range 4 {
		name := filepath.Join(parent, "."+base+stagingInfix+randomSuffix())
		f, err = os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm) //nolint:gosec // the caller names dest; the staging name is this package's
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	if err != nil {
		return "", err
	}
	name := f.Name()
	_, werr := f.Write(data)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}
