// The export_pdf template (plan scratch-and-pdf-reports.md §5, D23-D27;
// agentry-cli#730). The tools server writes it into a fresh render root
// beside the agent's Markdown (report.md), the run's theme (theme.json) and
// what the server worked out (meta.json: the title, the author line and the
// images it copied), and Typst renders it with the bundled fonts and the
// vendored cmarker only. Nothing in it comes from the model but report.md,
// which is read as Markdown and never as Typst:
//
// - raw-typst is false, so a `<!--raw-typst …-->` block is not run; cmarker
//   defaults it to true.
// - Every link goes through safe-link: http, https and mailto stay links,
//   anything else (javascript:, file:, an anchor) is its text.
// - Every image goes through safe-image: only a source the server copied
//   into the root (meta.images) is drawn; any other, a remote one, an SVG
//   in raw HTML, is its alt text.
// - The theme is data; its strings are drawn as text.
// - Heading levels never skip (below), so that PDF/UA-1 can hold.
#import "@preview/cmarker:0.1.10"

#let theme = json("theme.json")
#let meta = json("meta.json")

// Colour emoji straight after the text font: Typst takes each character
// from the first family that has it, and the CJK and symbol fonts draw some
// emoji (U+26A0, U+1F512) in monochrome. The text font covers the digits and
// marks the emoji font also has, so those never fall through to it.
#let fallback = (
  "Noto Color Emoji",
  "Noto Sans SC", "Noto Sans KR", "Noto Sans Arabic", "Noto Sans Hebrew",
  "Noto Sans Thai", "Noto Sans Devanagari", "Noto Sans Bengali",
  "Noto Sans Tamil", "Noto Sans Telugu", "Noto Sans Gujarati",
  "Noto Sans Gurmukhi", "Noto Sans Kannada", "Noto Sans Malayalam",
  "Noto Sans Oriya", "Noto Sans Sinhala", "Noto Sans Symbols",
  "Noto Sans Symbols 2", "Noto Sans Math",
)
#let fonts(primary) = (primary, ..fallback)
#let aligned = (left: left, center: center, right: right)
#let body-size = theme.sizes.bodyPt * 1pt

#set document(title: meta.title, author: if meta.author == "" { () } else { meta.author })
#set page(
  paper: theme.page.paper,
  margin: theme.page.marginMm * 1mm,
  header: if theme.header.text != "" {
    align(aligned.at(theme.header.align), text(size: 8pt, fill: rgb(theme.colours.text), theme.header.text))
  },
  footer: context {
    let numbers = if theme.footer.pageNumbers == "n" {
      counter(page).display("1")
    } else if theme.footer.pageNumbers == "n of N" {
      counter(page).display("1 of 1", both: true)
    }
    let parts = (theme.footer.text, numbers).filter(p => p != none and p != "")
    if parts.len() > 0 {
      align(aligned.at(theme.footer.align), text(size: 8pt, fill: rgb(theme.colours.text), parts.join([ · ])))
    }
  },
)
#set text(font: fonts(theme.fonts.body), size: body-size, fill: rgb(theme.colours.text))
#set par(justify: false)
#show heading: set text(font: fonts(theme.fonts.headings), fill: rgb(theme.colours.accent))
#show heading: it => {
  set text(size: body-size * theme.sizes.headingScale.at(it.level - 1, default: 1.0))
  it
}
#show raw: set text(font: fonts(theme.fonts.code), size: theme.sizes.codePt * 1pt)
#show raw.where(block: true): block.with(fill: rgb(theme.colours.codeBackground), inset: 6pt, radius: 2pt, width: 100%)
#show link: set text(fill: rgb(theme.colours.link))
#set table(
  inset: 4pt,
  stroke: 0.4pt + rgb(theme.colours.rules),
  fill: (x, y) => if y == 0 {
    rgb(theme.colours.tableHeader)
  } else if theme.tables.striped and calc.even(y) {
    rgb(theme.colours.codeBackground)
  },
)
#show table.cell.where(y: 0): set text(weight: if theme.tables.headerBold { "bold" } else { "regular" })
// A long token (a hash, a URL) may break anywhere rather than run off the
// page.
#show regex("\\S{36,}"): it => it.text.clusters().join(sym.zws)

#let schemes = ("http://", "https://", "mailto:")
#let safe-link(dest, ..args) = {
  let body = args.pos().at(0, default: none)
  if type(dest) == str and schemes.any(s => lower(dest).starts-with(s)) {
    link(dest, ..args)
  } else if body != none {
    body
  } else if type(dest) == str {
    dest
  }
}

#let safe-image(src, alt: none, ..args) = {
  let alt = if type(alt) == str and alt.trim() != "" { alt } else { none }
  if type(src) == str and src in meta.images {
    let copied = meta.images.at(src)
    let sizing = args.named()
    // PDF/UA-1: every image carries a text alternative; the file's name
    // stands in for one the Markdown did not give.
    figure(
      image(copied.path, alt: if alt == none { copied.name } else { alt }, width: sizing.at("width", default: auto), height: sizing.at("height", default: auto)),
      caption: alt,
    )
  } else if alt != none {
    emph(alt)
  } else if type(src) == str {
    emph(src)
  }
}

// PDF/UA-1 refuses a document whose first heading is not level 1 or whose
// levels skip (# then ###), which Markdown allows and agents write. The
// server renumbers the Markdown's headings before the render (scanMarkdown
// in markdown.go); a heading in raw HTML is drawn as a bold paragraph instead, so
// that none escapes the renumbering.
#let html-heading = (attrs, body) => block(strong(body))

#cmarker.render(
  read("report.md"),
  raw-typst: false,
  set-document-title: false,
  html: (h1: html-heading, h2: html-heading, h3: html-heading, h4: html-heading, h5: html-heading, h6: html-heading),
  scope: (link: safe-link, image: safe-image),
)
