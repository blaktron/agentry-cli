package validate

import (
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/blaktron/agentry/pkg/archive"
	"github.com/blaktron/agentry/pkg/manifest"
)

// Unfinished is a line a scaffold wrote for the author to replace, still in
// a file the package ships: the QuickAgent template's, `agentry init`'s or
// the Drafter's. The registry would publish it as it stands, so `agentry
// validate` and `agentry publish` refuse it.
type Unfinished struct {
	// File is the shipped file, relative to the package.
	File string `json:"file"`
	// Line is the 1-based line, absent for a manifest field.
	Line int `json:"line,omitempty"`
	// Marker names what was found, e.g. "TODO summary".
	Marker string `json:"marker"`
	// Message says what to do about it.
	Message string `json:"message"`
}

// PlaceholderNamespace is the namespace a scaffold writes when it knows
// none. It passes the namespace grammar, so only this check catches it.
const PlaceholderNamespace = "your-namespace"

// placeholderAuthor is the template's author line.
const placeholderAuthor = "your name"

// lineMarker is one scaffold sentence, found in the files a package ships.
type lineMarker struct {
	re      *regexp.Regexp
	marker  string
	message string
	// files limits the marker to the README or the LICENSE; empty means
	// every text file checked (the instruction, the README, the LICENSE).
	files string
}

// lineMarkers are the scaffold sentences, matched one line at a time. Each
// is a sentence a scaffold writes verbatim, so an author's own prose does
// not trip them: a line that merely mentions a TODO passes.
var lineMarkers = []lineMarker{
	{
		re:      regexp.MustCompile(`^\s*(description:\s*)?"?TODO (write|describe|one line)\b`),
		marker:  "TODO",
		message: "a scaffold's TODO line; write the text it asks for",
	},
	{
		re:      regexp.MustCompile(`^\s*PLACEHOLDER LICENSE NOTE\s*$`),
		marker:  "placeholder licence note",
		message: "replace the note with the full text of the licence agent.yaml declares",
	},
	{
		re:      regexp.MustCompile(`(?i)^\s*copyright\b.*<\s*(year|your name)\s*>`),
		marker:  "placeholder copyright line",
		message: "put the year and the copyright holder's name in place of the <…> placeholders",
	},
	{
		re:      regexp.MustCompile(`^\s*\*\*This README is about the template\.\*\*`),
		marker:  "the template's README",
		message: "replace the template's README with your own: what the QuickAgent does, what to give it, what it returns",
		files:   "readme",
	},
	{
		re:      regexp.MustCompile("^Drafted by the Drafter \\(`agentry draft`\\)\\. Review the manifest"),
		marker:  "the Drafter's closing line",
		message: "review the draft, then remove the Drafter's closing line from the README",
		files:   "readme",
	},
}

// fileKind sorts a shipped path into the files the line markers read:
// "readme", "license", "instruction", or "" for a file they skip.
func fileKind(p string, instructions []string) string {
	if slices.Contains(instructions, p) {
		return "instruction"
	}
	if strings.Contains(p, "/") {
		return ""
	}
	base := strings.ToUpper(strings.TrimSuffix(p, path.Ext(p)))
	switch base {
	case "README":
		return "readme"
	case "LICENSE", "LICENCE", "COPYING":
		return "license"
	}
	return ""
}

// findUnfinished lists the scaffold lines a package still ships: the
// manifest's placeholder fields, then each marker in the instruction, the
// README and the LICENSE, in file and line order.
func findUnfinished(m *manifest.Manifest, files []localFile) []Unfinished {
	out := []Unfinished{}
	if strings.HasPrefix(strings.TrimSpace(m.Summary), "TODO") {
		out = append(out, Unfinished{File: "agent.yaml", Marker: "TODO summary",
			Message: "summary is the scaffold's TODO line; write one line saying what this QuickAgent does"})
	}
	if m.Namespace == PlaceholderNamespace {
		out = append(out, Unfinished{File: "agent.yaml", Marker: "placeholder namespace",
			Message: fmt.Sprintf("namespace is %q; set your handle or your organization's", PlaceholderNamespace)})
	}
	for _, a := range m.Authors {
		if strings.EqualFold(strings.TrimSpace(a.Name), placeholderAuthor) {
			out = append(out, Unfinished{File: "agent.yaml", Marker: "placeholder author",
				Message: "an author is named \"Your Name\"; put the author's name, or remove the entry"})
			break
		}
	}
	instructions := m.InstructionCandidates()
	for _, f := range files {
		kind := fileKind(f.Path, instructions)
		if kind == "" {
			continue
		}
		for n, line := range strings.Split(string(f.Content), "\n") {
			for _, lm := range lineMarkers {
				if lm.files != "" && lm.files != kind {
					continue
				}
				if lm.re.MatchString(line) {
					out = append(out, Unfinished{File: f.Path, Line: n + 1, Marker: lm.marker, Message: lm.message})
					break
				}
			}
		}
	}
	return out
}

// UnfinishedIn lists the scaffold lines the package in dir would ship, read
// from the files publish.include resolves to. `agentry publish` refuses an
// upload while it lists any. An error is a package it could not read.
func UnfinishedIn(dir string) ([]Unfinished, error) {
	m, err := ReadManifest(dir)
	if err != nil {
		return nil, err
	}
	files, err := archive.WalkWorkspace(dir)
	if err != nil {
		return nil, fmt.Errorf("walk workspace %s: %w", dir, err)
	}
	scanFiles, readErrs := loadScanFiles(dir, m, files)
	if len(readErrs) > 0 {
		return nil, readErrs[0]
	}
	return findUnfinished(m, scanFiles), nil
}

// PrintUnfinished prints the scaffold lines, one per line, labelled.
func PrintUnfinished(w io.Writer, label string, us []Unfinished) {
	for _, u := range us {
		loc := u.File
		if u.Line > 0 {
			loc = fmt.Sprintf("%s:%d", u.File, u.Line)
		}
		_, _ = fmt.Fprintf(w, "  %s %s: %s — %s\n", label, loc, u.Marker, u.Message)
	}
}
