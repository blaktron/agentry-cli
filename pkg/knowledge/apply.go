package knowledge

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

// The apply step (plan knowledge.md §4.2): one Creator run is one bounded
// ingest, whose output is the pages it wrote or rewrote and the sources it
// extracted, named in a changes.json. Apply turns the current pack and that
// output into the next pack: it lands the changes, lints the result and
// drops what fails — a page, never the build — regenerates the index,
// appends the log, refuses a pack past 10 MB, and records the digest. The
// pack on disk changes only when all of that succeeded, by one rename.

// ChangesFile is the Creator's account of one ingest, beside the files it
// names; ChangesSchema is its format.
const (
	ChangesFile   = "changes.json"
	ChangesSchema = "knowledge/changes/v1"
)

// Changes is changes.json.
type Changes struct {
	Schema string `json:"schema"`
	// Write names files, relative to the pack, under wiki/ or sources/,
	// each carried at the same relative path beside changes.json.
	Write []string `json:"write,omitempty"`
	// Delete names files, relative to the pack, to remove.
	Delete []string `json:"delete,omitempty"`
	// Documents are the input documents this ingest took, for the log.
	Documents []string `json:"documents,omitempty"`
	// Summary is one line for the log.
	Summary string `json:"summary,omitempty"`
}

// ApplyOptions are the facts Apply cannot read from the output.
type ApplyOptions struct {
	// Name names a new pack; ignored when the pack exists.
	Name string
	// Now stamps the manifest and the log entry.
	Now time.Time
	// Creator is recorded as what last extended the pack.
	Creator Creator
}

// Drop is a change Apply did not make: a written file that failed the lint
// (reverted to what the pack held, or not added), or a delete kept back
// because a page that stays needs the file.
type Drop struct {
	Path   string
	Reason string
	// Kept is true for a delete kept back.
	Kept bool
}

// Result is what one Apply did.
type Result struct {
	Manifest Manifest
	Written  []string
	Deleted  []string
	Dropped  []Drop
}

// Apply applies the output in outDir to the pack at packDir, creating the
// pack when packDir holds none.
func Apply(packDir, outDir string, o ApplyOptions) (*Result, error) {
	ch, err := readChanges(outDir)
	if err != nil {
		return nil, err
	}
	var prev *Pack
	if _, err := os.Stat(filepath.Join(packDir, ManifestFile)); err == nil {
		if prev, err = Load(packDir); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	name := o.Name
	if prev != nil {
		name = prev.Manifest.Name
	}
	if err := ValidName(name); err != nil {
		return nil, fmt.Errorf("a new knowledge pack needs a name: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(packDir), 0o755); err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp(filepath.Dir(packDir), "."+name+".apply-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	if prev != nil {
		for _, sub := range []string{WikiDir, SourcesDir} {
			if err := copyDir(filepath.Join(packDir, sub), filepath.Join(stage, sub)); err != nil {
				return nil, err
			}
		}
	}
	for _, rel := range ch.Write {
		if err := copyFile(filepath.Join(outDir, filepath.FromSlash(rel)), filepath.Join(stage, filepath.FromSlash(rel))); err != nil {
			return nil, fmt.Errorf("changes.json writes %s: %w", rel, err)
		}
	}
	for _, rel := range ch.Delete {
		if err := os.Remove(filepath.Join(stage, filepath.FromSlash(rel))); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	if err := ensureLog(stage); err != nil {
		return nil, err
	}

	res := &Result{}
	written, deleted := set(ch.Write), set(ch.Delete)
	if err := settle(stage, packDir, prev != nil, written, deleted, res); err != nil {
		return nil, err
	}
	if err := WriteIndex(stage, name); err != nil {
		return nil, err
	}
	for rel := range written {
		res.Written = append(res.Written, rel)
	}
	for rel := range deleted {
		res.Deleted = append(res.Deleted, rel)
	}
	sort.Strings(res.Written)
	sort.Strings(res.Deleted)
	sort.Slice(res.Dropped, func(i, j int) bool { return res.Dropped[i].Path < res.Dropped[j].Path })
	if err := appendLog(stage, o.Now, ch, res); err != nil {
		return nil, err
	}

	d, err := TreeDigest(stage)
	if err != nil {
		return nil, err
	}
	pages, err := Pages(stage)
	if err != nil {
		return nil, err
	}
	m := Manifest{
		Schema: Schema, Name: name, Created: o.Now.UTC(), Updated: o.Now.UTC(), Creator: o.Creator,
		Pages: pages, Bytes: d.Bytes, SHA256: d.SHA256,
		// Anything this step writes is unscanned until a registry scans
		// it (plan D19).
		Scan: Scan{},
	}
	if prev != nil {
		m.ID, m.Created = prev.Manifest.ID, prev.Manifest.Created
	} else if m.ID, err = newID(); err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	raw = append(raw, '\n')
	if size := d.Bytes + int64(len(raw)); size > MaxBytes {
		return nil, fmt.Errorf("knowledge pack %s would be %d bytes: %w", name, size, ErrTooLarge)
	}
	if err := os.WriteFile(filepath.Join(stage, ManifestFile), raw, 0o644); err != nil { //nolint:gosec // the pack's clear manifest
		return nil, err
	}
	if err := swapIn(stage, packDir, prev != nil); err != nil {
		return nil, err
	}
	res.Manifest = m
	return res, nil
}

// readChanges decodes and holds changes.json to its rules.
func readChanges(outDir string) (*Changes, error) {
	raw, err := os.ReadFile(filepath.Join(outDir, ChangesFile)) //nolint:gosec // the output the caller named
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ChangesFile, err)
	}
	var ch Changes
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ch); err != nil {
		return nil, fmt.Errorf("%s: %w", ChangesFile, err)
	}
	if ch.Schema != ChangesSchema {
		return nil, fmt.Errorf("%s: schema %q is not %q", ChangesFile, ch.Schema, ChangesSchema)
	}
	seen := map[string]bool{}
	for _, list := range [][]string{ch.Write, ch.Delete} {
		for _, rel := range list {
			if err := changePath(rel); err != nil {
				return nil, fmt.Errorf("%s: %w", ChangesFile, err)
			}
			if seen[rel] {
				return nil, fmt.Errorf("%s names %s twice", ChangesFile, rel)
			}
			seen[rel] = true
		}
	}
	return &ch, nil
}

// changePath says whether the Creator may change rel.
func changePath(rel string) error {
	clean := path.Clean(rel)
	switch {
	case clean != rel || strings.HasPrefix(rel, "/") || (!strings.HasPrefix(rel, WikiDir+"/") && !strings.HasPrefix(rel, SourcesDir+"/")):
		return fmt.Errorf("%q is not a file under wiki/ or sources/", rel)
	case path.Base(rel) == IndexFile && strings.HasPrefix(rel, WikiDir+"/"):
		return fmt.Errorf("%s is generated from the pages; it is not written", rel)
	case rel == WikiDir+"/"+LogFile:
		return fmt.Errorf("%s is appended by the apply step; it is not written", rel)
	}
	return nil
}

// settle lints the staged tree and backs out the changes that fail, until
// what remains passes or nothing this change did can be backed out. A
// finding that a delete explains keeps that file; any other finding on a
// file this change wrote reverts the file to the pack's version, or
// removes it if it is new. Findings on files this change did not touch
// are the pack's own, and left alone.
func settle(stage, packDir string, hadPack bool, written, deleted map[string]bool, res *Result) error {
	for range len(written) + len(deleted) + 1 {
		fs, err := lintPages(stage)
		if err != nil {
			return err
		}
		changed := false
		byPage := map[string][]Finding{}
		var order []string
		for _, f := range fs {
			if _, ok := byPage[f.Page]; !ok {
				order = append(order, f.Page)
			}
			byPage[f.Page] = append(byPage[f.Page], f)
		}
		for _, pg := range order {
			explained := false
			for _, f := range byPage[pg] {
				for target := range deleted {
					if (f.Rule == "link" || f.Rule == "citation") && refersTo(f, pg, target) {
						if err := restore(packDir, stage, target, hadPack); err != nil {
							return err
						}
						delete(deleted, target)
						res.Dropped = append(res.Dropped, Drop{Path: target, Kept: true, Reason: pg + " needs it (" + f.Message + ")"})
						explained, changed = true, true
					}
				}
			}
			if explained || !written[pg] {
				continue
			}
			if err := restore(packDir, stage, pg, hadPack); err != nil {
				return err
			}
			delete(written, pg)
			reasons := make([]string, 0, len(byPage[pg]))
			for _, f := range byPage[pg] {
				reasons = append(reasons, f.Rule+": "+f.Message)
			}
			res.Dropped = append(res.Dropped, Drop{Path: pg, Reason: strings.Join(reasons, "; ")})
			changed = true
		}
		if !changed {
			return nil
		}
	}
	return nil
}

// refersTo says whether finding f, on page pg, is about target.
func refersTo(f Finding, pg, target string) bool {
	if f.Rule == "citation" {
		src, _ := strings.CutPrefix(target, SourcesDir+"/")
		return strings.HasPrefix(target, SourcesDir+"/") && strings.Contains(f.Message, citeDoc+src+" ")
	}
	link := strings.TrimSuffix(strings.TrimPrefix(f.Message, "link "), " resolves to nothing in the pack")
	link, _, _ = strings.Cut(link, "#")
	return path.Clean(path.Join(path.Dir(pg), link)) == target
}

// restore puts rel in stage back to what the pack held, or removes it.
func restore(packDir, stage, rel string, hadPack bool) error {
	dst := filepath.Join(stage, filepath.FromSlash(rel))
	if hadPack {
		src := filepath.Join(packDir, filepath.FromSlash(rel))
		if _, err := os.Lstat(src); err == nil {
			return copyFile(src, dst)
		}
	}
	if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

const logHeader = "# Log\n\nAppend-only: every ingest, with the documents it took. Written by the apply step.\n"

func ensureLog(stage string) error {
	p := filepath.Join(stage, WikiDir, LogFile)
	if _, err := os.Lstat(p); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(logHeader), 0o644) //nolint:gosec // a page of the pack
}

// appendLog adds this ingest's entry. The document names and the summary
// come from the model and are flattened to one line each, so that nothing
// in them can read as an entry of its own.
func appendLog(stage string, now time.Time, ch *Changes, res *Result) error {
	docs := make([]string, 0, len(ch.Documents))
	for _, d := range ch.Documents {
		docs = append(docs, oneLine(d))
	}
	title := strings.Join(docs, ", ")
	if title == "" {
		title = "(no documents named)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n## [%s] ingest | %s\n\n", now.UTC().Format("2006-01-02"), title)
	if s := oneLine(ch.Summary); s != "" {
		b.WriteString(s + "\n\n")
	}
	if len(res.Written) > 0 {
		b.WriteString("- wrote: " + strings.Join(res.Written, ", ") + "\n")
	}
	if len(res.Deleted) > 0 {
		b.WriteString("- deleted: " + strings.Join(res.Deleted, ", ") + "\n")
	}
	for _, d := range res.Dropped {
		verb := "dropped"
		if d.Kept {
			verb = "kept"
		}
		b.WriteString("- " + verb + " " + d.Path + ": " + oneLine(d.Reason) + "\n")
	}
	f, err := os.OpenFile(filepath.Join(stage, WikiDir, LogFile), os.O_APPEND|os.O_WRONLY, 0o644) //nolint:gosec // the staged log
	if err != nil {
		return err
	}
	_, err = f.WriteString(b.String())
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func oneLine(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsControl(r) }), " ")
}

// swapIn replaces the pack with the staged tree by rename.
func swapIn(stage, packDir string, hadPack bool) error {
	if !hadPack {
		return os.Rename(stage, packDir)
	}
	old := packDir + ".old-" + filepath.Base(stage)
	if err := os.Rename(packDir, old); err != nil {
		return err
	}
	if err := os.Rename(stage, packDir); err != nil {
		_ = os.Rename(old, packDir)
		return err
	}
	return os.RemoveAll(old)
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "kp_" + hex.EncodeToString(b), nil
}

func set(list []string) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, s := range list {
		out[s] = true
	}
	return out
}

// copyDir copies the regular files under src into dst; a missing src is an
// empty one. Anything but a directory or a regular file is refused.
func copyDir(src, dst string) error {
	if _, err := os.Lstat(src); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		return copyFile(p, filepath.Join(dst, rel))
	})
}

// copyFile copies one regular file, refusing anything else.
func copyFile(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", src)
	}
	in, err := os.Open(src) //nolint:gosec // checked to be a regular file above
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644) //nolint:gosec // a pack file
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}
