package engine

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/removal"
)

// WorkLockFile is the lock a process holds on a work directory of its own,
// <dir>/work.lock, from just after it makes the directory until it has deleted
// it. Like a run's lock (RunLockFile) the kernel releases it when the process
// dies however it dies, so a later process that can take it knows the
// directory's owner is gone and what it left in the clear, the documents of a
// knowledge build, their decoded text and its pages, can be removed.
const WorkLockFile = "work.lock"

// HoldWorkDir locks the work directory dir, which must exist, and returns
// the function that lets the lock go. It is called before anything sensitive
// is written into the directory and again, before the directory is deleted:
// on Windows an open file cannot be deleted, nor the directory holding it.
func HoldWorkDir(dir string) (release func(), err error) {
	path := filepath.Join(dir, WorkLockFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600) //nolint:gosec // fixed name in the process's own directory
	if err != nil {
		return nil, fmt.Errorf("lock the work directory %s: %w", dir, err)
	}
	if err := tryLockFile(f); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("lock the work directory %s: %w", dir, err)
	}
	return func() {
		if f == nil {
			return
		}
		_ = unlockFile(f)
		_ = f.Close()
		f = nil
	}, nil
}

// SweepWorkDirs removes the work directories under root whose names begin
// with prefix and whose owner is gone: a directory whose lock can be taken is
// a dead process's, and one whose lock is held is a live process's and is
// left alone. A directory with no lock file belongs to a process that died, or
// is starting, between making it and locking it, and nothing sensitive is
// written before the lock, so it goes only once it is older than grace.
//
// Best effort: it never fails the caller. It says what it removed on stderr,
// and names a directory it could not delete, which may hold documents in the
// clear. It returns the names it removed, sorted.
func SweepWorkDirs(root, prefix string, grace time.Duration, stderr io.Writer) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var swept, left []string
	for _, e := range entries {
		// A real directory only: a link in a work directory's place is not
		// followed out of the runner home.
		if !e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		dir := filepath.Join(root, e.Name())
		dead, err := workDirIsDead(dir, grace)
		if err != nil || !dead {
			continue
		}
		if err := removal.Remove(dir, true); err != nil && !removal.Gone(dir) {
			left = append(left, dir)
			continue
		}
		swept = append(swept, e.Name())
	}
	sort.Strings(swept)
	if stderr == nil {
		return swept
	}
	if len(swept) > 0 {
		_, _ = fmt.Fprintf(stderr, "removed what %d process(es) that had died left in their work directories: %s\n", len(swept), strings.Join(swept, ", "))
	}
	if len(left) > 0 {
		_, _ = fmt.Fprintf(stderr, "warning: could not delete %s, left by a process that had died: it may hold documents in the clear; delete it by hand\n", strings.Join(left, "; "))
	}
	return swept
}

// workDirIsDead says whether the work directory dir's owner is gone, and
// holds the lock no longer when it answers.
func workDirIsDead(dir string, grace time.Duration) (bool, error) {
	lockPath := filepath.Join(dir, WorkLockFile)
	fi, err := os.Lstat(lockPath)
	if os.IsNotExist(err) {
		di, err := os.Lstat(dir)
		if err != nil {
			return false, err
		}
		return time.Since(di.ModTime()) > grace, nil
	}
	if err != nil || !fi.Mode().IsRegular() {
		return false, err
	}
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0) //nolint:gosec // fixed name in a work directory
	if err != nil {
		return false, err
	}
	// Held, or a lock that cannot be taken at all, which proves nothing.
	if err := tryLockFile(f); err != nil {
		_ = f.Close()
		return false, nil
	}
	// Released before the directory is deleted (Windows): a process that
	// takes the lock between the two finds the directory gone or its files
	// deleted, and makes a directory of its own.
	_ = unlockFile(f)
	_ = f.Close()
	return true, nil
}
