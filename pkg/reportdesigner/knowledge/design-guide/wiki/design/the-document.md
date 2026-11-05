---
title: The design document
type: reference
summary: Every field of a version 3 report design, what it changes on the page, and the rules a document is held to before it is saved, with a whole document to copy.
sources: [web:https://docs.secureagentry.net/runner/]
updated: 2026-11-05
---

# The design document

A report design is one JSON object, `"agentryTheme": 3`, at most 24 KB. Every field below is required; write `""` or `[]` for none. Never write `logo`, `coverImage` or `customCover`: the application adds the design's own images [src: web:https://docs.secureagentry.net/runner/].

| Field | What it is |
| --- | --- |
| `agentryTheme` | Always `3`. |
| `name` | The design's name, at most 60 characters. |
| `basedOn` | The built-in id it started from (`plain`, `classic`, `modern`, `editorial`, `technical`, `boardroom`, `studio`, `field`, `civic`, `lumen`). |
| `version` | `1` for a new design. |
| `paper` | `a4` or `letter`. |
| `fonts` | `headings`, `body` and `code`, each a family from [the lists](the-lists.md) that fits its role. |
| `accent` | The first colour, `#rrggbb` in lower case: rules, bars, panels, and text where the style says. |
| `secondary` | A second colour, or `""`: the split cover's panel, the geometric cover's shapes. |
| `titleBlock` | How page 1 draws the title when there is no cover: `band` (white on the accent), `bar`, `ruled`, `none`. |
| `headings` | The top two section levels: `rule`, `bar`, `smallcaps`, `numbered`, `plain`. |
| `headingScale` | `compact`, `regular` or `generous`. |
| `margins` | `narrow`, `regular` or `wide`. |
| `tables` | `tinted` (the header row in the accent's tint) and `banded` (every other row), each `true` or `false`. |
| `quotes` | `callout` (a tinted box with an accent edge) or `plain`. |
| `brand` | `name` (60), `strapline` (120), up to four `contact` lines (120 each), `website` (a host name or https address, drawn as text), `confidentiality` (120). |
| `header` | `style` from the header styles, and three `slots`: left, centre, right. |
| `footer` | `pageNumbers` (`none`, `n`, `nOfN`) and three `slots`. |
| `cover` | `style` (a cover style or `custom`), `shows` (the facts the cover carries, each once), `when` (`long`, `always`, `never`). |

## Slots and tokens

A slot is one line of literal text and tokens, at most 120 characters: `{title}`, `{author}`, `{date}`, `{page}`, `{pages}`, `{brand}`, `{section}`, `{confidentiality}`. Anything else between braces is printed as it is. A slot is never code: markup in it is drawn as text.

## Contrast

Body text is always near-black. A colour that carries text must reach 4.5:1 on white; one that only colours shapes, rules and tints needs 3:1. The accent carries text with a `band` title block, `smallcaps` or `numbered` headings, and the `band`, `largeType`, `plate` and `custom` covers (a custom cover that fails is drawn as band). The secondary carries text on a `split` cover. `check_design` tells you each ratio and the nearest shade that passes.

## A whole document

```json
{
  "agentryTheme": 3,
  "name": "Harbour Works",
  "basedOn": "boardroom",
  "version": 1,
  "paper": "a4",
  "fonts": {"headings": "Libre Baskerville", "body": "Source Serif 4", "code": "IBM Plex Mono"},
  "accent": "#1b2a4a",
  "secondary": "#8a6a00",
  "titleBlock": "band",
  "headings": "rule",
  "headingScale": "regular",
  "margins": "wide",
  "tables": {"tinted": true, "banded": false},
  "quotes": "callout",
  "brand": {"name": "Harbour Works", "strapline": "", "contact": [], "website": "", "confidentiality": "Confidential"},
  "header": {"style": "brandLine", "slots": ["{brand}", "", "{date}"]},
  "footer": {"pageNumbers": "nOfN", "slots": ["{brand}", "{confidentiality}", ""]},
  "cover": {"style": "split", "shows": ["title", "subtitle", "author", "date", "brand", "logo", "confidentiality"], "when": "always"}
}
```
