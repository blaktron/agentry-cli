---
title: Input files
type: guide
summary: How the person's files reach a run, a repository or a file at an address that the runner fetches for it, the two views the input tools read them in, the manifest's inputs block that says what a QuickAgent takes and which view it reads first, the cases and what each declares, and how a draft asks for its files.
sources: [web:https://docs.secureagentry.net/runner/, web:https://docs.secureagentry.net/quickagents/, web:https://docs.secureagentry.net/agentry/hosted-runs/]
updated: 2026-10-12
---

# Input files

## How files reach a run

- The person adds files when they start a run, and they reach it read-only under `/inputs`. No capability is needed for them: `filesystem.read` is for the person's workspace, not for input files [src: web:https://docs.secureagentry.net/quickagents/].
- The run reads them through `list_inputs`, `read_input` and `search_inputs`, which every run has [src: web:https://docs.secureagentry.net/runner/].
- An archive (zip, tar, 7z, a compressed file) is unpacked before the run starts, and each file in it is an input of its own, read by its path inside the archive (`src.zip/app/main.py`). A repository's `.git` folder is listed but never read [src: web:https://docs.secureagentry.net/runner/].
- The answers to the intake's questions arrive as an input too, and so does a file an intake question of type `file` asks for.
- A run In Agentry takes at most twenty files of at most 10 MB each [src: web:https://docs.secureagentry.net/agentry/hosted-runs/].
- A declared MCP server sees none of the input files: they are not in its container (see [Declared MCP servers](../servers/mcp-servers.md)).

## A repository or a file at an address

A QuickAgent that works on a codebase asks for its repository with an intake question of type `repository`, and one that reads a file on the web asks for its address with a question of type `url` (see [The intake and the guide](intake-and-guide.md)). The person gives the address when they start the run, and the runner fetches it before the run starts [src: web:https://docs.secureagentry.net/runner/; doc:runtime-facts.md]:

- **Where it lands.** A repository is checked out at its default branch, or at the branch, tag or commit the person adds, under `/inputs/<id>/`, the question's id, with no `.git` folder. Its files are inputs like any other, read with `list_inputs`, `read_input` and `search_inputs` by their path (`repository/src/main.py`). A `url` source is one file, read as an uploaded file is.
- **No egress and no key for the source.** The fetch is the runner's, not the QuickAgent's. Never declare `github.com`, `api.github.com` or the repository's own host in `egress` to get it, never declare `GITHUB_TOKEN` or any other key to clone it, and never have the instructions download it. An API the task calls about it (an advisory database for its dependencies) is still declared for `fetch` as any host is (see [Network and fetch](egress-and-fetch.md)). For a private repository the person gives an access token when they start the run, and only the runner's fetch reads it: the run never sees it.
- **Its bounds** are in [the runtime facts](../../sources/runtime-facts.md). A source over its cap refuses the run before it starts, with the count.
- **What is checked before the run.** In Agentry, AI Review reads a repository's Markdown files (its README and docs), not its code. Executable and binary files in a repository are checked for malware, and its images are not read as text.
- **The intake answer** in `/inputs/intake.json` names what each source resolved to: the commit of a repository, the file of an address.

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
| review a codebase, scan source for vulnerabilities (a `repository` question) | `[code]` | `original` | `none` |
| review a website's HTML, CSS and scripts | `[code]` | `original` | `none` |
| audit a CSV, a log or a lockfile | `[data]` | `original` | `none` |
| read receipts, scans or screenshots | `[images]` | `text` | `none` |
| a mix (a contract and its spreadsheet) | each kind | `both` | `none` |

## How a draft asks for them

- **A codebase is asked for by a `repository` question**, never as a `.zip` to upload or as its files: the person gives its address and, for a private one, a token, and the runner fetches it. Declare `inputs` for it as for code (`kinds: [code]`, `view: original`), and name the question's folder in the instructions (`/inputs/repository/`).
- **A file at a web address is asked for by a `url` question**, not fetched by the QuickAgent, unless the task reads an API (then `fetch`, see [Network and fetch](egress-and-fetch.md)).
- Say what to bring in `guide.files`, one line naming the files and their formats, matching `kinds` ("Add the reports as PDF or Word files"). A QuickAgent whose only input is a source needs no `files` line: the question asks for it.
- Use an intake question of type `file` only for one particular file the instructions must tell apart from the others (a contract and a playbook, a before and an after).
- Ask for a document as it is (a Word file, a PDF, a spreadsheet). Never ask the person to convert anything to text first.
- The instructions' `## Inputs` say what each file means, which view to read it in when that differs from the declared one ("ask read_input for view \"text\" when only a PDF's words matter"), and what to do when one is missing or in an unexpected format.
