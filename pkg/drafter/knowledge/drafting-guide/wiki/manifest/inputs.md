---
title: Input files
type: guide
summary: How the person's files reach a run, the two views the input tools read them in, the manifest's inputs block that says what a QuickAgent takes and which view it reads first, the cases and what each declares, and how a draft asks for its files.
sources: [web:https://docs.secureagentry.net/runner/, web:https://docs.secureagentry.net/quickagents/, web:https://docs.secureagentry.net/agentry/hosted-runs/]
updated: 2026-10-06
---

# Input files

## How files reach a run

- The person adds files when they start a run, and they reach it read-only under `/inputs`. No capability is needed for them: `filesystem.read` is for the person's workspace, not for input files [src: web:https://docs.secureagentry.net/quickagents/].
- The run reads them through `list_inputs`, `read_input` and `search_inputs`, which every run has [src: web:https://docs.secureagentry.net/runner/].
- An archive (zip, tar, 7z, a compressed file) is unpacked before the run starts, and each file in it is an input of its own, read by its path inside the archive (`src.zip/app/main.py`). A repository's `.git` folder is listed but never read [src: web:https://docs.secureagentry.net/runner/].
- The answers to the intake's questions arrive as an input too, and so does a file an intake question of type `file` asks for.
- A run In Agentry takes at most twenty files of at most 10 MB each [src: web:https://docs.secureagentry.net/agentry/hosted-runs/].
- A declared MCP server sees none of the input files: they are not in its container (see [Declared MCP servers](../servers/mcp-servers.md)).

## The two views

Every input can be read in two views, and the run can ask `read_input` and `search_inputs` for either at any time [src: web:https://docs.secureagentry.net/runner/]:

- **`text`**: the text a decoder made of the file before the run: a PDF's or an office document's text, an image's recognised text, a web page as Markdown. Code, markup, configuration and data need no decoder, so their text is the file itself.
- **`original`**: the file as uploaded, byte for byte. A binary file (a PDF, a Word file, an image) comes as pages of base64 with its size and type, which only a task that works on the bytes wants.

Without a declaration each file has its own default: the decoder's text where one made it, and anything else as uploaded, HTML included.

**No command is needed to read an input.** The runner unpacks archives and the tools read every file in either view, so a draft never declares `cat`, `grep`, `ls`, `head`, `tar`, `unzip` or any other command to read, search, list or unpack the person's files (see [Commands](exec.md)).

## The inputs block

A draft whose QuickAgent takes files declares what it takes and which view it reads first:

```yaml
inputs:
  kinds: [code]        # documents | code | data | images, or any on its own
  view: original       # text | original | both
```

- **`kinds`** says what to bring, and the run setup asks for it: `documents` (prose to read: PDFs, Word and other office files, e-books), `code` (a codebase or its files: source, markup, build and configuration files, a repository, loose or as an archive), `data` (CSV, JSON, YAML, spreadsheets, logs, lockfiles), `images` (photographs, scans, screenshots, diagrams). Use `any` only when the task truly takes any file, and then alone.
- **`view`** is the view the run reads each file in first:
  - `text` when only the words matter: summarising documents, reading saved web pages as Markdown, extracting fields from scanned receipts.
  - `original` when the file itself matters: reviewing code, markup or a site's pages (their scripts and tags survive only as uploaded), checking a file's exact contents or formatting. Never pair `original` with `documents` or `images` unless the task works on their bytes: their original is base64.
  - `both` when the files are mixed or the task needs both: each file keeps its own default.
- Leave the block out of a QuickAgent that takes no files.
- The block grants nothing and is not part of what the person consents to: handing over a file is the grant.

| The task | `kinds` | `view` | `exec` |
| --- | --- | --- | --- |
| summarise reports, contracts or papers | `[documents]` | `text` | `none` |
| review a codebase, scan source for vulnerabilities | `[code]` | `original` | `none` |
| review a website's HTML, CSS and scripts | `[code]` | `original` | `none` |
| audit a CSV, a log or a lockfile | `[data]` | `original` | `none` |
| read receipts, scans or screenshots | `[images]` | `text` | `none` |
| a mix (a contract and its spreadsheet) | each kind | `both` | `none` |

## How a draft asks for them

- Say what to bring in `guide.files`, one line naming the files and their formats, matching `kinds` ("Add the repository as a .zip, or its files").
- Use an intake question of type `file` only for one particular file the instructions must tell apart from the others (a contract and a playbook, a before and an after).
- Ask for a document as it is (a Word file, a PDF, a spreadsheet), and a codebase as it is (an archive or its files). Never ask the person to convert anything to text first.
- The instructions' `## Inputs` say what each file means, which view to read it in when that differs from the declared one ("ask read_input for view \"text\" when only a PDF's words matter"), and what to do when one is missing or in an unexpected format.
