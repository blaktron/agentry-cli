---
title: The closed lists
type: reference
summary: Every name a report design's fields accept - the faces by role, the cover styles and the colours each sets text on, the header and footer styles, the title blocks, the heading styles and scales, the margins, the cover facts and the tokens - generated from the design package itself.
sources: [web:https://docs.secureagentry.net/runner/]
updated: 2026-11-05
---

# The closed lists

A design's fields take names from these lists and nothing else; a name not here is refused. This page is generated from the rules every design is held to, so it is exactly what they accept [src: web:https://docs.secureagentry.net/runner/].

## Faces

- **headings:** Inter, IBM Plex Sans, Source Sans 3, Work Sans, DM Sans, Nunito Sans, Raleway, Space Grotesk, Montserrat, Source Serif 4, Lora, Merriweather, IBM Plex Serif, EB Garamond, Libre Baskerville, Crimson Pro, Playfair Display, Fraunces, IBM Plex Mono, JetBrains Mono, Source Code Pro, Fira Code
- **body:** Inter, IBM Plex Sans, Source Sans 3, Work Sans, DM Sans, Nunito Sans, Raleway, Space Grotesk, Source Serif 4, Lora, Merriweather, IBM Plex Serif, EB Garamond, Libre Baskerville, Crimson Pro
- **code:** IBM Plex Mono, JetBrains Mono, Source Code Pro, Fira Code

## Cover styles

| Style | Carries text in |
| --- | --- |
| `band` | the accent |
| `split` | the secondary (the accent when there is none) |
| `geometric` | no colour |
| `largeType` | the accent |
| `framed` | no colour |
| `plate` | the accent |
| `custom` | the accent |

## The other fields

- `paper`: `a4`, `letter`
- `titleBlock`: `band`, `bar`, `ruled`, `none`
- `headings`: `rule`, `bar`, `smallcaps`, `numbered`, `plain`
- `headingScale`: `compact`, `regular`, `generous`
- `margins`: `narrow`, `regular`, `wide`
- `quotes`: `callout`, `plain`
- `header.style`: `none`, `rule`, `brandLine`, `titleAndPage`, `logoLeft`
- `footer.pageNumbers`: `none`, `n`, `nOfN`
- `cover.shows`: `title`, `subtitle`, `author`, `date`, `brand`, `logo`, `confidentiality`
- `cover.when`: `long`, `always`, `never`

## Tokens

A header or footer slot may hold: `{title}`, `{author}`, `{date}`, `{page}`, `{pages}`, `{brand}`, `{section}`, `{confidentiality}`.
