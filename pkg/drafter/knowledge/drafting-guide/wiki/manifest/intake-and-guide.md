---
title: The intake and the guide
type: guide
summary: The questions a QuickAgent asks before each run (types, a repository, a file at an address, a site to test and a target host among them, ids, when to ask at all) and the guide a person reads before a run (overview, prepare, files, task, connect), with their bounds and the fields to leave out.
sources: [web:https://docs.secureagentry.net/quickagents/, web:https://docs.secureagentry.net/agentry/hosted-runs/, web:https://docs.secureagentry.net/agentry/target-hosts/]
updated: 2026-10-14
---

# The intake and the guide

Both are what the person meets before a run: the run setup on the site, in Agentry and in the desktop shows the guide, then asks the intake's questions. Neither changes what the QuickAgent may touch [src: web:https://docs.secureagentry.net/quickagents/; web:https://docs.secureagentry.net/agentry/hosted-runs/].

## The intake

An intake question is asked before each run, and its answer reaches the run as an input (`/inputs/intake.json`). Ask only for what changes from run to run and cannot be found: a repository, a date range, a file. Most drafts need no question or one, and never more than your instruction's limit. A point that rarely changes gets a default, named in the notes, instead of a question [src: web:https://docs.secureagentry.net/quickagents/].

Each question is `{id, prompt, type, required}`:

- `id`: lowercase letters, digits and underscores, once each (`repository`, `date_range`).
- `prompt`: the question as the person reads it.
- `type`:
  - `text`, the default: a line of text.
  - `choice`: one of a fixed set. **A `choice` question always carries its `choices`**, each named once (`"choices": ["markdown", "json"]`). If you cannot list the answers, make it a `text` question.
  - `file`: a file the person adds, staged with the run's inputs. Name it in `guide.files` too, and its kind in `inputs.kinds` (see [Input files](inputs.md)).
  - `repository`: a git repository's https address, to which the person may add a branch, tag or commit in the same answer, so **never ask for the ref in a question of its own**. The runner fetches it into the run's inputs before the run starts, so it needs no `egress` and no key (see [Input files](inputs.md#a-repository-or-a-file-at-an-address)). Name the question for what it holds (`repository`, `codebase`) [src: web:https://docs.secureagentry.net/runner/].
  - `url`: the https address of one file (a PDF, a page, a dataset), fetched by the runner the same way and read as an uploaded file is [src: web:https://docs.secureagentry.net/runner/]. **A `url` question listed in `capabilities.browser.sites` is the site a browser opens instead**, an `http://` or `https://` address the runner never downloads ([A browser](browser.md)).
  - `host`: a machine the person names when the run starts, as `ssh` takes it (`user@host`, with `:port` when it is not 22), for a QuickAgent that sets up, manages or diagnoses a server. **Its id is listed in `capabilities.targetHosts`**, it is always `required: true`, and the run setup asks for the sign-in beside it, so never ask for a password or a key in a question of its own (see [Target hosts](target-hosts.md)) [src: web:https://docs.secureagentry.net/agentry/target-hosts/].
  - `secret`: only for a name declared in `secrets`, upper-cased from the `id` (`id` `github_token` for `GITHUB_TOKEN`). It is injected as that secret and never recorded. A declared key needs no question to be asked for: the run setup asks for every declared key it does not hold. Its `required` matches the key: `true` for a required key, left out for an optional one (see [Keys](secrets.md)).
- `required`: `true` when the run cannot go on without the answer.

The answers to `text`, `choice`, `file`, `repository`, `url` and `host` questions are part of the run's receipts, so never ask for a password or a key in a `text` question. A private source's token is not an answer, nor is a target host's sign-in: the run setup asks for each beside the address and keeps it out of the receipts.

## The guide

The `guide` is what a person reads before a run. It is shown to people only, never to the run's agent, so it says what the person must know and do, not how the agent works [src: web:https://docs.secureagentry.net/quickagents/]:

- `overview`: what it does and what it gives back, in two or three sentences of Markdown.
- `prepare` (at most 5 things to gather first): a list, each item one plain line.
- `files`: what files to bring, if any, in one line.
- `task`: the task field's placeholder, "e.g. …", in one line.
- `connect`, an object with help per declared key in Markdown, each at most 2,000 characters: where to get the key and the permission it needs, in a sentence or a few numbered steps (`{"GITHUB_TOKEN": "Where to get it and the permission it needs."}`). Each key of `connect` must be a declared secret, the QuickAgent's own or a declared server's. Ask for the least permission the task needs (read-only when it only reads).

**Leave out a field that has nothing to say.** Never write `"None"`, `"n/a"`, `null` or an empty text for it: with no key there is no `connect`, with no files there is no `files`. The bounds of each field are on [The manifest](manifest.md#the-rules-bounds).
