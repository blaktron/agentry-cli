package messenger

import (
	"strings"
	"testing"
)

func TestSplitRendersTelegramsHTML(t *testing.T) {
	parts, cut := SplitAs(FormatHTML, "# Tomorrow\n\n- **9:00** stand-up with `ops`\n- [the doc](https://example.com/a?b=1&c=2)\n\nSee *you* then, a_b_c & <x>.", 4096)
	if cut || len(parts) != 1 {
		t.Fatalf("parts = %d, cut = %v", len(parts), cut)
	}
	want := "<b>Tomorrow</b>\n\n• <b>9:00</b> stand-up with <code>ops</code>\n• <a href=\"https://example.com/a?b=1&amp;c=2\">the doc</a>\n\nSee <i>you</i> then, a_b_c &amp; &lt;x&gt;."
	if parts[0].Rich != want {
		t.Fatalf("HTML =\n%s\nwant\n%s", parts[0].Rich, want)
	}
}

func TestSplitKeepsCodeBlocksAsTheyAre(t *testing.T) {
	parts, _ := SplitAs(FormatHTML, "```go\nif a < b && **c** {\n}\n```", 4096)
	if want := "<pre><code class=\"language-go\">if a &lt; b &amp;&amp; **c** {\n}</code></pre>"; parts[0].Rich != want {
		t.Fatalf("HTML = %s", parts[0].Rich)
	}
	if parts[0].Plain != "if a < b && **c** {\n}" {
		t.Fatalf("Plain = %q", parts[0].Plain)
	}
}

func TestSplitBoundsEveryPartAndTheirCount(t *testing.T) {
	var b strings.Builder
	for i := range 400 {
		b.WriteString("Paragraph ")
		b.WriteString(strings.Repeat("word ", i%40))
		b.WriteString("& end.\n\n")
	}
	b.WriteString(strings.Repeat("x", 9000)) // one line longer than two messages
	const limit = 1000
	parts, cut := SplitAs(FormatHTML, b.String(), limit)
	if !cut || len(parts) != MaxParts {
		t.Fatalf("parts = %d, cut = %v; want %d and cut", len(parts), cut, MaxParts)
	}
	for i, p := range parts {
		if n := units(p.Rich); n > limit {
			t.Errorf("part %d is %d long, over %d", i, n, limit)
		}
	}
	if !strings.HasSuffix(parts[len(parts)-1].Plain, RestInAgentry) {
		t.Fatalf("the last part does not say the rest is in Agentry")
	}
}

func TestSplitCutsALongLineWithoutLosingIt(t *testing.T) {
	line := strings.Repeat("é", 2500)
	parts, cut := SplitAs(FormatHTML, line, 1000)
	if cut {
		t.Fatal("cut")
	}
	var got strings.Builder
	for _, p := range parts {
		if units(p.Rich) > 1000 {
			t.Fatalf("a part is %d long", units(p.Rich))
		}
		got.WriteString(p.Plain)
	}
	if got.String() != line {
		t.Fatal("the pieces do not make the line")
	}
}

func TestSplitAsWritesEachServicesFormat(t *testing.T) {
	md := "# Plan\n\n**bold** and *it* and ~~gone~~ with `a<b` and [docs](https://example.com/a)\n\n> quoted & kept"
	slackWant := "*Plan*\n\n*bold* and _it_ and ~gone~ with `a&lt;b` and <https://example.com/a|docs>\n\n> quoted &amp; kept"
	if parts, _ := SplitAs(FormatSlack, md, 4000); len(parts) != 1 || parts[0].Rich != slackWant {
		t.Fatalf("Slack =\n%s\nwant\n%s", parts[0].Rich, slackWant)
	}
	if parts, _ := SplitAs(FormatMarkdown, md+"\n\n---", 2000); len(parts) != 1 || parts[0].Rich != md+"\n\n———" {
		t.Fatalf("Discord = %q, want the Markdown as written, but for the rule", parts[0].Rich)
	}
	if parts, _ := SplitAs(FormatMarkdown, "```go\nx := \"```\"\n```", 2000); parts[0].Rich != "```go\nx := \"``\u200b`\"\n```" {
		t.Fatalf("Discord's code block = %q, want a fence inside the code broken", parts[0].Rich)
	}
}

func TestSplitAsBoundsEveryFormatsParts(t *testing.T) {
	long := strings.Repeat("word **and** <tag> & more\n\n", 2000)
	for _, c := range []struct {
		f     Format
		limit int
	}{{FormatSlack, 4000}, {FormatMarkdown, 2000}, {FormatHTML, 8000}} {
		parts, cut := SplitAs(c.f, long, c.limit)
		if !cut || len(parts) != MaxParts {
			t.Fatalf("format %d: %d parts, cut %v, want %d and cut", c.f, len(parts), cut, MaxParts)
		}
		for i, p := range parts {
			if units(p.Rich) > c.limit {
				t.Fatalf("format %d part %d is %d long, over %d", c.f, i, units(p.Rich), c.limit)
			}
		}
	}
}

func TestHTMLLineBreaksLeavesPreformattedBlocks(t *testing.T) {
	got := HTMLLineBreaks("a\nb<pre>c\nd</pre>e\n<pre><code class=\"language-go\">f\ng</code></pre>")
	if want := "a<br>b<pre>c\nd</pre>e<br><pre><code class=\"language-go\">f\ng</code></pre>"; got != want {
		t.Fatalf("HTMLLineBreaks = %q, want %q", got, want)
	}
}
