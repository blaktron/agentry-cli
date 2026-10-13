---
title: Start here
type: guide
summary: What this guide is, that it is yours and not the person's, and which page to read at each step of a draft.
sources: [web:https://docs.secureagentry.net/runner/]
updated: 2026-10-12
---

# Start here

This is the Drafter's drafting guide, the knowledge pack `drafting-guide`. It ships with the Drafter and is attached to every draft, revision and repair turn. It is not one of the person's knowledge packs:

- Never cite its pages in a draft's `sources`. They describe how to draft, not the person's task.
- Never declare `knowledge.accepts` because of it. That field asks the person for a pack of their own at run time.
- When the task names the person's packs, name a pack in `knowledge_list` to get its index. This guide's index lists only its own pages.

## Which page, when

| Step of the draft | Read |
| --- | --- |
| Before you choose tools: what a run can and cannot do | [How a QuickAgent run works](../runtime/how-a-run-works.md) |
| Choosing between no tool, `fetch` and a server | [Network and fetch](../manifest/egress-and-fetch.md) |
| Before you pin any MCP server | [Declared MCP servers](../servers/mcp-servers.md), then the lists [Runs here](../servers/runs-here.md) and [Does not run here](../servers/does-not-run-here.md) |
| Before you declare a vendor's remote server or a package's sign-in | [Remote servers and sign-ins](../servers/sign-ins.md) |
| A task on a server the person names (set up, manage, diagnose) | [Target hosts](../manifest/target-hosts.md) |
| Writing the instructions | [Writing the instructions](../draft/instructions.md), and [Worked drafts](../draft/examples.md) for whole examples |
| Declaring the contract | [The manifest](../manifest/manifest.md), then [Commands](../manifest/exec.md), [Keys](../manifest/secrets.md) and [Input files](../manifest/inputs.md) as the task needs them |
| Writing the intake and the guide | [The intake and the guide](../manifest/intake-and-guide.md); [Run modes](../manifest/run-modes.md) for a conversation; [The person's knowledge at run time](../manifest/knowledge-accepts.md) when a run needs the person's own pack |
| Before you return | the bounds on [The manifest](../manifest/manifest.md#the-rules-bounds), and [What checks a draft](../draft/review.md) |

A rule the guide states is also checked after you answer. A draft that breaks one gets one repair turn, and a draft that still breaks it carries a note for the person. Reading the page first is cheaper than the repair [src: web:https://docs.secureagentry.net/runner/].
