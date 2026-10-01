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

## What it returns

`AGENT.md` and `notes.md`, written with `write_report`, and a final answer
in the shape `output.schema.json` describes: the manifest's fields, the
published QuickAgents that already do the job, and the questions the draft
leaves open. The application turns them into the draft you review.
