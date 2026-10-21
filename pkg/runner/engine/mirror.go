package engine

// A persistent agent's share mirror: the folder an MCP server the person
// let see the share mounts read-only at the share's path, in place of the
// share itself, where the share is checked (spec.ServerShareMirror). It
// holds a copy of each share file whose bytes the share check passed, clean
// or read unchecked, and nothing else: a file the check found is left out,
// a file not checked yet is not there, and a file removed from the share is
// removed. The runner alone writes it, before each turn, so a server sees
// what the agent may read, as of that turn's check.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// shareMirror is a segment's share mirror: the folder, the folder each
// copy is written in before it is moved into place, and the index of what
// the folder holds.
type shareMirror struct {
	dir, staging, index string
	// files are the copies the folder holds, by their path beneath it.
	files map[string]mirrorFile
}

// mirrorFile is one copy in the mirror: the digest of the bytes copied,
// and the size and time the copy had once in place, by which a copy
// changed since is told apart without reading it.
type mirrorFile struct {
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	MTimeNano int64  `json:"mtimeNano"`
}

// mirrorIndex is the index's file.
type mirrorIndex struct {
	Version int                   `json:"version"`
	Files   map[string]mirrorFile `json:"files"`
}

const mirrorIndexVersion = 1

// errChangedSinceCheck is a copy whose bytes are not the ones checked.
var errChangedSinceCheck = errors.New("the file changed after it was checked")

// ShareMirrorBeside are the staging folder and the index the engine keeps
// beside the share mirror dir (PersistentSegment.ShareMirror).
func ShareMirrorBeside(dir string) (staging, index string) {
	return dir + ".staging", dir + ".json"
}

// folder is the mirror's folder; "" for none.
func (m *shareMirror) folder() string {
	if m == nil {
		return ""
	}
	return m.dir
}

// openShareMirror makes the mirror folder dir, and its staging folder,
// when they are missing, and reads the index of what it holds; an index
// that cannot be read is taken as empty, so every copy is made again.
func openShareMirror(dir string) (*shareMirror, error) {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return nil, fmt.Errorf("the share mirror %q is not a clean absolute path", dir)
	}
	if strings.Contains(strings.TrimPrefix(dir, filepath.VolumeName(dir)), ":") {
		// The gateway's bind lists are joined with colons.
		return nil, fmt.Errorf("the share mirror %q has a colon in its path", dir)
	}
	staging, index := ShareMirrorBeside(dir)
	m := &shareMirror{dir: dir, staging: staging, index: index, files: map[string]mirrorFile{}}
	for _, d := range []string{m.dir, m.staging} {
		if err := os.MkdirAll(d, 0o755); err != nil { //nolint:gosec // read by the servers' containers
			return nil, fmt.Errorf("the share mirror: %w", err)
		}
		fi, err := os.Lstat(d)
		if err != nil {
			return nil, fmt.Errorf("the share mirror: %w", err)
		}
		if !fi.IsDir() {
			return nil, fmt.Errorf("the share mirror %s is not a folder", d)
		}
	}
	if b, err := os.ReadFile(m.index); err == nil { //nolint:gosec // the agent's own state file
		var idx mirrorIndex
		if json.Unmarshal(b, &idx) == nil && idx.Version == mirrorIndexVersion && idx.Files != nil {
			m.files = idx.Files
		}
	}
	return m, nil
}

// passed says a verdict lets a server see the file: one the check read,
// clean or unchecked, never one it found.
func passed(v toolsrv.ShareVerdict) bool {
	return (v.Verdict == toolsrv.ShareClean || v.Verdict == toolsrv.ShareUnchecked) && plainDigest(v.SHA256)
}

// put moves into the mirror a copy of rel the check passed: src, a file of
// the runner's own holding the bytes that were checked, whose digest is
// sum. The copy is written in the staging folder and moved into place, so
// a server never reads half of it.
func (m *shareMirror) put(rel, src, sum string) error {
	if !filepath.IsLocal(filepath.FromSlash(rel)) {
		return fmt.Errorf("%s is not a path beneath the share", rel)
	}
	in, err := os.Open(src) //nolint:gosec // the runner's own copy
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	return m.install(rel, in, sum)
}

// install writes r into the staging folder, checks that its bytes are the
// digest sum, and moves them to rel in the mirror. Bytes of another digest
// are not moved in, and the copy of rel the mirror held is removed: the
// file changed after it was checked, and the next check covers it.
func (m *shareMirror) install(rel string, r io.Reader, sum string) error {
	tmp, err := os.CreateTemp(m.staging, "copy-*")
	if err != nil {
		return err
	}
	h := sha256.New()
	_, cerr := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, maxShareCheckBytes+1))
	if err := tmp.Chmod(0o644); cerr == nil {
		cerr = err
	}
	if err := tmp.Close(); cerr == nil {
		cerr = err
	}
	if cerr == nil && hex.EncodeToString(h.Sum(nil)) != sum {
		cerr = fmt.Errorf("%s: %w", rel, errChangedSinceCheck)
	}
	if cerr != nil {
		_ = os.Remove(tmp.Name())
		m.remove(rel)
		return cerr
	}
	root, err := os.OpenRoot(m.dir)
	if err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	defer func() { _ = root.Close() }()
	native := filepath.FromSlash(rel)
	// The folders on the way are made through the mirror's root, so none
	// can lead out of it; a folder in the way of a file, or a file in the
	// way of a folder, goes.
	if parent := filepath.Dir(native); parent != "." {
		if fi, err := root.Lstat(parent); err == nil && !fi.IsDir() {
			_ = root.Remove(parent)
		}
		if err := root.MkdirAll(parent, 0o755); err != nil {
			_ = os.Remove(tmp.Name())
			return err
		}
	}
	if fi, err := root.Lstat(native); err == nil && !fi.Mode().IsRegular() {
		_ = root.RemoveAll(native)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(m.dir, native)); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	fi, err := root.Lstat(native)
	if err != nil {
		return err
	}
	m.files[rel] = mirrorFile{SHA256: sum, Size: fi.Size(), MTimeNano: fi.ModTime().UnixNano()}
	return nil
}

// remove takes rel's copy out of the mirror.
func (m *shareMirror) remove(rel string) {
	delete(m.files, rel)
	if !filepath.IsLocal(filepath.FromSlash(rel)) {
		return
	}
	root, err := os.OpenRoot(m.dir)
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	_ = root.RemoveAll(filepath.FromSlash(rel))
}

// sync makes the mirror hold, for each file of the share whose verdict
// passed, a copy of the bytes that verdict was made for, and nothing else.
// A copy it holds already is kept while it is as it was put; one it lacks
// is copied from the share and moved in only when its bytes are the
// checked digest. Everything else in the folder is removed.
func (m *shareMirror) sync(share string, verdicts map[string]toolsrv.ShareVerdict) error {
	if m == nil {
		return nil
	}
	if err := m.syncTo(share, verdicts); err != nil {
		return fmt.Errorf("the share mirror: %w", err)
	}
	return nil
}

// syncTo is sync's work.
func (m *shareMirror) syncTo(share string, verdicts map[string]toolsrv.ShareVerdict) error {
	root, err := os.OpenRoot(m.dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	want := map[string]string{}
	for rel, v := range verdicts {
		if passed(v) && filepath.IsLocal(filepath.FromSlash(rel)) {
			want[rel] = v.SHA256
		}
	}
	for rel, f := range m.files {
		fi, err := root.Lstat(filepath.FromSlash(rel))
		if want[rel] != f.SHA256 || err != nil || !fi.Mode().IsRegular() || fi.Size() != f.Size || fi.ModTime().UnixNano() != f.MTimeNano {
			m.remove(rel)
		}
	}
	var problems []error
	if missing := m.missing(want); len(missing) > 0 {
		src, err := os.OpenRoot(share)
		if err != nil {
			return err
		}
		defer func() { _ = src.Close() }()
		for _, rel := range missing {
			if err := m.copyFromShare(src, rel, want[rel]); err != nil {
				problems = append(problems, fmt.Errorf("copy share/%s: %w", rel, err))
			}
		}
	}
	m.prune(root)
	if err := m.saveIndex(); err != nil {
		problems = append(problems, err)
	}
	return errors.Join(problems...)
}

// missing are the files wanted that the mirror does not hold, in order.
func (m *shareMirror) missing(want map[string]string) []string {
	var out []string
	for rel := range want {
		if _, ok := m.files[rel]; !ok {
			out = append(out, rel)
		}
	}
	slices.Sort(out)
	return out
}

// copyFromShare copies rel, a regular file beneath the share, into the
// mirror through the share's root, when its bytes are still sum.
func (m *shareMirror) copyFromShare(src *os.Root, rel, sum string) error {
	native := filepath.FromSlash(rel)
	fi, err := src.Lstat(native)
	if err != nil || !fi.Mode().IsRegular() {
		return nil // gone, or not a file: the next check drops it
	}
	f, err := src.Open(native)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	if err := m.install(rel, f, sum); err != nil && !errors.Is(err, errChangedSinceCheck) {
		return err
	}
	return nil
}

// prune removes from the mirror everything the index does not hold: a
// file, a link or anything else, and a folder left empty.
func (m *shareMirror) prune(root *os.Root) {
	var dirs []string
	_ = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == "." {
			return nil
		}
		if d.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		if _, ok := m.files[p]; !ok || !d.Type().IsRegular() {
			_ = root.Remove(filepath.FromSlash(p))
		}
		return nil
	})
	// Deepest first, so a folder emptied by its own folders goes too.
	slices.SortFunc(dirs, func(a, b string) int { return strings.Count(b, "/") - strings.Count(a, "/") })
	for _, d := range dirs {
		_ = root.Remove(filepath.FromSlash(d)) // only an empty one goes
	}
	// The staging folder holds nothing between two syncs.
	if entries, err := os.ReadDir(m.staging); err == nil {
		for _, e := range entries {
			_ = os.RemoveAll(filepath.Join(m.staging, e.Name()))
		}
	}
}

// saveIndex writes the index of what the mirror holds (0600).
func (m *shareMirror) saveIndex() error {
	b, err := json.Marshal(mirrorIndex{Version: mirrorIndexVersion, Files: m.files})
	if err != nil {
		return err
	}
	tmp := m.index + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.index)
}
