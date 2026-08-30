// Path safety for every filesystem-facing tool (RUNNER-PLAN §R11
// invariant 5): fs_read, fs_list, fs_write, and write_report all funnel
// through resolveUnder, so there is exactly one place that decides whether
// a model-supplied path stays inside its root — and one place the
// traversal corpus has to hit.
//
// Two layers, because each catches what the other cannot. The lexical
// check refuses absolute paths, backslashes, and ".." escapes without
// touching the filesystem, so it also protects paths that do not exist
// yet (fs_write targets). The symlink check resolves the deepest EXISTING
// ancestor of the target, so a link planted inside the workspace cannot
// alias a lexically clean path back out of it. A link created between
// check and use is out of scope: the tools container is the only writer
// during a run and handles calls serially (§R3).

package toolsrv

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// resolveUnder joins rel (a slash-separated path from the model) under
// root and returns the cleaned absolute path, refusing absolute inputs,
// ".." escapes, and any path whose symlink-resolved location leaves the
// symlink-resolved root. Empty rel is refused here: a tool with a default
// (fs_list's ".") must apply it before resolving, so "empty means root"
// is a per-tool decision, never a silent one.
func resolveUnder(root, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("empty path")
	}
	if strings.Contains(rel, "\\") {
		return "", fmt.Errorf("path %q contains a backslash; tool paths are slash-separated", rel)
	}
	// Both checks: path.IsAbs sees the model's slash form, filepath.IsAbs
	// the host form — on Windows only the latter catches "C:/x".
	if path.IsAbs(rel) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path %q is absolute; tool paths are relative to their root", rel)
	}
	clean := path.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path %q escapes the tool root", rel)
	}
	abs := filepath.Join(root, filepath.FromSlash(clean))

	// The lexical check cannot see symlinks. Resolve the root once (it may
	// itself sit behind links, e.g. /tmp on macOS), then resolve the
	// deepest existing ancestor of the target: for an existing target that
	// is the target itself, for a to-be-created one it is the directory
	// the write will really land in.
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve tool root %s: %w", root, err)
	}
	anc := abs
	for {
		if _, err := os.Lstat(anc); err == nil {
			break
		}
		parent := filepath.Dir(anc)
		if parent == anc {
			break
		}
		anc = parent
	}
	resolvedAnc, err := filepath.EvalSymlinks(anc)
	if err != nil {
		// Includes dangling symlinks: refusing them errs on the safe side.
		return "", fmt.Errorf("resolve %s: %w", anc, err)
	}
	if !within(resolvedRoot, resolvedAnc) {
		return "", fmt.Errorf("path %q escapes the tool root via a symlink", rel)
	}
	return abs, nil
}

// within reports whether p equals root or lies below it. Both arguments
// must already be symlink-resolved absolute paths; the separator suffix
// keeps "/work" from matching "/workspace".
func within(root, p string) bool {
	if p == root {
		return true
	}
	return strings.HasPrefix(p, root+string(filepath.Separator))
}

// allowedByGrant applies the manifest's paths-mode restriction (spec.go
// Access.Paths): with a non-empty grant list, the cleaned rel must equal
// an entry or sit under an entry treated as a directory prefix — entry
// "a/b" allows "a/b" and "a/b/c", never "a/bc". An empty list is the
// whole-workspace grant and allows everything; resolveUnder still bounds
// it to the root. Comparisons are slash-separated because both sides come
// from slash-path sources (the manifest and the model).
func allowedByGrant(paths []string, rel string) bool {
	if len(paths) == 0 {
		return true
	}
	clean := path.Clean(rel)
	for _, entry := range paths {
		e := path.Clean(entry)
		if clean == e || strings.HasPrefix(clean, e+"/") {
			return true
		}
	}
	return false
}
