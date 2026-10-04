package decode

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/blaktron/agentry-cli/pkg/plainpath"

	runnerdecode "github.com/blaktron/agentry-cli/runnerimage/decode"
)

// DocumentsFile is the list of what a run was given, at the root of what it
// reads (the Knowledge Creator's inputs; a hosted run's, agentry-cli#321).
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
	// Unpacked says it is an archive whose members are documents of their
	// own, named <archive>/<member>: "whole", or why only in part.
	Unpacked string `json:"unpacked,omitempty"`
}

// Stage lays out what a run reads from this decode, in dst (a directory the
// caller made): each readable document's text at its Read path, and
// documents.json naming every document — those that could not be read too,
// with why. It returns the documents and the paths to stage under /inputs.
// Two documents that would be read as one file are refused rather than one
// hiding the other.
func (r *Result) Stage(givenAs map[string]string, dst string) ([]Document, []string, error) {
	claimed := map[string]string{}
	claim := func(name, by string) error {
		if prev, dup := claimed[name]; dup && prev != by {
			return fmt.Errorf("the documents %s and %s would both be read as %s; rename one", prev, by, name)
		}
		claimed[name] = by
		return nil
	}
	for _, f := range r.Report.Files {
		switch {
		case f.In != "" && (f.Outcome == runnerdecode.OutcomeDecoded || f.Outcome == runnerdecode.OutcomeText):
			// A member is read only as the text copied out for it.
			if err := claim(f.Output, f.Path); err != nil {
				return nil, nil, err
			}
		case f.Outcome == runnerdecode.OutcomeDecoded:
			if err := claim(f.Output, f.Path); err != nil {
				return nil, nil, err
			}
			if err := claim(f.Path, f.Path); err != nil {
				return nil, nil, err
			}
		case f.Outcome == runnerdecode.OutcomeText:
			if err := claim(f.Path, f.Path); err != nil {
				return nil, nil, err
			}
		}
	}
	docs := make([]Document, 0, len(r.Report.Files))
	for _, f := range r.Report.Files {
		doc := Document{Name: f.Path, Family: f.Family}
		if g := givenAs[f.Path]; g != "" && g != f.Path {
			doc.GivenAs = g
		}
		var err error
		switch {
		case f.Outcome == runnerdecode.OutcomeText && f.In != "":
			doc.Read = f.Output
			err = r.CopyText(f.Path, filepath.Join(dst, filepath.FromSlash(doc.Read)))
		case f.Outcome == runnerdecode.OutcomeText:
			doc.Read = f.Path
			err = r.CopyOriginal(f.Path, filepath.Join(dst, filepath.FromSlash(doc.Read)))
		case f.Outcome == runnerdecode.OutcomeUnpacked:
			doc.Unpacked = "whole"
			if why := UnpackedInPart(f); why != "" {
				doc.Unpacked = "in part: " + why
			}
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

// Readable are the documents that were read as text or decoded into it.
func (r *Result) Readable() []runnerdecode.File {
	var out []runnerdecode.File
	for _, f := range r.Report.Files {
		if f.Outcome == runnerdecode.OutcomeText || f.Outcome == runnerdecode.OutcomeDecoded {
			out = append(out, f)
		}
	}
	return out
}

// CopyOriginal copies the document staged at path, as given, to dst.
func (r *Result) CopyOriginal(path, dst string) error {
	return copyOut(r.In, path, dst, MaxDocumentBytes)
}

// MaxDocumentBytes bounds one document a decode reads back.
const MaxDocumentBytes = 256 << 20

// CopyText copies the text decoded from the document at path to dst, or,
// for an archive member read as text, the text copied out for it.
func (r *Result) CopyText(path, dst string) error {
	for _, f := range r.Report.Files {
		if f.Path == path && (f.Outcome == runnerdecode.OutcomeDecoded || (f.Outcome == runnerdecode.OutcomeText && f.In != "")) {
			return copyOut(r.Out, f.Output, dst, MaxTextBytes)
		}
	}
	return fmt.Errorf("%s was not decoded", path)
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
	// Every part of the path, not only the last (agentry-cli#421).
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
