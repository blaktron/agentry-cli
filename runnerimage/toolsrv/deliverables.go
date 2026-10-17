package toolsrv

// Deliverables: the files a run was for. write_report and export_pdf take
// two optional arguments, deliverable and title, and the tools server keeps
// the marks in a ledger under the run's receipts. The ledger is the agent's
// word and nothing more: after the run the runner reads it, checks every
// path and title again, and hashes each file itself before the envelope names
// one.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxDeliverableTitleRunes is the longest title a deliverable takes.
const MaxDeliverableTitleRunes = 120

// Deliverables is the deliverables ledger's grant. The engine stamps Ledger
// into the run's own copy of the spec, never into the compiled plan; a spec
// without one still serves both tools, which then cannot mark a file.
type Deliverables struct {
	// Ledger is the container path each mark is appended to, under OutDir.
	Ledger string `json:"ledger,omitempty"`
}

// DeliverableEntry is one line of the deliverables ledger: a path under
// reports/ (as the output folder spells it, reports/…) the agent marked, with
// its title, or, with Marked false, a path it took the mark off again. The
// last line for a path wins.
type DeliverableEntry struct {
	Path   string `json:"path"`
	Title  string `json:"title,omitempty"`
	Marked bool   `json:"marked"`
}

func validateDeliverables(spec *Spec) error {
	l := spec.Deliverables.Ledger
	if l == "" {
		return nil
	}
	if rel, err := filepath.Rel(spec.OutDir, l); !filepath.IsAbs(l) || err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("deliverables.ledger %q must be an absolute path under outDir %q", l, spec.OutDir)
	}
	return nil
}

// deliverableProps are the two arguments, as both tools' schemas carry them.
func deliverableProps() map[string]any {
	return map[string]any{
		"deliverable": boolProp("true when this file is what the person asked this run for, so that it is listed first among the run's results; leave it out for a working note, a draft or data you only used along the way"),
		"title":       strProp(fmt.Sprintf("with deliverable: a short name for the file, one line of plain text of at most %d characters (default: the file's name)", MaxDeliverableTitleRunes)),
	}
}

// deliverableMark is a call's decision about marking: none (the arguments
// said nothing), a mark with a title, or, with off, the mark taken off.
type deliverableMark struct {
	set   bool
	on    bool
	title string
}

// parseDeliverableMark reads the two arguments. A title without
// deliverable: true is refused, as is one that is not a line of plain text,
// before the tool writes anything.
func parseDeliverableMark(tool string, deliverable *bool, title string) (deliverableMark, error) {
	title = strings.TrimSpace(title)
	if deliverable == nil || !*deliverable {
		if title != "" {
			return deliverableMark{}, fmt.Errorf("%s: title names a deliverable: set deliverable to true with it, or leave title out", tool)
		}
		if deliverable != nil {
			return deliverableMark{set: true}, nil
		}
		return deliverableMark{}, nil
	}
	if title != "" {
		if err := CheckDeliverableTitle(title); err != nil {
			return deliverableMark{}, fmt.Errorf("%s: title %w", tool, err)
		}
	}
	return deliverableMark{set: true, on: true, title: title}, nil
}

// markDeliverable records the call's mark for rel (the path under
// reports/). It answers the sentence the tool's answer adds: empty when the
// call set no mark.
func (s *server) markDeliverable(rel string, m deliverableMark) (string, error) {
	if !m.set {
		return "", nil
	}
	entry := DeliverableEntry{Path: "reports/" + path.Clean(rel), Marked: m.on}
	if m.on {
		entry.Title = m.title
	}
	l := s.spec.Deliverables.Ledger
	if l == "" {
		if !m.on {
			return "", nil
		}
		return "; not marked as a deliverable: this run records none", nil
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(l), 0o755); err != nil {
		return "", err
	}
	f, err := os.OpenFile(l, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // the spec's own ledger path
	if err != nil {
		return "", err
	}
	s.own(l)
	_, werr := f.Write(append(b, '\n'))
	if err := errors.Join(werr, f.Close()); err != nil {
		return "", err
	}
	switch {
	case !m.on:
		return "; no longer marked as a deliverable", nil
	case m.title != "":
		return fmt.Sprintf("; marked as a deliverable: %s", m.title), nil
	}
	return "; marked as a deliverable", nil
}

// CheckDeliverableTitle holds a title to the rule the report design's free
// texts keep: one line of plain, printable text of at most
// MaxDeliverableTitleRunes characters, with no control, line-break or
// bidirectional formatting character that could make it read otherwise than
// it is written. The runner holds a ledger's titles to it again.
func CheckDeliverableTitle(title string) error {
	if strings.TrimSpace(title) == "" {
		return errors.New("must not be empty or only spaces")
	}
	if n := utf8.RuneCountInString(title); n > MaxDeliverableTitleRunes {
		return fmt.Errorf("is %d characters, over the limit of %d", n, MaxDeliverableTitleRunes)
	}
	for _, r := range title {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) || r == '\u2028' || r == '\u2029' {
			return errors.New("must be one line of plain text (no control, line-break or bidirectional formatting characters)")
		}
	}
	return nil
}
