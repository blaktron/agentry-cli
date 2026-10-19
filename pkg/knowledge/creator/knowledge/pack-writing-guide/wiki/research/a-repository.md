---
title: Reading a repository
type: guide
summary: How to build from a git repository the person named: its README and docs first, then the code for what the docs leave out, found with grep rather than read file by file, no generated files, and pages that cite the file and name the commit.
sources: [web:https://docs.secureagentry.net/knowledge/]
updated: 2026-10-19
---

# Reading a repository

A repository the person named was brought in before the build and is a folder of documents, `repo-<host>-<path>/`, listed in `documents.json` file by file. The task names its address, its ref and the commit it resolved to. Nothing inside `.git` is there, and no link, submodule or Git LFS content: only the files as they are at that commit [src: web:https://docs.secureagentry.net/knowledge/].

Read it in this order:

1. **What it is.** `README.md`, then `docs/` or `doc/`, a `CONTRIBUTING.md` and a changelog: the project's purpose, how it is installed and used, its terms and its parts. Write the overview and the main topic pages from these before opening any code, so the pack has its backbone if the build stops.
2. **What the docs leave out.** A command's flags, a configuration file's keys, a type's fields, a default value, the words of an error. Find them with `grep` over the folder (`grep` takes a regular expression and an area, such as `/inputs/repo-github.com-o-r`) and `glob` for files by name, then read only the file that answers with `read_input`. Reading a code base file by file spends the budget on nothing.
3. **Leave the generated files alone.** Lock files, minified or bundled output, vendored copies of other projects, build output, test fixtures and large data files. A page citing them is noise.
4. **Cite the file**, by its path as `documents.json` gives it: `[src: doc:repo-github.com-o-r/cmd/serve.go]`. On the page that introduces the project, name the repository's address and the commit from the task, so a reader knows which version the pack describes.
5. **Say what you did not read.** A repository can hold thousands of files; the pack is what matters to its reader, not an index of every file. In your last reply, name the parts you left unread and why.

A file from a web address (the task says "is the file from") is one more document under its own name, cited `[src: doc:<name>]` as any document is [src: web:https://docs.secureagentry.net/knowledge/].
