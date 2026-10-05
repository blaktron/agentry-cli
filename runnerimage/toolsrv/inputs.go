package toolsrv

// The decoded inputs (plan file-handling.md §5, D2; agentry-cli#560). The
// engine's decode step runs before any agent starts and writes, under the
// read-only InputsTextDir, an index of every input (InputsIndexFile) and
// the text decoded from each input that is not text already
// (InputsTextSubdir/<path>). list_inputs answers from the index,
// read_input serves the decoded text (the original only when it is text),
// and search_inputs searches that text. Without an index (a built-in
// whose caller decoded its documents itself, or a run with no inputs)
// list_inputs and read_input answer from /inputs as they always have, and
// search_inputs searches the inputs that are UTF-8 text.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The decoded-inputs layout the engine writes and this server reads.
const (
	InputsIndexFile   = "inputs.json"
	InputsIndexSchema = "inputs/v1"
	InputsTextSubdir  = "text"
)

// InputsIndex is every input of the run and what the decode step made of
// it, in path order.
type InputsIndex struct {
	Schema string       `json:"schema"`
	Files  []InputEntry `json:"files"`
}

// InputEntry is one input. Its Type and outcome restate the decode
// report's own (runnerimage/decode) as plain strings: this package is
// stdlib-only and independent of its siblings, and the engine fills them
// only from the decode's fixed vocabulary.
type InputEntry struct {
	// Path is the input's slash path under InputsDir.
	Path string `json:"path"`
	// Type is the kind of file the decode step detected (pdf, document,
	// image, text, …).
	Type string `json:"type"`
	// Bytes is the size of the input as given.
	Bytes int64 `json:"bytes"`
	// Unread, when set, is why the input could not be read; empty means
	// it was read.
	Unread string `json:"unread,omitempty"`
	// Decoded says its text is at InputsTextSubdir/<Path>; a read input
	// that was not decoded is text, read as it is.
	Decoded   bool   `json:"decoded,omitempty"`
	DecodedBy string `json:"decodedBy,omitempty"`
	// TextBytes is the size of the text read_input serves for it.
	TextBytes int64 `json:"textBytes,omitempty"`
	// InArchive is the archive it was unpacked from (plan file-handling.md
	// §6; agentry-cli#562): its Path is <archive>/<member>, it is not
	// under InputsDir, and its text, decoded or read as it is, is at
	// InputsTextSubdir/<Path>.
	InArchive string `json:"inArchive,omitempty"`
	// Unpacked says it is an archive whose members are entries of their
	// own; Partial, when set, is why only part of it was unpacked.
	Unpacked bool   `json:"unpacked,omitempty"`
	Partial  string `json:"partial,omitempty"`
}

// Outcome is the entry's outcome as list_inputs says it.
func (e InputEntry) Outcome() string {
	switch {
	case e.Unread != "":
		return "unread: " + e.Unread
	case e.Unpacked && e.Partial != "":
		return "unpacked in part: " + e.Partial
	case e.Unpacked:
		return "unpacked"
	}
	return "read"
}

// hasText says whether read_input and search_inputs serve a text for it.
func (e InputEntry) hasText() bool {
	return e.Unread == "" && !e.Unpacked
}

// The bounds on what the decoded-inputs tools read.
const (
	maxInputsIndexBytes = 4 << 20
	// maxSearchFileBytes is the most of one text search_inputs reads: the
	// decode step's own cut.
	maxSearchFileBytes = 10 << 20
	maxSearchQuery     = 256
	defaultSearchHits  = 20
	maxSearchHits      = 100
	searchExcerptBytes = 160
)

// inputsIndex reads the run's index: nil with no error when there is none.
func (s *server) inputsIndex() (*InputsIndex, error) {
	if s.spec.InputsTextDir == "" {
		return nil, nil
	}
	abs, err := resolveUnder(s.spec.InputsTextDir, InputsIndexFile)
	if err != nil {
		return nil, err
	}
	data, err := readCapped(abs, maxInputsIndexBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("the inputs index: %w", err)
	}
	var idx InputsIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("the inputs index: %w", err)
	}
	if idx.Schema != InputsIndexSchema {
		return nil, fmt.Errorf("the inputs index has schema %q, not %q", idx.Schema, InputsIndexSchema)
	}
	return &idx, nil
}

// textOf is where the text read_input serves for an entry lives, resolved
// under its own root: the decoded text, or the original when it is text.
func (s *server) textOf(e InputEntry) (string, error) {
	if e.Decoded || e.InArchive != "" {
		return resolveUnder(filepath.Join(s.spec.InputsTextDir, InputsTextSubdir), e.Path)
	}
	return resolveUnder(s.spec.InputsDir, e.Path)
}

// lookup finds the entry for a cleaned path.
func (idx *InputsIndex) lookup(clean string) (InputEntry, bool) {
	for _, e := range idx.Files {
		if e.Path == clean {
			return e, true
		}
	}
	return InputEntry{}, false
}

// under is the entries at or below a cleaned path ("." is every one).
func (idx *InputsIndex) under(clean string) []InputEntry {
	if clean == "." {
		return idx.Files
	}
	var out []InputEntry
	for _, e := range idx.Files {
		if e.Path == clean || strings.HasPrefix(e.Path, clean+"/") {
			out = append(out, e)
		}
	}
	return out
}

// listIndexed is list_inputs from the index: one input a line, under a
// header naming the columns.
func listIndexed(idx *InputsIndex, clean string) (string, bool) {
	entries := idx.under(clean)
	if len(entries) == 0 {
		if clean == "." {
			return "(no input files provided for this run)", false
		}
		return fmt.Sprintf("list_inputs: no input at %q", clean), true
	}
	var b strings.Builder
	b.WriteString("path\ttype\tbytes\toutcome\ttext bytes")
	for _, e := range entries {
		text := "-"
		if e.hasText() {
			text = strconv.FormatInt(e.TextBytes, 10)
		}
		fmt.Fprintf(&b, "\n%s\t%s\t%d\t%s\t%s", e.Path, e.Type, e.Bytes, e.Outcome(), text)
	}
	return b.String(), false
}

func searchInputsSchema() map[string]any {
	return objSchema(map[string]any{
		"query":      strProp("the word or phrase to find, matched without regard to case"),
		"path":       strProp("inputs-relative file or directory to search; empty or \".\" searches every input"),
		"maxResults": intProp(fmt.Sprintf("most matching lines to return (default %d, at most %d)", defaultSearchHits, maxSearchHits)),
	}, "query")
}

// searchTarget is one text search_inputs reads: the input's path as the
// model names it, and where its text is.
type searchTarget struct {
	path, abs string
}

func (s *server) toolSearchInputs(raw json.RawMessage) (string, bool) {
	var a struct {
		Query      string `json:"query"`
		Path       string `json:"path"`
		MaxResults int    `json:"maxResults"`
	}
	if err := unmarshalArgs(raw, &a); err != nil {
		return s.failed("search_inputs", err)
	}
	query := strings.TrimSpace(a.Query)
	switch {
	case query == "":
		return "search_inputs: query is required", true
	case len(query) > maxSearchQuery:
		return fmt.Sprintf("search_inputs: query is longer than %d bytes", maxSearchQuery), true
	case s.spec.InputsDir == "":
		return "search_inputs: no inputs channel configured", true
	}
	limit := a.MaxResults
	if limit <= 0 {
		limit = defaultSearchHits
	}
	limit = min(limit, maxSearchHits)
	clean := "."
	if a.Path != "" {
		clean = path.Clean(a.Path)
		if _, err := resolveUnder(s.spec.InputsDir, clean); err != nil {
			return s.failed("search_inputs", err)
		}
	}
	targets, err := s.searchTargets(clean)
	if err != nil {
		return s.failed("search_inputs", err)
	}
	needle := strings.ToLower(query)
	var hits []string
	total := 0
	for _, t := range targets {
		data, err := readPrefix(t.abs, maxSearchFileBytes)
		if err != nil || !utf8.Valid(data) {
			continue
		}
		for i, line := range strings.Split(string(data), "\n") {
			at := indexFold(line, needle)
			if at < 0 {
				continue
			}
			if total++; len(hits) < limit {
				hits = append(hits, fmt.Sprintf("%s:%d: %s", t.path, i+1, excerpt(line, at)))
			}
		}
	}
	if total == 0 {
		return fmt.Sprintf("no input text matches %q", query), false
	}
	out := strings.Join(hits, "\n")
	if total > len(hits) {
		out += fmt.Sprintf("\n[%d more matching lines not shown; narrow the query or pass path]", total-len(hits))
	}
	return out, false
}

// searchTargets is the texts under a cleaned path: from the index when
// there is one, else every regular file under /inputs (a link is never
// followed).
func (s *server) searchTargets(clean string) ([]searchTarget, error) {
	idx, err := s.inputsIndex()
	if err != nil {
		return nil, err
	}
	var out []searchTarget
	if idx != nil {
		for _, e := range idx.under(clean) {
			if !e.hasText() {
				continue
			}
			abs, err := s.textOf(e)
			if err != nil {
				continue
			}
			out = append(out, searchTarget{path: e.Path, abs: abs})
		}
		return out, nil
	}
	root, err := resolveUnder(s.spec.InputsDir, clean)
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(s.spec.InputsDir, p)
		if err != nil {
			return err
		}
		out = append(out, searchTarget{path: filepath.ToSlash(rel), abs: p})
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no input at %q", clean)
	}
	return out, err
}

// readPrefix reads at most limit bytes of a file, cut back to a rune
// boundary when the file is longer.
func readPrefix(abs string, limit int64) ([]byte, error) {
	data, more, err := readUpTo(abs, limit)
	if more {
		data = trimPartialRune(data)
	}
	return data, err
}

// indexFold is the byte offset in s of the first match of needle, a
// lower-cased query, without regard to case; -1 when there is none.
// Lower-casing can change a rune's length, so the offset is found in s
// itself, not in its lower-cased copy.
func indexFold(s, needle string) int {
	if !strings.Contains(strings.ToLower(s), needle) {
		return -1
	}
	for i := range s {
		if hasPrefixFold(s[i:], needle) {
			return i
		}
	}
	return -1
}

// hasPrefixFold says whether s begins with needle, a lower-cased string,
// without regard to case.
func hasPrefixFold(s, needle string) bool {
	for _, want := range needle {
		got, size := utf8.DecodeRuneInString(s)
		if size == 0 || unicode.ToLower(got) != want {
			return false
		}
		s = s[size:]
	}
	return true
}

// excerpt is up to searchExcerptBytes of a line around the match at byte
// at, cut at rune boundaries, with an ellipsis where it was cut.
func excerpt(line string, at int) string {
	line = strings.TrimRight(line, "\r")
	start := max(0, min(at, len(line))-searchExcerptBytes/3)
	for start > 0 && !utf8.RuneStart(line[start]) {
		start--
	}
	end := min(len(line), start+searchExcerptBytes)
	for end < len(line) && !utf8.RuneStart(line[end]) {
		end--
	}
	out := strings.TrimSpace(strings.ReplaceAll(line[start:end], "\t", " "))
	if start > 0 {
		out = "…" + out
	}
	if end < len(line) {
		out += "…"
	}
	return out
}
