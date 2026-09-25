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

// A release is verified once, when it is laid down (the digest, the
// attestation, the consent), and then run from workspace/ for as long as
// it stays installed. Nothing checked the tree again, so an edit to an
// installed BriefAgent's prompt, tool config or schema (by the person,
// another program, or malware on the machine) ran under the signature and
// review of a release it no longer matched, and the receipt named a
// release whose bytes did not run. install.json, which says how the
// release was admitted, was plain JSON anything with write access to the
// runner home could rewrite (agentry-cli#326; the operator, 2026-09-25: "a
// signed BriefAgent should only run if it has not been altered locally").
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
//     before this change, by another runner home, or before the key was
//     replaced): ErrUnsealed. The tree is not known to be what was laid
//     down, and is laid down again from the registry, re-verified, before
//     it is used. Doing that is always safe; it is what a first run does.
//
// Someone who can write the install directory but not read the key can
// force a re-fetch (by deleting or re-keying the record); they cannot make
// an edited tree run. plan/, runs/ and export/ are not covered: the first
// two are the runner's own (the plan has its own record), and export/ is
// the coding-agent shape, which a run never reads.

// RecordFile is the integrity record's name beside install.json.
const RecordFile = "integrity.json"

const recordFormat = "agentry-installed-release/v1"

// ErrUnsealed reports an installed release with no integrity record this
// runner home can check: missing, unreadable, malformed, or made under
// another key. The caller lays it down again from the registry.
var ErrUnsealed = errors.New("installed release has no integrity record from this runner home")

// ErrNotInstalled reports that the release is not installed at the
// directory: no install.json, another release's, or no workspace.
var ErrNotInstalled = errors.New("release not installed")

// ChangedError reports an installed release whose files, or whose
// install.json, changed after it was laid down. Paths are relative to the
// release's own files (workspace/), in the form the archive carries them.
type ChangedError struct {
	// Ref is the release as its install.json names it, when that still
	// parses; Dir is the release directory.
	Ref, Dir string
	// Changed, Added and Removed are sorted.
	Changed, Added, Removed []string
	// Record is set when the record or install.json itself is what failed:
	// install.json changed, or the record does not verify under this home's
	// key.
	Record string
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
		parts = append(parts, e.Record)
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
	return fmt.Sprintf("installed release %s changed after it was installed: %s — refusing to run it; %s re-fetches and re-verifies it",
		what, strings.Join(parts, "; "), e.RepairCommand())
}

// RepairCommand is the command that lays the release down again.
func (e *ChangedError) RepairCommand() string {
	if e.Ref == "" {
		return "`agentry install --repair <ns/name@version>`"
	}
	return fmt.Sprintf("`agentry install --repair %s`", e.Ref)
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
func keyID(key []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte("agentry installed-release key id\x00"))
	_, _ = h.Write(key)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

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
// tree is what was sealed, ErrUnsealed when no record this key made is
// there, *ChangedError otherwise. metaBytes is install.json as read.
func check(key []byte, dir string, metaBytes []byte) error {
	b, err := os.ReadFile(filepath.Join(dir, RecordFile)) //nolint:gosec // our install layout
	if err != nil {
		return ErrUnsealed
	}
	var r record
	if err := json.Unmarshal(b, &r); err != nil || r.Format != recordFormat || r.Key != keyID(key) {
		return ErrUnsealed
	}
	changed := &ChangedError{Dir: dir}
	if !hmac.Equal([]byte(r.mac(key)), []byte(r.MAC)) {
		// A record under this home's key that its key did not write: it
		// was edited, most likely to match an edited tree, so the file
		// lists it holds are no evidence of anything.
		changed.Record = fmt.Sprintf("its integrity record (%s) does not verify under this runner home's key", RecordFile)
		return changed
	}
	if sha256Hex(metaBytes) != r.InstallJSON {
		changed.Record = "install.json changed"
	}
	now, err := treeEntries(filepath.Join(dir, "workspace"))
	if err != nil {
		return err
	}
	for p, want := range r.Files {
		got, ok := now[p]
		switch {
		case !ok:
			changed.Removed = append(changed.Removed, p)
		case got != want:
			changed.Changed = append(changed.Changed, p)
		}
	}
	for p := range now {
		if _, ok := r.Files[p]; !ok {
			changed.Added = append(changed.Added, p)
		}
	}
	if changed.Record == "" && len(changed.Changed)+len(changed.Added)+len(changed.Removed) == 0 {
		return nil
	}
	sort.Strings(changed.Changed)
	sort.Strings(changed.Added)
	sort.Strings(changed.Removed)
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
// ErrUnsealed when it is installed but carries no record this runner
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
	cerr := ErrUnsealed
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
	case errors.Is(cerr, ErrUnsealed):
		return &meta, ErrUnsealed
	case cerr != nil:
		// The tree could not be read to check it: not a verdict either
		// way, and not something to run.
		return nil, cerr
	}
	return &meta, nil
}
