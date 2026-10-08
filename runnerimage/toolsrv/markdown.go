package toolsrv

// What export_pdf reads of a Markdown file before Typst sees it (plan
// scratch-and-pdf-reports.md §5; agentry-cli#730): its headings, renumbered
// so that PDF/UA-1 holds; its title; and the local image sources it names,
// so the server can copy those into the render root. This is a line scanner,
// not a Markdown parser: cmarker parses the file in the render, and what the
// scanner misses only costs a heading's number or an image drawn as its
// alt text, never a file outside the run's folders.

import (
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

// mdDoc is the scanner's result.
type mdDoc struct {
	// text is the Markdown with its headings renumbered.
	text string
	// title is the first heading's text, its inline markup removed; empty
	// when the file has no heading.
	title string
	// images are the image sources the file names, in order, once each, as
	// written (cmarker passes the template the same strings).
	images []string
}

var (
	// A line's container markers (block quotes, list items), then up to
	// three spaces: a heading or a fence may follow.
	mdContainer = ` {0,3}(?:>[ \t]?|(?:[-*+]|[0-9]{1,9}[.)])[ \t]+)`
	mdATX       = regexp.MustCompile(`^((?:` + mdContainer + `)*` + ` {0,3})(#{1,6})([ \t].*)?$`)
	mdFence     = regexp.MustCompile("^(?:" + mdContainer + ")*" + " {0,3}(`{3,}|~{3,})(.*)$")
	mdSetext    = regexp.MustCompile(`^ {0,3}(=+|-+)[ \t]*$`)
	// A line that cannot carry a setext underline's paragraph: blank,
	// indented code, or a block of its own.
	mdNotParagraph = regexp.MustCompile(`^(?:[ \t]*$| {4}|[ \t]{0,3}(?:>|[-*+][ \t]|[0-9]{1,9}[.)][ \t]|#|<|\||` + "```|~~~))")

	mdInlineImage = regexp.MustCompile(`!\[(?:[^\]\\]|\\.)*\]\(\s*(?:<([^>\n]*)>|([^\s()]+(?:\([^\s()]*\)[^\s()]*)*))`)
	mdRefDef      = regexp.MustCompile(`(?m)^ {0,3}\[((?:[^\]\\]|\\.)+)\]:[ \t]*(?:<([^>\n]*)>|(\S+))`)
	// An image by reference: ![alt][label], ![label][] or ![label].
	mdRefImage  = regexp.MustCompile(`!\[((?:[^\]\\]|\\.)*)\](?:\[((?:[^\]\\]|\\.)*)\])?`)
	mdHTMLImage = regexp.MustCompile(`(?i)<img\b[^>]*?\bsrc\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)

	mdLink      = regexp.MustCompile(`!?\[((?:[^\]\\]|\\.)*)\]\([^)]*\)`)
	mdStrongA   = regexp.MustCompile(`\*\*(\S(?:.*?\S)?)\*\*`)
	mdStrongU   = regexp.MustCompile(`__(\S(?:.*?\S)?)__`)
	mdEmphA     = regexp.MustCompile(`\*(\S(?:.*?\S)?)\*`)
	mdEmphU     = regexp.MustCompile(`(^|\W)_(\S(?:.*?\S)?)_(\W|$)`)
	mdStrike    = regexp.MustCompile(`~~(\S(?:.*?\S)?)~~`)
	mdTag       = regexp.MustCompile(`<[^>]*>`)
	mdEscape    = regexp.MustCompile(`\\([!-/:-@\[-` + "`" + `{-~])`)
	mdClosingAT = regexp.MustCompile(`[ \t]+#+[ \t]*$`)
)

// maxTitle bounds the title taken from a heading, in bytes.
const maxTitle = 200

// scanMarkdown renumbers the headings of src and finds its title and image
// sources. A heading is drawn at most one level below the one before it,
// and the first at level 1: PDF/UA-1 refuses any other order, and Markdown
// allows it. Only levels move, and only down; a setext heading of level 2
// that must become 1 gets an "=" underline.
func scanMarkdown(src string) mdDoc {
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	var doc mdDoc
	prev := 0
	renumber := func(level int) int {
		drawn := min(level, prev+1)
		prev = drawn
		return drawn
	}
	fence := ""
	for i, line := range lines {
		if fence != "" {
			if m := mdFence.FindStringSubmatch(line); m != nil && strings.HasPrefix(m[1], fence) && strings.TrimSpace(m[2]) == "" {
				fence = ""
			}
			continue
		}
		// A backtick fence's info string holds no backtick (CommonMark).
		if m := mdFence.FindStringSubmatch(line); m != nil && (m[1][0] != '`' || !strings.Contains(m[2], "`")) {
			fence = m[1]
			continue
		}
		if m := mdATX.FindStringSubmatch(line); m != nil {
			drawn := renumber(len(m[2]))
			lines[i] = m[1] + strings.Repeat("#", drawn) + m[3]
			if doc.title == "" {
				doc.title = plainTitle(mdClosingAT.ReplaceAllString(" "+strings.TrimSpace(m[3]), ""))
			}
			continue
		}
		if m := mdSetext.FindStringSubmatch(line); m != nil && i > 0 && !mdNotParagraph.MatchString(lines[i-1]) && !mdSetext.MatchString(lines[i-1]) {
			level := 1
			if m[1][0] == '-' {
				level = 2
			}
			if renumber(level) == 1 && level == 2 {
				lines[i] = strings.ReplaceAll(line, "-", "=")
			}
			if doc.title == "" {
				doc.title = plainTitle(lines[i-1])
			}
		}
	}
	doc.text = strings.Join(lines, "\n")
	seen := map[string]bool{}
	add := func(groups ...string) {
		for _, g := range groups {
			if g = strings.TrimSpace(g); g != "" && !seen[g] {
				seen[g] = true
				doc.images = append(doc.images, g)
				return
			}
		}
	}
	for _, m := range mdInlineImage.FindAllStringSubmatch(src, -1) {
		add(m[1:]...)
	}
	// A reference definition is an image's only when an image names its
	// label: a link's target is never copied, nor counted.
	used := map[string]bool{}
	for _, m := range mdRefImage.FindAllStringSubmatch(src, -1) {
		label := m[2]
		if label == "" {
			label = m[1]
		}
		used[refLabel(label)] = true
	}
	for _, m := range mdRefDef.FindAllStringSubmatch(src, -1) {
		if used[refLabel(m[1])] {
			add(m[2:]...)
		}
	}
	for _, m := range mdHTMLImage.FindAllStringSubmatch(src, -1) {
		add(m[1:]...)
	}
	return doc
}

// refLabel is a reference label as CommonMark matches it: case-folded,
// its inner whitespace collapsed.
func refLabel(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

// plainTitle is a heading's text without its inline markup, on one line
// and bounded.
func plainTitle(s string) string {
	s = mdLink.ReplaceAllString(s, "$1")
	for _, re := range []*regexp.Regexp{mdStrongA, mdStrongU, mdStrike, mdEmphA} {
		s = re.ReplaceAllString(s, "$1")
	}
	s = mdEmphU.ReplaceAllString(s, "$1$2$3")
	s = strings.ReplaceAll(mdTag.ReplaceAllString(s, ""), "`", "")
	s = mdEscape.ReplaceAllString(s, "$1")
	return oneLine(s, maxTitle)
}

// imageCandidates are the file names a source may mean: as written, with
// Markdown's backslash escapes taken out, and percent-decoded, in that
// order of preference; none for a source with a scheme (a remote image, a
// data: URI), which is never fetched.
func imageCandidates(src string) []string {
	if hasScheme(src) {
		return nil
	}
	if i := strings.IndexAny(src, "?#"); i >= 0 {
		src = src[:i]
	}
	out := []string{src}
	if u := mdEscape.ReplaceAllString(src, "$1"); u != src {
		out = append(out, u)
	}
	if d, err := url.PathUnescape(out[len(out)-1]); err == nil && d != out[len(out)-1] && utf8.ValidString(d) {
		out = append(out, d)
	}
	for i, c := range out {
		out[i] = path.Clean(c)
	}
	return out
}

// hasScheme says whether a source names a URI scheme (RFC 3986's
// "scheme:"), so is not a file name.
func hasScheme(src string) bool {
	i := strings.IndexByte(src, ':')
	if i <= 0 {
		return false
	}
	for j, c := range src[:i] {
		alpha := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		other := c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'
		if !alpha && (j == 0 || !other) {
			return false
		}
	}
	return true
}

// reportMarkdown is the Markdown export_pdf hands Typst for src under a
// theme: its headings renumbered, and its lines kept when the theme keeps
// them.
func reportMarkdown(src string, theme *Theme) mdDoc {
	doc := scanMarkdown(src)
	if theme.LineBreaks == "hard" {
		doc.text = hardLineBreaks(doc.text)
	}
	return doc
}

// shortLine is the length under which a paragraph's line is a line of its
// own (an address, a To line, a signature) and not prose wrapped in the
// source, which an agent wraps near 80 columns if it wraps at all.
const shortLine = 60

// mdHardBreak is a line that already ends in a hard break: a backslash, or
// two spaces.
var mdHardBreak = regexp.MustCompile(`(\\| {2,})$`)

// hardLineBreaks keeps each line of a paragraph on a line of its own (a
// theme's lineBreaks "hard"; agentry-cli#731): a letter's address block,
// its To, From and Date lines and its signature are written one to a line,
// and Markdown joins them. A short paragraph line (under shortLine runes)
// followed by another gets a CommonMark hard break, a trailing backslash;
// a longer one is prose wrapped in the source, which stays one paragraph. Lines that could be anything
// but a paragraph's are left alone (a heading, a list item, a quote, a
// fence and what it holds, indented code, HTML, a table row, a setext
// underline and the line above it), so the scanner can only miss a break,
// never change a block.
func hardLineBreaks(text string) string {
	lines := strings.Split(text, "\n")
	paragraph := func(i int) bool {
		if i >= len(lines) {
			return false
		}
		l := lines[i]
		return !mdNotParagraph.MatchString(l) && !mdSetext.MatchString(l) && !strings.Contains(l, "|")
	}
	fence := ""
	for i, line := range lines {
		if fence != "" {
			if m := mdFence.FindStringSubmatch(line); m != nil && strings.HasPrefix(m[1], fence) && strings.TrimSpace(m[2]) == "" {
				fence = ""
			}
			continue
		}
		if m := mdFence.FindStringSubmatch(line); m != nil && (m[1][0] != '`' || !strings.Contains(m[2], "`")) {
			fence = m[1]
			continue
		}
		if !paragraph(i) || !paragraph(i+1) || mdHardBreak.MatchString(line) || utf8.RuneCountInString(strings.TrimSpace(line)) >= shortLine {
			continue
		}
		// The next line starts a block of its own when it is a fence; a
		// paragraph above a setext underline is a heading.
		if mdFence.MatchString(lines[i+1]) || (i+2 < len(lines) && mdSetext.MatchString(lines[i+2])) {
			continue
		}
		lines[i] = line + `\`
	}
	return strings.Join(lines, "\n")
}
