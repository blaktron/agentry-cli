package reportdesign

import (
	"embed"
	"fmt"
)

// The five built-in themes, each a design language and each exactly a
// value of the editor's model: its embedded document is its Canonical,
// byte for byte.
//
//go:embed builtins/*.json
var builtinFiles embed.FS

// builtinIDs is the order the themes are offered in; Plain is the default
// and stays near-monochrome.
var builtinIDs = []string{"plain", "classic", "modern", "editorial", "technical"}

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
