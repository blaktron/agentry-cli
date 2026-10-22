# How this memory is kept

This folder is your memory: a small wiki you write and keep current, so
that what the person told you and what they shared with you is still known
in a later conversation. You keep it with three skills: memory-ingest,
memory-query and memory-lint. Read this file before changing the memory's
shape.

## The folders and files

- `SCHEMA.md` is this file: how the memory is kept.
- `index.md` is the way in: one line per page, grouped by type, each a link
  and a few words on what the page holds. Read it first, every time.
- `log.md` is the record of what you did to the memory, newest last. Never
  rewrite an earlier entry; add one.
- `pages/` holds the pages, one Markdown file each, named in lower case with
  hyphens (`pages/sam-the-person.md`, `pages/kitchen-renovation.md`).
- `pages/notes/`, when it exists, holds the notes from your data folder,
  copied in as they were when the memory was made. They are sources, not
  pages: they have no frontmatter. Fold each into the pages it belongs on
  (memory-ingest), cite it as `notes/<file>` and the date it was copied in,
  and leave the copies as they are.

## The sources

The memory's sources are what you were told or shown, and the memory never
changes them:

- **A file in the person's share** (`share/`): read it, never change it,
  move it or delete it to keep the memory. Cite it by its path and the date
  you read it.
- **Something the person said in conversation**: cite it as `conversation`
  and the date it was said.
- **A task's turn or a tool's answer**: cite it by what it was and the date.

A fact with no source does not go in the memory.

## The pages

Each page starts with this frontmatter:

```
---
title: Kitchen renovation
type: person | project | topic | preference | event | source
updated: 2026-01-31
sources:
  - share/quotes/builder-quote.pdf (read 2026-01-30)
  - conversation (2026-01-31)
---
```

- **person**: someone the person mentions: who they are to the person and
  what matters about them.
- **project**: something the person is working on: the goal, the state,
  what is next, the dates that matter.
- **topic**: what you have learned about a subject the person cares about.
- **preference**: how the person likes things done.
- **event**: something that happens on a date: what, when, who.
- **source**: what one shared file says, in short, for a file that is
  worth a page of its own.

Below the frontmatter, write short sections in plain language. Put each
fact on its own line or in its own sentence, and end it with its source in
square brackets: `The builder can start on 3 March [share/quotes/builder-quote.pdf, read 2026-01-30].`
Link related pages with relative links (`[the budget](kitchen-budget.md)`).

When a newer source says something different from an older one, keep both
lines, mark the older one `(superseded)`, and say which is current.

## The log

Each entry starts with a line in this form, then a sentence or two:

```
## [2026-01-31] ingest | share/quotes/builder-quote.pdf
```

The operations are `seed` (the memory was made), `ingest` (a source was
added), `answer` (an answer was filed back as a page), `lint` (the memory
was checked) and `fix` (a page was corrected).
