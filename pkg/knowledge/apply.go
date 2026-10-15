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
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/blaktron/agentry-cli/pkg/plainpath"
)

// The apply step: one Creator run is one bounded ingest, whose output is
// the pages it wrote or rewrote and the sources it extracted, named in a
// changes.json. Apply turns the current pack and that output into the next
// pack: it lands the changes, lints the result and drops what fails — a
// page, never the build — regenerates the index, appends the log, refuses a
// pack past 10 MB, and records the digest. The pack on disk changes only
// when all of that succeeded, by one rename.

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
	// MaxBytes caps the pack Apply makes, documents included: MaxBytes
	// when zero (a build In Agentry's, and any caller that names none);
	// MaxLocalBytes for a build on this computer.
	MaxBytes int64
}

func (o ApplyOptions) limit() int64 {
	if o.MaxBytes > 0 {
		return o.MaxBytes
	}
	return MaxBytes
}

// Drop is a change Apply did not make as written: a written file that
// failed the lint (reverted to what the pack held, or not added), a delete
// kept back because a page that stays needs the file, or a written page
// kept with its links to nothing in the pack made plain text.
type Drop struct {
	Path   string
	Reason string
	// Kept is true for a delete kept back, and for a page kept with its
	// dead links unlinked.
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
// pack when packDir holds none. Every path the output names is the model's,
// so each is held to changePath's rules and then read and written only
// through an os.Root — at outDir for what the output carries, at the
// staging directory for what the next pack holds — so no name, link or
// separator in it reaches a file outside either.
func Apply(packDir, outDir string, o ApplyOptions) (*Result, error) {
	res, err := apply(packDir, outDir, o)
	if err != nil {
		return nil, fmt.Errorf("apply an ingest to the knowledge pack at %s: %w", packDir, err)
	}
	return res, nil
}

func apply(packDir, outDir string, o ApplyOptions) (*Result, error) {
	out, err := os.OpenRoot(outDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = out.Close() }()
	ch, err := readChanges(out)
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
	stageDir, err := os.MkdirTemp(filepath.Dir(packDir), "."+name+".apply-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(stageDir) }()
	stage, err := os.OpenRoot(stageDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stage.Close() }()
	var packRoot *os.Root
	if prev != nil {
		if packRoot, err = os.OpenRoot(packDir); err != nil {
			return nil, err
		}
		defer func() { _ = packRoot.Close() }()
		files, err := treeFiles(packDir)
		if err != nil {
			return nil, err
		}
		for _, rel := range files {
			if _, err := copyBetween(packRoot, stage, rel, o.limit()); err != nil {
				return nil, fmt.Errorf("%s: %w", rel, err)
			}
		}
	}
	for _, rel := range ch.Write {
		if _, err := copyBetween(out, stage, rel, o.limit()); err != nil {
			if errors.Is(err, ErrTooLarge) {
				return nil, fmt.Errorf("changes.json writes %s, past the pack's %s: %w", rel, FormatCap(o.limit()), err)
			}
			return nil, fmt.Errorf("changes.json writes %s: %w", rel, err)
		}
	}
	for _, rel := range ch.Delete {
		if err := stage.Remove(filepath.FromSlash(rel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	if err := ensureLog(stageDir); err != nil {
		return nil, err
	}

	res := &Result{}
	written, deleted := set(ch.Write), set(ch.Delete)
	if err := settle(stageDir, stage, packRoot, written, deleted, res, o.limit()); err != nil {
		return nil, err
	}
	if err := WriteIndex(stageDir, name); err != nil {
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
	if err := appendLog(stageDir, o.Now, ch, res); err != nil {
		return nil, err
	}

	d, err := TreeDigest(stageDir)
	if err != nil {
		return nil, err
	}
	pages, err := Pages(stageDir)
	if err != nil {
		return nil, err
	}
	m := Manifest{
		Schema: Schema, Name: name, Created: o.Now.UTC(), Updated: o.Now.UTC(), Creator: o.Creator,
		Pages: pages, Bytes: d.Bytes, SHA256: d.SHA256,
		// Anything this step writes is unscanned until a registry scans
		// it.
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
	if size := d.Bytes + int64(len(raw)); size > o.limit() {
		return nil, fmt.Errorf("the pack would be %d bytes, past %s: %w", size, FormatCap(o.limit()), ErrTooLarge)
	}
	if err := os.WriteFile(filepath.Join(stageDir, ManifestFile), raw, 0o644); err != nil { //nolint:gosec // the pack's clear manifest
		return nil, err
	}
	// Windows refuses to rename a directory a handle is open on ("sharing
	// violation"), and both roots are open on the directories the swap
	// renames: they are closed first. Nothing reads through them after
	// this point.
	_ = stage.Close()
	if packRoot != nil {
		_ = packRoot.Close()
	}
	if err := swapIn(stageDir, packDir, prev != nil); err != nil {
		return nil, err
	}
	res.Manifest = m
	return res, nil
}

// readChanges decodes and holds changes.json to its rules.
func readChanges(out *os.Root) (*Changes, error) {
	if _, err := plainpath.File(out, ChangesFile); err != nil {
		return nil, fmt.Errorf("read %s: %w", ChangesFile, err)
	}
	raw, err := out.ReadFile(ChangesFile)
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

// changePath says whether the Creator may change rel: a slash path under
// wiki/ or sources/ whose every part is a plain name — no empty part, no
// "." or "..", no backslash, colon or control character, which some
// filesystems read as separators, drives or streams — and not a file the
// apply step writes itself. os.Root is the second line behind this one.
func changePath(rel string) error {
	notUnder := fmt.Errorf("%q is not a file under wiki/ or sources/", rel)
	parts := strings.Split(rel, "/")
	if len(parts) < 2 || (parts[0] != WikiDir && parts[0] != SourcesDir) {
		return notUnder
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsFunc(part, func(r rune) bool {
			return r == '\\' || r == ':' || unicode.IsControl(r)
		}) {
			return notUnder
		}
	}
	switch {
	case parts[0] == WikiDir && parts[len(parts)-1] == IndexFile:
		return fmt.Errorf("%s is generated from the pages; it is not written", rel)
	case rel == WikiDir+"/"+LogFile:
		return fmt.Errorf("%s is appended by the apply step; it is not written", rel)
	}
	return nil
}

// settle lints the staged tree and backs out the changes that fail, until
// what remains passes or nothing this change did can be backed out. A
// finding that a delete explains keeps that file. A page this change wrote
// whose only findings are links to nothing in the pack is kept with those
// links made plain text: a run stopped at its budget has pages that link
// to ones it had not yet written, and dropping each would drop the pages
// linking to it in turn, leaving a stopped build nothing. Any other
// finding on a file this change wrote reverts the file to the pack's
// version, or removes it if it is new. Findings on files this change did
// not touch are the pack's own, and left alone.
func settle(stageDir string, stage, pack *os.Root, written, deleted map[string]bool, res *Result, limit int64) error {
	// unlinked are the pages already unlinked once: a page that still fails
	// after its unlink (a link the raw text cannot match, or one the unlink
	// uncovered) is dropped like any other, so every pass takes a file out
	// of written or deleted, or unlinks a page not unlinked before, and the
	// loop ends within its budget.
	unlinked := map[string]bool{}
	for range 2*len(written) + len(deleted) + 1 {
		findings, err := lintPages(stageDir)
		if err != nil {
			return err
		}
		changed := false
		byPage := map[string][]Finding{}
		var order []string
		for _, f := range findings {
			if _, ok := byPage[f.Page]; !ok {
				order = append(order, f.Page)
			}
			byPage[f.Page] = append(byPage[f.Page], f)
		}
		// restored are the deletes kept back in this pass: a finding on
		// one explains the same finding on every page, which the next
		// pass lints again with the file back.
		restored := map[string]bool{}
		for _, pg := range order {
			explained := false
			for _, f := range byPage[pg] {
				if f.Target != "" && restored[f.Target] {
					explained = true
					continue
				}
				if f.Target != "" && deleted[f.Target] {
					if err := restore(pack, stage, f.Target, limit); err != nil {
						return err
					}
					delete(deleted, f.Target)
					restored[f.Target] = true
					res.Dropped = append(res.Dropped, Drop{Path: f.Target, Kept: true, Reason: pg + " needs it (" + f.Message + ")"})
					explained, changed = true, true
				}
			}
			if explained || !written[pg] {
				continue
			}
			if hrefs := deadLinksOnly(byPage[pg]); len(hrefs) > 0 && !unlinked[pg] {
				unlinked[pg] = true
				if err := unlink(stage, pg, hrefs); err != nil {
					return err
				}
				res.Dropped = append(res.Dropped, Drop{Path: pg, Kept: true, Reason: "links to pages not in the pack made plain text: " + strings.Join(hrefs, ", ")})
				changed = true
				continue
			}
			if err := restore(pack, stage, pg, limit); err != nil {
				return err
			}
			delete(written, pg)
			forgetKept(res, pg)
			reasons := make([]string, 0, len(byPage[pg]))
			for _, f := range byPage[pg] {
				reasons = append(reasons, string(f.Rule)+": "+f.Message)
			}
			res.Dropped = append(res.Dropped, Drop{Path: pg, Reason: strings.Join(reasons, "; ")})
			changed = true
		}
		if !changed {
			break
		}
	}
	// A backstop: whatever this change wrote that still fails is backed
	// out, so a page that fails the lint never lands. The passes above
	// already make progress each time, so no test reaches it today; it is
	// here so that a future rule in them cannot land a failing page.
	findings, err := lintPages(stageDir)
	if err != nil {
		return err
	}
	for _, f := range findings {
		if !written[f.Page] {
			continue
		}
		if err := restore(pack, stage, f.Page, limit); err != nil {
			return err
		}
		delete(written, f.Page)
		forgetKept(res, f.Page)
		res.Dropped = append(res.Dropped, Drop{Path: f.Page, Reason: string(f.Rule) + ": " + f.Message})
	}
	return nil
}

// forgetKept takes back the note that a page was kept unlinked, once the
// page is dropped after all.
func forgetKept(res *Result, pg string) {
	res.Dropped = slices.DeleteFunc(res.Dropped, func(d Drop) bool { return d.Path == pg && d.Kept && strings.HasPrefix(d.Path, WikiDir+"/") })
}

// deadLinksOnly answers the links a page writes to nothing in the pack,
// when those are its only findings; nil when it has any other.
func deadLinksOnly(fs []Finding) []string {
	var hrefs []string
	for _, f := range fs {
		if f.Rule != RuleLink || f.Href == "" {
			return nil
		}
		if !slices.Contains(hrefs, f.Href) {
			hrefs = append(hrefs, f.Href)
		}
	}
	return hrefs
}

// unlink rewrites the staged page rel with each link to one of hrefs made
// its plain text: "[the depot](rotterdam.md)" becomes "the depot", and an
// image "![map](m.md)" becomes "map". It reads the page as the lint does
// (lintPage): only the body after the frontmatter, line by line, outside
// fenced blocks and code spans, with linkRE's own grammar.
func unlink(stage *os.Root, rel string, hrefs []string) error {
	f, err := stage.Open(rel)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil {
		return err
	}
	w, err := stage.OpenFile(rel, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	if _, err := w.WriteString(unlinkText(string(body), hrefs)); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}

func unlinkText(text string, hrefs []string) string {
	fm, ok := parseFrontmatter(text)
	if !ok {
		return text
	}
	raw := strings.Split(text, "\n")
	stripped := strings.Split(stripCode(text), "\n")
	for i := fm.bodyLine; i < len(raw) && i < len(stripped); i++ {
		line := raw[i]
		if strings.TrimSpace(stripped[i]) == "" {
			// Blank, or inside a fenced block the lint does not read.
			continue
		}
		code := codeRE.FindAllStringIndex(line, -1)
		ms := linkRE.FindAllStringSubmatchIndex(line, -1)
		for j := len(ms) - 1; j >= 0; j-- {
			m := ms[j]
			if !slices.Contains(hrefs, line[m[2]:m[3]]) || inSpan(code, m[0], m[1]) {
				continue
			}
			from, textEnd := m[0], strings.IndexByte(line[m[0]:], ']')+m[0]
			if from > 0 && line[from-1] == '!' {
				from--
			}
			line = line[:from] + line[m[0]+1:textEnd] + line[m[1]:]
		}
		raw[i] = line
	}
	return strings.Join(raw, "\n")
}

// inSpan says whether [from, to) overlaps any of spans.
func inSpan(spans [][]int, from, to int) bool {
	for _, sp := range spans {
		if from < sp[1] && sp[0] < to {
			return true
		}
	}
	return false
}

// restore puts rel in the stage back to what the pack held, or removes it
// when the pack held none (or there is no pack yet).
func restore(pack, stage *os.Root, rel string, limit int64) error {
	if pack != nil {
		if _, err := pack.Lstat(filepath.FromSlash(rel)); err == nil {
			_, err := copyBetween(pack, stage, rel, limit)
			return err
		}
	}
	if err := stage.Remove(filepath.FromSlash(rel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
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
	return strings.Join(strings.FieldsFunc(s, unicode.IsControl), " ")
}

// swapIn replaces the pack with the staged tree by rename.
func swapIn(stage, packDir string, hadPack bool) error {
	if !hadPack {
		// knowledge create DIR takes an existing empty DIR. os.Rename will
		// not rename onto a directory (Go refuses on every OS, Windows
		// itself too), so an empty one is removed first; os.Remove never
		// removes one that holds anything.
		if err := os.Remove(packDir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s is not empty and holds no knowledge pack; use an empty or a new folder: %w", packDir, err)
		}
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

// copyBetween copies the regular file rel (a slash path) from under one
// root to the same path under another, creating its directories, refusing
// anything but a regular file and more than limit bytes.
func copyBetween(src, dst *os.Root, rel string, limit int64) (int64, error) {
	name := filepath.FromSlash(rel)
	if _, err := plainpath.File(src, rel); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, err
		}
		return 0, fmt.Errorf("%w; a knowledge pack holds only files and directories", err)
	}
	in, err := src.Open(name)
	if err != nil {
		return 0, err
	}
	defer func() { _ = in.Close() }()
	if dir := filepath.Dir(name); dir != "." {
		if err := dst.MkdirAll(dir, 0o755); err != nil {
			return 0, err
		}
	}
	w, err := dst.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(w, io.LimitReader(in, limit+1))
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > limit {
		err = ErrTooLarge
	}
	return n, err
}
