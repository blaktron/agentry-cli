// Package dirpublish replaces a directory whole: the new tree is built in
// a staging directory beside the destination and published by rename, so
// a reader finds either the previous tree or the new one, never a mix,
// and two writers of the same destination at once never share a
// half-built tree. The last to publish wins, and what it leaves is whole.
//
// It is for a directory more than one process may write at the same time,
// such as the runner home's plan and release directories, which a CLI run
// and a desktop run started together both write.
package dirpublish

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// stagingInfix and retiredInfix name the siblings a writer leaves while
// it publishes: .<base>.staging-<rand> and .<base>.retired-<rand>, hidden
// so nothing that lists the parent mistakes them for the real thing.
const (
	stagingInfix = ".staging-"
	retiredInfix = ".retired-"
)

// staleSibling is how old a staging or retired sibling must be before a
// later writer removes it as abandoned (its process died mid-write).
const staleSibling = time.Hour

// publishAttempts bounds how often a writer retries losing the rename to
// another writer publishing the same destination at the same moment;
// publishBackoff is the first pause between attempts, doubling (about a
// second in all).
const (
	publishAttempts = 8
	publishBackoff  = 5 * time.Millisecond
)

// Replace builds a new tree for dest with fill, in a staging directory
// beside dest that is this writer's alone, and publishes it in place of
// whatever is at dest: the last writer to publish wins. If fill fails, or
// the tree cannot be published, the staging directory is removed and dest
// is left as it was; fill's own error comes back unwrapped.
func Replace(dest string, fill func(dir string) error) error {
	_, err := ReplaceUnless(dest, fill, nil)
	return err
}

// ReplaceUnless is Replace for a destination whose content is fixed by its
// name, such as a release directory named for its digest: settled, called
// before each attempt to put the new tree in place, reports whether dest
// already holds a whole tree this writer would only duplicate (another
// writer finished first). Then the staged tree is discarded and dest, with
// whatever its writer has since put inside it, is left alone; published
// is false. A nil settled never settles.
func ReplaceUnless(dest string, fill func(dir string) error, settled func() bool) (published bool, err error) {
	staging, err := stage(dest)
	if err != nil {
		return false, fmt.Errorf("stage %s: %w", dest, err)
	}
	if err := fill(staging); err != nil {
		_ = os.RemoveAll(staging)
		return false, err
	}
	published, err = publish(staging, dest, settled)
	if !published {
		_ = os.RemoveAll(staging)
	}
	if err != nil {
		return false, fmt.Errorf("publish %s: %w", dest, err)
	}
	return published, nil
}

// stage creates a staging directory for dest, private to this writer by
// its random name and made the way the directory it becomes would be
// (0755 under the process umask), after clearing any abandoned siblings
// of dest.
func stage(dest string) (string, error) {
	parent, base := filepath.Dir(dest), filepath.Base(dest)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	sweepAbandonedSiblings(parent, base)
	var err error
	for range 4 {
		dir := filepath.Join(parent, "."+base+stagingInfix+randomSuffix())
		if err = os.Mkdir(dir, 0o755); err == nil {
			return dir, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	return "", err
}

// InFlight reports whether a directory entry is a staging or retired
// sibling a writer may still be using (younger than an abandoned one):
// something a sweep of the parent directory must leave alone, or it can
// delete a tree from under the rename that publishes it.
func InFlight(e fs.DirEntry) bool {
	name := e.Name()
	if !e.IsDir() || !strings.HasPrefix(name, ".") || (!strings.Contains(name, stagingInfix) && !strings.Contains(name, retiredInfix)) {
		return false
	}
	info, err := e.Info()
	return err == nil && time.Since(info.ModTime()) < staleSibling
}

// publish makes staging dest: the directory in place (if any) is renamed
// aside, then staging is renamed into its place, and the retired
// directory is removed. Between the two renames dest does not exist,
// which a reader takes as "not there yet" and builds it itself. If
// another writer publishes in that gap, the rename fails because dest
// exists again, and this writer retires that one and tries again. Any
// failed rename is retried while staging is still there; only one that
// keeps failing through every attempt is an error.
//
// With settled, staging is first renamed straight into place, which
// fails while dest exists (on every platform, for a non-empty directory),
// and a dest that is there is retired only when settled says it is not
// already what this writer would publish: a writer that finished first
// is never swept away, whatever the timing.
func publish(staging, dest string, settled func() bool) (bool, error) {
	parent, base := filepath.Dir(dest), filepath.Base(dest)
	var lastErr error
	wait := publishBackoff
	for attempt := range publishAttempts {
		if attempt > 0 {
			// A lost race resolves at once; the pause is for a
			// directory a reader briefly holds open, which Windows
			// will not rename.
			time.Sleep(wait)
			wait *= 2
		}
		if settled != nil {
			err := os.Rename(staging, dest)
			if err == nil {
				return true, nil
			}
			if _, serr := os.Stat(staging); serr != nil {
				return false, err
			}
			if settled() {
				return false, nil
			}
			// dest is there and not what this writer would publish:
			// retire it as an unsettled writer would.
		}
		retired := filepath.Join(parent, "."+base+retiredInfix+randomSuffix())
		err := os.Rename(dest, retired)
		moved := err == nil
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			// dest is there but would not move; try again, and say
			// why if it never does.
			lastErr = err
			continue
		}
		err = os.Rename(staging, dest)
		if moved {
			_ = os.RemoveAll(retired)
		}
		if err == nil {
			return true, nil
		}
		lastErr = err
		if _, serr := os.Stat(staging); serr != nil {
			// Nothing left to publish: not a race to wait out.
			return false, err
		}
	}
	return false, fmt.Errorf("%s kept changing or would not move: %w", dest, lastErr)
}

// sweepAbandonedSiblings removes staging and retired siblings of base
// older than staleSibling. Best effort: a write in progress elsewhere is
// minutes old at most, and a failure here costs only disk.
func sweepAbandonedSiblings(parent, base string) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || (!strings.HasPrefix(name, "."+base+stagingInfix) && !strings.HasPrefix(name, "."+base+retiredInfix)) {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < staleSibling {
			continue
		}
		_ = os.RemoveAll(filepath.Join(parent, name))
	}
}

func randomSuffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
