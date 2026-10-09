---
title: The manifest
type: guide
summary: Every field of the manifest a draft returns, what to write in each, and the rules' bounds by value. Check the draft against the bounds before you return it.
sources: [web:https://docs.secureagentry.net/quickagents/, doc:runtime-facts.md]
updated: 2026-10-09
---

# The manifest

`manifest` in your answer is the whole `agent.yaml` as a JSON object, in manifest schema v2. The schema is closed: a field it does not know is refused, so write only the fields below, and leave out a field that has nothing to say [src: web:https://docs.secureagentry.net/quickagents/].

## The fields

- `schemaVersion`: `"2"`.
- `name`: lowercase letters, digits and hyphens, from the task (`pr-review-summary`), at most 65 characters, starting and ending with a letter or a digit.
- `namespace`: `"draft"`; the Builder sets the person's own. `version`: `"0.1.0"`. A revision keeps both values too: the Builder sets the person's namespace and the next version.
- `summary`: one plain sentence saying what the QuickAgent does (the bounds below).
- `license`: `"Apache-2.0"` unless the description names another SPDX licence.
- `tags`: from the task, lowercase letters, digits and hyphens.
- `modes`: `["autonomous"]`, `["interactive"]` or both; `interactive.opening` with `interactive`. See [Run modes](run-modes.md).
- `entrypoints`: `{"instruction": "AGENT.md"}`.
- `intake`: the questions asked before each run, each `{id, prompt, type, required}`. See [The intake and the guide](intake-and-guide.md).
- `guide`: what a person reads before a run. See [The intake and the guide](intake-and-guide.md).
- `inputs`: what files the QuickAgent takes and which view it reads them in first, `{"kinds": [...], "view": "text" | "original" | "both"}`, a field of the manifest itself and never under `capabilities`; left out when it takes no files. See [Input files](inputs.md).
- `capabilities`: the least contract that does the task, line by line below.
- `knowledge`: a field of the manifest itself, never under `capabilities`, and only `accepts`. See [The person's knowledge at run time](knowledge-accepts.md).
- `publish`: `{"include": ["agent.yaml", "AGENT.md", "README.md", "LICENSE"]}`. The Builder writes the README and the licence file.

## The capabilities

Every capability is shown to the person, asked for at install and scanned. Declare only what the instructions use [src: web:https://docs.secureagentry.net/quickagents/]:

- `network.egress`: `none`, or the exact DNS hosts the QuickAgent's own `fetch` calls. See [Network and fetch](egress-and-fetch.md).
- `filesystem.read` and `filesystem.write`: `none`. Input files arrive under `/inputs` without a grant, and reports go out through `write_report` without one (see [Input files](inputs.md)). Use `workspace` or `{paths: [...]}` only when the instructions truly work on the person's workspace, which only a run on their computer has.
- `exec`: `none`, or `{commands: [...]}` from the commands every run provides. See [Commands](exec.md).
- `secrets`: the environment variable names of the keys the QuickAgent's own `fetch` uses. A key an MCP server needs goes in that server's `secrets`. See [Keys](secrets.md).
- `optionalSecrets`: the declared keys, its own or a server's, a run can start without. Every other declared key is required. See [Keys](secrets.md).
- `dataAccess`: from `email`, `calendar`, `files`, `tickets`, `code`, `messages`, `payments`, `other`, only what the task touches.
- `mcpServers`: a package server is `{"npm" or "pypi": package, "version": exact, "tools": [...], "egress": [hosts], "secrets": [NAMES], "env": {NAME: value}}`, the keys it does not need left out, and an `"oauth"` block only as copied from [Remote servers and sign-ins](../servers/sign-ins.md#package-sign-ins). A vendor's remote server is `{"url": address, "auth": "oauth" or "secret" or "none", "scopes": [...], "tools": [...]}`, with `"secrets": [one NAME]` only for `"auth": "secret"` and `"scopes"` only for `"auth": "oauth"`. See [Declared MCP servers](../servers/mcp-servers.md) before you declare one.

## The rules' bounds

The registry refuses a manifest past any of these. Count before you return [src: web:https://docs.secureagentry.net/quickagents/].

- `summary`: one line of at most 140 characters; aim for about 100.
- `tags`: at most 10, each named once.
- `guide.overview`: at most 4,096 bytes.
- `guide.prepare`: each item one line of at most 200 characters.
- `guide.files`: one line of at most 500 characters.
- `guide.task`: one line of at most 200 characters.
- `inputs.kinds`: from `documents`, `code`, `data`, `images`, each named once, or `any` alone; `inputs.view`: `text`, `original` or `both`.
- `guide.connect`: each key's help at most 2,000 characters.
- `interactive.opening`: at most 500 characters.
- `knowledge.accepts.prompt`: one line of at most 300 characters.
- `capabilities.mcpServers`: at most 8 servers, remote ones included; each package pinned at an exact version. A remote server's address is `https://` on a public host name and port 443, with no user name, query or fragment, at most 512 characters, and it declares at most 32 scopes [src: doc:runtime-facts.md; web:https://docs.secureagentry.net/quickagents/].

Your own limits are tighter than the rules' on some of these (the intake, `prepare`, the notes) and are in your instruction, under *Decide, don't ask*.

A whole draft, field by field, is on [Worked drafts](../draft/examples.md).
