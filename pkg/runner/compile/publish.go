package compile

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

// A compile writes its files into a staging directory beside the plan
// directory and publishes it with renames, so that plan.json, its
// integrity record and the generated files a reader finds under planDir
// always come from one compile (agentry-cli#289). Two processes compiling
// the same plan at once (two runs of one release and mode; the CLI and the
// desktop recompile each other's plans) used to write into the same
// directory: one's plan.json could sit beside the other's record, which a
// reader cannot tell from an edit, and every later run was refused.

// stagingInfix and retiredInfix name the siblings a compile leaves while
// it publishes: .<mode>.staging-<rand> and .<mode>.retired-<rand>, hidden
// so nothing that lists plan/ mistakes them for a mode.
const (
	stagingInfix = ".staging-"
	retiredInfix = ".retired-"
)

// staleSibling is how old a staging or retired sibling must be before a
// later compile removes it as abandoned (its process died mid-compile).
const staleSibling = time.Hour

// publishAttempts bounds how often a compile retries losing the rename to
// another compile publishing the same plan directory at the same moment;
// publishBackoff is the first pause between attempts, doubling (about a
// second in all).
const publishAttempts = 8

const publishBackoff = 5 * time.Millisecond

// newStagingDir creates the staging directory for planDir, clearing any
// abandoned siblings first.
func newStagingDir(planDir string) (string, error) {
	parent, base := filepath.Dir(planDir), filepath.Base(planDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", fmt.Errorf("create plan dir: %w", err)
	}
	sweepAbandonedSiblings(parent, base)
	dir, err := os.MkdirTemp(parent, "."+base+stagingInfix)
	if err != nil {
		return "", fmt.Errorf("create plan dir: %w", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // the plan dir is not secret; MkdirTemp makes it 0700
		return "", fmt.Errorf("create plan dir: %w", err)
	}
	return dir, nil
}

// publishPlanDir makes staging the plan directory: the directory in place
// (if any) is renamed aside, then staging is renamed into its place, and
// the retired directory is removed. Between the two renames planDir does
// not exist, which a reader takes as "no plan" and recompiles. If another
// compile publishes in that gap, the rename fails because planDir exists
// again, and this compile retires that one and tries again: the last to
// publish wins, and each published directory is whole. Any failed rename
// is retried while staging is still there; only one that keeps failing
// through every attempt is an error.
func publishPlanDir(staging, planDir string) error {
	parent, base := filepath.Dir(planDir), filepath.Base(planDir)
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
		retired := filepath.Join(parent, "."+base+retiredInfix+randomSuffix())
		err := os.Rename(planDir, retired)
		moved := err == nil
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			// planDir is there but would not move; try again, and say
			// why if it never does.
			lastErr = err
			continue
		}
		err = os.Rename(staging, planDir)
		if moved {
			_ = os.RemoveAll(retired)
		}
		if err == nil {
			return nil
		}
		lastErr = err
		if _, serr := os.Stat(staging); serr != nil {
			// Nothing left to publish: not a race to wait out.
			return fmt.Errorf("publish plan dir: %w", err)
		}
	}
	return fmt.Errorf("publish plan dir: another compile kept replacing %s: %w", planDir, lastErr)
}

// sweepAbandonedSiblings removes staging and retired siblings of base
// older than staleSibling. Best effort: a compile in progress elsewhere is
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
