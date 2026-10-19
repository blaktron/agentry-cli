# The Knowledge Creator

The Knowledge Creator is the QuickAgent that builds **knowledge packs**. You
give it documents, a brief (what the pack should cover and for whom), or
both, and it compiles them into a small wiki of short, cited markdown pages.
A QuickAgent run can then search that wiki to answer questions. Adding
documents to an existing pack runs it again, and it rewrites only the pages
the new documents change. Specific documents make the best knowledge:
without them it researches the web and its own knowledge, which can be
wrong or out of date.

## This is the package that runs

The Knowledge Creator is built into the Agentry CLI and the desktop app, and
it runs from there: on your computer when you run `agentry knowledge create`
or `agentry knowledge add` (or build a pack in the desktop), and on an
Agentry sandbox host when you create knowledge in Agentry. This release has
the same bytes. The registry built its archive from the package the CLI
carries, and the archive's digest is the one the CLI reports. Run
`agentry builtins` to print it and compare it with the digest on this page.

Installing this release does not give you the Knowledge Creator. The
application runs it as a built-in, with the steps and tools listed below. If
you install this release, it runs as an ordinary QuickAgent with only what
its manifest declares.

## What the application does around it

The manifest declares no network access, no file access and no commands, and
one data category, `files`. When Agentry builds a pack:

- **Before the run**, the runner decodes each document to text: a PDF, an
  office file or an image becomes a `.txt` file, and a text document stays
  as it is. The Knowledge Creator runs no decoder itself. It reads the text
  through the inputs channel (`list_inputs`, `read_input`).
- **During the run**, when it extends an existing pack, it can read the pack
  as it stands with `knowledge_list`, `knowledge_search` and
  `knowledge_read`. It writes each page with `write_report`. Like every run,
  it has a scratch folder for its working notes, private to the run and
  deleted with it, and `grep` and `glob` to search its documents, the pack
  and that folder.
- **When the build searches the web** (the default for a build with no
  documents, a choice for one with them), the application grants it two
  tools its manifest does not declare: `web_search` and `web_read`. AI Review
  reads every search result and every page before the model does, and one it
  flags, or cannot scan, is withheld. On your computer that takes your own
  TypeSafe key and a search key; in Agentry the registry searches and scans.
  It cites a page it read as `[src: web:<url>]` and keeps no copy of it, and
  cites what its model knows as `[src: model]`. A brief is kept in the pack
  as `sources/brief.md`. `web_read` opens only an address one of the
  build's own searches returned. A page whose text is drawn by scripts is
  rendered in a headless Chromium the application runs once a page, on a
  network whose one way out admits public sites alone; the model never
  drives that browser, and AI Review reads the rendered text before the
  model does.
- **Its guide.** The release ships a knowledge pack,
  `knowledge/pack-writing-guide`: how a page reads, citing, building from
  many documents, and researching the web. The application attaches it to
  every build, beside the pack a build extends.
- **After the run**, the application checks every page and drops any that
  break the pack's rules, regenerates the index and appends the log. A pack
  built in Agentry is scanned before it is final.

`INSTRUCTION.md` is the instruction the model follows. It covers where its
knowledge comes from (the documents, then the pack, then the web, then its
own), how pages are named and cited, and why the pack is written one
document or subject at a time, so a build stopped by its budget keeps what
it wrote.
