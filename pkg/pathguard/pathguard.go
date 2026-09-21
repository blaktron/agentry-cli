// Package pathguard is the refusal every recursive delete of a derived
// directory passes, and the containment check every write at a
// manifest-named path passes. The install clears a directory named by a
// registry's resolve response and writes a file the same response names;
// neither string is covered by the archive digest or the attestation, so
// each is judged here before the filesystem sees it (security review
// 2026-09-20 §4.7). Exported so the desktop client, which has the same two
// seams, can share it.
package pathguard

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
)

// RefuseDangerousDir refuses a directory this code must never delete
// recursively: one that is not absolute, a filesystem root, the home
// directory itself, or an ancestor of it. Anywhere else is the person's
// to lose — the managed roots are theirs to relocate.
func RefuseDangerousDir(dir, home string) error {
	if dir == "" || !filepath.IsAbs(dir) {
		return errors.New("not an absolute path")
	}
	clean := filepath.Clean(dir)
	if filepath.Dir(clean) == clean {
		return errors.New("a filesystem root")
	}
	home = filepath.Clean(home)
	if runtime.GOOS == "windows" {
		// Paths compare case-insensitively there: c:\users\bob is C:\Users\Bob.
		clean, home = strings.ToLower(clean), strings.ToLower(home)
	}
	rel, err := filepath.Rel(clean, home)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("the home directory or one of its ancestors")
	}
	return nil
}

// DirectlyUnder reports whether dir is exactly one path segment below root
// (both cleaned): the one shape a managed install directory may have, so a
// destination a bad name resolved to is never cleared whatever it is.
func DirectlyUnder(root, dir string) bool {
	if root == "" || dir == "" {
		return false
	}
	clean := filepath.Clean(dir)
	base := filepath.Base(clean)
	if base == "." || base == ".." || base == string(filepath.Separator) {
		return false
	}
	return filepath.Dir(clean) == filepath.Clean(root)
}

// Under reports whether p is strictly inside dir (after cleaning both).
func Under(dir, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(p))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	return true
}
