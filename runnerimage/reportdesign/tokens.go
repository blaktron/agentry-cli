package reportdesign

import (
	"slices"
	"strings"
)

// Tokens is the closed set a header or footer slot may name, each written
// between braces ({title}): the report's title, its author, its date, the
// page number, the page count, the brand's name, the current level-1
// heading, and the brand's confidentiality line. The template substitutes
// them and sets the result as content, never evaluates it; anything else
// between braces is literal text.
var Tokens = []string{"title", "author", "date", "page", "pages", "brand", "section", "confidentiality"}

// ExpandSlot substitutes a slot's tokens from facts, keyed by token name
// without the braces, and leaves everything else as it is, braces around an
// unknown word included: a token is a name of Tokens between a brace pair
// with no brace inside it. A token facts does not name expands to nothing.
func ExpandSlot(slot string, facts map[string]string) string {
	var b strings.Builder
	rest := slot
	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			b.WriteString(rest)
			return b.String()
		}
		close := strings.IndexByte(rest[open:], '}')
		if close < 0 {
			b.WriteString(rest)
			return b.String()
		}
		name := rest[open+1 : open+close]
		if !slices.Contains(Tokens, name) {
			// Not a token: the brace is literal, and the scan goes on
			// after it, so "{{title}}" expands to "{" + the title + "}".
			b.WriteString(rest[:open+1])
			rest = rest[open+1:]
			continue
		}
		b.WriteString(rest[:open])
		b.WriteString(facts[name])
		rest = rest[open+close+1:]
	}
}

// TokensIn lists the tokens a slot names, in order, each once.
func TokensIn(slot string) []string {
	var out []string
	rest := slot
	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			return out
		}
		close := strings.IndexByte(rest[open:], '}')
		if close < 0 {
			return out
		}
		name := rest[open+1 : open+close]
		if !slices.Contains(Tokens, name) {
			rest = rest[open+1:]
			continue
		}
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
		rest = rest[open+close+1:]
	}
}
