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

## [2026-10-14] refactor | evidence without internal references (Drafter 1.15.1)

The server lists' evidence lines say what was seen and when, instead of naming internal tickets, and the sources file's comments describe the file. No fact about a server changed.

- updated: sources/servers.yaml, servers/does-not-run-here.md, servers/sign-ins.md (both generated)

## [2026-10-15] update | key-free first, and no hosted browser (Drafter 1.15.2)

A draft asked to run a website test suite declared a hosted browser service's remote server with a token, instead of the browser the runner gives. The servers page gains a section on choosing a server or an API that needs a key, an account or a paid plan only when nothing key-free does the task, with the notes saying why; the browser, keys, fetch and review pages say the same and name the hosted browser services, which the check after an answer now sends to a repair turn by package or by address.

- updated: servers/mcp-servers.md, manifest/browser.md, manifest/secrets.md, manifest/egress-and-fetch.md, draft/review.md

## [2026-10-16] update | PyPI servers run on glibc (Drafter 1.15.3)

PyPI servers now install and run on Debian (glibc), Python 3.12.15, so a dependency's `manylinux` wheels install. The servers page says the npm runtime is still musl and the PyPI runtime glibc, and that a PyPI dependency now stops the install only when it publishes no `manylinux` wheel. markitdown-mcp and opensearch-mcp-server-py, and the dependencies onnxruntime, magika and scikit-learn, come off *Does not run here*: each was listed only because musl had no wheel for it. The Python runtime's entry names the new image and what it holds.

- updated: servers/mcp-servers.md (also: what else a runtime holds), sources/servers.yaml, sources/runtime-facts.md, servers/does-not-run-here.md (generated)

## [2026-10-17] update | deliverables (Drafter 1.15.4)

A run's deliverables are the files it was for, and it marks them: `deliverable: true` and a one-line `title` on the `write_report` call that writes the file, or on `export_pdf` for a PDF. The how-a-run-works page says so and gains a section on a result that is a file, the instructions page tells a draft's `## Output` to name the file and have the run mark it, and the runtime facts state the two arguments and the runner's pick when a run marks none.

- updated: runtime/how-a-run-works.md, draft/instructions.md, sources/runtime-facts.md

## [2026-10-18] ingest | a browser run's budget (Drafter 1.15.5)

A browser page section, Fitting the run's budget, from the first live runs of browser QuickAgents, which ran out of tool calls or time before writing a report: say the budget in the instructions, one `browser_evaluate` per page, every fetch in a page script bounded (a timeout, a few at a time, a cap, no credentials), never the same call five times running, the report written early and rewritten after each step, and only the site opened. The runtime facts gain a run's budget, on the person's computer (100 tool calls, the CLI's default policy, raised from 60 in the same release) and In Agentry by tier (Pro's raised to 100 too), and docker-agent's limit on identical calls; AGENT.md's step 4 names it; the check after a draft sends a page fetch with no timeout, or a key pressed five or more times running, to the repair turn.

- updated: manifest/browser.md, sources/runtime-facts.md

## [2026-10-20] ingest | a shipped pack (Drafter 1.17.0)

A draft ships the packs its person chose to ship, and the task names them. A new page, Writing for a shipped pack, teaches what that changes: declare each pack in `knowledge.ships` and add `knowledge/**` to `publish.include`, write the instructions to say when to look in the pack at `/knowledge/<name>` and what to look for, cite the pages relied on, say what to do when the pack is silent, read a pack's procedure pages as reference that the instructions point to, and add one note that the pack ships. The page on the person's knowledge at run time no longer says a draft never ships a pack: it ships only the packs the task names, and the post-step removes any other entry, adds a chosen one the draft left out, and puts `knowledge/**` in `publish.include`.

- added: manifest/knowledge-ships.md
- updated: manifest/knowledge-accepts.md, guide/start-here.md

## [2026-10-21] ingest | what the scan holds, by declaration (Drafter 1.17.1)

What checks a draft is no longer a page of today's facts awaiting a rewrite: the scan rates S4's hits about secrets, sending data and destroying it by what the manifest declares (high only for an undeclared host, key or program; medium or low for a declared one, which publishes), a line that describes an action without telling the agent to take it is not a hit, and AI Review reads what the instructions tell the agent to do. The injection techniques still hold whatever is declared, so the advice never to quote a working attack string stays.

- updated: draft/review.md, index.md

