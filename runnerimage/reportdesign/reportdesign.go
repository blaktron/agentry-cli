// Package reportdesign is the one definition of a report design: the theme
// document version 3 that sets the look of a person's PDF reports, a whole
// brand. The server validates a design with it before storing one in the
// account's library, the CLI before the runner's template reads one, and the
// desktop before it saves one; the site's editor reads the same rules through
// the openapi's ReportDesign schema, which a core test holds to Schema.
//
// A design is data, never markup, with one bounded exception. Colours are
// #rrggbb; fonts, title blocks, heading treatments, heading scales, margins,
// quote styles, paper, page numbers, header styles, cover styles and cover
// facts are names from closed lists; every free text (the design's name, the
// brand block's lines, the header's and the footer's slots) is one line of
// plain text that the template sets as content, never evaluates. A slot may
// name a token of a closed set ({title}, {page}, …), which the template
// substitutes before it sets the text; anything else between braces is
// literal. There is no path, URL or file name in a design: the brand's
// website is drawn as text. The one exception is the custom cover's Typst
// source, which is code: bounded, compiled only under the renderer's
// confinement with the render root as its whole world, and carried only
// inside a document this package validated (cover.go).
//
// The images (the logo, the cover image, a custom cover's images and
// thumbnail) are the person's own pixels, as this package re-encoded them
// (logo.go, image.go), never a URL. They sit outside the document's byte cap
// and a server answers them only when asked.
//
// The look stays restrained: an accent and an optional secondary colour,
// each with a derived tint; body text is near-black whatever the design.
// Where a colour touches text it must reach 4.5:1 on white, and where it
// colours only graphics 3:1. Which it is follows from the design, and the
// runner's template must draw it so (AccentOnText, SecondaryOnText):
//
//   - titleBlock "band" sets the title in white on a full-width accent band;
//   - headings "smallcaps" and "numbered" set level 1 and 2 headings in the
//     accent;
//   - a cover that is drawn (cover.when is not "never") puts text on or in a
//     colour as its style declares (CoverStyles): "band" and "largeType" the
//     accent, "plate" and "custom" the accent too (each draws as "band" when
//     it cannot be drawn as itself), "split" the secondary, or the accent
//     when the design has none;
//   - every other choice colours only rules, bars, shapes, tinted cells and
//     callout edges, and keeps text near-black.
package reportdesign

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// DocumentVersion is the value of "agentryTheme" this package reads. A
// theme document of version 1 or 2 is refused, with no mapping.
const DocumentVersion = 3

// Bounds on a design.
const (
	// MaxDocumentBytes caps a design's canonical document without its
	// images and its custom cover. A canonical design is well under it;
	// the cap bounds what a client may make the server parse and store.
	MaxDocumentBytes = 24 << 10
	// MaxNameRunes caps the design's name and the brand's name, labels the
	// person sees.
	MaxNameRunes = 60
	// MaxLineRunes caps every other line of text: the brand's strapline,
	// contact lines, website and confidentiality line, and each header and
	// footer slot.
	MaxLineRunes = 120
	// MaxContactLines caps the brand block's contact lines.
	MaxContactLines = 4
	// SlotCount is the number of slots in the header and in the footer:
	// left, centre, right.
	SlotCount = 3
	// MaxVersion caps the design's revision number.
	MaxVersion = 1_000_000
	// MaxLibrary caps an account's library: how many designs it keeps.
	MaxLibrary = 10
)

// MaxFooterTextRunes is the cap on the footer's one line in a version-2
// document, which Version2 writes from a slot: one slot's worth.
const MaxFooterTextRunes = MaxLineRunes

// Design is a report design: the theme document, version 3. Field order is
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
	// Accent is the first colour, #rrggbb in lower case. Its tint is
	// derived (Tint), never stored.
	Accent string `json:"accent"`
	// Secondary is the second colour, #rrggbb in lower case, or "" for
	// none. Its tint is derived as the accent's is.
	Secondary string `json:"secondary"`
	// TitleBlock is a TitleBlocks value, the title's treatment on page 1.
	TitleBlock string `json:"titleBlock"`
	// Headings is a HeadingStyles value, the treatment of level 1 and 2.
	Headings string `json:"headings"`
	// HeadingScale is a HeadingScales value, how much larger than the
	// body each heading level is set.
	HeadingScale string `json:"headingScale"`
	// Margins is a MarginPresets value, the page's margins.
	Margins string `json:"margins"`
	Tables  Tables `json:"tables"`
	// Quotes is a QuoteStyles value.
	Quotes string `json:"quotes"`
	Brand  Brand  `json:"brand"`
	Header Header `json:"header"`
	Footer Footer `json:"footer"`
	Cover  Cover  `json:"cover"`
	// Logo is the person's logo, drawn in the title block, on the cover
	// where its facts include it and in a logoLeft header; the zero Logo
	// is none, and is not written.
	Logo Logo `json:"logo,omitzero"`
	// CoverImage is the design's cover image, drawn by the plate cover
	// style and offered to a custom cover; the zero Image is none, and is
	// not written.
	CoverImage Image `json:"coverImage,omitzero"`
	// CustomCover is the design's custom cover, drawn when the cover style
	// is "custom"; the zero CustomCover is none, and is not written.
	CustomCover CustomCover `json:"customCover,omitzero"`
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

// Brand is the brand block: who the report is from. Every line is optional
// ("" or no contact lines for none) and is drawn where a header, a footer or
// a cover asks for it, as text.
type Brand struct {
	// Name is the brand's name, at most MaxNameRunes; the {brand} token.
	Name string `json:"name"`
	// Strapline is one line under the name, at most MaxLineRunes.
	Strapline string `json:"strapline"`
	// Contact is up to MaxContactLines lines (an address, a telephone, an
	// email), each at most MaxLineRunes and none empty.
	Contact []string `json:"contact"`
	// Website is a host name or an https address, at most MaxLineRunes,
	// drawn as text and never made a link by the template.
	Website string `json:"website"`
	// Confidentiality is a line such as "Confidential", at most
	// MaxLineRunes; the {confidentiality} token.
	Confidentiality string `json:"confidentiality"`
}

// Header is the running header, drawn from page 2 on a report, on every
// page of a memo and never on a letter.
type Header struct {
	// Style is a HeaderStyles value.
	Style string `json:"style"`
	// Slots are the left, centre and right texts, each a template of
	// literal text and tokens (Tokens) of at most MaxLineRunes; "" for an
	// empty slot.
	Slots [SlotCount]string `json:"slots"`
}

// Footer is the page footer.
type Footer struct {
	// PageNumbers is a PageNumberStyles value.
	PageNumbers string `json:"pageNumbers"`
	// Slots are the left, centre and right texts, as the header's.
	Slots [SlotCount]string `json:"slots"`
}

// Cover is the cover page.
type Cover struct {
	// Style is a CoverStyles value: a built-in layout, or "custom" for the
	// design's custom cover (which falls back to "band" when it cannot be
	// drawn).
	Style string `json:"style"`
	// Shows are the facts the cover carries, each a CoverFacts value, no
	// value twice, in the order given.
	Shows []string `json:"shows"`
	// When is a CoverWhens value: "long" draws the cover for a titled
	// report past the template's length threshold, "always" for every
	// titled report, "never" for none.
	When string `json:"when"`
}

// The closed lists. Each is the whole set of accepted names, in the order an
// editor offers them.
var (
	Papers           = []string{"a4", "letter"}
	TitleBlocks      = []string{"band", "bar", "ruled", "none"}
	HeadingStyles    = []string{"rule", "bar", "smallcaps", "numbered", "plain"}
	HeadingScales    = []string{"compact", "regular", "generous"}
	MarginPresets    = []string{"narrow", "regular", "wide"}
	QuoteStyles      = []string{"callout", "plain"}
	PageNumberStyles = []string{"none", "n", "nOfN"}
	HeaderStyles     = []string{"none", "rule", "brandLine", "titleAndPage", "logoLeft"}
	CoverFacts       = []string{"title", "subtitle", "author", "date", "brand", "logo", "confidentiality"}
	CoverWhens       = []string{"long", "always", "never"}
)

// CoverStyle is a cover layout and its declaration of which of the design's
// colours carry text on it, which the contrast rule reads.
type CoverStyle struct {
	// Name is the style's name, a Cover.Style value.
	Name string
	// AccentOnText says the accent carries text: white text on an accent
	// panel, or the title set in the accent.
	AccentOnText bool
	// SecondaryOnText says the secondary carries text, or the accent does
	// when the design has no secondary.
	SecondaryOnText bool
}

var coverStyles = []CoverStyle{
	// The title in white on a full-height accent panel at the top third.
	{Name: "band", AccentOnText: true},
	// A secondary panel down the left third with the brand and the logo in
	// white, the title in ink on white.
	{Name: "split", SecondaryOnText: true},
	// An ink title on white with accent and secondary shapes.
	{Name: "geometric"},
	// The title at display size in the accent, nothing else above the fold.
	{Name: "largeType", AccentOnText: true},
	// A hairline frame, centred title, brand and date.
	{Name: "framed"},
	// The cover image across the top half, the title block below; without
	// a cover image it draws as band, so the accent must carry text.
	{Name: "plate", AccentOnText: true},
	// The design's custom cover, the person's own code: what it draws is
	// theirs, but a custom cover that cannot be drawn falls back to band,
	// so the accent must carry text.
	{Name: "custom", AccentOnText: true},
}

// CoverStyles is every cover style with its declaration, in the order an
// editor offers them.
func CoverStyles() []CoverStyle { return slices.Clone(coverStyles) }

// CoverStyleNames is the cover styles' names.
func CoverStyleNames() []string {
	out := make([]string, len(coverStyles))
	for i, s := range coverStyles {
		out[i] = s.Name
	}
	return out
}

// CoverStyleByName finds a cover style by its name.
func CoverStyleByName(name string) (CoverStyle, bool) {
	for _, s := range coverStyles {
		if s.Name == name {
			return s, true
		}
	}
	return CoverStyle{}, false
}

// Kind is a family's kind.
type Kind string

// The kinds of family.
const (
	Sans  Kind = "sans"
	Serif Kind = "serif"
	Mono  Kind = "mono"
)

// Family is a font family the Typst image carries. The runner sets it with
// Noto behind it for the scripts it lacks.
type Family struct {
	// Name is the family's name as Typst and the web fonts know it.
	Name string
	Kind Kind
	// HeadingsOnly marks a display face, offered for headings only.
	HeadingsOnly bool
	// Version2 is the family a version-2 projection of a design names in
	// its place: the family itself where version 2 knew it, else the
	// nearest of the first fourteen in the same role.
	Version2 string
}

var families = []Family{
	{Name: "Inter", Kind: Sans, Version2: "Inter"},
	{Name: "IBM Plex Sans", Kind: Sans, Version2: "IBM Plex Sans"},
	{Name: "Source Sans 3", Kind: Sans, Version2: "Source Sans 3"},
	{Name: "Work Sans", Kind: Sans, Version2: "Work Sans"},
	{Name: "DM Sans", Kind: Sans, Version2: "Inter"},
	{Name: "Nunito Sans", Kind: Sans, Version2: "Source Sans 3"},
	{Name: "Raleway", Kind: Sans, Version2: "Work Sans"},
	{Name: "Space Grotesk", Kind: Sans, Version2: "IBM Plex Sans"},
	{Name: "Montserrat", Kind: Sans, HeadingsOnly: true, Version2: "Montserrat"},
	{Name: "Source Serif 4", Kind: Serif, Version2: "Source Serif 4"},
	{Name: "Lora", Kind: Serif, Version2: "Lora"},
	{Name: "Merriweather", Kind: Serif, Version2: "Merriweather"},
	{Name: "IBM Plex Serif", Kind: Serif, Version2: "IBM Plex Serif"},
	{Name: "EB Garamond", Kind: Serif, Version2: "EB Garamond"},
	{Name: "Libre Baskerville", Kind: Serif, Version2: "Lora"},
	{Name: "Crimson Pro", Kind: Serif, Version2: "EB Garamond"},
	{Name: "Playfair Display", Kind: Serif, HeadingsOnly: true, Version2: "Playfair Display"},
	{Name: "Fraunces", Kind: Serif, HeadingsOnly: true, Version2: "Playfair Display"},
	{Name: "IBM Plex Mono", Kind: Mono, Version2: "IBM Plex Mono"},
	{Name: "JetBrains Mono", Kind: Mono, Version2: "JetBrains Mono"},
	{Name: "Source Code Pro", Kind: Mono, Version2: "Source Code Pro"},
	{Name: "Fira Code", Kind: Mono, Version2: "JetBrains Mono"},
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
// document ("fonts.body", "brand.contact.2", "customCover.images.0"; "" for
// the document as a whole), and what is wrong. The message never quotes the
// value back, beyond a colour.
type FieldError struct {
	Field   string
	Message string
	// kind is ErrImageTooLarge or ErrImageFormat for an image's refusal.
	kind error
}

// Unwrap is the refusal's kind, where it has one (ErrImageTooLarge,
// ErrImageFormat), for errors.Is.
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
	AgentryTheme *int             `json:"agentryTheme"`
	Name         *string          `json:"name"`
	BasedOn      *string          `json:"basedOn"`
	Version      *int             `json:"version"`
	Paper        *string          `json:"paper"`
	Fonts        *wireFonts       `json:"fonts"`
	Accent       *string          `json:"accent"`
	Secondary    *string          `json:"secondary"`
	TitleBlock   *string          `json:"titleBlock"`
	Headings     *string          `json:"headings"`
	HeadingScale *string          `json:"headingScale"`
	Margins      *string          `json:"margins"`
	Tables       *wireTables      `json:"tables"`
	Quotes       *string          `json:"quotes"`
	Brand        *wireBrand       `json:"brand"`
	Header       *wireHeader      `json:"header"`
	Footer       *wireFooter      `json:"footer"`
	Cover        *wireCover       `json:"cover"`
	Logo         *wireImage       `json:"logo"`
	CoverImage   *wireImage       `json:"coverImage"`
	CustomCover  *wireCustomCover `json:"customCover"`
}

type wireImage struct {
	PNG  *string `json:"png"`
	JPEG *string `json:"jpeg"`
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

type wireBrand struct {
	Name            *string   `json:"name"`
	Strapline       *string   `json:"strapline"`
	Contact         *[]string `json:"contact"`
	Website         *string   `json:"website"`
	Confidentiality *string   `json:"confidentiality"`
}

type wireHeader struct {
	Style *string   `json:"style"`
	Slots *[]string `json:"slots"`
}

type wireFooter struct {
	PageNumbers *string   `json:"pageNumbers"`
	Slots       *[]string `json:"slots"`
}

type wireCover struct {
	Style *string   `json:"style"`
	Shows *[]string `json:"shows"`
	When  *string   `json:"when"`
}

type wireCustomCover struct {
	Source    *string      `json:"source"`
	Images    *[]wireImage `json:"images"`
	Thumbnail *wireImage   `json:"thumbnail"`
}

var unknownField = regexp.MustCompile(`unknown field "([^"]*)"`)

// Parse reads a design document and validates it: every field present but
// the optional images and custom cover (a "logo": null is no logo, as if
// the key were left out, and so for "coverImage" and "customCover"), no
// other key, no second value after it, at most MaxDocumentBytes in its
// canonical form besides the images and the custom cover, and every rule
// of Validate. A refusal is a *FieldError.
func Parse(data []byte) (Design, error) {
	// Before reading it: the most a document may be with every image and
	// the custom cover at their largest, which counts the whole document
	// as sent.
	if len(data) > MaxDocumentWithAssets {
		return Design{}, fieldErr("", "the document is %d bytes, over the %d bytes a document may be with its images and custom cover", len(data), MaxDocumentWithAssets)
	}
	if !utf8.Valid(data) {
		return Design{}, fieldErr("", "the document is not UTF-8")
	}
	// The version first, so an earlier document is refused by its version
	// and not by the first key it has that this one does not.
	var head struct {
		AgentryTheme *int `json:"agentryTheme"`
	}
	if err := json.Unmarshal(data, &head); err == nil && head.AgentryTheme != nil && *head.AgentryTheme != DocumentVersion {
		return Design{}, fieldErr("agentryTheme", "must be %d; an earlier theme document is not read", DocumentVersion)
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
	d, err := w.design()
	if err != nil {
		return Design{}, err
	}
	if n := len(d.WithoutAssets().Canonical()); n > MaxDocumentBytes {
		return Design{}, fieldErr("", "the document is %d bytes without its images and custom cover, over the %d-byte limit", n, MaxDocumentBytes)
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
	case "slice", "array":
		return "an array"
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
	case w.Secondary == nil:
		return Design{}, missing("secondary")
	case w.TitleBlock == nil:
		return Design{}, missing("titleBlock")
	case w.Headings == nil:
		return Design{}, missing("headings")
	case w.HeadingScale == nil:
		return Design{}, missing("headingScale")
	case w.Margins == nil:
		return Design{}, missing("margins")
	case w.Tables == nil:
		return Design{}, missing("tables")
	case w.Tables.Tinted == nil:
		return Design{}, missing("tables.tinted")
	case w.Tables.Banded == nil:
		return Design{}, missing("tables.banded")
	case w.Quotes == nil:
		return Design{}, missing("quotes")
	case w.Brand == nil:
		return Design{}, missing("brand")
	case w.Brand.Name == nil:
		return Design{}, missing("brand.name")
	case w.Brand.Strapline == nil:
		return Design{}, missing("brand.strapline")
	case w.Brand.Contact == nil:
		return Design{}, missing("brand.contact")
	case w.Brand.Website == nil:
		return Design{}, missing("brand.website")
	case w.Brand.Confidentiality == nil:
		return Design{}, missing("brand.confidentiality")
	case w.Header == nil:
		return Design{}, missing("header")
	case w.Header.Style == nil:
		return Design{}, missing("header.style")
	case w.Header.Slots == nil:
		return Design{}, missing("header.slots")
	case w.Footer == nil:
		return Design{}, missing("footer")
	case w.Footer.PageNumbers == nil:
		return Design{}, missing("footer.pageNumbers")
	case w.Footer.Slots == nil:
		return Design{}, missing("footer.slots")
	case w.Cover == nil:
		return Design{}, missing("cover")
	case w.Cover.Style == nil:
		return Design{}, missing("cover.style")
	case w.Cover.Shows == nil:
		return Design{}, missing("cover.shows")
	case w.Cover.When == nil:
		return Design{}, missing("cover.when")
	}
	headerSlots, err := slots("header.slots", *w.Header.Slots)
	if err != nil {
		return Design{}, err
	}
	footerSlots, err := slots("footer.slots", *w.Footer.Slots)
	if err != nil {
		return Design{}, err
	}
	logo, err := w.Logo.logo()
	if err != nil {
		return Design{}, err
	}
	coverImage, err := w.CoverImage.image("coverImage")
	if err != nil {
		return Design{}, err
	}
	custom, err := w.CustomCover.customCover()
	if err != nil {
		return Design{}, err
	}
	return Design{
		AgentryTheme: *w.AgentryTheme,
		Name:         *w.Name,
		BasedOn:      *w.BasedOn,
		Version:      *w.Version,
		Paper:        *w.Paper,
		Fonts:        Fonts{Headings: *w.Fonts.Headings, Body: *w.Fonts.Body, Code: *w.Fonts.Code},
		Accent:       *w.Accent,
		Secondary:    *w.Secondary,
		TitleBlock:   *w.TitleBlock,
		Headings:     *w.Headings,
		HeadingScale: *w.HeadingScale,
		Margins:      *w.Margins,
		Tables:       Tables{Tinted: *w.Tables.Tinted, Banded: *w.Tables.Banded},
		Quotes:       *w.Quotes,
		Brand: Brand{
			Name:            *w.Brand.Name,
			Strapline:       *w.Brand.Strapline,
			Contact:         nonNil(*w.Brand.Contact),
			Website:         *w.Brand.Website,
			Confidentiality: *w.Brand.Confidentiality,
		},
		Header:      Header{Style: *w.Header.Style, Slots: headerSlots},
		Footer:      Footer{PageNumbers: *w.Footer.PageNumbers, Slots: footerSlots},
		Cover:       Cover{Style: *w.Cover.Style, Shows: nonNil(*w.Cover.Shows), When: *w.Cover.When},
		Logo:        logo,
		CoverImage:  coverImage,
		CustomCover: custom,
	}, nil
}

// slots reads a header's or a footer's slots: exactly SlotCount strings.
func slots(field string, in []string) ([SlotCount]string, error) {
	var out [SlotCount]string
	if len(in) != SlotCount {
		return out, fieldErr(field, "must be %d texts: left, centre, right", SlotCount)
	}
	copy(out[:], in)
	return out, nil
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// Validate holds a design to every rule: the version, the closed lists, the
// fonts' roles, the colours' form and contrast, every line of text, the
// slots, the cover's facts, and the images and the custom cover when it has
// them. A refusal is a *FieldError naming the first field found wrong, in
// document order, the contrast rule last among the document's own fields
// because the styles that decide it come after the colours.
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
	if d.Secondary != "" && !hexColour.MatchString(d.Secondary) {
		return fieldErr("secondary", "must be a colour #rrggbb in lower-case hex, or empty for none")
	}
	if err := oneOf("titleBlock", d.TitleBlock, TitleBlocks); err != nil {
		return err
	}
	if err := oneOf("headings", d.Headings, HeadingStyles); err != nil {
		return err
	}
	if err := oneOf("headingScale", d.HeadingScale, HeadingScales); err != nil {
		return err
	}
	if err := oneOf("margins", d.Margins, MarginPresets); err != nil {
		return err
	}
	if err := oneOf("quotes", d.Quotes, QuoteStyles); err != nil {
		return err
	}
	if err := d.Brand.validate(); err != nil {
		return err
	}
	if err := oneOf("header.style", d.Header.Style, HeaderStyles); err != nil {
		return err
	}
	if err := checkSlots("header.slots", d.Header.Slots); err != nil {
		return err
	}
	if err := oneOf("footer.pageNumbers", d.Footer.PageNumbers, PageNumberStyles); err != nil {
		return err
	}
	if err := checkSlots("footer.slots", d.Footer.Slots); err != nil {
		return err
	}
	if err := d.Cover.validate(); err != nil {
		return err
	}
	// The contrast rule, once every choice that decides it is known.
	if err := checkContrast("accent", d.Accent, d.AccentOnText()); err != nil {
		return err
	}
	if d.Secondary != "" {
		if err := checkContrast("secondary", d.Secondary, d.SecondaryOnText()); err != nil {
			return err
		}
	}
	if d.Logo != (Logo{}) {
		if err := checkLogo(d.Logo); err != nil {
			return err
		}
	}
	if d.CoverImage != (Image{}) {
		if err := checkImage("coverImage", d.CoverImage, MaxCoverImageBytes, MaxCoverImageSide, false); err != nil {
			return err
		}
	}
	if !d.CustomCover.IsZero() {
		return d.CustomCover.Validate()
	}
	return nil
}

func (b Brand) validate() error {
	if err := checkText("brand.name", b.Name, MaxNameRunes, true); err != nil {
		return err
	}
	if err := checkText("brand.strapline", b.Strapline, MaxLineRunes, true); err != nil {
		return err
	}
	if len(b.Contact) > MaxContactLines {
		return fieldErr("brand.contact", "is %d lines, over the limit of %d", len(b.Contact), MaxContactLines)
	}
	for i, line := range b.Contact {
		if err := checkText(fmt.Sprintf("brand.contact.%d", i), line, MaxLineRunes, false); err != nil {
			return err
		}
	}
	if err := checkText("brand.website", b.Website, MaxLineRunes, true); err != nil {
		return err
	}
	if b.Website != "" && !website.MatchString(b.Website) {
		return fieldErr("brand.website", "must be a host name such as example.com, or an https address, with no spaces; it is drawn as text")
	}
	return checkText("brand.confidentiality", b.Confidentiality, MaxLineRunes, true)
}

// website is a host name (two or more labels, the last letters only) with
// an optional path, or the same under https://. No other scheme, no
// credentials, no port and no query: it is a line to print, not a link.
var website = regexp.MustCompile(`(?i)^(?:https://)?(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}(?:/[a-z0-9._~%!$&'()*+,;=:@/-]*)?$`)

func (c Cover) validate() error {
	if _, ok := CoverStyleByName(c.Style); !ok {
		return fieldErr("cover.style", "must be one of %s", strings.Join(CoverStyleNames(), ", "))
	}
	seen := map[string]bool{}
	for i, fact := range c.Shows {
		if !slices.Contains(CoverFacts, fact) {
			return fieldErr(fmt.Sprintf("cover.shows.%d", i), "must be one of %s", strings.Join(CoverFacts, ", "))
		}
		if seen[fact] {
			return fieldErr(fmt.Sprintf("cover.shows.%d", i), "names a fact the cover already shows")
		}
		seen[fact] = true
	}
	return oneOf("cover.when", c.When, CoverWhens)
}

func checkSlots(field string, slots [SlotCount]string) error {
	for i, s := range slots {
		if err := checkText(fmt.Sprintf("%s.%d", field, i), s, MaxLineRunes, true); err != nil {
			return err
		}
	}
	return nil
}

// checkContrast holds a colour to the contrast its use needs on white, and
// names the nearest shade that passes when it does not.
func checkContrast(field, colour string, onText bool) error {
	need, use := GraphicsContrast, "colours only graphics"
	if onText {
		need, use = TextContrast, "colours text"
	}
	ratio := onWhite(colour)
	if ratio >= need {
		return nil
	}
	near, _ := NearestPassing(colour, need)
	return fieldErr(field, "%s reaches %.2f:1 on white, and %s that %s needs %.1f:1; the nearest shade that does is %s", colour, shownRatio(ratio, need), article(field), use, need, near)
}

func article(field string) string {
	if field == "secondary" {
		return "a secondary"
	}
	return "an accent"
}

// shownRatio is the ratio as a refusal prints it, to two places: rounded,
// unless that rounds up to the line the ratio falls short of, when it is
// cut instead, so a refusal never reads as a pass.
func shownRatio(ratio, need float64) float64 {
	shown := math.Round(ratio*100) / 100
	if shown < need {
		return shown
	}
	// Cut, and step down once more should the product's rounding in floating
	// point have carried it onto the line.
	if shown = math.Floor(ratio*100) / 100; shown >= need {
		shown -= 0.01
	}
	return shown
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
// control characters (a line break among them), no format characters (the
// bidirectional controls, zero-width characters, the soft hyphen and the
// tag block), which draw nothing or make the line read otherwise than it is
// written, and no line or paragraph separator.
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
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == 0x2028 || r == 0x2029 {
			return fieldErr(field, "must be one line of plain text (no control, line-break, bidirectional formatting or other invisible format characters)")
		}
	}
	return nil
}

// AccentOnText reports whether the design sets text in its accent or on it:
// a band title block (the title in white on the accent), headings set in the
// accent (smallcaps, numbered), or a cover that is drawn in a style that
// declares the accent (band, largeType, plate, custom; split without a
// secondary).
// The runner's template draws exactly these with the accent on text;
// everything else colours only graphics.
func (d Design) AccentOnText() bool {
	if d.TitleBlock == "band" || d.Headings == "smallcaps" || d.Headings == "numbered" {
		return true
	}
	if d.Cover.When == "never" {
		return false
	}
	style, _ := CoverStyleByName(d.Cover.Style)
	return style.AccentOnText || (style.SecondaryOnText && d.Secondary == "")
}

// SecondaryOnText reports whether the design sets text on its secondary: a
// cover that is drawn in a style that declares it (split), when the design
// has a secondary.
func (d Design) SecondaryOnText() bool {
	if d.Secondary == "" || d.Cover.When == "never" {
		return false
	}
	style, _ := CoverStyleByName(d.Cover.Style)
	return style.SecondaryOnText
}

// MinContrast is the contrast the design's accent must reach on white: 4.5:1
// where it touches text, 3:1 where it colours only graphics.
func (d Design) MinContrast() float64 {
	if d.AccentOnText() {
		return TextContrast
	}
	return GraphicsContrast
}

// MinSecondaryContrast is the contrast the design's secondary must reach on
// white, by the same rule.
func (d Design) MinSecondaryContrast() float64 {
	if d.SecondaryOnText() {
		return TextContrast
	}
	return GraphicsContrast
}

// WithoutAssets is the design with no logo, cover image or custom cover: the
// document a server stores and answers unless asked for its assets.
func (d Design) WithoutAssets() Design {
	d.Logo, d.CoverImage, d.CustomCover = Logo{}, Image{}, CustomCover{}
	return d
}

// normalized is the design with every list non-nil, so its canonical form
// writes [] and never null.
func (d Design) normalized() Design {
	d.Brand.Contact = nonNil(d.Brand.Contact)
	d.Cover.Shows = nonNil(d.Cover.Shows)
	if !d.CustomCover.IsZero() {
		d.CustomCover.Images = nonNil(d.CustomCover.Images)
	}
	return d
}

// Canonical is the design's canonical document: its JSON in field order,
// indented by two spaces, with a final newline. A built-in's embedded
// document is exactly its Canonical, and the receipt hashes it, the images
// and the custom cover included. A design without a logo, a cover image or
// a custom cover has no key for it. Characters such as "&" and "<" are
// written as they are, not escaped.
func (d Design) Canonical() []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(d.normalized()); err != nil {
		// A Design is strings, ints, bools and lists of strings; it always
		// encodes.
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
