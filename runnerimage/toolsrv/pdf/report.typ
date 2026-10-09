// The export_pdf template (plan scratch-and-pdf-reports.md §5, D23-D27,
// agentry-cli#730; theme document version 2 since report-designs.md §4,
// agentry-cli#784, DR-83). The tools server writes it into a fresh render
// root beside the agent's Markdown (report.md), the run's report design
// (design.json, pkg/reportdesign's canonical document) and what the server
// worked out (meta.json: the form, the title, the author line, the images it
// copied, the accent's tint, whether a cover is drawn, a memo's header), and
// Typst renders it with the bundled fonts and the vendored cmarker only.
// Nothing in it comes from the model but report.md, which is read as
// Markdown and never as Typst:
//
// - raw-typst is false, so a `<!--raw-typst …-->` block is not run; cmarker
//   defaults it to true.
// - Every link goes through safe-link: http, https and mailto stay links,
//   anything else (javascript:, file:, an anchor) is its text.
// - Every image goes through safe-image: only a source the server copied
//   into the root (meta.images) is drawn; any other, a remote one, an SVG
//   in raw HTML, is its alt text.
// - The design is data; its strings (the footer line) and the server's
//   (the title, a memo's lines) are drawn as text.
// - Heading levels never skip (below), so that PDF/UA-1 can hold.
//
// The look is the design's and nothing else (plan D2): one accent and its
// tint, body text near-black, a title block, a heading treatment, tinted
// tables and quote callouts, coloured rules. The accent touches text only
// where pkg/reportdesign's AccentOnText says it may (DEV-230), which its
// contrast rule was checked for: a band title block sets the title in white
// on the accent, and smallcaps and numbered headings set the top two
// section levels in the accent. Every other choice keeps text near-black and
// puts the accent on rules, bars, tints and callout edges alone. A link is
// near-black, underlined in the accent.
//
// The form is the agent's (D1, D15): report (a title block, and a cover page
// of its own for a titled report past a length, which the server decides),
// letter (the title as a letterhead, the lines kept, no page numbers on a
// one-page letter) or memo (a To / From / Date / Subject block under the
// title, no cover).
#import "@preview/cmarker:0.1.10"

#let design = json("design.json")
#let meta = json("meta.json")

#let ink = rgb("#1a1a1a")
#let muted = rgb("#5b6068")
#let hairline = rgb("#c9cdd3")
#let code-fill = rgb("#f3f4f6")
#let accent = rgb(design.accent)
#let tint = rgb(meta.tint)
#let form = meta.form

// Each design family with Noto behind it for the scripts it lacks, then
// colour emoji: Typst takes each character from the first family that has
// it, and the CJK and symbol fonts draw some emoji (U+26A0, U+1F512) in
// monochrome. Noto Sans and Noto Serif cover the digits and marks the emoji
// font also has, so those never fall through to it.
#let scripts = (
  "Noto Color Emoji",
  "Noto Sans SC", "Noto Sans KR", "Noto Sans Arabic", "Noto Sans Hebrew",
  "Noto Sans Thai", "Noto Sans Devanagari", "Noto Sans Bengali",
  "Noto Sans Tamil", "Noto Sans Telugu", "Noto Sans Gujarati",
  "Noto Sans Gurmukhi", "Noto Sans Kannada", "Noto Sans Malayalam",
  "Noto Sans Oriya", "Noto Sans Sinhala", "Noto Sans Symbols",
  "Noto Sans Symbols 2", "Noto Sans Math",
)
#let fonts(family, noto) = (family, noto, ..scripts)
#let body-font = fonts(design.fonts.body, meta.notoBody)
#let heading-font = fonts(design.fonts.headings, meta.notoHeadings)
#let code-font = fonts(design.fonts.code, "Noto Sans Mono")

#let body-size = if form == "letter" { 11pt } else { 10.5pt }
#let margin = if form == "letter" { 25mm } else { 22mm }

// The section levels a heading treatment and the numbering apply to: the
// top two below the title. A report whose first block is its title has its
// title drawn here as level 1, so its sections are levels 2 and 3.
#let top = if meta.titleBlock { 2 } else { 1 }
#let treated(level) = level == top or level == top + 1

// --- The footer: the design's line and page numbers, over an accent rule.
#let page-numbers(total) = {
  let style = design.footer.pageNumbers
  if style == "none" or (form == "letter" and total <= 1) {
    none
  } else if style == "n" {
    counter(page).display("1")
  } else {
    counter(page).display("1 of 1", both: true)
  }
}

#set document(title: meta.title, author: if meta.author == "" { () } else { meta.author })
#set page(
  paper: if design.paper == "letter" { "us-letter" } else { "a4" },
  margin: margin,
  header: context {
    if form != "letter" and counter(page).get().first() > 1 {
      line(length: 100%, stroke: 0.5pt + accent)
    }
  },
  footer: context {
    let numbers = page-numbers(counter(page).final().first())
    let line-text = design.footer.text
    if numbers != none or line-text != "" {
      set text(size: 8pt, fill: muted)
      line(length: 100%, stroke: 0.5pt + accent)
      v(-0.4em)
      grid(columns: (1fr, auto), align: (left, right), line-text, if numbers == none { [] } else { numbers })
    }
  },
)
#set text(font: body-font, size: body-size, weight: "regular", fill: ink)
#set par(justify: false, leading: 0.68em)

// --- Headings. Level 1 is the largest; a treated level takes the design's
// treatment. Only the top two section levels are numbered, and only under
// "numbered"; the numbers count from the top section level.
#show heading: set text(font: heading-font, weight: "bold", fill: ink)
#show heading: it => {
  let scale = (1.6, 1.3, 1.12, 1.0, 1.0, 1.0).at(it.level - 1, default: 1.0)
  set text(size: body-size * scale)
  set block(above: 1.5em, below: 0.8em)
  it
}
#let section-numbering(..n) = n.pos().slice(top - 1).map(str).join(".")
#show heading: it => {
  if not treated(it.level) {
    return it
  }
  let style = design.headings
  if style == "rule" {
    block(below: 0.9em, {
      it
      v(-0.55em)
      line(length: 100%, stroke: (if it.level == top { 1pt } else { 0.5pt }) + accent)
    })
  } else if style == "bar" {
    block(below: 0.8em, {
      v(0.3em)
      box(width: 2.2em, height: 3pt, fill: accent)
      v(-0.6em)
      it
    })
  } else if style == "smallcaps" {
    set text(fill: accent, tracking: 0.04em)
    smallcaps(it)
  } else if style == "numbered" {
    set text(fill: accent)
    it
  } else {
    it
  }
}
#show heading.where(level: top): set heading(numbering: if design.headings == "numbered" { section-numbering } else { none })
#show heading.where(level: top + 1): set heading(numbering: if design.headings == "numbered" { section-numbering } else { none })

// --- Body elements.
#show raw: set text(font: code-font, size: 9pt, weight: "regular")
#show raw.where(block: true): block.with(fill: code-fill, inset: 7pt, radius: 2pt, width: 100%)
#show link: it => underline(stroke: 0.6pt + accent, offset: 2pt, it)
#set table(
  inset: (x: 5pt, y: 4pt),
  stroke: (x, y) => (
    top: if y == 1 { 0.8pt + accent } else if y > 1 { 0.4pt + hairline },
    bottom: 0.4pt + hairline,
  ),
  fill: (x, y) => if y == 0 and design.tables.tinted {
    tint
  } else if y > 0 and design.tables.banded and calc.even(y) {
    tint
  },
)
#show table.cell.where(y: 0): set text(weight: "bold")
#show quote.where(block: true): it => {
  if design.quotes == "callout" {
    block(width: 100%, fill: tint, stroke: (left: 3pt + accent), inset: (x: 10pt, y: 8pt), it.body)
  } else {
    block(width: 100%, stroke: (left: 1pt + hairline), inset: (left: 10pt, y: 2pt), it.body)
  }
}
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

// --- The title block. The server takes a report's opening heading out of
// report.md (meta.titleBlock) and the template draws it here, as the level 1
// heading it was, never numbered, in the design's style: a band (white on
// the accent), a bar beside it, rules above and below, or none.
#let title-heading(size) = {
  set text(size: size)
  show heading: it => block(above: 0pt, below: 0pt, it.body)
  heading(level: 1, numbering: none, meta.title)
}
#let title-block(size, below: 1.4em) = {
  let style = design.titleBlock
  let title = title-heading(size)
  if style == "band" {
    block(width: 100%, fill: accent, inset: (x: 14pt, y: 16pt), below: below, {
      // A show-set rule, not a set rule: the document's heading rule sets
      // ink on the heading itself, and only a closer rule on the heading
      // overrides it. The core's contrast check assumes white here.
      show heading: set text(fill: white)
      title
    })
  } else if style == "bar" {
    block(width: 100%, stroke: (left: 4pt + accent), inset: (left: 12pt, y: 4pt), below: below, title)
  } else if style == "ruled" {
    block(width: 100%, below: below, {
      line(length: 100%, stroke: 1pt + accent)
      v(0.5em)
      title
      v(0.5em)
      line(length: 100%, stroke: 0.5pt + accent)
    })
  } else {
    block(width: 100%, below: below, title)
  }
}
#let title-size = if form == "letter" { body-size * 1.5 } else { body-size * 2 }

// --- The cover: a page of its own before a long titled report, holding the
// title block and the day; the pages after it are numbered from 1.
#if meta.cover {
  page(header: none, footer: none, {
    v(1fr)
    title-block(body-size * 2.4, below: 1.2em)
    text(fill: muted, meta.date)
    v(2fr)
  })
  counter(page).update(1)
} else if meta.titleBlock {
  title-block(title-size)
}

// --- A memo's header: the To / From / Cc / Date / Subject lines it opened
// with, as a block of their own over an accent rule.
#if form == "memo" and meta.memo.len() > 0 {
  block(width: 100%, below: 1.4em, {
    grid(
      columns: (auto, 1fr),
      column-gutter: 1.2em,
      row-gutter: 0.65em,
      ..meta.memo.map(f => (text(weight: "bold", f.label + ":"), f.value)).flatten(),
    )
    v(0.4em)
    line(length: 100%, stroke: 0.8pt + accent)
  })
}

#cmarker.render(
  read("report.md"),
  raw-typst: false,
  set-document-title: false,
  html: (h1: html-heading, h2: html-heading, h3: html-heading, h4: html-heading, h5: html-heading, h6: html-heading),
  scope: (link: safe-link, image: safe-image),
)
