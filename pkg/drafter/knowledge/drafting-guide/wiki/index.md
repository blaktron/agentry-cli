---
title: Index
type: index
summary: Every page in drafting-guide, one line each, by section. Generated; do not edit by hand.
---

# drafting-guide

17 pages.

## Draft

- [Worked drafts](draft/examples.md) — Three whole drafts, each from a description to the manifest, the shape of its instructions and its notes; one with no tools, one with fetch and a key, and one with a declared MCP server, each modelled on one of Agentry's first-party example QuickAgents.
- [Writing the instructions](draft/instructions.md) — How to write the AGENT.md a draft returns, the instruction its QuickAgent's agent follows; the six headings in order, what each holds, how much to write, and what to leave out because the System Prompt already says it.
- [What checks a draft](draft/review.md) — The checks a draft meets after you answer (the rules, AI Review's check of the finished draft, one repair turn) and the scan that decides whether a submitted QuickAgent publishes, goes to review or is rejected. Today's facts; rewritten when the scan's rules on describing risk change.

## Guide

- [Start here](guide/start-here.md) — What this guide is, that it is yours and not the person's, and which page to read at each step of a draft.

## Manifest

- [Network and fetch](manifest/egress-and-fetch.md) — When a QuickAgent needs the network, how its own fetch tool works (methods, keys, redirects, the response cap, saving a body to the scratch folder), how to choose and name its hosts, and which hosts go on an MCP server instead.
- [Commands](manifest/exec.md) — When a QuickAgent declares commands at all (computation the tools cannot do, never reading its inputs), the base commands every run provides, where a command can write, what a run In Agentry provides, and Unsafe Mode on the desktop.
- [Input files](manifest/inputs.md) — How the person's files reach a run, a repository or a file at an address that the runner fetches for it, the two views the input tools read them in, the manifest's inputs block that says what a QuickAgent takes and which view it reads first, the cases and what each declares, and how a draft asks for its files.
- [The intake and the guide](manifest/intake-and-guide.md) — The questions a QuickAgent asks before each run (types, a repository or a file at an address among them, ids, when to ask at all) and the guide a person reads before a run (overview, prepare, files, task, connect), with their bounds and the fields to leave out.
- [The person's knowledge at run time](manifest/knowledge-accepts.md) — When a QuickAgent asks the person to attach a knowledge pack of their own at run time (knowledge.accepts), how to write its prompt, and what goes into the instructions instead. A draft never ships a pack.
- [The manifest](manifest/manifest.md) — Every field of the manifest a draft returns, what to write in each, and the rules' bounds by value. Check the draft against the bounds before you return it.
- [Run modes](manifest/run-modes.md) — Autonomous and interactive runs, when a draft adds interactive, what a conversation can and cannot do, and the opening line.
- [Keys](manifest/secrets.md) — How a QuickAgent declares the keys it needs (its own for fetch, a server's on that server), how to mark each one optional or required, how a run gets them without the model seeing them, and what a run does without an optional one.

## Runtime

- [How a QuickAgent run works](runtime/how-a-run-works.md) — What a run of the QuickAgent you draft can and cannot do, what the System Prompt already tells it, and how a run on the person's computer, a run In Agentry and a desktop run in Unsafe Mode differ.

## Servers

- [Does not run here](servers/does-not-run-here.md) — Declared MCP servers seen to fail in the runner, and packages whose presence as a direct dependency means a server cannot run, each with the reason, the runtime and the date. Generated from servers.yaml; do not edit by hand.
- [Declared MCP servers](servers/mcp-servers.md) — What a declared MCP server meets in the runner (a package installed, started and reached, or a vendor's remote server reached at its address), what that rules out, how a server signs in, and how to check a server before you pin it. Read it before you declare any server.
- [Runs here](servers/runs-here.md) — Declared MCP servers seen to install, start and list their tools in the runner, each with the versions, the runtime, the date and what a QuickAgent must declare for it. Generated from servers.yaml; do not edit by hand.
- [Remote servers and sign-ins](servers/sign-ins.md) — Vendors' own remote MCP servers, by address and how each signs in, and the package servers' OAuth sign-ins seen working, which a draft copies exactly. Generated from servers.yaml; do not edit by hand.
