package acpagent

import (
	"strings"
	"unicode"
)

// maxLabel bounds a one-line label (a title, a name, a path) the agent shows.
const maxLabel = 200

// cleanBody is text a client draws as the agent's own words (a reply, a
// tool's output): every control character but a newline and a tab is dropped,
// so it cannot send escape sequences to a client that draws on a terminal. A
// carriage return goes with the rest, and a line feed keeps the line.
func cleanBody(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			return -1
		}
		return r
	}, s)
}

// cleanText is text the person decides on or reads as the agent's own
// statement (the consent request, a note): cleanBody, and every format
// character (a bidirectional override, a zero-width space) and Unicode line
// or paragraph separator becomes "?", so it cannot read as other than it says
// or start a line that reads as another's.
func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			return -1
		case unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r):
			return '?'
		}
		return r
	}, s)
}

// cleanLine is a label: cleanText on one line of at most maxLabel bytes.
func cleanLine(s string) string {
	s = strings.Join(strings.Fields(strings.NewReplacer("\n", " ", "\t", " ").Replace(cleanText(s))), " ")
	if len(s) > maxLabel {
		cut := maxLabel
		for cut > 0 && !isRuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	return s
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
