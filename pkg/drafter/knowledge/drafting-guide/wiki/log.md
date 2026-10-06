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
