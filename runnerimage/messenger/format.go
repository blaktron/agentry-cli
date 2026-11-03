package messenger

import (
	"html"
	"regexp"
	"slices"
	"strings"
	"unicode/utf16"
)

// An answer is Markdown, as a model writes it. Telegram is sent it as the
// subset of HTML its Bot API parses (bold, italic, strikethrough, code,
// preformatted blocks, links and quotes), every other character escaped,
// and split into messages of at most its length. The text reaching here is
// already masked whole by the supervisor, so no cut ever leaves part of a
// secret on one side of it.

// Part is one message of an answer: its HTML, and the same text plain, sent
// instead when the service cannot parse the HTML.
type Part struct {
	HTML  string
	Plain string
}

// Split is an answer's messages for a service whose messages hold at most
// limit characters: at most MaxParts of them, the last ending with
// RestInAgentry when the answer is longer, and cut says it was.
func Split(markdown string, limit int) (parts []Part, cut bool) {
	reserve := units(RestInAgentry) + 2
	if limit < reserve+16 {
		limit = reserve + 16
	}
	// No block is longer than the last part's room, so every block fits a
	// part of its own.
	var blocks []block
	for _, b := range blocksOf(markdown) {
		if units(b.html) <= limit-reserve {
			blocks = append(blocks, b)
			continue
		}
		blocks = append(blocks, b.pieces(limit-reserve)...)
	}
	var cur []block
	curLen := 0
	for i, b := range blocks {
		room := limit
		if len(parts) == MaxParts-1 {
			// The last part keeps room for the notice that the rest is in
			// Agentry.
			room = limit - reserve
		}
		sep := 0
		if len(cur) > 0 {
			sep = 2
		}
		if n := units(b.html); len(cur) == 0 || curLen+sep+n <= room {
			cur = append(cur, b)
			curLen += sep + n
			continue
		}
		parts = append(parts, joinBlocks(cur))
		if len(parts) == MaxParts {
			cut = i < len(blocks)
			cur = nil
			break
		}
		cur, curLen = []block{b}, units(b.html)
	}
	if len(cur) > 0 {
		parts = append(parts, joinBlocks(cur))
	}
	if cut {
		last := &parts[len(parts)-1]
		last.HTML += "\n\n" + html.EscapeString(RestInAgentry)
		last.Plain += "\n\n" + RestInAgentry
	}
	return parts, cut
}

// units is the length of s as the service counts it: in UTF-16 code units,
// the most a character may be counted as.
func units(s string) int { return len(utf16.Encode([]rune(s))) }

// block is one paragraph, list, quote or code block of an answer, rendered.
type block struct {
	html, plain string
	code        bool
	lang        string
	raw         string // a code block's code, a paragraph's Markdown
}

func joinBlocks(bs []block) Part {
	h := make([]string, 0, len(bs))
	p := make([]string, 0, len(bs))
	for _, b := range bs {
		if b.html == "" && b.plain == "" {
			continue
		}
		h = append(h, b.html)
		p = append(p, b.plain)
	}
	return Part{HTML: strings.Join(h, "\n\n"), Plain: strings.Join(p, "\n\n")}
}

// pieces splits a block longer than limit into blocks of at most limit: a
// code block by its lines, each piece its own preformatted block; a
// paragraph by its lines; and a line longer than a message by its runes.
func (b block) pieces(limit int) []block {
	render := func(raw string) block {
		if b.code {
			return codeBlock(raw, b.lang)
		}
		return textBlock(raw)
	}
	fits := func(raw string) bool { return units(render(raw).html) <= limit }
	var out []block
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			out = append(out, render(strings.Join(cur, "\n")))
			cur = nil
		}
	}
	for _, line := range strings.Split(b.raw, "\n") {
		if fits(strings.Join(append(slices.Clone(cur), line), "\n")) {
			cur = append(cur, line)
			continue
		}
		flush()
		rs := []rune(line)
		for len(rs) > 0 && !fits(string(rs)) {
			// The most runes that fit; one always does, escaped and
			// wrapped, within any limit Split allows.
			lo, hi := 1, len(rs)-1
			for lo < hi {
				mid := (lo + hi + 1) / 2
				if fits(string(rs[:mid])) {
					lo = mid
				} else {
					hi = mid - 1
				}
			}
			out = append(out, render(string(rs[:lo])))
			rs = rs[lo:]
		}
		if len(rs) > 0 {
			cur = []string{string(rs)}
		}
	}
	flush()
	return out
}

// fence opens or closes a fenced code block.
var fence = regexp.MustCompile("^\\s*(```|~~~)\\s*([A-Za-z0-9_+.-]*)\\s*$")

// blocksOf reads an answer's Markdown as its blocks.
func blocksOf(md string) []block {
	md = strings.ReplaceAll(strings.ReplaceAll(md, "\r\n", "\n"), "\r", "\n")
	var out []block
	var para []string
	endPara := func() {
		if len(para) > 0 {
			out = append(out, textBlock(strings.Join(para, "\n")))
			para = nil
		}
	}
	lines := strings.Split(md, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if m := fence.FindStringSubmatch(line); m != nil {
			endPara()
			var code []string
			j := i + 1
			for ; j < len(lines); j++ {
				if f := fence.FindStringSubmatch(lines[j]); f != nil && f[1] == m[1] && f[2] == "" {
					break
				}
				code = append(code, lines[j])
			}
			out = append(out, codeBlock(strings.Join(code, "\n"), m[2]))
			i = j
			continue
		}
		if strings.TrimSpace(line) == "" {
			endPara()
			continue
		}
		para = append(para, line)
	}
	endPara()
	return out
}

// codeBlock renders code as a preformatted block.
func codeBlock(code, lang string) block {
	esc := html.EscapeString(code)
	h := "<pre>" + esc + "</pre>"
	if lang != "" {
		h = `<pre><code class="language-` + html.EscapeString(lang) + `">` + esc + "</code></pre>"
	}
	return block{html: h, plain: code, code: true, lang: lang, raw: code}
}

var (
	heading  = regexp.MustCompile(`^\s{0,3}#{1,6}\s+(.*?)\s*#*\s*$`)
	bullet   = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	quote    = regexp.MustCompile(`^\s{0,3}>\s?(.*)$`)
	rule     = regexp.MustCompile(`^\s{0,3}([-*_])(\s*[-*_]){2,}\s*$`)
	bold     = regexp.MustCompile(`\*\*([^*\n]+)\*\*|__([^_\n]+)__`)
	italic   = regexp.MustCompile(`(^|[^\w*])\*([^*\s][^*\n]*)\*($|[^\w*])|(^|[^\w])_([^_\s][^_\n]*)_($|[^\w])`)
	strike   = regexp.MustCompile(`~~([^~\n]+)~~`)
	link     = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^)\s]+)\)`)
	codeSpan = regexp.MustCompile("`([^`\n]+)`")
)

// textBlock renders a paragraph, a list or a quote.
func textBlock(md string) block {
	lines := strings.Split(md, "\n")
	var h, p []string
	quoted := len(lines) > 0
	for _, l := range lines {
		if !quote.MatchString(l) {
			quoted = false
		}
	}
	for _, l := range lines {
		switch {
		case quoted:
			l = quote.FindStringSubmatch(l)[1]
			h = append(h, inline(l))
			p = append(p, l)
			continue
		case rule.MatchString(l):
			h = append(h, "———")
			p = append(p, "———")
			continue
		}
		if m := heading.FindStringSubmatch(l); m != nil {
			h = append(h, "<b>"+inline(m[1])+"</b>")
			p = append(p, m[1])
			continue
		}
		if m := bullet.FindStringSubmatch(l); m != nil {
			h = append(h, m[1]+"• "+inline(m[2]))
			p = append(p, m[1]+"• "+m[2])
			continue
		}
		h = append(h, inline(l))
		p = append(p, l)
	}
	out := strings.Join(h, "\n")
	if quoted {
		out = "<blockquote>" + out + "</blockquote>"
	}
	return block{html: out, plain: strings.Join(p, "\n"), raw: md}
}

// inline renders a line's code spans, links, bold, italic and
// strikethrough; everything else is escaped. A code span's text is taken
// as it is.
func inline(s string) string {
	var b strings.Builder
	for {
		loc := codeSpan.FindStringSubmatchIndex(s)
		if loc == nil {
			b.WriteString(emphasis(s))
			return b.String()
		}
		b.WriteString(emphasis(s[:loc[0]]))
		b.WriteString("<code>" + html.EscapeString(s[loc[2]:loc[3]]) + "</code>")
		s = s[loc[1]:]
	}
}

func emphasis(s string) string {
	var b strings.Builder
	for {
		loc := link.FindStringSubmatchIndex(s)
		if loc == nil {
			b.WriteString(marks(html.EscapeString(s)))
			return b.String()
		}
		b.WriteString(marks(html.EscapeString(s[:loc[0]])))
		text, href := s[loc[2]:loc[3]], s[loc[4]:loc[5]]
		b.WriteString(`<a href="` + html.EscapeString(href) + `">` + marks(html.EscapeString(text)) + "</a>")
		s = s[loc[1]:]
	}
}

// marks renders bold, italic and strikethrough in escaped text, whose
// markers escaping leaves as they are.
func marks(s string) string {
	s = bold.ReplaceAllStringFunc(s, func(m string) string {
		g := bold.FindStringSubmatch(m)
		return "<b>" + g[1] + g[2] + "</b>"
	})
	s = italic.ReplaceAllStringFunc(s, func(m string) string {
		g := italic.FindStringSubmatch(m)
		if g[2] != "" {
			return g[1] + "<i>" + g[2] + "</i>" + g[3]
		}
		return g[4] + "<i>" + g[5] + "</i>" + g[6]
	})
	return strike.ReplaceAllString(s, "<s>$1</s>")
}
