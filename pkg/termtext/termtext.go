// Package termtext prepares author text (a release guide's overview and
// key help, the Drafter's narration) for a terminal: cleaned of what a
// terminal would act on or hide, and set in a gutter so it cannot pass for
// the CLI's own lines (agentry-cli#257, agentry-cli#419).
package termtext

import (
	"strings"
	"unicode"
)

// Gutter starts every line of quoted author text. The CLI's own lines start
// at column 0 or with two spaces (a contract line, a list item), so a line
// behind the gutter cannot read as one of them.
const Gutter = "  | "

// Safe is s as a terminal may show it: every control character but a
// newline or a tab, every C1 control and every format character (a bidi
// override, a zero-width space) becomes "?", so the text cannot send escape
// sequences (colours, cursor moves, a window title, a hyperlink) to the
// terminal it is printed on, or show other than what it says. The core
// refuses all of these in a guide already; this is the second defence, and
// the only one for text the core never validates.
func Safe(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || unicode.Is(unicode.Cf, r) {
			return '?'
		}
		return r
	}, s)
}

// Quote is s cleaned by Safe and set behind the Gutter, one line of s per
// line, with its surrounding blank lines trimmed. A blank line inside s
// keeps the gutter, so the quoted block reads as one.
func Quote(s string) string {
	lines := strings.Split(strings.Trim(Safe(s), "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(Gutter+l, " ")
	}
	return strings.Join(lines, "\n")
}
