---
title: How a page reads
type: guide
summary: The shape of a good pack page (one subject, the required frontmatter, a first paragraph that answers, a few hundred words), how to choose sections and slugs, how to link, and how to change or remove a page.
sources: [web:https://docs.secureagentry.net/knowledge/]
updated: 2026-10-19
---

# How a page reads

A page covers **one subject**: a thing, a procedure, a topic or a reference a reader would look up by name. Its title is that name. A reader finds it from the pack's index by its title and summary, so both say exactly what the page holds [src: web:https://docs.secureagentry.net/knowledge/].

## The shape

- **Frontmatter**, every key required: `title`, `type` (`topic`, `entity`, `procedure`, `reference` or `source`), `summary` (one line saying what the page holds), `sources` (every key the page cites) and `updated` (the build's date).
- **A `# Title` heading**, then a first paragraph of two or three sentences that answers what the page is about. A reader who stops there has the gist.
- **The body**: a paragraph or a short list per point, the most important first. A procedure is numbered steps, each one action, with what to check after it.
- **Length**: a few hundred words. A subject that grows past that is split into two pages that link each other.

## Sections and slugs

Put a page under `wiki/<section>/<slug>.md`. Keep the set of sections small and the same across the pack: `topics`, `entities`, `procedures`, `references`, or a plain word of your own where the corpus needs one. A slug is lowercase words joined by hyphens, unique in the whole pack, and names the subject (`cold-chain-alarm`, not `page-3`).

## Links

Link where one page mentions a subject another covers: `[Rotterdam depot](../entities/rotterdam-depot.md)`. Link only a page that exists, in the pack as it stands or written earlier in this build. When you write a page others should point to, rewrite those pages with the link.

## Changing a page

- Write a page again **in full**, true after this build. Never append an "update" section.
- A page written in this build cannot be read back from the pack: rewrite it from your copy in the scratch folder ([Many documents](../research/many-documents.md)) or from what you wrote.
- Remove a page only when this build makes it wrong or empty, by listing it under `delete` in `changes.json`. Never remove a page you wrote in this build: rewrite it.
