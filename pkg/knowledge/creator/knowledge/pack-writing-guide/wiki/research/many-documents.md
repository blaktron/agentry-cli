---
title: Many documents
type: guide
summary: How to build from many documents or long ones without losing work: a plan in the scratch folder, grep and glob to find what to read, a document at a time with its pages written before the next, and copies of the pages you will rewrite.
sources: [web:https://docs.secureagentry.net/knowledge/]
updated: 2026-10-19
---

# Many documents

A build stops at its time or turn budget, and it keeps only the pages already written. So the work is planned, searched and written as it goes [src: web:https://docs.secureagentry.net/knowledge/].

1. **Plan.** List the inputs with `list_inputs`. Write `/scratch/plan.md` with `scratch_write`: the subjects the pack needs, the sections, and which document covers each, the most important first. Keep it current; a later step reads it back with `scratch_read`.
2. **Search before you read.** `grep` finds a term, a name or a pattern across the documents' text under `/inputs`, and across the pack as it stands under `/knowledge/<pack>` when this build extends one. `glob` lists files by a path pattern. Read what they point to with `read_input` or `knowledge_read`. A search result is a pointer, never a source.
3. **A document at a time.** Read one (or a few short ones), then write or rewrite every page it supports before reading on. After each step the pack is true for everything read so far.
4. **Keep what you will rewrite.** When a page is one others will extend (an overview, a page others link to), keep a copy at `/scratch/pages/<section>/<slug>.md` and rewrite from it, because the pack's copy cannot be read back during the build.
5. **Say what you could not use.** A document listed as `unread`, or one you could not finish, goes in your last reply with the reason.

The scratch folder is private to the build and deleted with it: nothing in it reaches the pack except through `write_report`.
