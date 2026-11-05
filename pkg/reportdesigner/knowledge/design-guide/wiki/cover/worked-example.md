---
title: A worked example
type: example
summary: A whole custom cover that compiles and passes every check - an accent field across the top two thirds with the title in white, a secondary stripe, the facts in one corner, the logo and an optional photograph - with what each part does and how to vary it.
sources: [web:https://docs.secureagentry.net/runner/]
updated: 2026-11-05
---

# A worked example

The brief: "a confident cover for our quarterly board reports; navy and gold". The idea: **a deep field of the accent across the top two thirds, the title large and white at its foot, a thin gold stripe under it, the facts quiet in the white below** [src: web:https://docs.secureagentry.net/runner/].

```typst
#let cover(facts, design) = {
  let m = facts.margin * 1.2
  let shows(f) = f in facts.shows
  let gold = if facts.secondary != none { facts.secondary } else { facts.accent }

  // The field: the accent across the top two thirds.
  place(top + left, rect(width: 100%, height: 64%, fill: facts.accent))

  // A photograph, when the design has one, set into the field and darkened
  // by the accent laid over it, so the white title stays readable.
  if facts.images.len() > 0 {
    place(top + left, block(width: 100%, height: 64%, clip: true,
      image(facts.images.at(0), width: 100%, height: 100%, fit: "cover", alt: "Cover photograph")))
    place(top + left, rect(width: 100%, height: 64%, fill: facts.accent.transparentize(25%)))
  }

  // The logo, small, top left, on a white chip.
  if shows("logo") and facts.logo != none {
    place(top + left, dx: m, dy: m, box(fill: white, inset: 5pt, radius: 2pt, (facts.logo-image)(12mm)))
  }

  // The title at the foot of the field, white, with the subtitle under it:
  // a block as tall as the field, its content aligned to its bottom, so a
  // long title grows upward and never crosses the stripe.
  place(top + left, dx: m, block(width: 100% - 2 * m, height: 64% - 12mm, align(bottom + left, {
    (facts.title)(size: 40pt, fill: white, font: facts.fonts.headings, tracking: -0.5pt)
    if shows("subtitle") and facts.subtitle != "" {
      v(12pt)
      text(size: 15pt, fill: white, font: facts.fonts.body, facts.subtitle)
    }
  })))

  // The gold stripe where the field ends.
  place(top + left, dy: 64%, rect(width: 100%, height: 4mm, fill: gold))

  // The facts, quiet, aligned to the title's left edge.
  place(bottom + left, dx: m, dy: -m, block(width: 100% - 2 * m, {
    set text(size: 10pt, fill: facts.muted, font: facts.fonts.body)
    if shows("brand") and facts.brand.name != "" {
      text(size: 13pt, weight: "bold", fill: facts.ink, facts.brand.name)
      linebreak()
    }
    if shows("author") and facts.author != "" { facts.author; linebreak() }
    if shows("date") { facts.date; linebreak() }
    if shows("confidentiality") and facts.confidentiality != "" {
      v(6pt)
      text(size: 9pt, weight: "bold", tracking: 1pt, upper(facts.confidentiality))
    }
  }))
}
```

## What each part does

- **The field** carries the brand's colour at a scale no built-in style uses, so the cover reads at a distance.
- **The photograph** is optional: the same code draws a flat field without one. The accent laid over it at 75% keeps the white title at the contrast the design was checked for.
- **The title** is set once, with `facts.title`, in the heading face, white, slightly tightened.
- **The stripe** is the one use of the secondary, the idea's accent.
- **The facts** sit in one block on the title's left edge, and each appears only when `facts.shows` names it and it is not empty.

## How to vary it

- Move the field to the bottom third and set the title in the accent on white above it: a lighter, editorial cover.
- Replace the stripe with a large circle of the secondary bleeding off the right edge: a geometric cover.
- Drop the field and set the title at 64pt in the accent, left-aligned at the top third: a typographic cover. The accent then carries large text, which the contrast check already allows.
