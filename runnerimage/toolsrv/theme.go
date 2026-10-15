package toolsrv

// The run's report design: the theme document, version 2, that
// export_pdf's template reads to lay a report out. It is data, defined once
// in the core's pkg/reportdesign, which this image carries as a byte-for-byte
// copy (runnerimage/reportdesign), so a design is held here to
// the rules the core stored it under. The five built-in themes
// are that package's; the engine delivers the run's design in the tools spec
// (PDF.Theme), and the model never chooses one: it chooses only each
// PDF's form.

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/blaktron/agentry-cli/runnerimage/reportdesign"
)

// pdfFiles holds the template; the themes are reportdesign's.
//
//go:embed pdf/report.typ
var pdfFiles embed.FS

// DefaultTheme is the built-in theme a run gets when nothing chooses
// another: Plain.
const DefaultTheme = reportdesign.DefaultID

// BuiltinThemes are the ids of the built-in themes, Plain first.
func BuiltinThemes() []string { return reportdesign.BuiltinIDs() }

// IsBuiltinTheme says whether name is a built-in theme's id.
func IsBuiltinTheme(name string) bool { return slices.Contains(reportdesign.BuiltinIDs(), name) }

// BuiltinTheme is a built-in theme's document as the core ships it, its
// canonical form.
func BuiltinTheme(name string) ([]byte, error) {
	doc, ok := reportdesign.BuiltinDocument(name)
	if !ok {
		return nil, fmt.Errorf("no built-in report theme %q (there are: %s)", name, strings.Join(reportdesign.BuiltinIDs(), ", "))
	}
	return doc, nil
}

// ThemeStamp is the run's report design as the engine stamps it into the
// tools spec: the canonical document verbatim, and what the receipt names it
// by.
type ThemeStamp struct {
	// Name is the design's own label (a built-in's name, or the person's).
	Name string `json:"name"`
	// BasedOn is the built-in theme it started from.
	BasedOn string `json:"basedOn"`
	// Version is the design's revision, as text.
	Version string `json:"version"`
	// SHA256 is the hex SHA-256 of Document's bytes, which are the design's
	// canonical form, so it is pkg/reportdesign's SHA256 of the design.
	SHA256 string `json:"sha256"`
	// Document is the design's canonical JSON text, kept as a string so
	// that re-encoding the spec cannot change its bytes, and so its hash.
	Document string `json:"document"`
}

// StampTheme reads a design document, held to every rule of
// pkg/reportdesign, into the stamp the engine writes into the run's tools
// spec. The stamp carries the canonical document, whatever spacing the
// source had, so its hash is the design's identity everywhere (the core
// answers the same bytes).
func StampTheme(doc []byte) (*ThemeStamp, error) {
	d, err := reportdesign.Parse(doc)
	if err != nil {
		return nil, err
	}
	canonical := d.Canonical()
	sum := sha256.Sum256(canonical)
	return &ThemeStamp{Name: d.Name, BasedOn: d.BasedOn, Version: strconv.Itoa(d.Version), SHA256: hex.EncodeToString(sum[:]), Document: string(canonical)}, nil
}

// HasLogo says the stamped design carries the person's logo.
func (st *ThemeStamp) HasLogo() bool {
	d, err := reportdesign.Parse([]byte(st.Document))
	return err == nil && d.Logo != (reportdesign.Logo{})
}

// check holds a stamp to its own document: the hash, the design's rules, its
// canonical form, and the name, theme and version the receipt will repeat.
func (st *ThemeStamp) check() (reportdesign.Design, error) {
	sum := sha256.Sum256([]byte(st.Document))
	if hex.EncodeToString(sum[:]) != st.SHA256 {
		return reportdesign.Design{}, errors.New("pdf.theme: the document's SHA-256 is not the one stamped")
	}
	d, err := reportdesign.Parse([]byte(st.Document))
	if err != nil {
		return reportdesign.Design{}, fmt.Errorf("pdf.theme: %w", err)
	}
	if string(d.Canonical()) != st.Document {
		return reportdesign.Design{}, errors.New("pdf.theme: the document is not the design's canonical form")
	}
	if d.Name != st.Name || d.BasedOn != st.BasedOn || strconv.Itoa(d.Version) != st.Version {
		return reportdesign.Design{}, fmt.Errorf("pdf.theme: stamped as %q (%s, version %s), the document says %q (%s, version %d)", st.Name, st.BasedOn, st.Version, d.Name, d.BasedOn, d.Version)
	}
	return d, nil
}
