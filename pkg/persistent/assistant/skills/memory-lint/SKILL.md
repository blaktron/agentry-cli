---
name: memory-lint
description: Check your memory in memory/ and fix what has gone wrong in it, such as broken links, pages missing from the index, facts with no source, and pages that disagree. Use when the person asks you to tidy or check your memory, after a long run of ingests, or when an answer turned up something in the memory that looked wrong.
license: MIT
---

# Check your memory

1. **The index and the pages.** List `memory/pages/` with fs_list (and its
   folders). Each page has a line in `memory/index.md`, and each line of the
   index names a page that exists. Add or remove lines to make them agree.
   The files in `pages/notes/` are sources, not pages: they have no line of
   their own, and the index's line for that folder stays while it holds
   notes still to fold in (memory-ingest). Steps 2 and 4 skip them.
2. **The frontmatter.** Each page has a title, a type from SCHEMA.md, an
   `updated` date and its sources. Fill in what is missing from the page's
   own lines.
3. **The links.** Each relative link names a page that exists. Fix a link
   to a page that was renamed; remove one to a page that is gone.
4. **The sources.** Each fact ends with its source. A fact with none is
   given one if you can find it in `share/` or the log, and is otherwise
   removed and named to the person. A cited share file that no longer
   exists is noted on the page as removed from the share, with the date.
5. **Disagreements.** Where two pages say different things about the same
   subject, keep the newer, mark the older `(superseded)`, and tell the
   person about any you could not settle.
6. **Log it**: `## [date] lint | <what you checked>`, and a sentence on what
   you fixed and what you left for the person.

Never change a file in `share/` while checking the memory.
