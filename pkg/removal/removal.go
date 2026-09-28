// Package removal deletes what a run or a build leaves that must not stay
// on disk (a run's secrets file, a knowledge pack's pages and documents in
// the clear), and says when it could not. On Windows, Docker Desktop's
// file sharing can keep a handle on a file for a moment after its
// container is removed, and a file or a directory with an open handle
// cannot be deleted; so a failed delete is retried there for a bounded
// time before it is given up (agentry-cli#422).
package removal

import (
	"errors"
	"io/fs"
	"os"
	"runtime"
	"time"
)

// Tries and Delay bound the retry: about five seconds on Windows, one try
// elsewhere, where a delete that fails will not succeed by waiting.
var (
	Tries = triesFor(runtime.GOOS)
	Delay = 250 * time.Millisecond
)

func triesFor(goos string) int {
	if goos == "windows" {
		return 20
	}
	return 1
}

// op is the delete itself, a variable so a test can make it fail.
var op = func(path string, all bool) error {
	if all {
		return os.RemoveAll(path)
	}
	return os.Remove(path)
}

// Remove deletes path (all: with everything under it), retrying as Tries
// and Delay allow. Something already gone is not an error.
func Remove(path string, all bool) error {
	var err error
	for i := 0; i < max(Tries, 1); i++ {
		if i > 0 {
			time.Sleep(Delay)
		}
		if err = op(path, all); err == nil || errors.Is(err, fs.ErrNotExist) {
			return nil
		}
	}
	return err
}

// Gone says whether nothing is left at path.
func Gone(path string) bool {
	_, err := os.Lstat(path)
	return errors.Is(err, fs.ErrNotExist)
}
