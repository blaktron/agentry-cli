package reportdesign

import (
	"embed"
	"fmt"
)

// The ten built-in themes, each a design language and each a kit: a whole
// document (faces, colours, title block, headings, header, footer and cover)
// that is exactly a value of the editor's model, so its embedded document is
// its Canonical, byte for byte, and loading one is one click.
//
//go:embed builtins/*.json
var builtinFiles embed.FS

// builtinIDs is the order the themes are offered in; Plain is the default
// and stays near-monochrome. The first five are the themes a version-2
// document may name, with the same look in each version; the five after
// them are version 3's own.
var builtinIDs = []string{"plain", "classic", "modern", "editorial", "technical", "boardroom", "studio", "field", "civic", "lumen"}

// version2BasedOns are the built-ins a version-2 document may name: the
// first five.
const version2Count = 5

// version2Nearest is the version-2 built-in a version-2 projection names
// for each later one: the nearest in faces and tone.
var version2Nearest = map[string]string{
	"boardroom": "classic",
	"studio":    "modern",
	"field":     "technical",
	"civic":     "classic",
	"lumen":     "modern",
}

// version2BasedOn is the built-in a version-2 projection names: the design's
// own where version 2 knows it, else the nearest of the first five.
func version2BasedOn(id string) string {
	for _, v2 := range builtinIDs[:version2Count] {
		if id == v2 {
			return id
		}
	}
	if near, ok := version2Nearest[id]; ok {
		return near
	}
	return DefaultID
}

// DefaultID is the theme a run takes when nothing else chose one.
const DefaultID = "plain"

var builtins = loadBuiltins()

func loadBuiltins() map[string]Design {
	out := make(map[string]Design, len(builtinIDs))
	for _, id := range builtinIDs {
		data, err := builtinFiles.ReadFile("builtins/" + id + ".json")
		if err != nil {
			panic(fmt.Sprintf("reportdesign: built-in %s: %v", id, err))
		}
		// Parse would call Validate, which reads builtins for basedOn;
		// decode here and hold each to Validate in the tests.
		d, err := decodeOnly(data)
		if err != nil {
			panic(fmt.Sprintf("reportdesign: built-in %s: %v", id, err))
		}
		out[id] = d
	}
	return out
}

// BuiltinIDs is the built-in themes' ids, in the order they are offered.
func BuiltinIDs() []string { return append([]string(nil), builtinIDs...) }

// Builtin is the built-in theme with that id.
func Builtin(id string) (Design, bool) {
	d, ok := builtins[id]
	return d, ok
}

// Builtins is every built-in theme, in the order they are offered.
func Builtins() []Design {
	out := make([]Design, 0, len(builtinIDs))
	for _, id := range builtinIDs {
		out = append(out, builtins[id])
	}
	return out
}

// Default is the Plain theme.
func Default() Design { return builtins[DefaultID] }

// BuiltinDocument is a built-in theme's embedded document, as shipped.
func BuiltinDocument(id string) ([]byte, bool) {
	if _, ok := builtins[id]; !ok {
		return nil, false
	}
	data, err := builtinFiles.ReadFile("builtins/" + id + ".json")
	return data, err == nil
}
