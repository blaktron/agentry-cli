package reportdesign

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Version2Document is the value of "agentryTheme" in a version-2 document,
// the projection a renderer that reads only version 2 is handed.
const Version2Document = 2

// version2 is the theme document version 2, in its field order: one accent,
// three faces, a title block, a footer line and the logo.
type version2 struct {
	AgentryTheme int            `json:"agentryTheme"`
	Name         string         `json:"name"`
	BasedOn      string         `json:"basedOn"`
	Version      int            `json:"version"`
	Paper        string         `json:"paper"`
	Fonts        Fonts          `json:"fonts"`
	Accent       string         `json:"accent"`
	TitleBlock   string         `json:"titleBlock"`
	Headings     string         `json:"headings"`
	Tables       Tables         `json:"tables"`
	Quotes       string         `json:"quotes"`
	Footer       version2Footer `json:"footer"`
	Logo         Logo           `json:"logo,omitzero"`
}

type version2Footer struct {
	PageNumbers string `json:"pageNumbers"`
	Text        string `json:"text"`
}

// Version2 is the design's version-2 projection, as a canonical version-2
// document: what a renderer that reads only version 2 draws in its place.
// The accent, the faces, the title block, the headings, the tables, the
// quotes and the page numbers carry over; a family version 2 did not have
// becomes the nearest of its first fourteen (Family.Version2); a built-in
// version 2 did not have becomes the nearest of its five; the footer's line
// is the first slot that has text, its {brand} and {confidentiality} tokens
// filled from the brand block and its other tokens dropped, cut to
// MaxFooterTextRunes; the secondary, the brand block, the header, the cover,
// the heading scale, the margins, the cover image and the custom cover are
// left out, since version 2 has no place for them. The logo carries over.
// Version 2's rule on the accent (4.5:1 on white under a band title or
// smallcaps or numbered headings, 3:1 otherwise) asks no more of it than
// this version's, so a design that validates projects to one that does.
func (d Design) Version2() []byte {
	v := version2{
		AgentryTheme: Version2Document,
		Name:         d.Name,
		BasedOn:      version2BasedOn(d.BasedOn),
		Version:      d.Version,
		Paper:        d.Paper,
		Fonts: Fonts{
			Headings: version2Family(d.Fonts.Headings),
			Body:     version2Family(d.Fonts.Body),
			Code:     version2Family(d.Fonts.Code),
		},
		Accent:     d.Accent,
		TitleBlock: d.TitleBlock,
		Headings:   d.Headings,
		Tables:     d.Tables,
		Quotes:     d.Quotes,
		Footer:     version2Footer{PageNumbers: d.Footer.PageNumbers, Text: d.version2FooterText()},
		Logo:       d.Logo,
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
	return b.Bytes()
}

func version2Family(name string) string {
	f, ok := FamilyByName(name)
	if !ok || f.Version2 == "" {
		return name
	}
	return f.Version2
}

// version2FooterText is the footer line a version-2 reader gets: the first
// slot with text, its static tokens filled and the rest dropped.
func (d Design) version2FooterText() string {
	facts := map[string]string{"brand": d.Brand.Name, "confidentiality": d.Brand.Confidentiality}
	for _, slot := range d.Footer.Slots {
		if slot == "" {
			continue
		}
		text := strings.Join(strings.Fields(ExpandSlot(slot, facts)), " ")
		if text == "" {
			continue
		}
		if utf8.RuneCountInString(text) > MaxFooterTextRunes {
			runes := []rune(text)
			text = strings.TrimSpace(string(runes[:MaxFooterTextRunes]))
		}
		return text
	}
	return ""
}
