package decode

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/plainpath"

	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
)

// DocumentsFile is the list of what a run was given, at the root of what it
// reads (the Knowledge Creator's inputs; a hosted run's).
const DocumentsFile = "documents.json"

// Document is one line of documents.json.
type Document struct {
	// Name is the document as it was staged, and as a knowledge pack cites
	// it: [src: doc:<Name>].
	Name string `json:"name"`
	// GivenAs is the path it was given as, when that differs.
	GivenAs string `json:"givenAs,omitempty"`
	// Read is the file to read for it: the document itself when it is
	// text, else the text decoded from it. Empty when it could not be read.
	Read   string              `json:"read,omitempty"`
	Family runnerdecode.Family `json:"family"`
	// DecodedBy names the decoder when the text was decoded.
	DecodedBy string `json:"decodedBy,omitempty"`
	// Unread says why a document could not be read, in the decode's words:
	// the answer for an undecodable file, the same for every caller.
	Unread string `json:"unread,omitempty"`
}

// archiveNotRead is why a built-in's build lists an archive as unread when
// none of the files it holds could be read and kept as given.
const archiveNotRead = "an archive none of whose files could be read and kept as given"

// memberNotKept is why a member that was read is not: its own bytes could
// not be kept as they were in the archive (past the cap, or not the bytes
// the malware check read).
const memberNotKept = "a file in an archive that could not be kept as it was given"

// Stage lays out what a run reads from this decode, in dst (a directory the
// caller made): each readable document's text at its Read path, and
// documents.json naming every document — those that could not be read too,
// with why. It returns the documents and the paths to stage under /inputs.
// Two documents that would be read as one file are refused rather than one
// hiding the other. Stage is the built-ins' layout (the Knowledge Creator,
// the Voice Writer), which keep each document as given. A decode that kept
// members (Options.KeepMembers) lists each member of an archive as a
// document of its own, named <archive>/<member>, and the archive is not a
// document then, since a file and a folder of its members cannot share a
// name; a member that was read but could not be kept as given is unread,
// and an archive none of whose members was kept is listed, unread, as it
// was given. Without kept members an archive is unread and its members are
// left out.
func (r *Result) Stage(givenAs map[string]string, dst string) ([]Document, []string, error) {
	kept := map[string]bool{} // the archives some member of which is kept
	if r.KeepMembers {
		for _, f := range r.Report.Files {
			if f.In != "" && f.Original != "" {
				top, _, _ := strings.Cut(f.Path, "/")
				kept[top] = true
			}
		}
	}
	listed := func(f runnerdecode.File) bool {
		top, _, _ := strings.Cut(f.Path, "/")
		switch {
		case f.In == "":
			return f.Outcome != runnerdecode.OutcomeUnpacked || !kept[f.Path]
		case !kept[top]:
			return false
		default:
			// Within a kept archive: each member but a nested archive,
			// whose own members are listed.
			return f.Outcome != runnerdecode.OutcomeUnpacked
		}
	}
	readable := func(f runnerdecode.File) bool {
		if f.Outcome != runnerdecode.OutcomeText && f.Outcome != runnerdecode.OutcomeDecoded {
			return false
		}
		return f.In == "" || f.Original != ""
	}
	claimed := map[string]string{}
	claim := func(name, by string) error {
		if prev, dup := claimed[name]; dup && prev != by {
			return fmt.Errorf("the documents %s and %s would both be read as %s; rename one", prev, by, name)
		}
		claimed[name] = by
		return nil
	}
	for _, f := range r.Report.Files {
		if !listed(f) || !readable(f) {
			continue
		}
		switch f.Outcome {
		case runnerdecode.OutcomeDecoded:
			if err := claim(f.Output, f.Path); err != nil {
				return nil, nil, err
			}
			fallthrough
		case runnerdecode.OutcomeText:
			if err := claim(f.Path, f.Path); err != nil {
				return nil, nil, err
			}
		}
	}
	docs := make([]Document, 0, len(r.Report.Files))
	for _, f := range r.Report.Files {
		if !listed(f) {
			continue
		}
		doc := Document{Name: f.Path, Family: f.Family}
		if g := givenAs[f.Path]; g != "" && g != f.Path {
			doc.GivenAs = g
		}
		var err error
		switch {
		case f.In != "" && (f.Outcome == runnerdecode.OutcomeText || f.Outcome == runnerdecode.OutcomeDecoded) && f.Original == "":
			doc.Unread = memberNotKept
		case f.Outcome == runnerdecode.OutcomeText:
			doc.Read = f.Path
			err = r.CopyOriginal(f.Path, filepath.Join(dst, filepath.FromSlash(doc.Read)))
		case f.Outcome == runnerdecode.OutcomeUnpacked:
			doc.Unread = archiveNotRead
		case f.Outcome == runnerdecode.OutcomeDecoded:
			doc.Read, doc.DecodedBy = f.Output, f.Tool
			err = r.CopyText(f.Path, filepath.Join(dst, filepath.FromSlash(doc.Read)))
		default:
			doc.Unread = UnreadReason(f)
		}
		if err != nil {
			return nil, nil, err
		}
		docs = append(docs, doc)
	}
	raw, err := json.MarshalIndent(map[string]any{"documents": docs}, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(filepath.Join(dst, DocumentsFile), append(raw, '\n'), 0o644); err != nil { //nolint:gosec // read by the run
		return nil, nil, err
	}
	entries, err := os.ReadDir(dst)
	if err != nil {
		return nil, nil, err
	}
	inputs := make([]string, 0, len(entries))
	for _, e := range entries {
		inputs = append(inputs, filepath.Join(dst, e.Name()))
	}
	sort.Strings(inputs)
	return docs, inputs, nil
}

// Keepable says whether the document named path can be kept as given:
// a file as staged, or a member whose original the decode kept.
func (r *Result) Keepable(path string) bool {
	for _, f := range r.Report.Files {
		if f.Path == path {
			return f.In == "" || f.Original != ""
		}
	}
	return false
}

// Skipped names the documents Stage listed as unread: the built-ins'
// account of what a build did not read.
func Skipped(docs []Document) []string {
	var out []string
	for _, d := range docs {
		if d.Unread != "" {
			out = append(out, d.Name)
		}
	}
	return out
}

// Readable are the documents that were read as text or decoded into it.
func (r *Result) Readable() []runnerdecode.File {
	var out []runnerdecode.File
	for _, f := range r.Report.Files {
		if f.In == "" && (f.Outcome == runnerdecode.OutcomeText || f.Outcome == runnerdecode.OutcomeDecoded) {
			out = append(out, f)
		}
	}
	return out
}

// CopyOriginal copies the document at path, as given, to dst: a file as
// staged, or a member of an archive whose own bytes the decode kept.
func (r *Result) CopyOriginal(path, dst string) error {
	for _, f := range r.Report.Files {
		if f.Path == path && f.In != "" {
			if f.Original == "" {
				return fmt.Errorf("%s is in an archive and was not kept as given", path)
			}
			return copyOut(r.Out, f.Original, dst, MaxDocumentBytes)
		}
	}
	return copyOut(r.In, path, dst, MaxDocumentBytes)
}

// MaxDocumentBytes bounds one document a decode reads back.
const MaxDocumentBytes = 256 << 20

// CopyText copies the text decoded from the document at path to dst, or,
// for an archive member read as text, the text copied out for it.
func (r *Result) CopyText(path, dst string) error {
	for _, f := range r.Report.Files {
		if f.Path == path && f.HasText() {
			return copyOut(r.Out, f.Output, dst, MaxTextBytes)
		}
	}
	return fmt.Errorf("%s was not decoded", path)
}

// CopyMarkdown copies the Markdown view of the file read as text at path
// (HTML, converted by pandoc) to dst.
func (r *Result) CopyMarkdown(path, dst string) error {
	for _, f := range r.Report.Files {
		if f.Path == path && f.Markdown != "" {
			return copyOut(r.Out, f.Markdown, dst, MaxTextBytes)
		}
	}
	return fmt.Errorf("%s has no Markdown view", path)
}

// copyOut copies a regular file under root to dst, through an os.Root:
// never through a link, never out of root, never past limit.
func copyOut(root, rel, dst string, limit int64) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	name := filepath.FromSlash(rel)
	// Every part of the path, not only the last.
	fi, err := plainpath.File(r, rel)
	if err != nil || fi.Size() > limit {
		return fmt.Errorf("%s is not a regular file within %d bytes", rel, limit)
	}
	in, err := r.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644) //nolint:gosec // a document or its text, read by the run
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(in, limit+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > limit {
		err = fmt.Errorf("%s grew past %d bytes", rel, limit)
	}
	return err
}
