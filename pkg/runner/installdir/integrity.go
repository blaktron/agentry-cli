package installdir

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/integritykey"
)

// A release is checked once, when it is laid down (the digest, the
// attestation, the consent), and then run from workspace/ for as long as
// it stays installed. Nothing checked the tree again, so an edit to an
// installed QuickAgent's prompt, tool config or schema (by the person or
// another program) ran under the signature and review of a release it no
// longer matched, and the receipt named a release whose bytes did not
// run. install.json, which says how the
// release was admitted, was plain JSON anything with write access to the
// runner home could rewrite (agentry-cli#326; the operator, 2026-09-25: "a
// signed QuickAgent should only run if it has not been altered locally").
//
// The integrity record closes that. Every lay-down writes
// <release>/integrity.json inside the tree it publishes: a digest of every
// file under workspace/ (content, and whether it is executable), a digest
// of install.json's exact bytes, and an HMAC-SHA256 over all of it under
// the runner home's integrity key (integritykey, the key the plan record
// uses), which lives outside every install directory. Inspect checks the
// tree against it before a release is used:
//
//   - the record verifies and the tree matches: the release is what was
//     laid down;
//   - the record verifies and a file changed, appeared or disappeared, or
//     install.json changed; or the record names this home's key and its
//     MAC does not verify (a record edited to match an edited tree): a
//     *ChangedError naming the paths. It is refused, never quietly
//     repaired: an edited install is worth a person's attention, and
//     `agentry install --repair` is the way back;
//   - no record, or a record made under another key (a release laid down
//     by an older CLI, by another runner home, or before the key was
//     replaced): ErrNoRecord. The tree is not known to be what was laid
//     down, so it is not used: the caller fetches the release again, puts
//     it through the same digest and attestation gates as a first fetch,
//     and lays it down in its place.
//
// What it stops, and what it does not. Someone who can write an install
// directory but can neither read nor replace the key (a person's own
// edit, another program writing into the install, a tree copied from
// elsewhere) can force a re-fetch, by deleting or re-keying the record;
// they cannot make an edited tree run. Someone who can write the runner
// home itself can replace the key and make records of their own, as they
// could replace the CLI; the record does not stand against them. The
// runner checks the tree when it resolves the release, again before it
// compiles, and once more on the per-run copy it executes (CheckCopy), so
// an edit made meanwhile does not run either. plan/, runs/ and export/ are
// not covered: the first two are the runner's own (the plan has its own
// record), and export/ is the coding-agent shape, which a run never reads.

// RecordFile is the integrity record's name beside install.json.
const RecordFile = "integrity.json"

const recordFormat = "agentry-installed-release/v1"

// ErrNoRecord reports an installed release with no integrity record this
// runner home can check: missing, unreadable, malformed, or made under
// another key. The caller lays it down again from the registry.
var ErrNoRecord = errors.New("installed release has no integrity record from this runner home")

// ErrNotInstalled reports that the release is not installed at the
// directory: no install.json, another release's, or no workspace.
var ErrNotInstalled = errors.New("release not installed")

// RecordFailure names what failed when the record or install.json itself
// is what changed, rather than a file of the release.
type RecordFailure string

const (
	// InstallJSONChanged: install.json is not the one laid down.
	InstallJSONChanged RecordFailure = "install-json-changed"
	// RecordInvalid: the record names this home's key and does not verify
	// under it, so it was edited; its file list is no evidence of anything.
	RecordInvalid RecordFailure = "record-invalid"
)

var recordFailureText = map[RecordFailure]string{
	InstallJSONChanged: "install.json changed",
	RecordInvalid:      "its integrity record (" + RecordFile + ") does not verify under this runner home's key",
}

// ChangedError reports an installed release whose files, or whose
// install.json, changed after it was laid down. Paths are relative to the
// release's own files (workspace/), in the form the archive carries them.
type ChangedError struct {
	// Ref names the release. Inspect reads it from install.json, which is
	// untrusted once the record has failed: a caller that resolved the
	// release sets it from what it resolved. Dir is the release directory.
	Ref, Dir string
	// Changed, Added and Removed are sorted.
	Changed, Added, Removed []string
	// Record is set when the record or install.json itself failed.
	Record RecordFailure
	// Repair, when set, is the command that lays the release down again
	// in place of `agentry install --repair <ref>`: a local install
	// (agentry-cli#491) is laid down again from its folder, since no
	// registry holds it.
	Repair string
}

// maxNamed bounds how many paths of each kind the message spells out; the
// fields carry them all.
const maxNamed = 10

func (e *ChangedError) Error() string {
	what := e.Ref
	if what == "" {
		what = e.Dir
	} else {
		what = fmt.Sprintf("%s (%s)", e.Ref, e.Dir)
	}
	var parts []string
	if e.Record != "" {
		parts = append(parts, recordFailureText[e.Record])
	}
	for _, k := range []struct {
		label string
		paths []string
	}{{"changed", e.Changed}, {"added", e.Added}, {"removed", e.Removed}} {
		if len(k.paths) == 0 {
			continue
		}
		named := k.paths
		more := ""
		if len(named) > maxNamed {
			more = fmt.Sprintf(" and %d more", len(named)-maxNamed)
			named = named[:maxNamed]
		}
		parts = append(parts, fmt.Sprintf("%s %s%s", k.label, strings.Join(named, ", "), more))
	}
	if e.Repair != "" {
		return fmt.Sprintf("installed release %s changed after it was installed: %s — refused; `%s` lays it down again from its folder",
			what, strings.Join(parts, "; "), e.Repair)
	}
	return fmt.Sprintf("installed release %s changed after it was installed: %s — refused; `%s` lays it down again from a fresh download, checked as any install is",
		what, strings.Join(parts, "; "), e.RepairCommand())
}

// RepairCommand is the command that lays the release down again.
func (e *ChangedError) RepairCommand() string {
	if e.Repair != "" {
		return e.Repair
	}
	if e.Ref == "" {
		return "agentry install --repair <ns/name@version>"
	}
	return "agentry install --repair " + e.Ref
}

// record is integrity.json.
type record struct {
	Format string `json:"format"`
	// Key names the key the MAC is under, without revealing it.
	Key string `json:"key"`
	// InstallJSON is the sha256 of install.json's exact bytes.
	InstallJSON string `json:"installJson"`
	// Files maps each file under workspace/ (slash-separated) to its entry
	// (fileEntry).
	Files map[string]string `json:"files"`
	MAC   string            `json:"mac"`
}

// keyID names a key without revealing it.
func keyID(key []byte) string { return integritykey.ID(key, "agentry installed-release key id") }

// mac is the record's HMAC over its format, key id, install.json digest and
// every file entry in path order. NUL separates the fields: it can appear
// in neither a path nor an entry.
func (r *record) mac(key []byte) string {
	m := hmac.New(sha256.New, key)
	_, _ = fmt.Fprintf(m, "%s\x00%s\x00install.json\x00%s\x00", r.Format, r.Key, r.InstallJSON)
	paths := make([]string, 0, len(r.Files))
	for p := range r.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		_, _ = fmt.Fprintf(m, "%s\x00%s\x00", p, r.Files[p])
	}
	return hex.EncodeToString(m.Sum(nil))
}

// fileEntry is what the record holds for one entry of the tree: a regular
// file's content digest, prefixed x: when executable and f: otherwise; a
// symlink's target (a lay-down copies files, so a symlink is only ever
// someone else's); anything else by its type, so that it never matches.
func fileEntry(path string, d fs.DirEntry) (string, error) {
	info, err := d.Info()
	if err != nil {
		return "", err
	}
	switch mode := info.Mode(); {
	case mode.IsRegular():
		f, err := os.Open(path) //nolint:gosec // a file of the release being checked
		if err != nil {
			return "", err
		}
		defer func() { _ = f.Close() }()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return "", err
		}
		kind := "f"
		if mode.Perm()&0o111 != 0 {
			kind = "x"
		}
		return kind + ":sha256:" + hex.EncodeToString(h.Sum(nil)), nil
	case mode&fs.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		return "l:" + target, nil
	default:
		return "other:" + mode.Type().String(), nil
	}
}

// treeEntries walks a workspace and returns every non-directory entry. A
// workspace that is gone has no entries: every file the record names is
// then removed.
func treeEntries(workspace string) (map[string]string, error) {
	files := map[string]string{}
	if _, err := os.Lstat(workspace); errors.Is(err, fs.ErrNotExist) {
		return files, nil
	}
	err := filepath.WalkDir(workspace, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(workspace, p)
		if err != nil {
			return err
		}
		entry, err := fileEntry(p, d)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = entry
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the installed tree: %w", err)
	}
	return files, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// seal writes dir's integrity record under key, over workspace/ and
// install.json as they are now. The lay-down calls it last, inside the tree
// it is about to publish.
func seal(key []byte, dir string) error {
	meta, err := os.ReadFile(filepath.Join(dir, "install.json")) //nolint:gosec // our install layout
	if err != nil {
		return fmt.Errorf("seal the install: %w", err)
	}
	files, err := treeEntries(filepath.Join(dir, "workspace"))
	if err != nil {
		return fmt.Errorf("seal the install: %w", err)
	}
	r := &record{Format: recordFormat, Key: keyID(key), InstallJSON: sha256Hex(meta), Files: files}
	r.MAC = r.mac(key)
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("seal the install: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, RecordFile), append(b, '\n'), 0o644); err != nil { //nolint:gosec // digests and a MAC, not a secret
		return fmt.Errorf("seal the install: %w", err)
	}
	return nil
}

// check holds dir against its integrity record under key: nil when the
// tree is what was sealed, ErrNoRecord when no record this key made is
// there, *ChangedError otherwise. metaBytes is install.json as read.
func check(key []byte, dir string, metaBytes []byte) error {
	b, err := os.ReadFile(filepath.Join(dir, RecordFile)) //nolint:gosec // our install layout
	if err != nil {
		return ErrNoRecord
	}
	var r record
	if err := json.Unmarshal(b, &r); err != nil || r.Format != recordFormat || r.Key != keyID(key) {
		return ErrNoRecord
	}
	changed := &ChangedError{Dir: dir}
	if !hmac.Equal([]byte(r.mac(key)), []byte(r.MAC)) {
		// A record under this home's key that its key did not write: it
		// was edited, most likely to match an edited tree, so the file
		// lists it holds are no evidence of anything.
		changed.Record = RecordInvalid
		return changed
	}
	if sha256Hex(metaBytes) != r.InstallJSON {
		changed.Record = InstallJSONChanged
	}
	now, err := treeEntries(filepath.Join(dir, "workspace"))
	if err != nil {
		return err
	}
	compareEntries(changed, r.Files, now, func(want, got string) bool { return want == got })
	if changed.Record == "" && len(changed.Changed)+len(changed.Added)+len(changed.Removed) == 0 {
		return nil
	}
	return changed
}

// compareEntries fills changed with the difference between the record's
// entries and the tree's, same deciding whether two entries match.
func compareEntries(changed *ChangedError, want, now map[string]string, same func(want, got string) bool) {
	for p, w := range want {
		got, ok := now[p]
		switch {
		case !ok:
			changed.Removed = append(changed.Removed, p)
		case !same(w, got):
			changed.Changed = append(changed.Changed, p)
		}
	}
	for p := range now {
		if _, ok := want[p]; !ok {
			changed.Added = append(changed.Added, p)
		}
	}
	sort.Strings(changed.Changed)
	sort.Strings(changed.Added)
	sort.Strings(changed.Removed)
}

// contentOf is an entry without its file-type prefix: a regular file's
// content digest, whatever its mode.
func contentOf(entry string) string {
	if strings.HasPrefix(entry, "f:") || strings.HasPrefix(entry, "x:") {
		return entry[2:]
	}
	return entry
}

// sameFiles reports whether workspace holds exactly srcTree's files, by
// content, as CopyTreeSkipping copies them (through symlinks, leaving out
// what is not part of a release).
func sameFiles(srcTree, workspace string) error {
	want := map[string]string{}
	err := filepath.WalkDir(srcTree, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != srcTree && notPartOfARelease(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(srcTree, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p) //nolint:gosec // the verified source being laid down
		if err != nil {
			return err
		}
		want[filepath.ToSlash(rel)] = sha256Hex(b)
		return nil
	})
	if err != nil {
		return fmt.Errorf("read the release's files: %w", err)
	}
	now, err := treeEntries(workspace)
	if err != nil {
		return err
	}
	diff := &ChangedError{Dir: workspace}
	compareEntries(diff, want, now, func(w, got string) bool { return w == contentOf(got) })
	if len(diff.Changed)+len(diff.Added)+len(diff.Removed) > 0 {
		return fmt.Errorf("the copy of the release being laid down is not the verified files (changed %v, added %v, removed %v) — not sealed, nothing published", diff.Changed, diff.Added, diff.Removed)
	}
	return nil
}

// CheckCopy holds a copy of an installed release's files (a run's
// per-run workspace, made after Inspect) to the release's integrity
// record, so an edit made to the install between the check and the copy
// does not run. A copy is made by reading through symlinks and writing
// under the process umask, so it is compared on content alone: a symlink
// planted in the install after the check arrives in the copy as the
// content of what it points at, which the record does not hold.
// releaseDir is the installed release, copyDir the copy of its
// workspace/.
func CheckCopy(releaseDir, copyDir string) error {
	metaBytes, err := os.ReadFile(filepath.Join(releaseDir, "install.json")) //nolint:gosec // our install layout
	if err != nil {
		return fmt.Errorf("check the run's copy of the release: %w", err)
	}
	home, err := HomeOf(releaseDir)
	if err != nil {
		return err
	}
	key, err := integritykey.Read(home)
	if err != nil {
		return fmt.Errorf("check the run's copy of the release: %w", err)
	}
	b, err := os.ReadFile(filepath.Join(releaseDir, RecordFile)) //nolint:gosec // our install layout
	if err != nil {
		return fmt.Errorf("check the run's copy of the release: %w", ErrNoRecord)
	}
	var r record
	if err := json.Unmarshal(b, &r); err != nil || r.Format != recordFormat || r.Key != keyID(key) {
		return fmt.Errorf("check the run's copy of the release: %w", ErrNoRecord)
	}
	changed := &ChangedError{Dir: releaseDir}
	if !hmac.Equal([]byte(r.mac(key)), []byte(r.MAC)) {
		changed.Record = RecordInvalid
		return changed
	}
	if sha256Hex(metaBytes) != r.InstallJSON {
		changed.Record = InstallJSONChanged
	}
	now, err := treeEntries(copyDir)
	if err != nil {
		return err
	}
	compareEntries(changed, r.Files, now, func(want, got string) bool { return contentOf(want) == contentOf(got) })
	if changed.Record == "" && len(changed.Changed)+len(changed.Added)+len(changed.Removed) == 0 {
		return nil
	}
	return changed
}

// HomeOf is the runner home a release directory sits in: every release is
// laid down at <home>/agents/<ns>/<name>/<release> (ReleaseDir), and its
// integrity record is made under that home's key.
func HomeOf(releaseDir string) (string, error) {
	agents := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Clean(releaseDir))))
	if filepath.Base(agents) != "agents" {
		return "", fmt.Errorf("%s is not a release directory in a runner home (<home>/agents/<ns>/<name>/<release>)", releaseDir)
	}
	return filepath.Dir(agents), nil
}

// Inspect reads the release installed at dir and holds it to its integrity
// record before anything trusts it: the Meta and nil when the release with
// wantDigest is installed there and is what was laid down;
// ErrNotInstalled when it is not installed there; the Meta and
// ErrNoRecord when it is installed but carries no record this runner
// home's key made; *ChangedError when it changed after it was laid down.
// The record is checked before install.json is believed, so an edited
// install.json is reported as changed, not as another release.
func Inspect(dir, wantDigest string) (*Meta, error) {
	metaBytes, err := os.ReadFile(filepath.Join(dir, "install.json")) //nolint:gosec // our install layout
	if err != nil {
		return nil, ErrNotInstalled
	}
	home, err := HomeOf(dir)
	if err != nil {
		return nil, err
	}
	// Read, never create: Inspect writes nothing (run --plan calls it on
	// the real home). A home with no key has recorded nothing under one.
	cerr := ErrNoRecord
	key, err := integritykey.Read(home)
	switch {
	case err == nil:
		cerr = check(key, dir, metaBytes)
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	var changed *ChangedError
	if errors.As(cerr, &changed) {
		var m Meta
		if json.Unmarshal(metaBytes, &m) == nil {
			changed.Ref = m.Ref
		}
		return nil, changed
	}
	var meta Meta
	if err := json.Unmarshal(metaBytes, &meta); err != nil || meta.Digest != wantDigest {
		return nil, ErrNotInstalled
	}
	if _, err := os.Stat(filepath.Join(dir, "workspace")); err != nil {
		return nil, ErrNotInstalled
	}
	switch {
	case errors.Is(cerr, ErrNoRecord):
		return &meta, ErrNoRecord
	case cerr != nil:
		// The tree could not be read to check it: not a verdict either
		// way, and not something to run.
		return nil, cerr
	}
	return &meta, nil
}
