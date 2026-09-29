# The Knowledge Creator

The Knowledge Creator is the QuickAgent that builds **knowledge packs**. You
give it documents, and it compiles them into a small wiki of short, cited
markdown pages. A QuickAgent run can then search that wiki to answer
questions. Adding documents to an existing pack runs it again, and it
rewrites only the pages the new documents change.

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
  `knowledge_read`. It writes each page with `write_report`.
- **After the run**, the application checks every page and drops any that
  break the pack's rules, regenerates the index and appends the log. A pack
  built in Agentry is scanned before it is final.

`INSTRUCTION.md` is the instruction the model follows. It covers how pages
are named, how they cite the documents, and why the pack is written one
document at a time, so a build stopped by its budget keeps what it wrote.
