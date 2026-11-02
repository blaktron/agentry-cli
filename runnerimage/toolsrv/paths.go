// Path safety for every filesystem-facing tool: fs_read, fs_list, fs_write,
// and write_report all funnel through resolveUnder, so there is exactly one
// place that decides whether a model-supplied path stays inside its root —
// and one place the traversal corpus has to hit.
//
// Two layers, because each catches what the other cannot. The lexical
// check refuses absolute paths, backslashes, and ".." escapes without
// touching the filesystem, so it also protects paths that do not exist
// yet (fs_write targets). The symlink check resolves the deepest EXISTING
// ancestor of the target, so a link planted inside the workspace cannot
// alias a lexically clean path back out of it. A link created between
// check and use, by a program running in the background, is caught at use
// in the folders a program can write: their tools open beneath the root
// (package beneath). No program writes the other roots.

package toolsrv

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/blaktron/agentry-cli/runnerimage/beneath"
)

// resolveUnder joins rel (a slash-separated path from the model) under
// root and returns the cleaned absolute path, refusing absolute inputs,
// ".." escapes, and any path whose symlink-resolved location leaves the
// symlink-resolved root. Empty rel is refused here: a tool with a default
// (fs_list's ".") must apply it before resolving, so "empty means root"
// is a per-tool decision, never a silent one.
func resolveUnder(root, rel string) (string, error) {
	abs, err := lexicallyUnder(root, rel)
	if err != nil {
		return "", err
	}

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

// lexicallyUnder is resolveUnder's first layer: rel joined under root,
// refused when it is empty, absolute, holds a backslash or climbs out with
// "..", all without touching the filesystem.
func lexicallyUnder(root, rel string) (string, error) {
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
	return filepath.Join(root, filepath.FromSlash(clean)), nil
}

// underResolver checks many paths beneath one root by resolveUnder's
// rules, for a tool that checks every file of an area in one call (the
// inputs' search targets). resolveUnder resolves the root and then the
// whole path for each file, one metadata call per path element, and where
// each call is a round trip (a host folder bind-mounted into a VM-backed
// engine) an area of a thousand files outlasts a harness's wait for the
// tool. This resolves the root once and each folder once, so a file that
// is not a link costs one Lstat. A path that is a link, or is missing, is
// left to resolveUnder whole. Links are known by Lstat's ModeSymlink, which
// is what a link is in the Linux container this server runs in.
type underResolver struct {
	root string
	// resolved is the symlink-resolved root, or err why it is not;
	// resolved once, at the first path.
	resolved string
	err      error
	done     bool
	// dirs holds each folder beneath root resolved so far.
	dirs  map[string]string
	calls resolverCalls
}

// resolverCalls are the two filesystem calls an underResolver makes:
// os.Lstat and filepath.EvalSymlinks, but in a test that counts or slows
// them.
type resolverCalls struct {
	lstat func(string) (fs.FileInfo, error)
	eval  func(string) (string, error)
}

func osResolverCalls() resolverCalls {
	return resolverCalls{lstat: os.Lstat, eval: filepath.EvalSymlinks}
}

func newUnderResolver(root string, calls resolverCalls) *underResolver {
	return &underResolver{root: filepath.Clean(root), dirs: map[string]string{}, calls: calls}
}

// resolve is resolveUnder(r.root, rel).
func (r *underResolver) resolve(rel string) (string, error) {
	abs, err := lexicallyUnder(r.root, rel)
	if err != nil {
		return "", err
	}
	if !r.done {
		r.done = true
		if r.resolved, r.err = r.calls.eval(r.root); r.err != nil {
			r.err = fmt.Errorf("resolve tool root %s: %w", r.root, r.err)
		}
	}
	if r.err != nil {
		return "", r.err
	}
	if abs == r.root {
		return abs, nil
	}
	fi, err := r.calls.lstat(abs)
	if err != nil || fi.Mode()&fs.ModeSymlink != 0 {
		return resolveUnder(r.root, rel)
	}
	dir, err := r.dir(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", abs, err)
	}
	if !within(r.resolved, filepath.Join(dir, filepath.Base(abs))) {
		return "", fmt.Errorf("path %q escapes the tool root via a symlink", rel)
	}
	return abs, nil
}

// dir is the symlink-resolved path of d, a folder lexically beneath the
// root: its parent's resolved path and its own name when it is no link,
// what the link resolves to when it is one.
func (r *underResolver) dir(d string) (string, error) {
	if d == r.root {
		return r.resolved, nil
	}
	if got, ok := r.dirs[d]; ok {
		return got, nil
	}
	fi, err := r.calls.lstat(d)
	if err != nil {
		return "", err
	}
	var got string
	if parent := filepath.Dir(d); fi.Mode()&fs.ModeSymlink != 0 || parent == d {
		if got, err = r.calls.eval(d); err != nil {
			return "", err
		}
	} else {
		if got, err = r.dir(parent); err != nil {
			return "", err
		}
		got = filepath.Join(got, filepath.Base(d))
	}
	r.dirs[d] = got
	return got, nil
}

// resolvers checks paths beneath several roots in one call, an
// underResolver a root; a nil one checks each path with resolveUnder.
type resolvers struct {
	calls resolverCalls
	roots map[string]*underResolver
}

func newResolvers(calls resolverCalls) *resolvers {
	return &resolvers{calls: calls, roots: map[string]*underResolver{}}
}

func (rs *resolvers) under(root, rel string) (string, error) {
	if rs == nil {
		return resolveUnder(root, rel)
	}
	r := rs.roots[root]
	if r == nil {
		r = newUnderResolver(root, rs.calls)
		rs.roots[root] = r
	}
	return r.resolve(rel)
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

// workspacePath checks a name the model gave a workspace tool, as
// resolveUnder does, and returns it cleaned and relative to the workspace.
// The beneath.Dir the tool then opens it through is what holds it beneath the
// workspace: a link a background process swaps in between this check and
// the open cannot take the read or the write out of it.
func (s *server) workspacePath(rel string) (string, error) {
	// resolveUnder refuses an absolute path too; this says which root the
	// path is relative to, so the model can retry in the form that works.
	if path.IsAbs(rel) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path %q is not in %s; give the path relative to it, such as notes.md for %s/notes.md", rel, s.workspaceRoot(), s.workspaceRoot())
	}
	if _, err := resolveUnder(s.spec.WorkspaceDir, rel); err != nil {
		return "", err
	}
	return path.Clean(rel), nil
}

// workspaceRel is a path the model gave a workspace tool, in the form the
// tools take: relative to the workspace. The System Prompt and the search
// tools show the workspace's folders by their full paths, so a path beneath
// the workspace given in full is the same path relative to it, and the
// workspace itself is ".". Any other path is returned as given, for the
// grant and workspacePath to judge: nothing is reached that the relative
// form would not reach.
func (s *server) workspaceRel(p string) string {
	if !filepath.IsAbs(s.spec.WorkspaceDir) {
		return p
	}
	root := path.Clean(s.workspaceRoot())
	clean := path.Clean(p)
	if clean == root {
		return "."
	}
	if rel, ok := strings.CutPrefix(clean, root+"/"); ok {
		return rel
	}
	return p
}

// workspaceRoot is the workspace's path as the model sees it.
func (s *server) workspaceRoot() string { return filepath.ToSlash(s.spec.WorkspaceDir) }

// outsideGrant is a workspace tool's refusal of a path its grant does not
// cover, naming the paths it does, so the model can retry with one of them.
func (s *server) outsideGrant(tool string, granted []string, rel string) string {
	return fmt.Sprintf("%s: %q is outside the granted paths, which are %s, relative to %s: give a path that begins with one of them, such as %s/…",
		tool, path.Clean(rel), strings.Join(granted, ", "), s.workspaceRoot(), path.Clean(granted[0]))
}

// mkdirsIn makes dir and any missing parent beneath root, each through
// root, and returns the directories it made, in order. A directory that is
// there is left as it is.
func mkdirsIn(root *beneath.Dir, dir string, mode os.FileMode) ([]string, error) {
	if dir == "." || dir == "" {
		return nil, nil
	}
	var made []string
	cur := ""
	for _, part := range strings.Split(dir, "/") {
		cur = path.Join(cur, part)
		err := root.Mkdir(cur, mode)
		switch {
		case err == nil:
			made = append(made, cur)
		case errors.Is(err, fs.ErrExist):
		default:
			return made, err
		}
	}
	return made, nil
}
