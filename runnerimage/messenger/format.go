package messenger

import (
	"html"
	"regexp"
	"slices"
	"strings"
	"unicode/utf16"
)

// An answer is Markdown, as a model writes it. Each service is sent it in
// its own format (Format), every character that format gives a meaning
// escaped but the marks it renders (bold, italic, strikethrough, code,
// preformatted blocks, links and quotes), and split into messages of at
// most its length. The text reaching here is already masked whole by the
// supervisor, so no cut ever leaves part of a secret on one side of it.

// Format is how a service is written to.
type Format int

const (
	// FormatHTML is the subset of HTML Telegram's Bot API parses, which a
	// Matrix client renders too.
	FormatHTML Format = iota
	// FormatMarkdown is Discord's Markdown, which takes a model's own
	// marks as they are.
	FormatMarkdown
	// FormatSlack is Slack's mrkdwn: *bold*, _italic_, ~strike~ and
	// <url|text> links, with &, < and > escaped.
	FormatSlack
)

// Part is one message of an answer: in the service's format, and the same
// text plain, sent instead where the service cannot parse the format and
// beside it where the service takes both.
type Part struct {
	Rich  string
	Plain string
}

// SplitAs is an answer's messages in format f for a service whose messages
// hold at most limit characters: at most MaxParts of them, the last ending
// with RestInAgentry when the answer is longer, and cut says it was.
func SplitAs(f Format, markdown string, limit int) (parts []Part, cut bool) {
	reserve := units(RestInAgentry) + 2
	if limit < reserve+16 {
		limit = reserve + 16
	}
	// No block is longer than the last part's room, so every block fits a
	// part of its own.
	var blocks []block
	for _, b := range blocksOf(f, markdown) {
		if units(b.rich) <= limit-reserve {
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
		if n := units(b.rich); len(cur) == 0 || curLen+sep+n <= room {
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
		cur, curLen = []block{b}, units(b.rich)
	}
	if len(cur) > 0 {
		parts = append(parts, joinBlocks(cur))
	}
	if cut {
		last := &parts[len(parts)-1]
		last.Rich += "\n\n" + escapeFor(f, RestInAgentry)
		last.Plain += "\n\n" + RestInAgentry
	}
	return parts, cut
}

// units is the length of s as the service counts it: in UTF-16 code units,
// the most a character may be counted as.
func units(s string) int { return len(utf16.Encode([]rune(s))) }

// block is one paragraph, list, quote or code block of an answer, rendered.
type block struct {
	rich, plain string
	f           Format
	code        bool
	lang        string
	raw         string // a code block's code, a paragraph's Markdown
}

func joinBlocks(bs []block) Part {
	h := make([]string, 0, len(bs))
	p := make([]string, 0, len(bs))
	for _, b := range bs {
		if b.rich == "" && b.plain == "" {
			continue
		}
		h = append(h, b.rich)
		p = append(p, b.plain)
	}
	return Part{Rich: strings.Join(h, "\n\n"), Plain: strings.Join(p, "\n\n")}
}

// pieces splits a block longer than limit into blocks of at most limit: a
// code block by its lines, each piece its own preformatted block; a
// paragraph by its lines; and a line longer than a message by its runes.
func (b block) pieces(limit int) []block {
	render := func(raw string) block {
		if b.code {
			return codeBlock(b.f, raw, b.lang)
		}
		return textBlock(b.f, raw)
	}
	fits := func(raw string) bool { return units(render(raw).rich) <= limit }
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
func blocksOf(f Format, md string) []block {
	md = strings.ReplaceAll(strings.ReplaceAll(md, "\r\n", "\n"), "\r", "\n")
	var out []block
	var para []string
	endPara := func() {
		if len(para) > 0 {
			out = append(out, textBlock(f, strings.Join(para, "\n")))
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
			out = append(out, codeBlock(f, strings.Join(code, "\n"), m[2]))
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
func codeBlock(f Format, code, lang string) block {
	var h string
	switch f {
	case FormatMarkdown:
		// A fence inside the code would end the block early.
		h = "```" + lang + "\n" + strings.ReplaceAll(code, "```", "``\u200b`") + "\n```"
	case FormatSlack:
		h = "```\n" + slackEscape(strings.ReplaceAll(code, "```", "``\u200b`")) + "\n```"
	default:
		esc := html.EscapeString(code)
		h = "<pre>" + esc + "</pre>"
		if lang != "" {
			h = `<pre><code class="language-` + html.EscapeString(lang) + `">` + esc + "</code></pre>"
		}
	}
	return block{rich: h, plain: code, f: f, code: true, lang: lang, raw: code}
}

// escapeFor is text with every character f gives a meaning escaped.
func escapeFor(f Format, s string) string {
	switch f {
	case FormatMarkdown:
		return s
	case FormatSlack:
		return slackEscape(s)
	}
	return html.EscapeString(s)
}

var slackEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// slackEscape escapes the three characters mrkdwn reads as its own.
func slackEscape(s string) string { return slackEscaper.Replace(s) }

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
func textBlock(f Format, md string) block {
	lines := strings.Split(md, "\n")
	if f == FormatMarkdown {
		// Discord renders a model's Markdown itself, but for a rule.
		for i, l := range lines {
			if rule.MatchString(l) {
				lines[i] = "———"
			}
		}
		out := strings.Join(lines, "\n")
		return block{rich: out, plain: out, f: f, raw: md}
	}
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
			if f == FormatSlack {
				h = append(h, "> "+inline(f, l))
			} else {
				h = append(h, inline(f, l))
			}
			p = append(p, l)
			continue
		case rule.MatchString(l):
			h = append(h, "———")
			p = append(p, "———")
			continue
		}
		if m := heading.FindStringSubmatch(l); m != nil {
			if f == FormatSlack {
				h = append(h, "*"+inline(f, m[1])+"*")
			} else {
				h = append(h, "<b>"+inline(f, m[1])+"</b>")
			}
			p = append(p, m[1])
			continue
		}
		if m := bullet.FindStringSubmatch(l); m != nil {
			h = append(h, m[1]+"• "+inline(f, m[2]))
			p = append(p, m[1]+"• "+m[2])
			continue
		}
		h = append(h, inline(f, l))
		p = append(p, l)
	}
	out := strings.Join(h, "\n")
	if quoted && f == FormatHTML {
		out = "<blockquote>" + out + "</blockquote>"
	}
	return block{rich: out, plain: strings.Join(p, "\n"), f: f, raw: md}
}

// inline renders a line's code spans, links, bold, italic and
// strikethrough in format f; everything else is escaped. A code span's text
// is taken as it is.
func inline(f Format, s string) string {
	var b strings.Builder
	for {
		loc := codeSpan.FindStringSubmatchIndex(s)
		if loc == nil {
			b.WriteString(emphasis(f, s))
			return b.String()
		}
		b.WriteString(emphasis(f, s[:loc[0]]))
		if f == FormatSlack {
			b.WriteString("`" + slackEscape(s[loc[2]:loc[3]]) + "`")
		} else {
			b.WriteString("<code>" + html.EscapeString(s[loc[2]:loc[3]]) + "</code>")
		}
		s = s[loc[1]:]
	}
}

func emphasis(f Format, s string) string {
	var b strings.Builder
	for {
		loc := link.FindStringSubmatchIndex(s)
		if loc == nil {
			b.WriteString(marks(f, escapeFor(f, s)))
			return b.String()
		}
		b.WriteString(marks(f, escapeFor(f, s[:loc[0]])))
		text, href := s[loc[2]:loc[3]], s[loc[4]:loc[5]]
		if f == FormatSlack {
			// A link's address cannot hold the characters mrkdwn reads,
			// and its text cannot hold the bar that ends the address.
			b.WriteString("<" + slackEscape(href) + "|" + marks(f, slackEscape(strings.ReplaceAll(text, "|", "¦"))) + ">")
		} else {
			b.WriteString(`<a href="` + html.EscapeString(href) + `">` + marks(f, html.EscapeString(text)) + "</a>")
		}
		s = s[loc[1]:]
	}
}

// marks renders bold, italic and strikethrough in escaped text, whose
// markers escaping leaves as they are. Slack's italic comes first, so the
// bold it writes as single asterisks is not read as italic again.
func marks(f Format, s string) string {
	if f == FormatSlack {
		s = italic.ReplaceAllStringFunc(s, func(m string) string {
			g := italic.FindStringSubmatch(m)
			if g[2] != "" {
				return g[1] + "_" + g[2] + "_" + g[3]
			}
			return g[4] + "_" + g[5] + "_" + g[6]
		})
		s = bold.ReplaceAllStringFunc(s, func(m string) string {
			g := bold.FindStringSubmatch(m)
			return "*" + g[1] + g[2] + "*"
		})
		return strike.ReplaceAllString(s, "~$1~")
	}
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

// HTMLLineBreaks is FormatHTML for a Matrix client, which collapses a line
// break outside a preformatted block as a browser does: each such break
// becomes <br>.
func HTMLLineBreaks(h string) string {
	var b strings.Builder
	for {
		i := strings.Index(h, "<pre>")
		j := strings.Index(h, "<pre><code")
		if j >= 0 && (i < 0 || j < i) {
			i = j
		}
		if i < 0 {
			b.WriteString(strings.ReplaceAll(h, "\n", "<br>"))
			return b.String()
		}
		end := strings.Index(h[i:], "</pre>")
		if end < 0 {
			b.WriteString(strings.ReplaceAll(h[:i], "\n", "<br>") + h[i:])
			return b.String()
		}
		end += i + len("</pre>")
		b.WriteString(strings.ReplaceAll(h[:i], "\n", "<br>"))
		b.WriteString(h[i:end])
		h = h[end:]
	}
}
