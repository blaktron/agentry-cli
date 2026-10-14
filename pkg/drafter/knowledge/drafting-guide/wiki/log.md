# Log

Append-only: every change to the drafting guide, with what it took in. Written by hand with each release of the Drafter.

## [2026-10-05] ingest | the runner's code and the public docs (Drafter 1.8.0)

The guide's first pages: where to start, how a run works, and what a declared MCP server meets in the runner. The runtime facts are generated from the CLI's code.

- wrote: guide/start-here.md, runtime/how-a-run-works.md, servers/mcp-servers.md, sources/runtime-facts.md

## [2026-10-05] ingest | a survey of 50 MCP servers in the runner (Drafter 1.8.1)

The two soft lists, Runs here (24 servers) and Does not run here (26 servers and 9 dependencies), from a survey that installed and started 50 servers with the runner's own builder and gateway. The servers page gains the guide to judging a server on neither list and the user and HOME a server runs as.

- wrote: sources/servers.yaml, servers/runs-here.md, servers/does-not-run-here.md (generated from sources/servers.yaml)
- updated: servers/mcp-servers.md, guide/start-here.md

## [2026-10-05] ingest | the Drafter's instruction and the public docs (Drafter 1.8.2)

The reference half of the Drafter's instruction moves into the guide, and the instruction keeps the basics and a map of the pages. The manifest, its bounds, the commands, the network and fetch, the intake and the guide, the run modes and the person's knowledge each get a page, and so do writing the instructions and three worked drafts. Input files, keys and what checks a draft are short pages of today's facts, rewritten when those parts change.

- wrote: manifest/manifest.md, manifest/exec.md, manifest/egress-and-fetch.md, manifest/intake-and-guide.md, manifest/run-modes.md, manifest/knowledge-accepts.md, manifest/inputs.md, manifest/secrets.md, draft/instructions.md, draft/examples.md, draft/review.md
- updated: guide/start-here.md, servers/mcp-servers.md, sources/runtime-facts.md

## [2026-10-06] ingest | the post-step's list check (Drafter 1.8.3)

The check after a draft now holds each pin to the two lists: a pin *Does not run here* covers, or a server with a listed direct dependency, goes to the repair turn, and an npm install script is a note. An entry on *Does not run here* speaks for every version of its server unless its reason is bound to the version seen; a *Runs here* entry speaks for its own version. The notes say what admitted each server.

- updated: servers/mcp-servers.md, servers/runs-here.md, servers/does-not-run-here.md, draft/review.md

## [2026-10-06] ingest | the input cases and the inputs block (Drafter 1.9.0)

Input files rewritten from today's facts to the settled guide: the two views every input is read in (its text, or as uploaded), the manifest's `inputs` block (`kinds`, `view`) that says what a QuickAgent takes and which view it reads first, a table of the common cases, and how a draft asks for its files. Commands drops the archive readers' how-to: the runner unpacks archives and the input tools read every file, so no command is declared to read inputs, and the check after a draft sends an archive reader to the repair turn. How a run works says a run that cannot do its task ends as blocked.

- updated: manifest/inputs.md, manifest/exec.md, manifest/manifest.md, manifest/intake-and-guide.md, runtime/how-a-run-works.md

## [2026-10-06] ingest | optional and required keys (Drafter 1.10.0)

The Keys page is rewritten from today's facts to the settled guide: every declared key is marked optional (`optionalSecrets`) when the QuickAgent still does its job without it, and left required when it cannot, with examples of each, the instructions written for both cases, and a `secret` question's `required` matching its key (rule V10). It is no longer marked as today's facts. The manifest and intake pages name the field and the rule.

- updated: manifest/secrets.md, manifest/manifest.md, manifest/intake-and-guide.md

## [2026-10-12] ingest | target hosts (Drafter 1.14.0)

A new page, Target hosts: when a task works on a machine the person names, the draft asks for it with a `host` question listed in `capabilities.targetHosts`, declares no SSH client, egress or key for it, and writes the instructions for the host tools and the shell (look before changing, never lock the person out, list every change). Such a QuickAgent runs only on the person's computer. The intake, manifest, commands, run-modes and how-a-run-works pages name the type, the line and the bound; the worked drafts gain a fourth; the runtime facts gain the host tools and their limits; the check after a draft sends an SSH client in `exec` to the repair turn.

- wrote: manifest/target-hosts.md
- updated: guide/start-here.md, manifest/intake-and-guide.md, manifest/manifest.md, manifest/exec.md, manifest/run-modes.md, runtime/how-a-run-works.md, draft/examples.md, draft/review.md, sources/runtime-facts.md

## [2026-10-14] ingest | a browser (Drafter 1.15.0)

A new page, A browser: when a task tests a website the person names as a person sees it (pages, forms, links, a phone's width), the draft asks for the site with a `url` question listed in `capabilities.browser.sites`, adds `hosts` only for what its pages load, keeps `scope` at `site`, declares a key the browser types in `capabilities.secrets` and `capabilities.browser.secrets` (never with `scope: public`), brings no browser of its own, and writes the instructions for the browser tools (bare screenshot file names, nothing real changed on the site). It runs on the person's computer and In Agentry for a public site. The intake, manifest, commands, network, keys, how-a-run-works, start-here and review pages name the line; the worked drafts gain a fifth; the runtime facts gain the browser's tools and bounds; the check after a draft sends a browser of its own, as a command or an MCP server, to the repair turn.

- wrote: manifest/browser.md
- updated: guide/start-here.md, manifest/intake-and-guide.md, manifest/manifest.md, manifest/exec.md, manifest/egress-and-fetch.md, manifest/secrets.md, runtime/how-a-run-works.md, draft/examples.md, draft/review.md, sources/runtime-facts.md
