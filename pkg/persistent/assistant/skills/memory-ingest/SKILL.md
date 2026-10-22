---
name: memory-ingest
description: Add what you were told or shown to your memory in memory/, as pages with their sources. Use when the person asks you to remember something, shares a file and wants it kept, tells you a fact about themselves or their plans that will matter later, or when memory/pages/notes/ holds notes still to fold in.
license: MIT
---

# Add to your memory

Your memory is the wiki in `memory/`. `memory/SCHEMA.md` says how it is kept:
read it once before your first ingest in a conversation.

1. **Read the source.** A file in `share/`: read it with fs_read, and never
   change, move or delete it. Something said in conversation: take the
   person's own words. Note the date (each message starts with it).
2. **Find what it touches.** Read `memory/index.md`, then search the pages
   with grep for the names, places and subjects in the source. Read each
   page it touches.
3. **Write the pages.** Add the new facts to the pages they belong on, each
   ending with its source in square brackets, and update each page's
   `updated` date and `sources` list. Make a new page only for something
   that has no page yet; give it the frontmatter SCHEMA.md describes. When
   the source says something different from a page, keep both lines, mark
   the older `(superseded)`, and tell the person.
4. **Keep the index.** Add a line to `memory/index.md` for each new page,
   under its type, and change the line of a page whose subject changed.
5. **Log it.** Add one entry to the end of `memory/log.md`:
   `## [date] ingest | <the source>`, and a sentence on which pages changed.
6. **Tell the person** in a sentence what you now remember and where.

For the notes in `memory/pages/notes/`, copied from `data/notes/` when the
memory was made: fold each into the pages it belongs on, citing it
as `notes/<file>` and the date it was copied in, then remove its line from
the index's notes section once nothing in it is left to fold in. Leave the
copies as they are, and never change `data/notes/`.

Never put in the memory a fact you cannot give a source for, a secret, a
password or a key, or anything the person asked you not to keep.
