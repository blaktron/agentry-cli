---
title: Input files
type: guide
summary: How the person's files reach a run today (read-only, as text, through the input tools), how a draft asks for them, and the limits of a run In Agentry. Today's facts; rewritten when the manifest can declare how a QuickAgent takes its files.
sources: [web:https://docs.secureagentry.net/runner/, web:https://docs.secureagentry.net/quickagents/, web:https://docs.secureagentry.net/agentry/hosted-runs/]
updated: 2026-10-05
---

# Input files

This page states how input files work today. It is rewritten when the manifest gains a declaration of how a QuickAgent takes its files.

## How files reach a run

- The person adds files when they start a run, and they reach it read-only under `/inputs`. No capability is needed for them: `filesystem.read` is for the person's workspace, not for input files [src: web:https://docs.secureagentry.net/quickagents/].
- The run reads them through `list_inputs`, `read_input` and `search_inputs`, which every run has. Documents, PDFs and images are turned into text before the run starts, and archives are unpacked, so the agent reads text without a command [src: web:https://docs.secureagentry.net/runner/].
- The answers to the intake's questions arrive as an input too, and so does a file an intake question of type `file` asks for.
- A run In Agentry takes at most twenty files of at most 10 MB each [src: web:https://docs.secureagentry.net/agentry/hosted-runs/].
- A declared MCP server sees none of the input files: they are not in its container (see [Declared MCP servers](../servers/mcp-servers.md)).

## How a draft asks for them

- Say what to bring in `guide.files`, one line naming the files and their formats ("Add the lockfiles: package-lock.json, go.sum").
- Use an intake question of type `file` only for one particular file the instructions must tell apart from the others (a contract and a playbook, a before and an after).
- Ask for a document as it is (a Word file, a PDF, a spreadsheet). Never ask the person to convert it to text first.
- The instructions' `## Inputs` say what each file means and what to do when one is missing or in an unexpected format.
