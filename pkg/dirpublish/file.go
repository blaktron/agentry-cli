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
// desktop run started together both create on a fresh install) is
// written whole to a staging file beside it and put in place by link or
// rename, so a reader finds no file, the previous file or the new one,
// never a partly written one.

// CreateFile puts data at dest only if nothing is there: it is written to
// a staging file beside dest, private to this writer, and hard-linked into
// place, which fails while dest exists, so a file another writer (or a
// person) put there meanwhile is never replaced and a reader never sees
// it partly written. created is false when dest was already there; either
// way the staging file is removed.
//
// On a filesystem without hard links (FAT, some network shares) dest is
// created exclusively and written in place instead: still never replacing
// a file that is there, but a reader in the instant of the write can find
// it partial, as before this package wrote files.
func CreateFile(dest string, data []byte, perm fs.FileMode) (created bool, err error) {
	staging, err := stageFile(dest, data, perm)
	if err != nil {
		return false, fmt.Errorf("stage %s: %w", dest, err)
	}
	defer func() { _ = os.Remove(staging) }()
	lerr := link(staging, dest)
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
	created, err = createInPlace(dest, data, perm)
	if err != nil {
		return false, fmt.Errorf("publish %s: %w (link: %v)", dest, err, lerr)
	}
	return created, nil
}

// link is os.Link, a variable so a test can stand in a filesystem without
// hard links.
var link = os.Link

// createInPlace is CreateFile's fallback: dest created exclusively and
// written where it stands.
func createInPlace(dest string, data []byte, perm fs.FileMode) (bool, error) {
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm) //nolint:gosec // the caller names dest
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, werr := f.Write(data)
	if err := errors.Join(werr, f.Close()); err != nil {
		_ = os.Remove(dest)
		return false, err
	}
	return true, nil
}

// ReplaceFile puts data at dest in place of whatever is there: it is
// written to a staging file beside dest and renamed over it, so a reader
// finds the previous file or the new one whole. The last writer wins.
// When dest is a symbolic link, the file it points to is the one
// replaced, and an existing file keeps its permission bits (perm applies
// to a new one), as a write in place would have left them. A rename
// refused because a reader briefly holds dest open (Windows) is retried
// for about a second.
//
// Windows also refuses to replace a file by rename for a while after it
// was written, when a scanner such as Defender has mapped it, though the
// file can still be renamed away. When the rename over dest is refused,
// dest is retired aside and the new file renamed into the name it leaves
// free, as Replace does for a directory; a reader in between finds no
// file.
func ReplaceFile(dest string, data []byte, perm fs.FileMode) error {
	kept := false
	if target, err := filepath.EvalSymlinks(dest); err == nil {
		dest = target
		if info, err := os.Stat(target); err == nil {
			perm, kept = info.Mode().Perm(), true
		}
	}
	staging, err := stageFile(dest, data, perm)
	if err != nil {
		return fmt.Errorf("stage %s: %w", dest, err)
	}
	if kept {
		// Created under the umask; the file it replaces had these bits.
		if err := os.Chmod(staging, perm); err != nil {
			_ = os.Remove(staging)
			return fmt.Errorf("stage %s: %w", dest, err)
		}
	}
	wait := publishBackoff
	for attempt := range publishAttempts {
		if attempt > 0 {
			time.Sleep(wait)
			wait *= 2
		}
		if err = replace(staging, dest); err == nil {
			return nil
		}
		if _, serr := os.Stat(staging); serr != nil {
			break
		}
		if retireFile(staging, dest) == nil {
			return nil
		}
	}
	_ = os.Remove(staging)
	return fmt.Errorf("publish %s: %w", dest, err)
}

// replace renames staging over dest. A seam: the tests refuse it as
// Windows does while a scanner still maps a file that was just written.
var replace = os.Rename

// errNotAFile is retireFile's refusal of a dest that is not a regular
// file: only a file is ever retired in place of the new one.
var errNotAFile = errors.New("not a regular file")

// retireFile publishes staging at dest when a rename over dest was
// refused: dest, a regular file, is renamed aside to a retired sibling,
// staging is renamed into the name it leaves free, and the retired file
// is removed (a sibling sweep removes one that will not go). If staging
// does not go in and no other writer has published dest meanwhile, the
// retired file is put back, so a failed write leaves the previous file.
func retireFile(staging, dest string) error {
	info, err := os.Lstat(dest)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errNotAFile
	}
	retired := filepath.Join(filepath.Dir(dest), "."+filepath.Base(dest)+retiredInfix+randomSuffix())
	if err := os.Rename(dest, retired); err != nil {
		return err
	}
	err = os.Rename(staging, dest)
	if err != nil {
		if _, serr := os.Lstat(dest); errors.Is(serr, fs.ErrNotExist) && os.Rename(retired, dest) == nil {
			return err
		}
	}
	_ = os.Remove(retired)
	return err
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
