package toolsrv

// The theme document (plan scratch-and-pdf-reports.md §5, D8, D13;
// agentry-cli#730): what export_pdf's template reads to lay a report out.
// It is data, versioned by "agentryTheme", and the one seam a person's own
// design standard will plug into later: one more document of this shape,
// rendered by the same template. The built-in themes ship in this package
// (pdf/*.json), and the engine delivers the run's theme in the tools spec
// (PDF.Theme); the model never chooses one (D19).

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"unicode/utf8"
)

//go:embed pdf/report.typ pdf/plain.json
var pdfFiles embed.FS

// ThemeVersion is the theme document's schema version, its "agentryTheme".
const ThemeVersion = 1

// DefaultTheme is the theme a run gets when nothing chooses another.
const DefaultTheme = "plain"

// builtinThemes are the themes in this image, by name. Plain alone in
// agentry-cli#730; the plan's other four arrive with agentry-cli#731.
var builtinThemes = []string{DefaultTheme}

// BuiltinTheme is a built-in theme's document, exactly the bytes whose
// SHA-256 a receipt names.
func BuiltinTheme(name string) ([]byte, error) {
	if !slices.Contains(builtinThemes, name) {
		return nil, fmt.Errorf("no built-in report theme %q (there are: %v)", name, builtinThemes)
	}
	return pdfFiles.ReadFile("pdf/" + name + ".json")
}

// ThemeStamp is the run's theme as the engine stamps it into the tools
// spec: the document verbatim, and what the receipt names it by.
type ThemeStamp struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// SHA256 is the hex SHA-256 of Document's bytes.
	SHA256 string `json:"sha256"`
	// Document is the theme document's JSON text, kept as a string so that
	// re-encoding the spec cannot change its bytes, and so its hash.
	Document string `json:"document"`
}

// StampTheme reads a theme document into the stamp the engine writes into
// the run's tools spec: it is parsed and checked here first, so a document
// the template could not use never reaches a run.
func StampTheme(doc []byte) (*ThemeStamp, error) {
	t, err := ParseTheme(doc)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(doc)
	return &ThemeStamp{Name: t.Name, Version: t.Version, SHA256: hex.EncodeToString(sum[:]), Document: string(doc)}, nil
}

// check holds a stamp to its own document: the hash, and the name and
// version the receipt will repeat.
func (st *ThemeStamp) check() (*Theme, error) {
	sum := sha256.Sum256([]byte(st.Document))
	if hex.EncodeToString(sum[:]) != st.SHA256 {
		return nil, errors.New("pdf.theme: the document's SHA-256 is not the one stamped")
	}
	t, err := ParseTheme([]byte(st.Document))
	if err != nil {
		return nil, fmt.Errorf("pdf.theme: %w", err)
	}
	if t.Name != st.Name || t.Version != st.Version {
		return nil, fmt.Errorf("pdf.theme: stamped as %s %s, the document says %s %s", st.Name, st.Version, t.Name, t.Version)
	}
	return t, nil
}

// Theme is a theme document, version 1. Every field is required; the
// template reads them all.
type Theme struct {
	AgentryTheme int    `json:"agentryTheme"`
	Name         string `json:"name"`
	Version      string `json:"version"`
	Page         struct {
		// Paper is Typst's paper name: "a4" or "us-letter".
		Paper    string  `json:"paper"`
		MarginMM float64 `json:"marginMm"`
	} `json:"page"`
	// Fonts name a bundled family for each role.
	Fonts struct {
		Body     string `json:"body"`
		Headings string `json:"headings"`
		Code     string `json:"code"`
	} `json:"fonts"`
	Sizes struct {
		BodyPt float64 `json:"bodyPt"`
		CodePt float64 `json:"codePt"`
		// HeadingScale multiplies BodyPt for heading levels 1 to 6.
		HeadingScale []float64 `json:"headingScale"`
	} `json:"sizes"`
	// Colours are #rrggbb.
	Colours struct {
		Text           string `json:"text"`
		Accent         string `json:"accent"`
		Link           string `json:"link"`
		TableHeader    string `json:"tableHeader"`
		Rules          string `json:"rules"`
		CodeBackground string `json:"codeBackground"`
	} `json:"colours"`
	Header struct {
		Text  string `json:"text"`
		Align string `json:"align"`
	} `json:"header"`
	Footer struct {
		Text  string `json:"text"`
		Align string `json:"align"`
		// PageNumbers is "none", "n" or "n of N".
		PageNumbers string `json:"pageNumbers"`
	} `json:"footer"`
	Tables struct {
		HeaderBold bool `json:"headerBold"`
		Banded     bool `json:"banded"` // alternate body rows shaded
	} `json:"tables"`
}

// The bundled families a theme may name for a role; the fallback fonts for
// other scripts and emoji are the template's own.
var themeFonts = []string{"Noto Sans", "Noto Serif", "Noto Sans Mono"}

var (
	themeName    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	themeVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	themeColour  = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

// maxThemeText bounds a header's or footer's text, in bytes.
const maxThemeText = 200

// ParseTheme decodes a theme document strictly (no unknown field, every
// value in range) so that the template meets only values it can draw.
func ParseTheme(doc []byte) (*Theme, error) {
	var t Theme
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("the theme does not decode: %w", err)
	}
	if dec.More() {
		return nil, errors.New("the theme is more than one JSON document")
	}
	var problems []string
	bad := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }
	if t.AgentryTheme != ThemeVersion {
		bad("agentryTheme is %d, want %d", t.AgentryTheme, ThemeVersion)
	}
	if !themeName.MatchString(t.Name) {
		bad("name %q is not a lower-case name", t.Name)
	}
	if !themeVersion.MatchString(t.Version) {
		bad("version %q is not major.minor.patch", t.Version)
	}
	if t.Page.Paper != "a4" && t.Page.Paper != "us-letter" {
		bad("page.paper %q is neither a4 nor us-letter", t.Page.Paper)
	}
	if t.Page.MarginMM < 10 || t.Page.MarginMM > 40 {
		bad("page.marginMm %v is outside 10 to 40", t.Page.MarginMM)
	}
	for role, f := range map[string]string{"body": t.Fonts.Body, "headings": t.Fonts.Headings, "code": t.Fonts.Code} {
		if !slices.Contains(themeFonts, f) {
			bad("fonts.%s %q is not one of %v", role, f, themeFonts)
		}
	}
	if t.Sizes.BodyPt < 7 || t.Sizes.BodyPt > 16 {
		bad("sizes.bodyPt %v is outside 7 to 16", t.Sizes.BodyPt)
	}
	if t.Sizes.CodePt < 6 || t.Sizes.CodePt > 14 {
		bad("sizes.codePt %v is outside 6 to 14", t.Sizes.CodePt)
	}
	if len(t.Sizes.HeadingScale) != 6 {
		bad("sizes.headingScale has %d entries, want 6", len(t.Sizes.HeadingScale))
	}
	for i, v := range t.Sizes.HeadingScale {
		if v < 0.8 || v > 3 {
			bad("sizes.headingScale[%d] %v is outside 0.8 to 3", i, v)
		}
	}
	for name, c := range map[string]string{"text": t.Colours.Text, "accent": t.Colours.Accent, "link": t.Colours.Link,
		"tableHeader": t.Colours.TableHeader, "rules": t.Colours.Rules, "codeBackground": t.Colours.CodeBackground} {
		if !themeColour.MatchString(c) {
			bad("colours.%s %q is not #rrggbb", name, c)
		}
	}
	for name, a := range map[string]string{"header.align": t.Header.Align, "footer.align": t.Footer.Align} {
		if a != "left" && a != "center" && a != "right" {
			bad("%s %q is not left, center or right", name, a)
		}
	}
	for name, s := range map[string]string{"header.text": t.Header.Text, "footer.text": t.Footer.Text} {
		if len(s) > maxThemeText || !utf8.ValidString(s) {
			bad("%s is not UTF-8 text of at most %d bytes", name, maxThemeText)
		}
	}
	if !slices.Contains([]string{"none", "n", "n of N"}, t.Footer.PageNumbers) {
		bad("footer.pageNumbers %q is not none, n or \"n of N\"", t.Footer.PageNumbers)
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return nil, fmt.Errorf("the theme is not usable: %v", problems)
	}
	return &t, nil
}
