---
title: A custom cover's conventions
type: reference
summary: The contract between a custom cover and the report template - the cover(facts, design) function, every fact it is given, the title it must set, the files it may read, the fonts, alt text, one page, no imports - and the kinds of failure an export passes a cover over for.
sources: [web:https://docs.secureagentry.net/runner/]
updated: 2026-11-05
---

# A custom cover's conventions

A custom cover is Typst code, at most 32 KiB, that the report template calls to draw the first page of a titled report. When it fails, the export draws the built-in band cover instead and the person's receipt says so. Follow every rule below and it never fails [src: web:https://docs.secureagentry.net/runner/].

## The function

`cover.typ` defines one function, which the template imports and calls:

```typst
#let cover(facts, design) = {
  // what you return is drawn as the cover page
}
```

What it returns is drawn inside a block the size of the page, with no margin: place things with `place`, `align`, `block`, `stack`, `grid`, `rect`, `circle`, `line`, `polygon`, `text` and `image`. Content that runs past the page is clipped. Anything at the top of the file outside the function, `#set` and `#show` rules included, does not reach the page; set rules inside the function do.

## The title

The cover **must** set the report's title with `facts.title`, a function:

```typst
(facts.title)(size: 34pt, fill: white, font: none, weight: "bold", tracking: 0pt)
```

Every argument is optional. It is a block of the width it is given, ending below the title's descenders, so `v(…)` after it spaces what follows. It sets the title as the document's level 1 heading, which a tagged PDF needs before the report's sections. A cover that never calls it fails. Call it once. A report's title may be two words or twenty: give the title room to grow and keep it clear of everything else on the page, for instance by placing the facts in a block of their own below it rather than at a fixed distance from the page's foot. For the title as a string, to measure or split it, use `facts.title-text`; never draw the title twice.

## The facts

| Fact | What it is |
| --- | --- |
| `facts.title` | The function above. |
| `facts.title-text` | The title as a string. |
| `facts.subtitle` | The report's subtitle, or `""`. |
| `facts.author` | The author line, or `""`. |
| `facts.date` | The day of the export, `YYYY-MM-DD`. |
| `facts.shows` | The facts the person wants on a cover: an array of `"title"`, `"subtitle"`, `"author"`, `"date"`, `"brand"`, `"logo"`, `"confidentiality"`. Draw what it names and leave out what it does not. |
| `facts.brand` | `name`, `strapline`, `contact` (an array of lines), `website`, `confidentiality`, each `""` when empty. |
| `facts.confidentiality` | The confidentiality line, or `""`. |
| `facts.accent`, `facts.tint` | The accent and its pale tint, as colours. |
| `facts.secondary`, `facts.secondary-tint` | The secondary and its tint, or `none` when the design has none. |
| `facts.ink`, `facts.muted` | Near-black for text, and grey for quieter text. |
| `facts.fonts` | `headings`, `body` and `code`: font lists to pass as `text(font: facts.fonts.headings)`. Noto stands behind each, so every script renders. |
| `facts.logo` | The logo's path, or `none`. |
| `facts.logo-image` | A function: `(facts.logo-image)(16mm)` draws the logo at most that tall, with its alt text, or `none` when there is no logo. |
| `facts.cover-image` | The design's cover image's path, or `none`. |
| `facts.images` | The paths of the images this cover may draw: `cover/1.png` and on. |
| `facts.page` | `width` and `height` of the page. |
| `facts.margin` | The design's margin, a length to inset by. |

`design` is the design document itself, for anything else (`design.headings`, `design.paper`).

## Images and alt text

Draw an image only from `facts.logo`, `facts.cover-image` or `facts.images`, and give every image alt text:

```typst
image(facts.images.at(0), width: 100%, height: 100%, fit: "cover", alt: "The harbour at dawn")
```

An image without alt text fails the tagged PDF and the cover is passed over. Use `(facts.logo-image)(size)` for the logo: it has its alt text.

## What a cover cannot do

- **Import a package.** `#import "@preview/…"` fails: the renderer has none and fetches nothing. Draw with Typst's own shapes and text.
- **Read beyond its folder.** It may read `report.md`, `design.json`, `meta.json` and the images above, and nothing else.
- **Draw more than one page.** A `pagebreak()` or a `page(…)` inside it fails.
- **Run long.** The whole export has a time and memory limit. Keep loops small: a pattern of a few hundred shapes is fine, tens of thousands are not.

## Why an export passes a cover over

`render_preview` answers the same kinds, so fix each before you finish:

| Kind | The cause |
| --- | --- |
| `compile` | A syntax error, an unknown name or a file it cannot read. |
| `import` | It imports a package. |
| `altText` | An image without alt text. |
| `pages` | A page break or a page setting inside it. |
| `title` | It never called `facts.title`. |
| `time`, `memory` | It ran past the render's limits. |
| `image` | One of its images could not be re-encoded. |
