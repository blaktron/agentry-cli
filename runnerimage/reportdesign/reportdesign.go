// Package reportdesign is the one definition of a report design (agentry-notes
// plans/report-designs.md §4, D11, D14, D17, D22; agentry#1024): the theme
// document version 2 that sets the look of a person's PDF reports. The server
// validates a design with it before storing one on the account, the CLI before
// the runner's template reads one, and the desktop before it saves one; the
// site's editor reads the same rules through the openapi's ReportDesign
// schema, which a core test holds to Schema.
//
// A design is data, never markup (plan invariant 2): colours are #rrggbb,
// fonts, title blocks, heading treatments, quote styles, paper and page
// numbers are names from closed lists, and the two free texts (the design's
// name and the footer line) are one line of plain text that the template sets
// as content, never evaluates. There is no path, URL or file name in it. A
// logo, when the person has one, is the pixels themselves (logo.go).
//
// The look stays restrained (D2): one accent and its tint, which Tint derives;
// body text is near-black whatever the design. Where the accent touches text
// it must reach 4.5:1 on white, and where it colours only graphics 3:1 (D17,
// invariant 7). Which it is follows from the design, and the runner's template
// must draw it so (AccentOnText):
//
//   - titleBlock "band" sets the title in white on a full-width accent band;
//   - headings "smallcaps" and "numbered" set level 1 and 2 headings in the
//     accent;
//   - every other choice colours only rules, bars, tinted cells and callout
//     edges, and keeps text near-black.
package reportdesign

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// DocumentVersion is the value of "agentryTheme" this package reads. Version 1
// (DR-82, the scratch-and-pdf-reports plan's theme document) is refused, with
// no mapping (D16, D20).
const DocumentVersion = 2

// Bounds on a design.
const (
	// MaxDocumentBytes caps a design document as it is sent, not counting
	// its logo's field (D11). A canonical design is well under it; the cap
	// bounds what a client may make the server parse.
	MaxDocumentBytes = 16 << 10
	// MaxNameRunes caps the design's name, a label the person sees.
	MaxNameRunes = 60
	// MaxFooterTextRunes caps the footer's one line of text (plan §4).
	MaxFooterTextRunes = 120
	// MaxVersion caps the design's revision number.
	MaxVersion = 1_000_000
)

// Design is a report design: the theme document, version 2. Field order is
// the canonical JSON's key order.
type Design struct {
	// AgentryTheme is the document version, always DocumentVersion.
	AgentryTheme int `json:"agentryTheme"`
	// Name is the design's label: a built-in's own name, or the person's.
	Name string `json:"name"`
	// BasedOn is the id of the built-in theme the design started from.
	BasedOn string `json:"basedOn"`
	// Version is the design's revision, a positive integer. A built-in
	// carries Agentry's revision of it; an edited design keeps the number
	// it was loaded with unless its editor says otherwise.
	Version int `json:"version"`
	// Paper is a Papers value: "a4" or "letter" (US Letter).
	Paper string `json:"paper"`
	Fonts Fonts  `json:"fonts"`
	// Accent is the one colour, #rrggbb in lower case. Its tint is
	// derived (Tint), never stored.
	Accent string `json:"accent"`
	// TitleBlock is a TitleBlocks value, the title's treatment on page 1.
	TitleBlock string `json:"titleBlock"`
	// Headings is a HeadingStyles value, the treatment of level 1 and 2.
	Headings string `json:"headings"`
	Tables   Tables `json:"tables"`
	// Quotes is a QuoteStyles value.
	Quotes string `json:"quotes"`
	Footer Footer `json:"footer"`
	// Logo is the person's logo, drawn only in the title block; the zero
	// Logo is none, and is not written (agentry#1025).
	Logo Logo `json:"logo,omitzero"`
}

// Fonts names the three faces, each a family of Families.
type Fonts struct {
	// Headings is any family.
	Headings string `json:"headings"`
	// Body is a sans or serif family that is not headings-only.
	Body string `json:"body"`
	// Code is a mono family.
	Code string `json:"code"`
}

// Tables are the table styles: a tinted header row, and alternate rows tinted.
type Tables struct {
	Tinted bool `json:"tinted"`
	Banded bool `json:"banded"`
}

// Footer is the page footer.
type Footer struct {
	// PageNumbers is a PageNumberStyles value.
	PageNumbers string `json:"pageNumbers"`
	// Text is one line of plain text, at most MaxFooterTextRunes, set as
	// content; "" for none.
	Text string `json:"text"`
}

// The closed lists. Each is the whole set of accepted names, in the order an
// editor offers them.
var (
	Papers           = []string{"a4", "letter"}
	TitleBlocks      = []string{"band", "bar", "ruled", "none"}
	HeadingStyles    = []string{"rule", "bar", "smallcaps", "numbered", "plain"}
	QuoteStyles      = []string{"callout", "plain"}
	PageNumberStyles = []string{"none", "n", "nOfN"}
)

// Kind is a family's kind.
type Kind string

// The kinds of family.
const (
	Sans  Kind = "sans"
	Serif Kind = "serif"
	Mono  Kind = "mono"
)

// Family is a font family the Typst image carries (agentry-dockerimages#58;
// plan D3, D13). The runner sets it with Noto behind it for the scripts it
// lacks.
type Family struct {
	// Name is the family's name as Typst and the web fonts know it.
	Name string
	Kind Kind
	// HeadingsOnly marks a display face, offered for headings only.
	HeadingsOnly bool
}

var families = []Family{
	{Name: "Inter", Kind: Sans},
	{Name: "IBM Plex Sans", Kind: Sans},
	{Name: "Source Sans 3", Kind: Sans},
	{Name: "Work Sans", Kind: Sans},
	{Name: "Montserrat", Kind: Sans, HeadingsOnly: true},
	{Name: "Source Serif 4", Kind: Serif},
	{Name: "Lora", Kind: Serif},
	{Name: "Merriweather", Kind: Serif},
	{Name: "IBM Plex Serif", Kind: Serif},
	{Name: "EB Garamond", Kind: Serif},
	{Name: "Playfair Display", Kind: Serif, HeadingsOnly: true},
	{Name: "IBM Plex Mono", Kind: Mono},
	{Name: "JetBrains Mono", Kind: Mono},
	{Name: "Source Code Pro", Kind: Mono},
}

// Families is every family a design may name, in the order an editor offers
// them. The runner image must carry each one.
func Families() []Family { return slices.Clone(families) }

// FamilyByName finds a family by its exact name.
func FamilyByName(name string) (Family, bool) {
	for _, f := range families {
		if f.Name == name {
			return f, true
		}
	}
	return Family{}, false
}

// FontsFor lists the family names a role accepts: "headings", "body" or
// "code".
func FontsFor(role string) []string {
	var out []string
	for _, f := range families {
		if fontFits(role, f) {
			out = append(out, f.Name)
		}
	}
	return out
}

func fontFits(role string, f Family) bool {
	switch role {
	case "headings":
		return true
	case "body":
		return f.Kind != Mono && !f.HeadingsOnly
	case "code":
		return f.Kind == Mono
	}
	return false
}

// FieldError is a design's refusal: the field, as a dotted path into the
// document ("fonts.body"; "" for the document as a whole), and what is wrong.
// The message never quotes the value back, beyond a colour.
type FieldError struct {
	Field   string
	Message string
	// kind is ErrLogoTooLarge or ErrLogoFormat for a logo's refusal.
	kind error
}

// Unwrap is the refusal's kind, where it has one (ErrLogoTooLarge,
// ErrLogoFormat), for errors.Is.
func (e *FieldError) Unwrap() error { return e.kind }

func (e *FieldError) Error() string {
	if e.Field == "" {
		return "report design: " + e.Message
	}
	return "report design: " + e.Field + ": " + e.Message
}

func fieldErr(field, format string, args ...any) error {
	return &FieldError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// wire is the document as it is decoded: pointers, so a missing field is
// told apart from a zero one.
type wire struct {
	AgentryTheme *int        `json:"agentryTheme"`
	Name         *string     `json:"name"`
	BasedOn      *string     `json:"basedOn"`
	Version      *int        `json:"version"`
	Paper        *string     `json:"paper"`
	Fonts        *wireFonts  `json:"fonts"`
	Accent       *string     `json:"accent"`
	TitleBlock   *string     `json:"titleBlock"`
	Headings     *string     `json:"headings"`
	Tables       *wireTables `json:"tables"`
	Quotes       *string     `json:"quotes"`
	Footer       *wireFooter `json:"footer"`
	Logo         *wireLogo   `json:"logo"`
}

type wireLogo struct {
	PNG *string `json:"png"`
}

type wireFonts struct {
	Headings *string `json:"headings"`
	Body     *string `json:"body"`
	Code     *string `json:"code"`
}

type wireTables struct {
	Tinted *bool `json:"tinted"`
	Banded *bool `json:"banded"`
}

type wireFooter struct {
	PageNumbers *string `json:"pageNumbers"`
	Text        *string `json:"text"`
}

var unknownField = regexp.MustCompile(`unknown field "([^"]*)"`)

// Parse reads a design document and validates it: every field present but
// the optional logo, no other key, no second value after it, at most
// MaxDocumentBytes besides the logo's field, and every rule of Validate. A
// refusal is a *FieldError.
func Parse(data []byte) (Design, error) {
	overLimit := func(n int) error {
		return fieldErr("", "the document is %d bytes without its logo, over the %d-byte limit", n, MaxDocumentBytes)
	}
	if len(data) > MaxDocumentBytes+maxLogoField+len(`"logo":{"png":""}`) {
		return Design{}, overLimit(len(data))
	}
	if !utf8.Valid(data) {
		return Design{}, fieldErr("", "the document is not UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var w wire
	if err := dec.Decode(&w); err != nil {
		if m := unknownField.FindStringSubmatch(err.Error()); m != nil {
			return Design{}, fieldErr("", "unknown key %q", m[1])
		}
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Field != "" {
			return Design{}, fieldErr(typeErr.Field, "must be %s", jsonType(typeErr.Type.Kind().String()))
		}
		return Design{}, fieldErr("", "not a JSON object: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Design{}, fieldErr("", "more than one JSON value")
	}
	if n := len(data); w.Logo == nil || w.Logo.PNG == nil {
		if n > MaxDocumentBytes {
			return Design{}, overLimit(n)
		}
	} else if n -= len(*w.Logo.PNG); n > MaxDocumentBytes {
		return Design{}, overLimit(n)
	}
	d, err := w.design()
	if err != nil {
		return Design{}, err
	}
	return d, d.Validate()
}

// decodeOnly is Parse without Validate, for the embedded built-ins, which
// Validate itself reads.
func decodeOnly(data []byte) (Design, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var w wire
	if err := dec.Decode(&w); err != nil {
		return Design{}, err
	}
	return w.design()
}

func jsonType(kind string) string {
	switch kind {
	case "int", "int64":
		return "a number"
	case "bool":
		return "a boolean"
	case "struct", "ptr":
		return "an object"
	}
	return "a " + kind
}

func (w wire) design() (Design, error) {
	missing := func(field string) error { return fieldErr(field, "is required") }
	switch {
	case w.AgentryTheme == nil:
		return Design{}, missing("agentryTheme")
	case w.Name == nil:
		return Design{}, missing("name")
	case w.BasedOn == nil:
		return Design{}, missing("basedOn")
	case w.Version == nil:
		return Design{}, missing("version")
	case w.Paper == nil:
		return Design{}, missing("paper")
	case w.Fonts == nil:
		return Design{}, missing("fonts")
	case w.Fonts.Headings == nil:
		return Design{}, missing("fonts.headings")
	case w.Fonts.Body == nil:
		return Design{}, missing("fonts.body")
	case w.Fonts.Code == nil:
		return Design{}, missing("fonts.code")
	case w.Accent == nil:
		return Design{}, missing("accent")
	case w.TitleBlock == nil:
		return Design{}, missing("titleBlock")
	case w.Headings == nil:
		return Design{}, missing("headings")
	case w.Tables == nil:
		return Design{}, missing("tables")
	case w.Tables.Tinted == nil:
		return Design{}, missing("tables.tinted")
	case w.Tables.Banded == nil:
		return Design{}, missing("tables.banded")
	case w.Quotes == nil:
		return Design{}, missing("quotes")
	case w.Footer == nil:
		return Design{}, missing("footer")
	case w.Footer.PageNumbers == nil:
		return Design{}, missing("footer.pageNumbers")
	case w.Footer.Text == nil:
		return Design{}, missing("footer.text")
	case w.Logo != nil && w.Logo.PNG == nil:
		return Design{}, missing("logo.png")
	}
	var logo Logo
	if w.Logo != nil {
		logo.PNG = *w.Logo.PNG
		if logo.PNG == "" {
			return Design{}, fieldErr("logo.png", "must not be empty; a design without a logo has no logo key")
		}
	}
	return Design{
		AgentryTheme: *w.AgentryTheme,
		Name:         *w.Name,
		BasedOn:      *w.BasedOn,
		Version:      *w.Version,
		Paper:        *w.Paper,
		Fonts:        Fonts{Headings: *w.Fonts.Headings, Body: *w.Fonts.Body, Code: *w.Fonts.Code},
		Accent:       *w.Accent,
		TitleBlock:   *w.TitleBlock,
		Headings:     *w.Headings,
		Tables:       Tables{Tinted: *w.Tables.Tinted, Banded: *w.Tables.Banded},
		Quotes:       *w.Quotes,
		Footer:       Footer{PageNumbers: *w.Footer.PageNumbers, Text: *w.Footer.Text},
		Logo:         logo,
	}, nil
}

// Validate holds a design to every rule: the version, the closed lists, the
// fonts' roles, the colour's form and contrast, the two texts, and the logo
// when it has one. A refusal is a *FieldError naming the first field found
// wrong, in document order.
func (d Design) Validate() error {
	if d.AgentryTheme != DocumentVersion {
		return fieldErr("agentryTheme", "must be %d; an earlier theme document is not read", DocumentVersion)
	}
	if err := checkText("name", d.Name, MaxNameRunes, false); err != nil {
		return err
	}
	if _, ok := Builtin(d.BasedOn); !ok {
		return fieldErr("basedOn", "must be one of %s", strings.Join(BuiltinIDs(), ", "))
	}
	if d.Version < 1 || d.Version > MaxVersion {
		return fieldErr("version", "must be a whole number from 1 to %d", MaxVersion)
	}
	if err := oneOf("paper", d.Paper, Papers); err != nil {
		return err
	}
	for _, f := range []struct{ role, name string }{
		{"headings", d.Fonts.Headings}, {"body", d.Fonts.Body}, {"code", d.Fonts.Code},
	} {
		fam, ok := FamilyByName(f.name)
		if !ok {
			return fieldErr("fonts."+f.role, "must name a family of the report designs' list")
		}
		if !fontFits(f.role, fam) {
			return fieldErr("fonts."+f.role, "%s cannot set %s; choose one of %s", fam.Name, roleText(f.role), strings.Join(FontsFor(f.role), ", "))
		}
	}
	if !hexColour.MatchString(d.Accent) {
		return fieldErr("accent", "must be a colour #rrggbb in lower-case hex")
	}
	if err := oneOf("titleBlock", d.TitleBlock, TitleBlocks); err != nil {
		return err
	}
	if err := oneOf("headings", d.Headings, HeadingStyles); err != nil {
		return err
	}
	// The contrast rule, once the choices that decide it are known.
	if ratio, need := onWhite(d.Accent), d.MinContrast(); ratio < need {
		near, _ := NearestPassing(d.Accent, need)
		use := "colours only graphics"
		if d.AccentOnText() {
			use = "colours text"
		}
		return fieldErr("accent", "%s reaches %.2f:1 on white, and an accent that %s needs %.1f:1; the nearest shade that does is %s", d.Accent, ratio, use, need, near)
	}
	if err := oneOf("quotes", d.Quotes, QuoteStyles); err != nil {
		return err
	}
	if err := oneOf("footer.pageNumbers", d.Footer.PageNumbers, PageNumberStyles); err != nil {
		return err
	}
	if err := checkText("footer.text", d.Footer.Text, MaxFooterTextRunes, true); err != nil {
		return err
	}
	if d.Logo != (Logo{}) {
		return checkLogo(d.Logo)
	}
	return nil
}

func roleText(role string) string {
	switch role {
	case "body":
		return "body text (it is a mono or a headings-only face)"
	case "code":
		return "code (it is not a mono face)"
	}
	return role
}

var hexColour = regexp.MustCompile(`^#[0-9a-f]{6}$`)

func oneOf(field, v string, list []string) error {
	if slices.Contains(list, v) {
		return nil
	}
	return fieldErr(field, "must be one of %s", strings.Join(list, ", "))
}

// checkText holds a free text to one line of plain, printable text: no
// control characters (a line break among them) and no bidirectional
// formatting characters, which could make the line read otherwise than it
// is written.
func checkText(field, s string, maxRunes int, mayBeEmpty bool) error {
	if strings.TrimSpace(s) == "" {
		if mayBeEmpty && s == "" {
			return nil
		}
		return fieldErr(field, "must not be empty or only spaces")
	}
	if n := utf8.RuneCountInString(s); n > maxRunes {
		return fieldErr(field, "is %d characters, over the limit of %d", n, maxRunes)
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) || r == ' ' || r == ' ' {
			return fieldErr(field, "must be one line of plain text (no control, line-break or bidirectional formatting characters)")
		}
	}
	return nil
}

// AccentOnText reports whether the design sets text in its accent or on it:
// a band title block (the title in white on the accent) or headings set in
// the accent (smallcaps, numbered). The runner's template draws exactly
// these with the accent on text; everything else colours only graphics.
func (d Design) AccentOnText() bool {
	return d.TitleBlock == "band" || d.Headings == "smallcaps" || d.Headings == "numbered"
}

// MinContrast is the contrast the design's accent must reach on white: 4.5:1
// where it touches text, 3:1 where it colours only graphics (D17).
func (d Design) MinContrast() float64 {
	if d.AccentOnText() {
		return TextContrast
	}
	return GraphicsContrast
}

// Canonical is the design's canonical document: its JSON in field order,
// indented by two spaces, with a final newline. A built-in's embedded
// document is exactly its Canonical (invariant 8), and the receipt hashes it,
// the logo's bytes included. A design with no logo has no "logo" key.
// Characters such as "&" and "<" are written as they are, not escaped.
func (d Design) Canonical() []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(d); err != nil {
		// A Design is strings, ints and bools; it always encodes.
		panic(err)
	}
	return b.Bytes()
}

// SHA256 is the lower-case hex SHA-256 of Canonical, the design's identity in
// a run's receipt.
func (d Design) SHA256() string {
	sum := sha256.Sum256(d.Canonical())
	return hex.EncodeToString(sum[:])
}
