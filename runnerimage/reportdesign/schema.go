package reportdesign

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
)

// Schema is the theme document's JSON Schema (draft 2020-12), built from the
// same lists Validate reads, so it cannot drift from them. The openapi's
// ReportDesign schema is held to it by the core's api tests.
//
// The schema says what a document may hold, the families each role takes
// included. The rules it cannot say are Validate's alone: each colour's
// contrast on white (MinContrast, MinSecondaryContrast), that a text is one
// line of plain text (not only spaces, no control, line-break or invisible
// format characters), the brand website's form, that a cover fact is named
// once, the custom cover's byte bounds (the schema counts characters) and
// what an image's base64 must hold: a PNG or a JPEG within its bounds.
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
	line := map[string]any{"type": "string", "maxLength": MaxLineRunes}
	slots := map[string]any{"type": "array", "minItems": SlotCount, "maxItems": SlotCount, "items": line}
	img := func(maxBytes int) map[string]any {
		field := map[string]any{"type": "string", "minLength": 1, "maxLength": base64.StdEncoding.EncodedLen(maxBytes)}
		return map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"minProperties":        1,
			"maxProperties":        1,
			"properties":           map[string]any{"png": field, "jpeg": field},
		}
	}
	doc := object(
		[]string{"agentryTheme", "name", "basedOn", "version", "paper", "fonts", "accent", "secondary", "titleBlock", "headings", "headingScale", "margins", "tables", "quotes", "brand", "header", "footer", "cover"},
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
			"accent":       map[string]any{"type": "string", "pattern": hexColour.String()},
			"secondary":    map[string]any{"type": "string", "pattern": "^(#[0-9a-f]{6})?$"},
			"titleBlock":   str(TitleBlocks),
			"headings":     str(HeadingStyles),
			"headingScale": str(HeadingScales),
			"margins":      str(MarginPresets),
			"tables": object([]string{"tinted", "banded"}, map[string]any{
				"tinted": map[string]any{"type": "boolean"},
				"banded": map[string]any{"type": "boolean"},
			}),
			"quotes": str(QuoteStyles),
			"brand": object([]string{"name", "strapline", "contact", "website", "confidentiality"}, map[string]any{
				"name":            map[string]any{"type": "string", "maxLength": MaxNameRunes},
				"strapline":       line,
				"contact":         map[string]any{"type": "array", "maxItems": MaxContactLines, "items": map[string]any{"type": "string", "minLength": 1, "maxLength": MaxLineRunes}},
				"website":         line,
				"confidentiality": line,
			}),
			"header": object([]string{"style", "slots"}, map[string]any{
				"style": str(HeaderStyles),
				"slots": slots,
			}),
			"footer": object([]string{"pageNumbers", "slots"}, map[string]any{
				"pageNumbers": str(PageNumberStyles),
				"slots":       slots,
			}),
			"cover": object([]string{"style", "shows", "when"}, map[string]any{
				"style": str(CoverStyleNames()),
				"shows": map[string]any{"type": "array", "maxItems": len(CoverFacts), "uniqueItems": true, "items": str(CoverFacts)},
				"when":  str(CoverWhens),
			}),
			// Optional: a design without a logo, a cover image or a custom
			// cover has no key for it.
			"logo": object([]string{"png"}, map[string]any{
				"png": map[string]any{"type": "string", "minLength": 1, "maxLength": maxLogoField},
			}),
			"coverImage": img(MaxCoverImageBytes),
			"customCover": object([]string{"source", "images"}, map[string]any{
				"source":    map[string]any{"type": "string", "minLength": 1, "maxLength": MaxCoverSourceBytes},
				"images":    map[string]any{"type": "array", "maxItems": MaxCoverImages, "items": img(MaxCoverImagesBytes)},
				"thumbnail": object([]string{"png"}, map[string]any{"png": map[string]any{"type": "string", "minLength": 1, "maxLength": base64.StdEncoding.EncodedLen(MaxThumbnailBytes)}}),
			}),
		})
	doc["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	doc["$id"] = "https://secureagentry.net/schemas/report-design-3.schema.json"
	doc["title"] = "Agentry report design (theme document version 3)"
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		panic(err)
	}
	return b.Bytes()
}
