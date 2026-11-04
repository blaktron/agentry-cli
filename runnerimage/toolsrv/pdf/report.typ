// The export_pdf template (theme document version 3). The tools server writes
// it into a fresh render root beside the agent's Markdown (report.md), the
// run's report design (design.json, pkg/reportdesign's canonical document
// without its images) and what the server worked out (meta.json: the form,
// the title and subtitle, the author line, the day, the images it copied,
// each colour's tint, the family names Typst lists for the design's faces,
// whether a cover is drawn and in which style, the cover image, the logo,
// a memo's header, and the header's and the footer's slots split into their
// literal text and their tokens), and Typst renders it with the bundled
// fonts and the vendored cmarker only. Nothing in it comes from the model
// but report.md, which is read as Markdown and never as Typst:
//
// - raw-typst is false, so a `<!--raw-typst …-->` block is not run; cmarker
//   defaults it to true.
// - Every link goes through safe-link: http, https and mailto stay links,
//   anything else (javascript:, file:, an anchor) is its text.
// - Every image goes through safe-image: only a source the server copied
//   into the root (meta.images) is drawn; any other, a remote one, an SVG
//   in raw HTML, is its alt text.
// - The design is data; its strings (the brand block, each header and
//   footer slot) and the server's (the title, a memo's lines) are drawn as
//   text. A slot's tokens were split out by the server: a part is literal
//   text, set as text, or the name of a token this template fills (the
//   page number, the page count, the current section); the static tokens
//   (the title, the author, the date, the brand, the confidentiality line)
//   were already put in as text. Nothing in a slot is ever evaluated.
// - Heading levels never skip (below), so that PDF/UA-1 can hold.
//
// The look is the design's and nothing else: an accent and an optional
// secondary colour, each with its tint, body text near-black, a title block,
// a heading treatment and scale, a margin preset, tinted tables and quote
// callouts, coloured rules, a running header in one of five styles, a
// footer of three slots, and a cover page in one of six styles. A colour
// touches text only where pkg/reportdesign's AccentOnText and
// SecondaryOnText say it may, which its contrast rule was checked for: a
// band title block and a band or plate cover set the title in white on the
// accent, a largeType cover sets the title in the accent, a split cover sets
// the brand in white on the secondary (the accent when there is none), and
// smallcaps and numbered headings set the top two section levels in the
// accent. Every other choice keeps text near-black and puts the colours on
// rules, bars, shapes, tints and callout edges alone. A link is near-black,
// underlined in the accent.
//
// The person's logo, when the design has one, is drawn from logo.png, which
// the server wrote from the logo's decoded pixels (never the bytes the
// design carried), sized here and never by the design: in the title block,
// on a cover whose facts include it, and small at the left of a logoLeft
// header. The cover image, when a plate cover draws it, is the server's
// re-encoding of the design's, from cover.png or cover.jpg.
//
// The form is the agent's: report (a title block; a cover page of its own
// when the design's cover says always, or says long and the report runs
// past the length the server decides; the header from page 2), letter (the
// title as a letterhead, the lines kept, no header, no page numbers on a
// one-page letter) or memo (a To / From / Date / Subject block under the
// title, no cover, the header on every page).
#import "@preview/cmarker:0.1.10"

#let design = json("design.json")
#let meta = json("meta.json")

// The server refuses an earlier document before the render; this is the
// template's own guard, so a stale copy can never draw one.
#if design.agentryTheme != 3 {
  panic("the report design is theme document version " + str(design.agentryTheme) + "; this template reads version 3")
}

#let ink = rgb("#1a1a1a")
#let muted = rgb("#5b6068")
#let hairline = rgb("#c9cdd3")
#let code-fill = rgb("#f3f4f6")
#let accent = rgb(design.accent)
#let tint = rgb(meta.tint)
#let has-secondary = design.secondary != ""
#let secondary = if has-secondary { rgb(design.secondary) } else { accent }
#let secondary-tint = if has-secondary { rgb(meta.secondaryTint) } else { tint }
#let form = meta.form

// Each design family with Noto behind it for the scripts it lacks, then
// colour emoji: Typst takes each character from the first family that has
// it, and the CJK and symbol fonts draw some emoji (U+26A0, U+1F512) in
// monochrome. Noto Sans and Noto Serif cover the digits and marks the emoji
// font also has, so those never fall through to it. The family names are
// the ones Typst lists for the image's files (meta.fonts), which for a
// variable font can carry its default optical size.
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
#let body-font = fonts(meta.fonts.body, meta.notoBody)
#let heading-font = fonts(meta.fonts.headings, meta.notoHeadings)
#let code-font = fonts(meta.fonts.code, "Noto Sans Mono")

#let body-size = if form == "letter" { 11pt } else { 10.5pt }

// The margin preset, a letter's a little wider in each.
#let margin = (narrow: 16mm, regular: 22mm, wide: 28mm).at(design.margins) + (if form == "letter" { 3mm } else { 0mm })

// The heading scale preset: how much larger than the body the top three
// heading levels are set.
#let heading-scales = (compact: (1.4, 1.2, 1.05), regular: (1.6, 1.3, 1.12), generous: (1.9, 1.45, 1.2)).at(design.headingScale)

// The page, for the cover's panels.
#let page-w = if design.paper == "letter" { 8.5in } else { 210mm }
#let page-h = if design.paper == "letter" { 11in } else { 297mm }

// The section levels a heading treatment and the numbering apply to: the
// top two below the title. A report whose first block is its title has its
// title drawn here as level 1, so its sections are levels 2 and 3.
#let top-level = if meta.titleBlock { 2 } else { 1 }
#let treated(level) = level == top-level or level == top-level + 1

// --- The slots. A slot comes from the server as parts: literal text, set
// as text, or a token this template fills. {section} is the current
// heading of the top section level: the last one begun before this page.
#let current-section() = {
  let before = query(selector(heading.where(level: top-level)).before(here()))
  if before.len() == 0 { none } else { before.last().body }
}
#let slot-content(parts) = context {
  for part in parts {
    if "text" in part {
      part.text
    } else if part.token == "page" {
      str(counter(page).get().first())
    } else if part.token == "pages" {
      str(counter(page).final().first())
    } else if part.token == "section" {
      let section = current-section()
      if section != none { section }
    }
  }
}
#let slot-filled(parts) = parts.len() > 0
#let any-slot(slots) = slots.any(slot-filled)

// --- The logo: at most `tall` high and three and a half times that wide,
// its own proportions kept, with a text alternative for PDF/UA-1. None when
// the design has no logo.
#let logo-image(tall) = if meta.logo == none { none } else {
  let ratio = meta.logo.width / meta.logo.height
  let wide = tall * 3.5
  let h = tall
  if tall * ratio > wide { h = wide / ratio }
  image(meta.logo.path, height: h, alt: "Logo")
}

// --- The running header: from page 2 on a report, every page on a memo,
// never on a letter. The style sets the look; the slots set the words.
#let header-drawn(page-number) = (form == "memo") or (form == "report" and page-number > 1)
#let slot-row(slots, left-strong: false) = grid(
  columns: (1fr, auto, 1fr),
  align: (left, center, right),
  column-gutter: 1em,
  if left-strong {
    set text(font: heading-font, weight: "bold", fill: ink, tracking: 0.03em)
    slot-content(slots.at(0))
  } else {
    slot-content(slots.at(0))
  },
  slot-content(slots.at(1)),
  slot-content(slots.at(2)),
)
#let header-block() = {
  let style = design.header.style
  let slots = meta.header.slots
  set text(size: 8pt, fill: muted)
  if style == "rule" {
    if any-slot(slots) {
      slot-row(slots)
      v(-0.4em)
    }
    line(length: 100%, stroke: 0.5pt + accent)
  } else if style == "brandLine" {
    slot-row(slots, left-strong: true)
    v(-0.4em)
    line(length: 100%, stroke: 0.5pt + hairline)
  } else if style == "titleAndPage" {
    slot-row(slots)
    v(-0.4em)
    line(length: 100%, stroke: 0.5pt + accent)
  } else if style == "logoLeft" {
    let logo = logo-image(6mm)
    grid(
      columns: (auto, 1fr, auto),
      align: (left + horizon, center + horizon, right + horizon),
      column-gutter: 1em,
      if logo == none { slot-content(slots.at(0)) } else { logo },
      slot-content(slots.at(1)),
      slot-content(slots.at(2)),
    )
    v(-0.4em)
    line(length: 100%, stroke: 0.5pt + hairline)
  }
}

// --- The footer: the three slots and the page numbers, over an accent
// rule; the numbers sit at the right, after the right slot's text.
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
#let footer-block() = context {
  let numbers = page-numbers(counter(page).final().first())
  let slots = meta.footer.slots
  if numbers != none or any-slot(slots) {
    set text(size: 8pt, fill: muted)
    line(length: 100%, stroke: 0.5pt + accent)
    v(-0.4em)
    grid(
      columns: (1fr, auto, 1fr),
      align: (left, center, right),
      column-gutter: 1em,
      slot-content(slots.at(0)),
      slot-content(slots.at(1)),
      {
        slot-content(slots.at(2))
        if numbers != none {
          if slot-filled(slots.at(2)) { h(1em) }
          numbers
        }
      },
    )
  }
}

#set document(title: meta.title, author: if meta.author == "" { () } else { meta.author })
#set page(
  paper: if design.paper == "letter" { "us-letter" } else { "a4" },
  margin: margin,
  header: context {
    if design.header.style != "none" and header-drawn(counter(page).get().first()) {
      header-block()
    }
  },
  footer: footer-block(),
)
#set text(font: body-font, size: body-size, weight: "regular", fill: ink)
#set par(justify: false, leading: 0.68em)

// --- Headings. Level 1 is the largest; a treated level takes the design's
// treatment. Only the top two section levels are numbered, and only under
// "numbered"; the numbers count from the top section level.
#show heading: set text(font: heading-font, weight: "bold", fill: ink)
#show heading: it => {
  let scale = heading-scales.at(it.level - 1, default: 1.0)
  set text(size: body-size * scale)
  set block(above: 1.5em, below: 0.8em)
  it
}
#let section-numbering(..n) = n.pos().slice(top-level - 1).map(str).join(".")
#show heading: it => {
  if not treated(it.level) {
    return it
  }
  let style = design.headings
  if style == "rule" {
    block(below: 0.9em, {
      it
      v(-0.55em)
      line(length: 100%, stroke: (if it.level == top-level { 1pt } else { 0.5pt }) + accent)
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
#show heading.where(level: top-level): set heading(numbering: if design.headings == "numbered" { section-numbering } else { none })
#show heading.where(level: top-level + 1): set heading(numbering: if design.headings == "numbered" { section-numbering } else { none })

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

// beside-logo sets the title with the logo at its right, both centred on
// the same line. On a band (chip) the logo sits on a small white ground, so
// a logo drawn for white paper keeps its own colours' contrast and is never
// lost against the accent.
#let beside-logo(title, tall, chip: false, show-logo: true) = {
  let logo = if show-logo { logo-image(tall) } else { none }
  if logo == none {
    title
  } else {
    grid(
      columns: (1fr, auto),
      column-gutter: 14pt,
      align: (left + horizon, right + horizon),
      title,
      if chip { box(fill: white, inset: 4pt, radius: 2pt, logo) } else { logo },
    )
  }
}

// --- The title block. The server takes a report's opening heading out of
// report.md (meta.titleBlock) and the template draws it here, as the level 1
// heading it was, never numbered, in the design's style: a band (white on
// the accent), a bar beside it, rules above and below, or none; with the
// person's logo at its right in each, and the subtitle, when the report
// opened with one, under the title.
#let title-heading(size) = {
  set text(size: size)
  show heading: it => block(above: 0pt, below: 0pt, it.body)
  heading(level: 1, numbering: none, meta.title)
}
#let subtitle-line(size, fill: muted) = if meta.subtitle != "" {
  v(0.35em)
  text(size: size, fill: fill, meta.subtitle)
}
#let titled(size) = {
  title-heading(size)
  subtitle-line(size * 0.55)
}
#let title-block(size, below: 1.4em, logo-tall: 14mm, show-logo: true) = {
  let style = design.titleBlock
  if style == "band" {
    block(width: 100%, fill: accent, inset: (x: 14pt, y: 16pt), below: below, {
      // A show-set rule, not a set rule: the document's heading rule sets
      // ink on the heading itself, and only a closer rule on the heading
      // overrides it. The core's contrast check assumes white here.
      show heading: set text(fill: white)
      beside-logo({ title-heading(size); subtitle-line(size * 0.55, fill: white) }, logo-tall, chip: true, show-logo: show-logo)
    })
  } else if style == "bar" {
    block(width: 100%, stroke: (left: 4pt + accent), inset: (left: 12pt, y: 4pt), below: below, beside-logo(titled(size), logo-tall, show-logo: show-logo))
  } else if style == "ruled" {
    block(width: 100%, below: below, {
      line(length: 100%, stroke: 1pt + accent)
      v(0.5em)
      beside-logo(titled(size), logo-tall, show-logo: show-logo)
      v(0.5em)
      line(length: 100%, stroke: 0.5pt + accent)
    })
  } else {
    block(width: 100%, below: below, beside-logo(titled(size), logo-tall, show-logo: show-logo))
  }
}
#let title-size = if form == "letter" { body-size * 1.5 } else { body-size * 2 }

// --- The cover: a page of its own before a titled report, in the design's
// cover style, holding the facts the design's cover shows. The title is
// always on it: it is the document's level 1 heading, and the body's own
// pages then open with their first section. The pages after it are
// numbered from 1: the counter is set back on the cover itself, before
// the next page steps it, so that page's header already sees 1.
#let shows(fact) = fact in design.cover.shows
#let brand = design.brand
#let has-brand = brand.name != "" or brand.strapline != "" or brand.contact.len() > 0 or brand.website != ""
#let cover-logo(tall) = if shows("logo") { logo-image(tall) } else { none }

// The facts under the title: the subtitle, the author line and the day.
#let cover-facts(size: body-size * 1.25, fill: muted) = {
  set text(fill: fill)
  v(1em)
  if shows("subtitle") and meta.subtitle != "" {
    text(size: size, meta.subtitle)
    linebreak()
  }
  if shows("author") and meta.author != "" {
    v(0.6em)
    text(size: body-size, meta.author)
    linebreak()
  }
  if shows("date") {
    v(0.2em)
    text(size: body-size, meta.date)
  }
}

// The brand block: who the report is from, as lines of text.
#let brand-block(fill: ink, align-to: left) = if shows("brand") and has-brand {
  set text(fill: fill, size: body-size * 0.95)
  set align(align-to)
  if brand.name != "" {
    text(font: heading-font, weight: "bold", size: body-size * 1.1, brand.name)
    linebreak()
  }
  if brand.strapline != "" {
    text(brand.strapline)
    linebreak()
  }
  for line in brand.contact {
    text(line)
    linebreak()
  }
  if brand.website != "" {
    text(brand.website)
  }
}
#let confidentiality-line(fill: muted) = if shows("confidentiality") and brand.confidentiality != "" {
  text(size: body-size * 0.9, fill: fill, brand.confidentiality)
}

#let cover-size = body-size * 2.6
#let inset = margin

// band: the title in white on a full-width accent panel at the top third.
#let cover-band() = stack(
  spacing: 0pt,
  block(width: 100%, height: 33%, fill: accent, inset: (x: inset, top: inset, bottom: inset * 0.8), {
    show heading: set text(fill: white)
    set text(fill: white)
    align(bottom, beside-logo(title-heading(cover-size), 22mm, chip: true, show-logo: shows("logo")))
  }),
  block(width: 100%, height: 67%, inset: (x: inset, y: inset * 0.8), {
    cover-facts()
    v(1fr)
    brand-block()
    v(0.8em)
    confidentiality-line()
  }),
)

// split: a secondary panel down the left third with the brand and the
// logo in white, the title in ink on white.
#let cover-split() = grid(
  columns: (33%, 1fr),
  rows: 100%,
  block(width: 100%, height: 100%, fill: secondary, inset: (x: inset * 0.6, y: inset), {
    set text(fill: white)
    let logo = cover-logo(16mm)
    if logo != none { box(fill: white, inset: 4pt, radius: 2pt, logo) }
    v(1fr)
    brand-block(fill: white)
    v(1em)
    confidentiality-line(fill: white)
  }),
  block(width: 100%, height: 100%, inset: (x: inset, y: inset), {
    v(30%)
    title-heading(cover-size)
    cover-facts()
  }),
)

// geometric: an ink title on white with accent and secondary shapes.
#let cover-geometric() = {
  place(top + right, dx: 40mm, dy: -50mm, circle(radius: 75mm, fill: accent))
  place(bottom + left, dx: -30mm, dy: 30mm, circle(radius: 55mm, fill: secondary-tint))
  place(bottom + left, dx: 0mm, dy: 0mm, rect(width: 55%, height: 7mm, fill: secondary))
  block(width: 100%, height: 100%, inset: (x: inset, y: inset), {
    let logo = cover-logo(16mm)
    if logo != none { logo }
    v(28%)
    box(width: 2.5em, height: 4pt, fill: accent)
    v(0.6em)
    title-heading(cover-size)
    cover-facts()
    v(1fr)
    brand-block()
    v(0.8em)
    confidentiality-line()
    v(12mm)
  })
}

// largeType: the title at display size in the accent, nothing else above
// the fold.
#let cover-large-type() = block(width: 100%, height: 100%, inset: (x: inset, y: inset), {
  show heading: set text(fill: accent)
  title-heading(body-size * 3.8)
  v(1fr)
  cover-facts(size: body-size * 1.4)
  v(1.5em)
  let logo = cover-logo(14mm)
  if logo != none {
    logo
    v(0.6em)
  }
  brand-block()
  v(0.8em)
  confidentiality-line()
})

// framed: a hairline frame, centred title, brand and date.
#let cover-framed() = block(width: 100%, height: 100%, inset: inset * 0.7, {
  rect(width: 100%, height: 100%, stroke: 0.75pt + accent, inset: inset, {
    set align(center)
    let logo = cover-logo(18mm)
    if logo != none { logo }
    v(1fr)
    title-heading(cover-size)
    v(0.8em)
    line(length: 30%, stroke: 0.75pt + accent)
    cover-facts()
    v(1fr)
    brand-block(align-to: center)
    v(0.8em)
    confidentiality-line()
  })
})

// plate: the cover image across the top half, the title block below.
#let cover-plate() = stack(
  spacing: 0pt,
  block(width: 100%, height: 50%, clip: true,
    image(meta.coverImage.path, width: 100%, height: 100%, fit: "cover", alt: "Cover image")),
  block(width: 100%, height: 50%, inset: (x: inset, y: inset * 0.8), {
    title-block(cover-size, below: 1.2em, logo-tall: 20mm, show-logo: shows("logo"))
    cover-facts()
    v(1fr)
    brand-block()
    v(0.8em)
    confidentiality-line()
  }),
)

#if meta.cover {
  page(margin: 0mm, header: none, footer: none, {
    let style = meta.coverStyle
    if style == "split" {
      cover-split()
    } else if style == "geometric" {
      cover-geometric()
    } else if style == "largeType" {
      cover-large-type()
    } else if style == "framed" {
      cover-framed()
    } else if style == "plate" {
      cover-plate()
    } else {
      cover-band()
    }
    // Placed, so it takes no room in the page's flow.
    place(counter(page).update(0))
  })
} else if meta.titleBlock {
  title-block(title-size)
} else if meta.logo != none {
  // A document with no title of its own has no title block; its logo, when
  // there is one, is drawn alone at the top right of page 1, where a title
  // block's would be.
  block(width: 100%, below: 1.4em, align(right, logo-image(14mm)))
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
