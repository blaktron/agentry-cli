package toolsrv

import (
	"html"
	"net/url"
	"strings"
)

// HTMLText is the text of an HTML page as web_read gives it: what a reader
// sees, with the markup stripped. Scripts,
// styles, templates, embedded documents and elements marked hidden
// (the hidden attribute, aria-hidden="true", an inline display:none or
// visibility:hidden) are left out; headings, list items, paragraphs and
// line breaks become lines; a link keeps its absolute http or https address
// after its text, so the Drafter can follow it. Entities are decoded.
//
// It is a reader's view, not a parser's: malformed markup degrades to less
// text, never to markup. What is left is still read by AI Review before a
// model sees it; leaving hidden text out only means the model is not given
// what a person would not see either.
func HTMLText(body []byte, base *url.URL) string {
	s := strings.ToValidUTF8(string(body), "\uFFFD")
	lower := asciiLower(s)
	var out strings.Builder
	var text strings.Builder
	pre := 0
	var links []string // the hrefs of the open <a> elements
	flush := func() {
		if text.Len() == 0 {
			return
		}
		t := html.UnescapeString(text.String())
		if pre == 0 {
			t = strings.Map(func(r rune) rune {
				if r == '\n' || r == '\r' || r == '\t' || r == '\f' {
					return ' '
				}
				return r
			}, t)
		}
		out.WriteString(t)
		text.Reset()
	}
	for i := 0; i < len(s); {
		if s[i] != '<' {
			j := strings.IndexByte(s[i:], '<')
			if j < 0 {
				j = len(s) - i
			}
			text.WriteString(s[i : i+j])
			i += j
			continue
		}
		switch {
		case strings.HasPrefix(s[i:], "<!--"):
			flush()
			if j := strings.Index(s[i+4:], "-->"); j >= 0 {
				i += 4 + j + 3
			} else {
				i = len(s)
			}
			continue
		case strings.HasPrefix(s[i:], "<!") || strings.HasPrefix(s[i:], "<?"):
			flush()
			i = skipPast(s, i, '>')
			continue
		}
		name, closing, attrs, end, ok := parseTag(s, i)
		if !ok {
			// A '<' that opens no tag is text.
			text.WriteByte('<')
			i++
			continue
		}
		flush()
		i = end
		if closing {
			switch {
			case name == "a" && len(links) > 0:
				if href := links[len(links)-1]; href != "" {
					out.WriteString(" (" + href + ")")
				}
				links = links[:len(links)-1]
			case name == "pre":
				if pre > 0 {
					pre--
				}
				out.WriteByte('\n')
			case blockTags[name]:
				out.WriteByte('\n')
			}
			continue
		}
		if skipTags[name] || hiddenElement(attrs) {
			if !voidTags[name] && !strings.HasSuffix(s[:end], "/>") {
				i = skipElement(s, lower, i, name)
			}
			continue
		}
		switch {
		case name == "br":
			out.WriteByte('\n')
		case name == "hr":
			out.WriteString("\n---\n")
		case name == "li":
			out.WriteString("\n- ")
		case len(name) == 2 && name[0] == 'h' && name[1] >= '1' && name[1] <= '6':
			out.WriteString("\n\n" + strings.Repeat("#", int(name[1]-'0')) + " ")
		case name == "pre":
			pre++
			out.WriteByte('\n')
		case name == "a":
			links = append(links, linkTarget(attrs["href"], base))
		case name == "td" || name == "th":
			out.WriteString(" ")
		case blockTags[name]:
			out.WriteByte('\n')
		}
	}
	flush()
	return tidyLines(out.String())
}

// blockTags start and end a line.
var blockTags = map[string]bool{
	"p": true, "div": true, "section": true, "article": true, "header": true, "footer": true, "main": true,
	"nav": true, "aside": true, "ul": true, "ol": true, "dl": true, "dt": true, "dd": true, "table": true,
	"tr": true, "blockquote": true, "figure": true, "figcaption": true, "form": true, "fieldset": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "title": true,
	"details": true, "summary": true, "address": true, "caption": true, "thead": true, "tbody": true,
}

// skipTags are elements whose content a reader does not see as text.
var skipTags = map[string]bool{
	"script": true, "style": true, "noscript": true, "template": true, "svg": true, "math": true,
	"iframe": true, "object": true, "embed": true, "canvas": true, "select": true,
}

// voidTags have no closing tag.
var voidTags = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true, "img": true, "input": true,
	"link": true, "meta": true, "param": true, "source": true, "track": true, "wbr": true,
}

// parseTag reads the tag starting at s[i] == '<': its lower-cased name,
// whether it closes, its attributes (names lower-cased, values unquoted and
// decoded) and the index just past its '>'.
func parseTag(s string, i int) (name string, closing bool, attrs map[string]string, end int, ok bool) {
	j := i + 1
	if j < len(s) && s[j] == '/' {
		closing = true
		j++
	}
	start := j
	for j < len(s) && (isAlnum(s[j]) || s[j] == '-' || s[j] == ':') {
		j++
	}
	if j == start || !isAlpha(s[start]) {
		return "", false, nil, 0, false
	}
	name = strings.ToLower(s[start:j])
	attrs = map[string]string{}
	for j < len(s) {
		for j < len(s) && isSpace(s[j]) {
			j++
		}
		if j >= len(s) {
			break
		}
		if s[j] == '>' {
			return name, closing, attrs, j + 1, true
		}
		if s[j] == '/' {
			j++
			continue
		}
		ns := j
		for j < len(s) && !isSpace(s[j]) && s[j] != '=' && s[j] != '>' && s[j] != '/' {
			j++
		}
		an := strings.ToLower(s[ns:j])
		for j < len(s) && isSpace(s[j]) {
			j++
		}
		val := ""
		if j < len(s) && s[j] == '=' {
			j++
			for j < len(s) && isSpace(s[j]) {
				j++
			}
			if j < len(s) && (s[j] == '"' || s[j] == '\'') {
				q := s[j]
				k := strings.IndexByte(s[j+1:], q)
				if k < 0 {
					return "", false, nil, 0, false
				}
				val = s[j+1 : j+1+k]
				j += k + 2
			} else {
				vs := j
				for j < len(s) && !isSpace(s[j]) && s[j] != '>' {
					j++
				}
				val = s[vs:j]
			}
		}
		if an != "" {
			attrs[an] = html.UnescapeString(val)
		}
	}
	return "", false, nil, 0, false
}

// hiddenElement reports whether an element's own attributes hide it.
func hiddenElement(attrs map[string]string) bool {
	if _, ok := attrs["hidden"]; ok {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(attrs["aria-hidden"]), "true") {
		return true
	}
	style := strings.ToLower(strings.ReplaceAll(attrs["style"], " ", ""))
	return strings.Contains(style, "display:none") || strings.Contains(style, "visibility:hidden")
}

// skipElement returns the index just past the element named name that opens
// before i, counting nested elements of the same name; an element never
// closed runs to the end of the page. lower is s with ASCII lower-cased, the
// same length as s.
func skipElement(s, lower string, i int, name string) int {
	depth := 1
	raw := name == "script" || name == "style"
	for i < len(s) {
		k := strings.Index(lower[i:], "<")
		if k < 0 {
			return len(s)
		}
		i += k
		switch {
		case strings.HasPrefix(lower[i:], "</"+name) && tagBoundary(lower, i+2+len(name)):
			depth--
			i = skipPast(s, i, '>')
			if depth == 0 {
				return i
			}
			continue
		case !raw && strings.HasPrefix(lower[i:], "<"+name) && tagBoundary(lower, i+1+len(name)):
			depth++
		}
		i++
	}
	return len(s)
}

func tagBoundary(s string, j int) bool {
	return j >= len(s) || isSpace(s[j]) || s[j] == '>' || s[j] == '/'
}

func skipPast(s string, i int, c byte) int {
	if k := strings.IndexByte(s[i:], c); k >= 0 {
		return i + k + 1
	}
	return len(s)
}

// linkTarget is an href as an absolute http or https address, or "".
func linkTarget(href string, base *url.URL) string {
	href = strings.TrimSpace(href)
	if href == "" || strings.HasPrefix(href, "#") {
		return ""
	}
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return ""
	}
	u.Fragment = ""
	return u.String()
}

// tidyLines folds each line's white space, trims it, and keeps at most one
// blank line in a row.
func tidyLines(s string) string {
	var b strings.Builder
	blank := 0
	for _, line := range strings.Split(s, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			blank++
			continue
		}
		if b.Len() > 0 {
			if blank > 0 {
				b.WriteString("\n\n")
			} else {
				b.WriteByte('\n')
			}
		}
		blank = 0
		b.WriteString(line)
	}
	return b.String()
}

// asciiLower lower-cases A to Z only, so every index into it is an index
// into s (strings.ToLower can change a string's length).
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

func isAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isAlnum(c byte) bool { return isAlpha(c) || (c >= '0' && c <= '9') }
func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' }
