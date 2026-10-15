package reportdesign

import (
	"bytes"
	"encoding/json"
)

// Schema is the theme document's JSON Schema (draft 2020-12), built from the
// same lists Validate reads, so it cannot drift from them. schema.json beside
// this file is its committed copy, which a test holds equal; the openapi's
// ReportDesign schema is held to it by the core's api tests.
//
// The schema says what a document may hold, the families each role takes
// included. Two rules it cannot say are Validate's alone: the accent's
// contrast on white (MinContrast), and that a text is one line of plain text
// (not only spaces, no control or bidirectional formatting characters). Nor
// can it say what a logo's base64 must hold: a PNG within the pixel bounds.
func Schema() []byte {
	str := func(enum []string) map[string]any { return map[string]any{"type": "string", "enum": enum} }
	object := func(required []string, props map[string]any) map[string]any {
		return map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             required,
			"properties":           props,
		}
	}
	doc := object(
		[]string{"agentryTheme", "name", "basedOn", "version", "paper", "fonts", "accent", "titleBlock", "headings", "tables", "quotes", "footer"},
		map[string]any{
			"agentryTheme": map[string]any{"type": "integer", "enum": []int{DocumentVersion}},
			"name":         map[string]any{"type": "string", "minLength": 1, "maxLength": MaxNameRunes},
			"basedOn":      str(BuiltinIDs()),
			"version":      map[string]any{"type": "integer", "minimum": 1, "maximum": MaxVersion},
			"paper":        str(Papers),
			"fonts": object([]string{"headings", "body", "code"}, map[string]any{
				"headings": str(FontsFor("headings")),
				"body":     str(FontsFor("body")),
				"code":     str(FontsFor("code")),
			}),
			"accent":     map[string]any{"type": "string", "pattern": hexColour.String()},
			"titleBlock": str(TitleBlocks),
			"headings":   str(HeadingStyles),
			"tables": object([]string{"tinted", "banded"}, map[string]any{
				"tinted": map[string]any{"type": "boolean"},
				"banded": map[string]any{"type": "boolean"},
			}),
			"quotes": str(QuoteStyles),
			"footer": object([]string{"pageNumbers", "text"}, map[string]any{
				"pageNumbers": str(PageNumberStyles),
				"text":        map[string]any{"type": "string", "maxLength": MaxFooterTextRunes},
			}),
			// Optional: a design without a logo has no logo key.
			"logo": object([]string{"png"}, map[string]any{
				"png": map[string]any{"type": "string", "minLength": 1, "maxLength": maxLogoField},
			}),
		})
	doc["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	doc["$id"] = "https://secureagentry.net/schemas/report-design-2.schema.json"
	doc["title"] = "Agentry report design (theme document version 2)"
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		panic(err)
	}
	return b.Bytes()
}
