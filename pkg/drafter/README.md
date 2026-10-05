# The Drafter

The Drafter is the QuickAgent behind **Describe it** in the Agentry Builder.
You describe a task in plain words, and it drafts a QuickAgent for that task:
a manifest (`agent.yaml`), an instruction file (`AGENT.md`) and short notes
on what it chose and why. It publishes nothing. You read the draft and decide
whether to submit it, and a submission is scanned like any other release.

## This is the package that runs

The Drafter is built into the Agentry CLI and the desktop app, and it runs
from there: on your computer when you run `agentry draft`, and on an Agentry
sandbox host when you use Describe it on the site or in Agentry. This release
has the same bytes. The registry built its archive from the package the CLI
carries, and the archive's digest is the one the CLI reports. Run
`agentry builtins` to print it and compare it with the digest on this page.

Installing this release does not give you the Drafter. The application runs
the Drafter as a built-in, with the tools listed below. If you install this
release, it runs as an ordinary QuickAgent with only what its manifest
declares.

## What it is given beyond its manifest

The manifest declares no network, no files and no commands. When Agentry
runs the Drafter, it grants three tools that no other QuickAgent can have:

- `registry_search`: searches Agentry's published QuickAgents, reads one
  QuickAgent's manifest, and reads the registry's vulnerability check of an
  exact MCP server version.
- `web_search`: searches the web. A draft has this tool only when a search
  provider is configured.
- `web_read`: reads one public web page as text.

AI Review reads every search result, page and registry answer before the
Drafter's model does. Anything it flags, or could not scan, is withheld from
the model, and the draft's notes list what was withheld. AI Review also reads
the description before the run starts and checks the finished draft.

When you give a draft your knowledge packs, it also has the three knowledge
tools every run with a pack has (`knowledge_list`, `knowledge_search`,
`knowledge_read`). It reads the packs before the web, and never ships them.
Nothing of a pack reaches its model unscanned: a pack Agentry scanned is read
as that scan left it, and any other pack is scanned by AI Review before the
draft starts. What a scan flagged is withheld, and the notes name it.

## Its drafting guide

The Drafter ships a knowledge pack of its own, `drafting-guide`, under
`knowledge/drafting-guide/` in this release, declared in `knowledge.ships`.
It covers how a QuickAgent run works and what a declared MCP server meets
in the runner: musl runtimes, no install scripts, no launch arguments,
stdio only, none of the run's files, and only port 443 to its declared
hosts. It also has a checklist for a server before it is pinned. The runner
attaches it to every run of the Drafter, through the same knowledge tools,
the repair turn included. It is part of this release, so the registry
scans it with the published release. It is not one of your packs and does
not count against a draft's limit of packs, but a pack of yours with the
same name is refused. An
install of this release from the registry is an ordinary QuickAgent and
carries the pack like any release that ships one.

## What it returns

`AGENT.md` and `notes.md`, written with `write_report`, and a final answer
in the shape `output.schema.json` describes: the manifest's fields, the
published QuickAgents that already do the job, and the questions the draft
leaves open. The application turns them into the draft you review.
