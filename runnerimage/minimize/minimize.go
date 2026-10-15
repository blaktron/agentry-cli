// Package minimize is the runner image's `minimize` role: run once by
// `docker build`, it turns the shared runner
// image into a per-QuickAgent exec image that holds ONLY the declared
// commands.
package minimize

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// binDirs are where executables live in the runner image (alpine).
var binDirs = []string{"/bin", "/sbin", "/usr/bin", "/usr/sbin", "/usr/local/bin", "/usr/local/sbin"}

// runnerBinary is this program inside the image; it always survives.
const runnerBinary = "agentry-runner"

// Minimize deletes every executable under root's bin directories that is
// not in keep (bare names) or the runner binary itself. Exported for the
// unit test, which runs it against a temporary tree.
func Minimize(root string, keep []string, logw io.Writer) error {
	keepSet := map[string]bool{runnerBinary: true}
	for _, k := range keep {
		k = strings.TrimSpace(k)
		if k == "" || strings.ContainsAny(k, `/\`) {
			return fmt.Errorf("minimize: %q is not a bare command name", k)
		}
		keepSet[k] = true
	}
	// Pass 1: which kept names are busybox applets (symlinks to a busybox
	// binary)? Their target must survive.
	targets := map[string]bool{}
	for _, dir := range binDirs {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !keepSet[e.Name()] {
				continue
			}
			p := filepath.Join(root, dir, e.Name())
			if resolved, err := filepath.EvalSymlinks(p); err == nil && resolved != p {
				targets[resolved] = true
			}
		}
	}
	var removed, kept []string
	for _, dir := range binDirs {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			continue
		}
		for _, e := range entries {
			p := filepath.Join(root, dir, e.Name())
			if keepSet[e.Name()] || targets[p] {
				kept = append(kept, p)
				continue
			}
			if e.IsDir() {
				continue
			}
			if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("minimize: remove %s: %w", p, err)
			}
			removed = append(removed, p)
		}
	}
	sort.Strings(kept)
	_, _ = fmt.Fprintf(logw, "minimize: kept %s; removed %d executables\n", strings.Join(kept, ", "), len(removed))
	for _, k := range keep {
		found := false
		for _, dir := range binDirs {
			if _, err := os.Lstat(filepath.Join(root, dir, k)); err == nil {
				found = true
				break
			}
		}
		if !found {
			_, _ = fmt.Fprintf(logw, "minimize: declared command %q is not present in the image (run_command will report it absent)\n", k)
		}
	}
	return nil
}
