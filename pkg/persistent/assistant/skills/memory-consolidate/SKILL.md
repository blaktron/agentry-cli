---
name: memory-consolidate
description: Tidy your memory in memory/ so it stays short to read and right - the index back under its limit, duplicate facts merged, long pages split, old superseded lines folded together, links and the index checked - without losing a current fact. Use when the person asks you to tidy or check your memory, when a message from Agentry asks you to, or when an answer turned up something in the memory that looked wrong.
license: MIT
---

# Tidy your memory

`memory/SCHEMA.md` says how the memory is kept. Tidying changes how facts
are kept, never what they say: every fact that is current before is current
after, with its source.

**Moving facts** (steps 2 and 4): a fact moves with the `~` lines it
replaced. On the page it moves to, each takes the next free number after
that page's highest `F`, and each `by F<n>` is changed to the moved line's
new number. A page you emptied this way is written as the single line
`#<slug> moved to <new slug>`, and its index line is removed. Log each
moved fact as `>`.

1. **The index and the pages agree.** List `memory/pages/`. Each page,
   except a moved one, has one line in `memory/index.md`, and each line
   names a page that exists; each gist says what its page holds now.
   `pages/notes/` is sources: its one line, `notes/`, stays only while a
   note is left to fold in.
2. **The index fits.** It has at most 200 lines and 25 KB. If it is over,
   move the facts of the oldest small pages of one subject onto one page of
   type `t` for that subject. Never drop a fact.
3. **Duplicates.** Where two current lines on a page say the same thing,
   mark the later one `~F<n> … | until <date> by F<m>`, naming the one kept.
4. **Long pages.** A page past 60 fact lines is split by subject: move part
   of its facts onto a new page, and link the two pages to each other.
5. **Old superseded lines.** Where a subject on a page has several `~` lines
   whose `until` date is more than 90 days ago, replace them with one `~`
   line that says what they were and until when, keeping the newest of
   their sources and `by` the current line.
6. **Headers and links.** Each page has its three header lines; `upd=` is
   the date of its newest line; each slug in `links:` names a page that
   exists and was not moved.
7. **Facts without a source** are given one from `share/` or the log if you
   can find it, and are otherwise removed and named to the person.
8. **Log it**: one line, `<date>T<time> consolidate memory <slug> >F<n> ~F<n> …`.

Write each page back whole with fs_write, every line you did not mean to
change exactly as it was. Never change a file in `share/`.
