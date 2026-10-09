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
It covers how a QuickAgent run works and what a declared MCP server meets in
the runner: musl runtimes, no install scripts, no launch arguments, stdio
only, none of the run's files, and only port 443 to its declared hosts. It
also has a checklist for a server before it is pinned, and two lists of
servers: *Runs here*, the servers a survey saw install, start and list their
tools in the runner, and *Does not run here*, the ones it saw fail, each
with the reason, the runtime and the date. The lists are one thing for the
Drafter to look at, not a judgement of any server's safety or quality
(`knowledge/drafting-guide/sources/servers.yaml`; the survey is
`scripts/mcp-server-survey` in the CLI's repository).

Since 1.8.2 the guide also holds the reference the Drafter's instruction
used to carry on every turn: every field of the manifest and the rules'
bounds, the commands every run provides, the network and the `fetch` tool,
the intake and the guide, run modes, a person's knowledge at run time, how
to write a QuickAgent's instructions, and three worked drafts. `AGENT.md`
keeps the role, the rules for deciding, the steps, each naming the pages it
needs, and the shape of the answer. The checks after a draft hold every rule
they check, whatever the Drafter read.

Since 1.8.3 the checks after a draft hold each MCP server pin to the two
lists. A pin on *Does not run here*, or a server whose own direct
dependencies (from the package metadata the check of its version already
reads) include a package the list names, gets the one repair turn, like any
rule the draft breaks. An npm install script is noted for you to check
before a run, not refused. The notes say what admitted each server the
draft keeps: its *Runs here* entry, or the Drafter's judgement with the
guide for a server on neither list. The lists stay soft: nothing outside
the Drafter reads them, and a server on neither list may be pinned. The
check sends nothing new off your computer.

Since 1.9.0 the guide teaches how a QuickAgent takes its files: every input
is read in its text or as uploaded, the runner unpacks archives, and the
manifest's `inputs` block says what kinds of file to bring and which view
to read first. A draft that takes files declares the block, writes
`guide.files` to match, and declares no command to read its inputs. The
checks after a draft correct the block's slips (a kind in the singular,
`any` beside named kinds, the block under `capabilities`) and send any
archive reader to the repair turn. A release that declares the block needs
CLI v0.6.73 or later to run; an older CLI refuses its manifest.

Since 1.10.0 the Drafter marks each key a draft declares: optional
(`capabilities.optionalSecrets`) when the QuickAgent still does its job
without it, required when it cannot. Its instructions say what to do when
an optional key was not given, and a `secret` question's `required` matches
its key. The Builders on the site and in the desktop show the marking, and
the author can change it. A release that marks a key optional needs CLI
v0.6.66 or later to run; an older CLI refuses its manifest.

Since 1.11.0 the Drafter may declare a vendor's own remote MCP server by its
address, and prefers one to a package for the same service. A remote server
signs in by OAuth, by one key sent as a bearer token, or not at all, and an
OAuth one asks whoever runs the QuickAgent to sign in at the vendor at the
first run of each version. The draft's notes say so, and say whose OAuth
client the vendor needs. A package server signs in only with a sign-in its
guide lists as seen working, copied exactly: the checks after a draft send
any other `oauth` block to the repair turn. The guide's
`servers/sign-ins.md` lists the vendors' servers and the sign-ins. A release
with a remote server needs CLI v0.6.69 or later to run, one signed in to by
OAuth CLI v0.6.70, and a package's sign-in CLI v0.6.71; an older CLI refuses
its manifest.

The runner attaches the guide to every run of the Drafter, through the same
knowledge tools, the repair turn included. It is part of this release, so
the registry scans it with the published release. It is not one of your
packs and does not count against a draft's limit of packs, but a pack of
yours with the same name is refused. An install of this release from the
registry is an ordinary QuickAgent and carries the pack like any release
that ships one.

## What it returns

`AGENT.md` and `notes.md`, written with `write_report`, and a final answer
in the shape `output.schema.json` describes: the manifest's fields, the
published QuickAgents that already do the job, and the questions the draft
leaves open. The application turns them into the draft you review.
